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

	svcapitypes "github.com/aws-controllers-k8s/fsx-controller/apis/v1alpha1"
	"github.com/aws-controllers-k8s/fsx-controller/pkg/util"
)

// recoverObservedSourceIDs replaces the copied-from-desired Spec.FileSystemID
// and Spec.VolumeID with the values DescribeBackups actually reports.
//
// DescribeBackups returns the source only nested (FileSystem.FileSystemId,
// Volume.VolumeId), so code-gen cannot name-match it back and latest would
// otherwise hold desired's value. A retargeted FileSystemRef, or a VolumeID
// added after create (which the `self == oldSelf` CEL rule skips, see
// community#3097), would then compare equal and be ignored while Synced stays
// True.
//
// A field is filled only when desired declares it. A volume backup still
// reports FileSystem, so filling an undeclared field would invent a delta.
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
	if desired.ko.Spec.FileSystemID != nil &&
		observed.FileSystem != nil && observed.FileSystem.FileSystemId != nil {
		ko.Spec.FileSystemID = observed.FileSystem.FileSystemId
	}
	if desired.ko.Spec.VolumeID != nil &&
		observed.Volume != nil && observed.Volume.VolumeId != nil {
		ko.Spec.VolumeID = observed.Volume.VolumeId
	}
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

	// Reject anything no API can carry before sending a tag change, so an
	// edit that mixes tags with an immutable field applies nothing (same order
	// as customUpdateFileSystem). This is a convergence check: the change
	// would otherwise be dropped silently and the delta would recur.
	// Reverting the field clears it.
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
