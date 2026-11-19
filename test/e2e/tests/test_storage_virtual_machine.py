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

"""Integration tests for the FSx StorageVirtualMachine resource.

An SVM needs an FSx for ONTAP file system, so the module stands up its own
SINGLE_AZ_1 ONTAP FileSystem CR and links the SVM to it through fileSystemRef.

UpdateStorageVirtualMachine only carries ActiveDirectoryConfiguration and
SvmAdminPassword. The e2e environment has no self-managed Active Directory,
so the update coverage here is tags (TagResource/UntagResource). The SVM is
created without an AD block, which also checks that Describe's view of a
non-AD SVM converges without a perpetual delta.

As in the DataRepositoryAssociation test, the whole lifecycle is one test so
pytest-xdist cannot reorder the steps or build a second file system.
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
    FSxValidator,
    StorageVirtualMachineValidator,
    get_controller_api_call_count,
    wait_for_synced_or_terminal,
)

FS_RESOURCE_PLURAL = "filesystems"
SVM_RESOURCE_PLURAL = "storagevirtualmachines"

TAG_KEY = "ack-fsx-e2e"
INITIAL_TAG_VALUE = "initial"
UPDATED_TAG_VALUE = "updated"
SECOND_TAG_KEY = "ack-fsx-e2e-second"
SECOND_TAG_VALUE = "second"
NEW_TAG_KEY = "ack-fsx-e2e-new"
NEW_TAG_VALUE = "new"

ROOT_VOLUME_SECURITY_STYLE = "UNIX"

INITIAL_TAGS = {TAG_KEY: INITIAL_TAG_VALUE, SECOND_TAG_KEY: SECOND_TAG_VALUE}
# TAG_KEY changed, SECOND_TAG_KEY removed and NEW_TAG_KEY added.
UPDATED_TAGS = {TAG_KEY: UPDATED_TAG_VALUE, NEW_TAG_KEY: NEW_TAG_VALUE}

# ONTAP file system: up to ~30 min to AVAILABLE and ~10 min to disappear.
FS_CREATE_WAIT_PERIODS = 80
FS_CREATE_PERIOD_LENGTH = 30
FS_DELETE_WAIT_PERIODS = 40
FS_DELETE_PERIOD_LENGTH = 30

CREATE_WAIT_AFTER_SECONDS = 30
SVM_CREATE_WAIT_PERIODS = 80
SVM_CREATE_PERIOD_LENGTH = 15
MODIFY_WAIT_AFTER_SECONDS = 15
SVM_MODIFY_WAIT_PERIODS = 40
SVM_MODIFY_PERIOD_LENGTH = 15
SVM_DELETE_WAIT_PERIODS = 60
SVM_DELETE_PERIOD_LENGTH = 15


def _delete_cr(ref, wait_periods, period_length):
    if k8s.get_resource_exists(ref):
        _, deleted = k8s.delete_custom_resource(
            ref, wait_periods=wait_periods, period_length=period_length,
        )
        assert deleted
        assert not k8s.get_resource_exists(ref)


@pytest.fixture(scope="module")
def ontap_file_system(fsx_client):
    resource_name = random_suffix_name("ack-fsx-svm-fs", 32)

    replacements = REPLACEMENT_VALUES.copy()
    replacements["FILE_SYSTEM_NAME"] = resource_name

    resource_data = load_fsx_resource(
        "ontap_file_system", additional_replacements=replacements,
    )
    ref = k8s.CustomResourceReference(
        CRD_GROUP, CRD_VERSION, FS_RESOURCE_PLURAL,
        resource_name, namespace="default",
    )
    k8s.create_custom_resource(ref, resource_data)

    # Wrapped so a mid-setup failure still tears down the file system.
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
def storage_virtual_machine(fsx_client, ontap_file_system):
    """Creates the SVM and waits for sync. Teardown runs before the file
    system's: FSx rejects deleting an ONTAP file system that still has
    non-root SVMs. test_crud normally deletes the SVM itself; teardown is the
    safety net.
    """
    (file_system_name, file_system_id) = ontap_file_system
    cr_name = random_suffix_name("ack-fsx-svm", 32)
    # SVM names are at most 47 characters of [a-zA-Z0-9_]; no hyphens.
    svm_name = random_suffix_name("ackfsxsvm", 20).replace("-", "_")

    replacements = REPLACEMENT_VALUES.copy()
    replacements["SVM_CR_NAME"] = cr_name
    replacements["SVM_NAME"] = svm_name
    replacements["FILE_SYSTEM_NAME"] = file_system_name
    replacements["ROOT_VOLUME_SECURITY_STYLE"] = ROOT_VOLUME_SECURITY_STYLE
    replacements["TAG_KEY"] = TAG_KEY
    replacements["TAG_VALUE"] = INITIAL_TAG_VALUE
    replacements["SECOND_TAG_KEY"] = SECOND_TAG_KEY
    replacements["SECOND_TAG_VALUE"] = SECOND_TAG_VALUE

    resource_data = load_fsx_resource(
        "storage_virtual_machine", additional_replacements=replacements,
    )
    logging.debug(resource_data)

    ref = k8s.CustomResourceReference(
        CRD_GROUP, CRD_VERSION, SVM_RESOURCE_PLURAL,
        cr_name, namespace="default",
    )
    k8s.create_custom_resource(ref, resource_data)

    svm_id = None
    try:
        time.sleep(CREATE_WAIT_AFTER_SECONDS)
        assert k8s.wait_resource_consumed_by_controller(ref) is not None
        cr = k8s.get_resource(ref) or {}
        svm_id = cr.get("status", {}).get("id")
        wait_for_synced_or_terminal(
            ref,
            wait_periods=SVM_CREATE_WAIT_PERIODS,
            period_length=SVM_CREATE_PERIOD_LENGTH,
        )
        svm_id = k8s.get_resource(ref)["status"]["id"]
        yield (ref, file_system_id, svm_id, svm_name)
    finally:
        if svm_id is None and k8s.get_resource_exists(ref):
            cr = k8s.get_resource(ref) or {}
            svm_id = cr.get("status", {}).get("id")
        _delete_cr(ref, SVM_DELETE_WAIT_PERIODS, SVM_DELETE_PERIOD_LENGTH)
        if svm_id is not None:
            assert StorageVirtualMachineValidator(fsx_client).wait_until_gone(svm_id)


@service_marker
class TestStorageVirtualMachine:
    def test_crud(self, fsx_client, storage_virtual_machine):
        (ref, file_system_id, svm_id, svm_name) = storage_virtual_machine
        validator = StorageVirtualMachineValidator(fsx_client)
        self._assert_created(ref, validator, file_system_id, svm_id, svm_name)
        updates_before = get_controller_api_call_count(
            "UpdateStorageVirtualMachine"
        )
        self._update_tags_and_assert(ref, validator, svm_id)
        self._assert_steady_state(ref, validator, svm_id, updates_before)
        self._delete_and_assert(ref, validator, svm_id)

    def _assert_created(self, ref, validator, file_system_id, svm_id, svm_name):
        assert svm_id is not None

        # Verify via the CR.
        condition.assert_synced(ref)
        cr = k8s.get_resource(ref)
        assert cr["status"]["lifecycle"] == "CREATED"
        assert cr["status"]["ackResourceMetadata"]["arn"] is not None
        assert cr["status"].get("uuid")
        # The runtime clears a reference-resolved field before persisting the
        # spec; the AWS check below proves the reference resolved.
        assert cr["spec"]["fileSystemRef"]["from"]["name"]
        assert cr["spec"]["name"] == svm_name

        # Verify via the AWS API.
        svm = validator.get(svm_id)
        assert svm is not None
        assert svm["Lifecycle"] == "CREATED"
        assert svm["FileSystemId"] == file_system_id
        assert svm["Name"] == svm_name
        assert cr["status"]["ackResourceMetadata"]["arn"] == svm["ResourceARN"]

        # Describe was observed to omit RootVolumeSecurityStyle (an earlier
        # run, created without one). The CR must keep the declared value
        # either way; if Describe does report it, it must match.
        logging.info(
            f"Describe RootVolumeSecurityStyle: {svm.get('RootVolumeSecurityStyle')}"
        )
        assert cr["spec"]["rootVolumeSecurityStyle"] == ROOT_VOLUME_SECURITY_STYLE
        assert svm.get("RootVolumeSecurityStyle") in (None, ROOT_VOLUME_SECURITY_STYLE)

        # Record what Describe reports for an SVM with no AD block (the read
        # hook's AD-removal check keys on SelfManagedActiveDirectoryConfiguration
        # rather than the outer struct in case FSx reports a partial block).
        ad = svm.get("ActiveDirectoryConfiguration")
        logging.info(f"Describe ActiveDirectoryConfiguration for non-AD SVM: {ad}")
        assert ad is None or ad.get("SelfManagedActiveDirectoryConfiguration") is None

        aws_tags = validator.get_tags(svm_id)
        tags.assert_ack_system_tags(tags=aws_tags)
        tags.assert_equal_without_ack_tags(expected=INITIAL_TAGS, actual=aws_tags)
        tags.assert_equal(
            expected=INITIAL_TAGS, actual=cr["spec"]["tags"],
            key_member_name="key", value_member_name="value",
        )

    def _assert_steady_state(self, ref, validator, svm_id, updates_before):
        """A non-AD SVM must not keep producing a delta.

        The preceding tag patch bumps generation and so forces a reconcile
        (an annotation change would not, without the IgnoreFieldDrift gate).
        A tags-only delta must skip UpdateStorageVirtualMachine, so an
        unchanged UpdateStorageVirtualMachine counter proves Describe's view
        (RootVolumeSecurityStyle, the absent AD block) converges with no
        lasting delta. No Terminal (e.g. from the AD-removal check) either.
        """
        updates_after = get_controller_api_call_count(
            "UpdateStorageVirtualMachine"
        )
        assert updates_after == updates_before, (
            f"UpdateStorageVirtualMachine count went from {updates_before} to "
            f"{updates_after}: "
            + (
                "the controller restarted mid-window (counter reset)"
                if updates_after < updates_before
                else "a non-Tags delta caused an UpdateStorageVirtualMachine call"
            )
        )
        cr = k8s.get_resource(ref)
        by_type = {c["type"]: c for c in cr["status"].get("conditions") or []}
        terminal = by_type.get(condition.CONDITION_TYPE_TERMINAL)
        assert terminal is None or terminal.get("status") != "True", terminal
        recoverable = by_type.get(condition.CONDITION_TYPE_RECOVERABLE)
        assert recoverable is None or recoverable.get("status") != "True", recoverable
        condition.assert_synced(ref)
        assert cr["spec"]["rootVolumeSecurityStyle"] == ROOT_VOLUME_SECURITY_STYLE
        assert validator.get(svm_id)["Lifecycle"] == "CREATED"

    def _update_tags_and_assert(self, ref, validator, svm_id):
        """Tags go through TagResource/UntagResource, not
        UpdateStorageVirtualMachine. Both calls run: TAG_KEY changes,
        SECOND_TAG_KEY is removed and NEW_TAG_KEY is added. The ACK system tags
        must survive the removal.
        """
        k8s.patch_custom_resource(ref, {
            "spec": {"tags": [
                {"key": TAG_KEY, "value": UPDATED_TAG_VALUE},
                {"key": NEW_TAG_KEY, "value": NEW_TAG_VALUE},
            ]},
        })
        time.sleep(MODIFY_WAIT_AFTER_SECONDS)

        # Synced may still be True from the pre-update reconcile; poll AWS.
        # Describe does not report SVM tags, so poll ListTagsForResource.
        assert validator.wait_until(
            svm_id,
            lambda _: tags.clean(validator.get_tags(svm_id)) == UPDATED_TAGS,
            "re-tagged",
            wait_periods=SVM_MODIFY_WAIT_PERIODS,
            period_length=SVM_MODIFY_PERIOD_LENGTH,
        )
        assert k8s.wait_on_condition(
            ref, condition.CONDITION_TYPE_RESOURCE_SYNCED, "True",
            wait_periods=SVM_MODIFY_WAIT_PERIODS,
            period_length=SVM_MODIFY_PERIOD_LENGTH,
        )

        aws_tags = validator.get_tags(svm_id)
        tags.assert_ack_system_tags(tags=aws_tags)
        tags.assert_equal_without_ack_tags(expected=UPDATED_TAGS, actual=aws_tags)
        cr = k8s.get_resource(ref)
        tags.assert_equal(
            expected=UPDATED_TAGS, actual=cr["spec"]["tags"],
            key_member_name="key", value_member_name="value",
        )

    def _delete_and_assert(self, ref, validator, svm_id):
        """The finalizer must be held until FSx has fully removed the SVM."""
        _delete_cr(ref, SVM_DELETE_WAIT_PERIODS, SVM_DELETE_PERIOD_LENGTH)

        # The CR is only gone once Describe reports NotFound, so AWS must
        # agree right away (allow a short eventual-consistency window).
        assert validator.wait_until_gone(svm_id, wait_periods=4, period_length=15)
