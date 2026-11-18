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

package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	ackcompare "github.com/aws-controllers-k8s/runtime/pkg/compare"
	ackerr "github.com/aws-controllers-k8s/runtime/pkg/errors"
	ackrtlog "github.com/aws-controllers-k8s/runtime/pkg/runtime/log"
	svcsdk "github.com/aws/aws-sdk-go-v2/service/fsx"
	svcsdktypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"

	svcapitypes "github.com/aws-controllers-k8s/fsx-controller/apis/v1alpha1"
	"github.com/aws-controllers-k8s/fsx-controller/pkg/util"
)

// recoverObservedSourceIDs sets Spec.FileSystemID/VolumeID from the nested
// IDs DescribeBackups reports, so changes the CEL rule misses (community#3097)
// show up in the delta. A declared field takes the observed value, nil
// included. When neither is declared (adoption), both are filled.
func recoverObservedSourceIDs(
	desired *resource,
	resp *svcsdk.DescribeBackupsOutput,
	ko *svcapitypes.Backup,
) {
	if desired == nil || desired.ko == nil || resp == nil || ko == nil {
		return
	}
	if len(resp.Backups) == 0 {
		return
	}
	// The request filters by BackupIds = [Status.ID], and sdkFind takes the
	// first element the same way.
	observed := resp.Backups[0]
	if desired.ko.Spec.FileSystemID == nil && desired.ko.Spec.VolumeID == nil {
		if observed.FileSystem != nil {
			ko.Spec.FileSystemID = observed.FileSystem.FileSystemId
		}
		if observed.Volume != nil {
			ko.Spec.VolumeID = observed.Volume.VolumeId
		}
		return
	}
	if desired.ko.Spec.FileSystemID != nil {
		ko.Spec.FileSystemID = nil
		if observed.FileSystem != nil {
			ko.Spec.FileSystemID = observed.FileSystem.FileSystemId
		}
	}
	if desired.ko.Spec.VolumeID != nil {
		ko.Spec.VolumeID = nil
		if observed.Volume != nil {
			ko.Spec.VolumeID = observed.Volume.VolumeId
		}
	}
}

// terminalIfFailed returns a Terminal error for a FAILED backup, which FSx
// never recovers. Skipped during deletion so the delete can still run.
func terminalIfFailed(ko *svcapitypes.Backup) error {
	if ko == nil || ko.Status.Lifecycle == nil {
		return nil
	}
	if ko.DeletionTimestamp != nil {
		return nil
	}
	if *ko.Status.Lifecycle != string(svcsdktypes.BackupLifecycleFailed) {
		return nil
	}
	reason := "no failure details reported"
	if ko.Status.FailureDetails != nil && ko.Status.FailureDetails.Message != nil {
		reason = *ko.Status.FailureDetails.Message
	}
	return ackerr.NewTerminalError(fmt.Errorf(
		"backup is in FAILED state and cannot recover: %s; create a new Backup", reason,
	))
}

// pathString renders a delta path. compare.Path keeps its parts unexported and
// exposes them only through MarshalJSON, so that is the supported way to read
// one back out.
func pathString(p ackcompare.Path) string {
	encoded, err := p.MarshalJSON()
	if err != nil {
		return ""
	}
	var parts struct{ Parts []string }
	if err := json.Unmarshal(encoded, &parts); err != nil {
		return ""
	}
	return strings.Join(parts.Parts, ".")
}

// deltaPathsOutsideTags returns the differing Spec paths that are not tags.
func deltaPathsOutsideTags(delta *ackcompare.Delta) []string {
	if delta == nil || !delta.DifferentExcept("Spec.Tags") {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, d := range delta.Differences {
		p := pathString(d.Path)
		if p == "" || strings.HasPrefix(p, "Spec.Tags") {
			continue
		}
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// syncTags brings the backup's AWS tags in line with Spec.Tags using the FSx
// TagResource/UntagResource operations (see util.SyncTags).
func (rm *resourceManager) syncTags(
	ctx context.Context,
	desired *resource,
	latest *resource,
) (err error) {
	rlog := ackrtlog.FromContext(ctx)
	exit := rlog.Trace("rm.syncTags")
	defer func() { exit(err) }()

	return util.SyncTags(
		ctx, desired.ko.Spec.Tags, latest.ko.Spec.Tags,
		latest.ko.Status.ACKResourceMetadata, convertToOrderedACKTags,
		rm.sdkapi, rm.metrics,
	)
}

// customUpdateBackup replaces the generated sdkUpdate. FSx has no
// UpdateBackup, so code-gen would emit a stub that makes every update --
// including a tags-only one -- terminal. Tags are the only mutable field and go
// through TagResource/UntagResource.
func (rm *resourceManager) customUpdateBackup(
	ctx context.Context,
	desired *resource,
	latest *resource,
	delta *ackcompare.Delta,
) (updated *resource, err error) {
	rlog := ackrtlog.FromContext(ctx)
	exit := rlog.Trace("rm.customUpdateBackup")
	defer func() { exit(err) }()

	// Reject non-tag changes before any tag call, so a mixed edit applies nothing.
	if paths := deltaPathsOutsideTags(delta); len(paths) > 0 {
		return nil, ackerr.NewTerminalError(fmt.Errorf(
			"cannot apply changes to %s: FSx backups cannot be modified after "+
				"creation (only tags can change); revert the field or create a "+
				"new Backup",
			strings.Join(paths, ", "),
		))
	}

	if delta.DifferentAt("Spec.Tags") {
		if err = rm.syncTags(ctx, desired, latest); err != nil {
			return nil, err
		}
	}

	// desired already carries the observed Status (runtime v0.63.0+).
	ko := desired.ko.DeepCopy()
	rm.setStatusDefaults(ko)
	return &resource{ko}, nil
}
