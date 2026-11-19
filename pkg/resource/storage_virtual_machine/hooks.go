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

package storage_virtual_machine

import (
	"context"
	"fmt"
	"time"

	ackcompare "github.com/aws-controllers-k8s/runtime/pkg/compare"
	ackerr "github.com/aws-controllers-k8s/runtime/pkg/errors"
	ackrequeue "github.com/aws-controllers-k8s/runtime/pkg/requeue"
	ackrtlog "github.com/aws-controllers-k8s/runtime/pkg/runtime/log"
	"github.com/aws/aws-sdk-go-v2/aws"
	svcsdk "github.com/aws/aws-sdk-go-v2/service/fsx"
	svcsdktypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"

	svcapitypes "github.com/aws-controllers-k8s/fsx-controller/apis/v1alpha1"
	"github.com/aws-controllers-k8s/fsx-controller/pkg/util"
)

// requeueWaitWhileDeleting is returned from the delete path while FSx still
// reports the SVM: DeleteStorageVirtualMachine is asynchronous.
var requeueWaitWhileDeleting = ackrequeue.NeededAfter(
	fmt.Errorf(
		"storage virtual machine is in %q state, requeuing until it is fully deleted",
		svcsdktypes.StorageVirtualMachineLifecycleDeleting,
	),
	15*time.Second,
)

// syncTags brings the SVM's AWS tags in line with Spec.Tags using
// TagResource/UntagResource (see util.SyncTags).
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

// getTags returns the SVM's tags from ListTagsForResource.
//
// DescribeStorageVirtualMachines does not report SVM tags (e2e: Describe had no
// Tags even after TagResource succeeded), so without this latest.Spec.Tags is
// always nil: every reconcile sees a Spec.Tags delta and re-issues TagResource,
// and removed tags are never untagged.
func (rm *resourceManager) getTags(
	ctx context.Context,
	arn string,
) (tags []*svcapitypes.Tag, err error) {
	rlog := ackrtlog.FromContext(ctx)
	exit := rlog.Trace("rm.getTags")
	defer func() { exit(err) }()

	tags = []*svcapitypes.Tag{}
	input := &svcsdk.ListTagsForResourceInput{ResourceARN: aws.String(arn)}
	for {
		resp, err := rm.sdkapi.ListTagsForResource(ctx, input)
		rm.metrics.RecordAPICall("READ_MANY", "ListTagsForResource", err)
		if err != nil {
			return nil, err
		}
		for _, t := range resp.Tags {
			tags = append(tags, &svcapitypes.Tag{Key: t.Key, Value: t.Value})
		}
		if resp.NextToken == nil || *resp.NextToken == "" {
			break
		}
		input.NextToken = resp.NextToken
	}
	if len(tags) == 0 {
		return nil, nil
	}
	return tags, nil
}

// setTagsFromAWS replaces ko.Spec.Tags with the tags FSx reports for the SVM.
// It is skipped while the SVM is DELETING, where tags no longer matter and
// the delete path only needs Describe to report the SVM gone.
func (rm *resourceManager) setTagsFromAWS(
	ctx context.Context,
	ko *svcapitypes.StorageVirtualMachine,
) error {
	if ko.Status.Lifecycle != nil &&
		*ko.Status.Lifecycle == string(svcsdktypes.StorageVirtualMachineLifecycleDeleting) {
		return nil
	}
	if ko.Status.ACKResourceMetadata == nil || ko.Status.ACKResourceMetadata.ARN == nil {
		return nil
	}
	tags, err := rm.getTags(ctx, string(*ko.Status.ACKResourceMetadata.ARN))
	if err != nil {
		return err
	}
	ko.Spec.Tags = tags
	return nil
}

// restoreADPassword copies desired's ActiveDirectoryConfiguration
// .SelfManagedActiveDirectoryConfiguration.Password secret reference onto ko.
//
// Describe reports the AD block as SelfManagedActiveDirectoryAttributes, which
// has no Password, so the generated setter drops the reference. Only the
// write-only password is restored: the rest of the block is updatable and
// reported by Describe, so restoring the whole struct would mask drift. When
// either side has no self-managed AD block nothing is restored, so no
// configuration the user did not submit is fabricated.
func restoreADPassword(
	desired *svcapitypes.StorageVirtualMachine,
	ko *svcapitypes.StorageVirtualMachine,
) {
	if desired == nil || ko == nil {
		return
	}
	src := desired.Spec.ActiveDirectoryConfiguration
	dst := ko.Spec.ActiveDirectoryConfiguration
	if src == nil || dst == nil ||
		src.SelfManagedActiveDirectoryConfiguration == nil ||
		dst.SelfManagedActiveDirectoryConfiguration == nil {
		return
	}
	dst.SelfManagedActiveDirectoryConfiguration.Password =
		src.SelfManagedActiveDirectoryConfiguration.Password
}

// restoreRootVolumeSecurityStyle copies desired's RootVolumeSecurityStyle onto
// ko when DescribeStorageVirtualMachines reported none.
//
// The field is create-only (UpdateStorageVirtualMachineInput has no member for
// it), so restoring the declared value cannot mask a change the controller
// could apply. Without this, an omitted value would be read as nil and produce
// a delta, and so an Update, on every reconcile. When Describe does report a
// style, the observed value is kept.
func restoreRootVolumeSecurityStyle(
	desired *svcapitypes.StorageVirtualMachine,
	ko *svcapitypes.StorageVirtualMachine,
) {
	if desired == nil || ko == nil {
		return
	}
	if ko.Spec.RootVolumeSecurityStyle == nil {
		ko.Spec.RootVolumeSecurityStyle = desired.Spec.RootVolumeSecurityStyle
	}
}

// rejectUnexpressibleADChanges returns a terminal error when desired removes
// the Active Directory block (or its self-managed configuration) while FSx
// still reports one.
//
// This is a convergence check, not validation, mirroring FileSystem's
// rejectUnexpressibleChanges: UpdateSvmActiveDirectoryConfiguration has no
// member to unjoin an SVM from AD, and the generated newUpdateRequestPayload
// omits a nil block. The request would succeed without effect, Describe would
// keep reporting the block, and every reconcile would issue another Update.
// Editing the Spec to restore the block clears the terminal condition.
//
// The check keys on SelfManagedActiveDirectoryConfiguration being reported,
// not just the outer struct, so a partial block FSx might report for an SVM
// that never joined AD is not rejected. The no-AD case is covered by e2e:
// test_storage_virtual_machine.py asserts Describe reports no
// SelfManagedActiveDirectoryConfiguration for an SVM created without AD.
func rejectUnexpressibleADChanges(
	desired *resource,
	latest *resource,
	delta *ackcompare.Delta,
) error {
	if delta == nil || !delta.DifferentAt("Spec.ActiveDirectoryConfiguration") {
		return nil
	}
	if latest == nil || latest.ko == nil {
		return nil
	}
	latestAD := latest.ko.Spec.ActiveDirectoryConfiguration
	if latestAD == nil || latestAD.SelfManagedActiveDirectoryConfiguration == nil {
		return nil
	}
	desiredAD := desired.ko.Spec.ActiveDirectoryConfiguration
	if desiredAD != nil && desiredAD.SelfManagedActiveDirectoryConfiguration != nil {
		return nil
	}
	return ackerr.NewTerminalError(fmt.Errorf(
		"cannot remove Spec.ActiveDirectoryConfiguration.SelfManagedActiveDirectoryConfiguration: " +
			"UpdateStorageVirtualMachine cannot unjoin a storage virtual machine " +
			"from Active Directory, so the change would never be applied in FSx; " +
			"restore the Active Directory configuration in the Spec",
	))
}

// terminalIfFailed returns a terminal error when FSx reports the SVM as
// FAILED. The returned resource is non-nil so Status (and
// LifecycleTransitionReason) is still patched alongside the Terminal
// condition.
//
// MISCONFIGURED is deliberately excluded: FSx documents it as recoverable, and
// a Terminal condition would stop the requeue that observes recovery.
//
// Never while the CR is being deleted: the delete path calls ReadOne first and
// aborts on any error but NotFound, and FAILED SVMs are deletable.
func terminalIfFailed(ko *svcapitypes.StorageVirtualMachine) error {
	if ko == nil || ko.Status.Lifecycle == nil {
		return nil
	}
	if ko.DeletionTimestamp != nil {
		return nil
	}
	if *ko.Status.Lifecycle != string(svcsdktypes.StorageVirtualMachineLifecycleFailed) {
		return nil
	}
	reason := "no lifecycle transition reason reported"
	if ko.Status.LifecycleTransitionReason != nil &&
		ko.Status.LifecycleTransitionReason.Message != nil {
		reason = *ko.Status.LifecycleTransitionReason.Message
	}
	return ackerr.NewTerminalError(fmt.Errorf(
		"storage virtual machine is in FAILED state and cannot recover: %s", reason,
	))
}

// setDeletingLifecycle records the lifecycle returned by
// DeleteStorageVirtualMachine (falling back to DELETING) on r, so Status shows
// the delete in progress. Later reconciles are held by the deletable.when
// guard, which does not patch Status, so this value stays.
func setDeletingLifecycle(
	r *resource,
	resp *svcsdk.DeleteStorageVirtualMachineOutput,
) {
	if resp != nil && resp.Lifecycle != "" {
		r.ko.Status.Lifecycle = aws.String(string(resp.Lifecycle))
		return
	}
	r.ko.Status.Lifecycle = aws.String(string(svcsdktypes.StorageVirtualMachineLifecycleDeleting))
}
