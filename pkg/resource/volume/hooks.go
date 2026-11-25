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

package volume

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

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
// reports the volume: DeleteVolume is asynchronous.
var requeueWaitWhileDeleting = ackrequeue.NeededAfter(
	fmt.Errorf(
		"volume is in %q state, requeuing until it is fully deleted",
		svcsdktypes.VolumeLifecycleDeleting,
	),
	15*time.Second,
)

// syncTags brings the volume's AWS tags in line with Spec.Tags using
// TagResource/UntagResource (see util.SyncTags). UpdateVolume has no Tags
// member.
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

// getTags returns the volume's tags from ListTagsForResource. DescribeVolumes
// does not report tags despite the model, which would otherwise leave a
// permanent Spec.Tags delta and wipe spec.tags.
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

// setTagsFromAWS sets ko.Spec.Tags from ListTagsForResource. It keeps
// desired's tags during deletion, so a tag-read failure cannot block delete,
// and does nothing until the ARN is known.
func (rm *resourceManager) setTagsFromAWS(
	ctx context.Context,
	desired *svcapitypes.Volume,
	ko *svcapitypes.Volume,
) error {
	if ko.DeletionTimestamp != nil ||
		(ko.Status.Lifecycle != nil &&
			*ko.Status.Lifecycle == string(svcsdktypes.VolumeLifecycleDeleting)) {
		keepDesiredTags(desired, ko)
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

// keepDesiredTags sets ko.Spec.Tags to a deep copy of desired's tags, so a
// nil from Describe (which reports no tags) is not patched onto the CR.
func keepDesiredTags(desired, ko *svcapitypes.Volume) {
	if desired == nil || ko == nil {
		return
	}
	if desired.Spec.Tags == nil {
		ko.Spec.Tags = nil
		return
	}
	ko.Spec.Tags = make([]*svcapitypes.Tag, len(desired.Spec.Tags))
	for i, t := range desired.Spec.Tags {
		ko.Spec.Tags[i] = t.DeepCopy()
	}
}

// restoreNonRoundTrippedSpecFields fixes up the ONTAP configuration that the
// Create and Describe setters replace wholesale. It restores a declared
// AggregateConfiguration.ConstituentsPerAggregate (the output shape has no
// such member) and drops an undeclared TieringPolicy.CoolingPeriod (FSx
// defaults it only for some policies, so it can't be late-initialized).
func restoreNonRoundTrippedSpecFields(
	desired *svcapitypes.Volume,
	ko *svcapitypes.Volume,
) {
	if desired == nil || ko == nil {
		return
	}
	src := desired.Spec.OntapConfiguration
	dst := ko.Spec.OntapConfiguration
	if src == nil || dst == nil {
		return
	}
	if src.AggregateConfiguration != nil &&
		src.AggregateConfiguration.ConstituentsPerAggregate != nil {
		if dst.AggregateConfiguration == nil {
			dst.AggregateConfiguration = &svcapitypes.CreateAggregateConfiguration{}
		}
		dst.AggregateConfiguration.ConstituentsPerAggregate =
			src.AggregateConfiguration.ConstituentsPerAggregate
	}
	if dst.TieringPolicy != nil &&
		(src.TieringPolicy == nil || src.TieringPolicy.CoolingPeriod == nil) {
		dst.TieringPolicy.CoolingPeriod = nil
	}
}

// terminalIfFailed returns a Terminal error when the volume is FAILED, which
// is unrecoverable. MISCONFIGURED is recoverable and stays retryable. Skipped
// during delete: the delete path's ReadOne aborts on any non-NotFound error.
func terminalIfFailed(ko *svcapitypes.Volume) error {
	if ko == nil || ko.Status.Lifecycle == nil {
		return nil
	}
	if ko.DeletionTimestamp != nil {
		return nil
	}
	if *ko.Status.Lifecycle != string(svcsdktypes.VolumeLifecycleFailed) {
		return nil
	}
	reason := "no lifecycle transition reason reported"
	if ko.Status.LifecycleTransitionReason != nil &&
		ko.Status.LifecycleTransitionReason.Message != nil {
		reason = *ko.Status.LifecycleTransitionReason.Message
	}
	return ackerr.NewTerminalError(fmt.Errorf(
		"volume is in FAILED state and cannot recover: %s", reason,
	))
}

// setDeletingLifecycle records the lifecycle returned by DeleteVolume (falling
// back to DELETING) on r, so Status shows the delete in progress. Later
// reconciles are held by the deletable.when guard, which does not patch
// Status, so this value stays.
func setDeletingLifecycle(
	r *resource,
	resp *svcsdk.DeleteVolumeOutput,
) {
	if resp != nil && resp.Lifecycle != "" {
		r.ko.Status.Lifecycle = aws.String(string(resp.Lifecycle))
		return
	}
	r.ko.Status.Lifecycle = aws.String(string(svcsdktypes.VolumeLifecycleDeleting))
}

// deleteVolumeAnnotationParameters holds the delete-time options read from
// annotations. Each is nil/empty when its annotation is absent.
type deleteVolumeAnnotationParameters struct {
	SkipFinalBackup                   *bool
	BypassSnaplockEnterpriseRetention *bool
	OpenZFSOptions                    []svcsdktypes.DeleteOpenZFSVolumeOption
}

// parseDeleteVolumeAnnotations parses the deletion annotations for
// volumeType only. An unparseable value is an error, so a typo cannot change
// what a delete destroys. util.ParseDeletionAnnotations validates against
// the FileSystem enum, whose values differ.
func parseDeleteVolumeAnnotations(
	volumeType svcsdktypes.VolumeType,
	annotations map[string]string,
) (*deleteVolumeAnnotationParameters, error) {
	params := &deleteVolumeAnnotationParameters{}
	var err error
	switch volumeType {
	case svcsdktypes.VolumeTypeOntap:
		if params.SkipFinalBackup, err = parseBoolAnnotation(
			annotations, svcapitypes.SkipFinalBackupAnnotation,
		); err != nil {
			return nil, err
		}
		if params.BypassSnaplockEnterpriseRetention, err = parseBoolAnnotation(
			annotations, svcapitypes.BypassSnaplockEnterpriseRetentionAnnotation,
		); err != nil {
			return nil, err
		}
	case svcsdktypes.VolumeTypeOpenzfs:
		if value, ok := annotations[svcapitypes.OpenZFSDeleteOptionsAnnotation]; ok && value != "" {
			// Validated against the SDK enum, so new values arrive with an SDK bump.
			known := svcsdktypes.DeleteOpenZFSVolumeOption("").Values()
			for _, raw := range strings.Split(value, ",") {
				opt := svcsdktypes.DeleteOpenZFSVolumeOption(strings.TrimSpace(raw))
				if !slices.Contains(known, opt) {
					return nil, fmt.Errorf(
						"invalid value %q for annotation %s: want a comma-separated list of %v",
						value, svcapitypes.OpenZFSDeleteOptionsAnnotation, known,
					)
				}
				params.OpenZFSOptions = append(params.OpenZFSOptions, opt)
			}
		}
	}
	return params, nil
}

func parseBoolAnnotation(annotations map[string]string, key string) (*bool, error) {
	value, ok := annotations[key]
	if !ok || value == "" {
		return nil, nil
	}
	b, err := strconv.ParseBool(value)
	if err != nil {
		return nil, fmt.Errorf("invalid value %q for annotation %s: %v", value, key, err)
	}
	return &b, nil
}

// setDeleteVolumeConfiguration fills in DeleteVolumeInput's per-type
// configuration (stripped from the model by generator.yaml) only when an
// annotation for that volume type sets something.
func setDeleteVolumeConfiguration(
	r *resource,
	input *svcsdk.DeleteVolumeInput,
) error {
	if r.ko.Spec.Type == nil {
		return nil
	}
	params, err := parseDeleteVolumeAnnotations(
		svcsdktypes.VolumeType(*r.ko.Spec.Type), r.ko.ObjectMeta.GetAnnotations(),
	)
	if err != nil {
		// Retryable, not Terminal: an annotation edit doesn't bump the generation,
		// so a Terminal error would stall the delete until the next resync.
		return err
	}
	switch svcsdktypes.VolumeType(*r.ko.Spec.Type) {
	case svcsdktypes.VolumeTypeOntap:
		if params.SkipFinalBackup != nil || params.BypassSnaplockEnterpriseRetention != nil {
			input.OntapConfiguration = &svcsdktypes.DeleteVolumeOntapConfiguration{
				SkipFinalBackup:                   params.SkipFinalBackup,
				BypassSnaplockEnterpriseRetention: params.BypassSnaplockEnterpriseRetention,
			}
		}
	case svcsdktypes.VolumeTypeOpenzfs:
		if len(params.OpenZFSOptions) > 0 {
			input.OpenZFSConfiguration = &svcsdktypes.DeleteVolumeOpenZFSConfiguration{
				Options: params.OpenZFSOptions,
			}
		}
	}
	return nil
}

// normalizeOpenZFSLists copies desired's NfsExports ClientConfigurations and
// UserAndGroupQuotas into latest when they are equivalent. FSx reorders both
// and adds root quota entries (Id 0, quota 0), so treat equivalent lists as
// equal. A real difference still produces a delta.
func normalizeOpenZFSLists(a, b *resource) {
	if a.ko.Spec.OpenZFSConfiguration == nil || b.ko.Spec.OpenZFSConfiguration == nil {
		return
	}
	desired := a.ko.Spec.OpenZFSConfiguration
	latest := b.ko.Spec.OpenZFSConfiguration

	if len(desired.NfsExports) == len(latest.NfsExports) {
		for i := range desired.NfsExports {
			d, l := desired.NfsExports[i], latest.NfsExports[i]
			if d == nil || l == nil {
				continue
			}
			if clientConfigurationsEquivalent(d.ClientConfigurations, l.ClientConfigurations) {
				l.ClientConfigurations = make([]*svcapitypes.OpenZFSClientConfiguration, len(d.ClientConfigurations))
				for j, c := range d.ClientConfigurations {
					l.ClientConfigurations[j] = c.DeepCopy()
				}
			}
		}
	}

	if quotasEquivalent(desired.UserAndGroupQuotas, latest.UserAndGroupQuotas) {
		if desired.UserAndGroupQuotas == nil {
			latest.UserAndGroupQuotas = nil
		} else {
			latest.UserAndGroupQuotas = make([]*svcapitypes.OpenZFSUserOrGroupQuota, len(desired.UserAndGroupQuotas))
			for j, q := range desired.UserAndGroupQuotas {
				latest.UserAndGroupQuotas[j] = q.DeepCopy()
			}
		}
	}
}

func strVal(s *string) string {
	if s == nil {
		return "\x00nil"
	}
	return *s
}

func int64Val(i *int64) string {
	if i == nil {
		return "nil"
	}
	return strconv.FormatInt(*i, 10)
}

func clientConfigurationKey(c *svcapitypes.OpenZFSClientConfiguration) string {
	if c == nil {
		return "\x00nil"
	}
	parts := []string{strVal(c.Clients)}
	for _, o := range c.Options {
		parts = append(parts, strVal(o))
	}
	return strings.Join(parts, "\x01")
}

// clientConfigurationsEquivalent reports whether a and b hold the same
// client configurations regardless of order. Options keep their order: FSx
// returned them as submitted.
func clientConfigurationsEquivalent(a, b []*svcapitypes.OpenZFSClientConfiguration) bool {
	if len(a) != len(b) {
		return false
	}
	counts := map[string]int{}
	for _, c := range a {
		counts[clientConfigurationKey(c)]++
	}
	for _, c := range b {
		k := clientConfigurationKey(c)
		if counts[k] == 0 {
			return false
		}
		counts[k]--
	}
	return true
}

func quotaKey(q *svcapitypes.OpenZFSUserOrGroupQuota) string {
	if q == nil {
		return "\x00nil"
	}
	return strVal(q.Type) + "\x01" + int64Val(q.ID) + "\x01" + int64Val(q.StorageCapacityQuotaGiB)
}

// isImplicitRootQuota reports whether q is the root entry FSx reports for
// Type without being asked: Id 0 with a quota of 0.
func isImplicitRootQuota(q *svcapitypes.OpenZFSUserOrGroupQuota) bool {
	return q != nil && q.ID != nil && *q.ID == 0 &&
		q.StorageCapacityQuotaGiB != nil && *q.StorageCapacityQuotaGiB == 0
}

// quotasEquivalent reports whether latest equals desired as a multiset once
// the implicit root entries desired does not declare are skipped.
func quotasEquivalent(desired, latest []*svcapitypes.OpenZFSUserOrGroupQuota) bool {
	declaredRoot := map[string]bool{}
	counts := map[string]int{}
	for _, q := range desired {
		if q != nil && q.ID != nil && *q.ID == 0 {
			declaredRoot[strVal(q.Type)] = true
		}
		counts[quotaKey(q)]++
	}
	for _, q := range latest {
		if isImplicitRootQuota(q) && !declaredRoot[strVal(q.Type)] {
			continue
		}
		k := quotaKey(q)
		if counts[k] == 0 {
			return false
		}
		counts[k]--
	}
	for _, n := range counts {
		if n != 0 {
			return false
		}
	}
	return true
}
