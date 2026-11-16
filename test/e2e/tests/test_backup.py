# Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License"). You may
# not use this file except in compliance with the License. A copy of the
# License is located at
#
# 	 http://aws.amazon.com/apache2.0/
#
# or in the "license" file accompanying this file. This file is distributed
# on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
# express or implied. See the License for the specific language governing
# permissions and limitations under the License.

"""Integration tests for the FSx Backup resource.

A user-initiated Lustre backup needs a PERSISTENT file system that is not
linked to a data repository, so the module stands up its own PERSISTENT_2
FileSystem CR from the shared file_system template (it does not share the DRA
test's file system) and points the Backup at it through fileSystemRef.

As with the DRA test, the lifecycle is one test so pytest-xdist cannot reorder
the steps or build a second 1200 GiB file system per step.
"""

import logging
import time

import pytest

from acktest import tags
from acktest.k8s import condition
from acktest.k8s import resource as k8s
from acktest.resources import random_suffix_name

from e2e import CRD_GROUP, CRD_VERSION, load_fsx_resource, service_marker
from e2e.replacement_values import REPLACEMENT_VALUES
from e2e.tests.helper import (
    BackupValidator,
    FSxValidator,
    wait_for_synced_or_terminal,
)

FS_RESOURCE_PLURAL = "filesystems"
BACKUP_RESOURCE_PLURAL = "backups"

FS_STORAGE_CAPACITY = 1200
FS_MAINTENANCE_WINDOW = "2:03:00"

TAG_KEY = "ack-fsx-e2e"
INITIAL_TAG_VALUE = "initial"
UPDATED_TAG_VALUE = "updated"
SECOND_TAG_KEY = "ack-fsx-e2e-second"
SECOND_TAG_VALUE = "second"
NEW_TAG_KEY = "ack-fsx-e2e-new"
NEW_TAG_VALUE = "new"

INITIAL_TAGS = {TAG_KEY: INITIAL_TAG_VALUE, SECOND_TAG_KEY: SECOND_TAG_VALUE}
# TAG_KEY changed, SECOND_TAG_KEY removed, NEW_TAG_KEY added: exercises both
# TagResource and UntagResource.
UPDATED_TAGS = {TAG_KEY: UPDATED_TAG_VALUE, NEW_TAG_KEY: NEW_TAG_VALUE}

# File system: 5-10 min to AVAILABLE and 5-10 min to disappear.
FS_CREATE_WAIT_PERIODS = 40
FS_CREATE_PERIOD_LENGTH = 30
FS_DELETE_WAIT_PERIODS = 30
FS_DELETE_PERIOD_LENGTH = 30

# Lustre backups go PENDING -> CREATING -> TRANSFERRING -> AVAILABLE, which can
# take well over ten minutes even for an empty file system.
CREATE_WAIT_AFTER_SECONDS = 30
BACKUP_CREATE_WAIT_PERIODS = 120
BACKUP_CREATE_PERIOD_LENGTH = 15
MODIFY_WAIT_AFTER_SECONDS = 15
BACKUP_MODIFY_WAIT_PERIODS = 40
BACKUP_MODIFY_PERIOD_LENGTH = 15
# DeleteBackup returns instantly and the backup leaves DescribeBackups.
BACKUP_DELETE_WAIT_PERIODS = 20
BACKUP_DELETE_PERIOD_LENGTH = 15


def _wait_for_backup_synced(ref, wait_periods, period_length):
    """wait_for_synced_or_terminal, but also fails as soon as the backup
    reports FAILED (which the controller leaves retryable) and surfaces FSx's
    FailureDetails.
    """
    for _ in range(wait_periods):
        cr = k8s.get_resource(ref) or {}
        status = cr.get("status", {})
        if status.get("lifecycle") == "FAILED":
            raise AssertionError(
                f"{ref.name} reached lifecycle FAILED: "
                f"{status.get('failureDetails')}"
            )
        conditions = status.get("conditions") or []
        by_type = {c["type"]: c for c in conditions}
        synced = by_type.get(condition.CONDITION_TYPE_RESOURCE_SYNCED)
        if synced is not None and synced.get("status") == "True":
            return
        terminal = by_type.get(condition.CONDITION_TYPE_TERMINAL)
        if terminal is not None and terminal.get("status") == "True":
            raise AssertionError(
                f"{ref.name} went terminal: {terminal.get('message')}"
            )
        time.sleep(period_length)
    cr = k8s.get_resource(ref) or {}
    raise AssertionError(
        f"{ref.name} did not sync within {wait_periods * period_length}s; "
        f"lifecycle {cr.get('status', {}).get('lifecycle')}"
    )


def _delete_cr(ref, wait_periods, period_length):
    if k8s.get_resource_exists(ref):
        _, deleted = k8s.delete_custom_resource(
            ref, wait_periods=wait_periods, period_length=period_length,
        )
        assert deleted
        assert not k8s.get_resource_exists(ref)


@pytest.fixture(scope="module")
def lustre_file_system(fsx_client):
    resource_name = random_suffix_name("ack-fsx-backup-fs", 32)

    replacements = REPLACEMENT_VALUES.copy()
    replacements["FILE_SYSTEM_NAME"] = resource_name
    replacements["STORAGE_CAPACITY"] = str(FS_STORAGE_CAPACITY)
    replacements["WEEKLY_MAINTENANCE_START_TIME"] = FS_MAINTENANCE_WINDOW
    replacements["TAG_KEY"] = TAG_KEY
    replacements["TAG_VALUE"] = INITIAL_TAG_VALUE
    replacements["SECOND_TAG_KEY"] = SECOND_TAG_KEY
    replacements["SECOND_TAG_VALUE"] = SECOND_TAG_VALUE

    resource_data = load_fsx_resource(
        "file_system", additional_replacements=replacements,
    )
    ref = k8s.CustomResourceReference(
        CRD_GROUP, CRD_VERSION, FS_RESOURCE_PLURAL,
        resource_name, namespace="default",
    )
    k8s.create_custom_resource(ref, resource_data)

    # Wrapped so a mid-setup failure still tears down the 1200 GiB file system.
    file_system_id = None
    try:
        k8s.wait_resource_consumed_by_controller(ref)
        wait_for_synced_or_terminal(
            ref,
            wait_periods=FS_CREATE_WAIT_PERIODS,
            period_length=FS_CREATE_PERIOD_LENGTH,
        )
        file_system_id = k8s.get_resource(ref)["status"]["id"]
        yield (resource_name, file_system_id)
    finally:
        if file_system_id is None and k8s.get_resource_exists(ref):
            cr = k8s.get_resource(ref) or {}
            file_system_id = cr.get("status", {}).get("id")
        _delete_cr(ref, FS_DELETE_WAIT_PERIODS, FS_DELETE_PERIOD_LENGTH)
        if file_system_id is not None:
            assert FSxValidator(fsx_client).wait_until_gone(file_system_id)


@pytest.fixture(scope="module")
def backup(fsx_client, lustre_file_system):
    """Creates the Backup and waits for sync. Teardown runs before the file
    system's; test_crud normally deletes the backup itself.
    """
    (file_system_name, file_system_id) = lustre_file_system
    resource_name = random_suffix_name("ack-fsx-backup", 32)

    replacements = REPLACEMENT_VALUES.copy()
    replacements["BACKUP_NAME"] = resource_name
    replacements["FILE_SYSTEM_NAME"] = file_system_name
    replacements["TAG_KEY"] = TAG_KEY
    replacements["TAG_VALUE"] = INITIAL_TAG_VALUE
    replacements["SECOND_TAG_KEY"] = SECOND_TAG_KEY
    replacements["SECOND_TAG_VALUE"] = SECOND_TAG_VALUE

    resource_data = load_fsx_resource(
        "backup", additional_replacements=replacements,
    )
    logging.debug(resource_data)

    ref = k8s.CustomResourceReference(
        CRD_GROUP, CRD_VERSION, BACKUP_RESOURCE_PLURAL,
        resource_name, namespace="default",
    )
    k8s.create_custom_resource(ref, resource_data)

    backup_id = None
    try:
        time.sleep(CREATE_WAIT_AFTER_SECONDS)
        cr = k8s.wait_resource_consumed_by_controller(ref)
        assert cr is not None
        cr = k8s.get_resource(ref) or {}
        backup_id = cr.get("status", {}).get("id")
        _wait_for_backup_synced(
            ref,
            wait_periods=BACKUP_CREATE_WAIT_PERIODS,
            period_length=BACKUP_CREATE_PERIOD_LENGTH,
        )
        backup_id = k8s.get_resource(ref)["status"]["id"]
        yield (ref, file_system_id, backup_id)
    finally:
        if backup_id is None and k8s.get_resource_exists(ref):
            cr = k8s.get_resource(ref) or {}
            backup_id = cr.get("status", {}).get("id")
        _delete_cr(ref, BACKUP_DELETE_WAIT_PERIODS, BACKUP_DELETE_PERIOD_LENGTH)
        if backup_id is not None:
            assert BackupValidator(fsx_client).wait_until_gone(backup_id)


@service_marker
class TestBackup:
    def test_crud(self, fsx_client, backup):
        (ref, file_system_id, backup_id) = backup
        validator = BackupValidator(fsx_client)
        self._assert_created(ref, validator, file_system_id, backup_id)
        self._update_tags_and_assert(ref, validator, backup_id)
        self._delete_and_assert(ref, validator, backup_id)

    def _assert_created(self, ref, validator, file_system_id, backup_id):
        assert backup_id is not None

        # Verify via the CR.
        condition.assert_synced(ref)
        cr = k8s.get_resource(ref)
        status = cr["status"]
        assert status["lifecycle"] == "AVAILABLE"
        assert status["type"] == "USER_INITIATED"
        assert status["ackResourceMetadata"]["arn"] is not None
        assert status["fileSystem"]["fileSystemID"] == file_system_id
        # The runtime clears a reference-resolved field before persisting the
        # spec; the AWS check below proves the reference resolved.
        assert cr["spec"]["fileSystemRef"]["from"]["name"]
        tags.assert_equal(
            expected=INITIAL_TAGS, actual=cr["spec"]["tags"],
            key_member_name="key", value_member_name="value",
        )

        # Verify via the AWS API.
        aws_backup = validator.get(backup_id)
        assert aws_backup is not None
        assert aws_backup["Lifecycle"] == "AVAILABLE"
        assert aws_backup["Type"] == "USER_INITIATED"
        assert aws_backup["FileSystem"]["FileSystemId"] == file_system_id
        assert aws_backup["ResourceARN"] == status["ackResourceMetadata"]["arn"]
        aws_tags = validator.get_tags(backup_id)
        tags.assert_ack_system_tags(tags=aws_tags)
        tags.assert_equal_without_ack_tags(expected=INITIAL_TAGS, actual=aws_tags)

    def _update_tags_and_assert(self, ref, validator, backup_id):
        """Tags are the only mutable field and go through
        TagResource/UntagResource (FSx has no UpdateBackup). The ACK system
        tags must survive the removal.
        """
        k8s.patch_custom_resource(ref, {
            "spec": {"tags": [
                {"key": TAG_KEY, "value": UPDATED_TAG_VALUE},
                {"key": NEW_TAG_KEY, "value": NEW_TAG_VALUE},
            ]},
        })
        time.sleep(MODIFY_WAIT_AFTER_SECONDS)

        # Exact match on the non-ACK tags, so this also waits for the removed
        # key to disappear.
        assert validator.wait_until(
            backup_id,
            lambda b: tags.clean(
                {t["Key"]: t["Value"] for t in b.get("Tags", [])}
            ) == UPDATED_TAGS,
            "re-tagged",
            wait_periods=BACKUP_MODIFY_WAIT_PERIODS,
            period_length=BACKUP_MODIFY_PERIOD_LENGTH,
        )
        assert k8s.wait_on_condition(
            ref, condition.CONDITION_TYPE_RESOURCE_SYNCED, "True",
            wait_periods=BACKUP_MODIFY_WAIT_PERIODS,
            period_length=BACKUP_MODIFY_PERIOD_LENGTH,
        )
        # A tags-only update must not go terminal (no NotImplemented stub).
        cr = k8s.get_resource(ref)
        for c in cr["status"].get("conditions") or []:
            if c["type"] == condition.CONDITION_TYPE_TERMINAL:
                assert c["status"] != "True", c.get("message")

        aws_tags = validator.get_tags(backup_id)
        tags.assert_ack_system_tags(tags=aws_tags)
        tags.assert_equal_without_ack_tags(expected=UPDATED_TAGS, actual=aws_tags)
        tags.assert_equal(
            expected=UPDATED_TAGS, actual=cr["spec"]["tags"],
            key_member_name="key", value_member_name="value",
        )

    def _delete_and_assert(self, ref, validator, backup_id):
        _delete_cr(ref, BACKUP_DELETE_WAIT_PERIODS, BACKUP_DELETE_PERIOD_LENGTH)
        # DeleteBackup is synchronous: DescribeBackups should report
        # BackupNotFound right away (allow a short consistency window).
        assert validator.wait_until_gone(
            backup_id, wait_periods=4, period_length=15,
        )
