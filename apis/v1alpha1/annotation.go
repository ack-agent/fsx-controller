// Copyright Amazon.com Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

package v1alpha1

import "fmt"

var (
	// SkipFinalBackupAnnotation sets SkipFinalBackup ("true" or "false") on
	// delete.
	//
	// FileSystem: absent leaves FSx's per-type default in effect: Lustre skips
	// the backup, Windows and OpenZFS take one. ONTAP file systems have no
	// delete configuration shape, so it has no effect there.
	//
	// Volume: sets DeleteVolumeOntapConfiguration.SkipFinalBackup (ONTAP
	// volumes only). Absent sends nothing, so FSx takes a final backup.
	//
	// A final backup outlives the deleted resource as an FSx Backup this
	// controller does not manage.
	SkipFinalBackupAnnotation = fmt.Sprintf("%s/skip-final-backup", GroupVersion.Group)

	// OpenZFSDeleteOptionsAnnotation sets DeleteFileSystemOpenZFSConfiguration
	// Options on delete, as a comma-separated list of the API's enum values --
	// today only DELETE_CHILD_VOLUMES_AND_SNAPSHOTS. Absent sends no options, so
	// FSx refuses to delete a file system that has child volumes and the
	// refusal surfaces as a condition: those children may be managed outside
	// this controller.
	//
	// For a Volume it sets DeleteVolumeOpenZFSConfiguration.Options, validated
	// against the DeleteOpenZFSVolumeOption enum. Absent sends nothing, so FSx
	// refuses to delete a volume that has child volumes or snapshots.
	//
	// No effect on other file system or volume types.
	OpenZFSDeleteOptionsAnnotation = fmt.Sprintf("%s/openzfs-delete-options", GroupVersion.Group)

	// BypassSnaplockEnterpriseRetentionAnnotation sets
	// DeleteVolumeOntapConfiguration.BypassSnaplockEnterpriseRetention ("true"
	// or "false") when an ONTAP Volume is deleted (ONTAP Volume only). Absent
	// sends nothing, so FSx's default (false: retained WORM files block the
	// delete) applies. Setting it to "true" deletes a SnapLock Enterprise
	// volume that still holds retained files; it is never defaulted on.
	BypassSnaplockEnterpriseRetentionAnnotation = fmt.Sprintf("%s/bypass-snaplock-enterprise-retention", GroupVersion.Group)
)
