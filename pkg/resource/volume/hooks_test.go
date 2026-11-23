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
	"errors"
	"testing"

	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	ackerr "github.com/aws-controllers-k8s/runtime/pkg/errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	svcsdk "github.com/aws/aws-sdk-go-v2/service/fsx"
	svcsdktypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	svcapitypes "github.com/aws-controllers-k8s/fsx-controller/apis/v1alpha1"
)

func ontapVolume(cfg *svcapitypes.CreateOntapVolumeConfiguration) *svcapitypes.Volume {
	return &svcapitypes.Volume{
		Spec: svcapitypes.VolumeSpec{
			Type:               aws.String("ONTAP"),
			OntapConfiguration: cfg,
		},
	}
}

func TestRestoreNonRoundTrippedSpecFields(t *testing.T) {
	t.Run("restores declared ConstituentsPerAggregate", func(t *testing.T) {
		desired := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{
			AggregateConfiguration: &svcapitypes.CreateAggregateConfiguration{
				Aggregates:               []*string{aws.String("aggr1")},
				ConstituentsPerAggregate: aws.Int64(4),
			},
		})
		ko := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{
			AggregateConfiguration: &svcapitypes.CreateAggregateConfiguration{
				Aggregates: []*string{aws.String("aggr1")},
			},
		})
		restoreNonRoundTrippedSpecFields(desired, ko)
		got := ko.Spec.OntapConfiguration.AggregateConfiguration.ConstituentsPerAggregate
		if got == nil || *got != 4 {
			t.Fatalf("ConstituentsPerAggregate = %v, want 4", got)
		}
	})
	t.Run("restores into a missing AggregateConfiguration", func(t *testing.T) {
		desired := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{
			AggregateConfiguration: &svcapitypes.CreateAggregateConfiguration{
				ConstituentsPerAggregate: aws.Int64(2),
			},
		})
		ko := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{})
		restoreNonRoundTrippedSpecFields(desired, ko)
		if ko.Spec.OntapConfiguration.AggregateConfiguration == nil ||
			*ko.Spec.OntapConfiguration.AggregateConfiguration.ConstituentsPerAggregate != 2 {
			t.Fatalf("AggregateConfiguration = %+v, want ConstituentsPerAggregate 2",
				ko.Spec.OntapConfiguration.AggregateConfiguration)
		}
	})
	t.Run("does not fabricate AggregateConfiguration when undeclared", func(t *testing.T) {
		desired := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{})
		ko := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{})
		restoreNonRoundTrippedSpecFields(desired, ko)
		if ko.Spec.OntapConfiguration.AggregateConfiguration != nil {
			t.Fatalf("AggregateConfiguration = %+v, want nil", ko.Spec.OntapConfiguration.AggregateConfiguration)
		}
	})
	t.Run("does not fabricate an ONTAP block absent from the response", func(t *testing.T) {
		desired := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{
			AggregateConfiguration: &svcapitypes.CreateAggregateConfiguration{
				ConstituentsPerAggregate: aws.Int64(2),
			},
		})
		ko := ontapVolume(nil)
		restoreNonRoundTrippedSpecFields(desired, ko)
		if ko.Spec.OntapConfiguration != nil {
			t.Fatalf("OntapConfiguration = %+v, want nil", ko.Spec.OntapConfiguration)
		}
	})
	t.Run("drops an undeclared CoolingPeriod", func(t *testing.T) {
		desired := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{
			TieringPolicy: &svcapitypes.TieringPolicy{Name: aws.String("AUTO")},
		})
		ko := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{
			TieringPolicy: &svcapitypes.TieringPolicy{
				Name: aws.String("AUTO"), CoolingPeriod: aws.Int64(31),
			},
		})
		restoreNonRoundTrippedSpecFields(desired, ko)
		if ko.Spec.OntapConfiguration.TieringPolicy.CoolingPeriod != nil {
			t.Fatalf("CoolingPeriod = %v, want nil", *ko.Spec.OntapConfiguration.TieringPolicy.CoolingPeriod)
		}
		if *ko.Spec.OntapConfiguration.TieringPolicy.Name != "AUTO" {
			t.Fatalf("Name changed")
		}
	})
	t.Run("drops CoolingPeriod when TieringPolicy is undeclared", func(t *testing.T) {
		desired := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{})
		ko := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{
			TieringPolicy: &svcapitypes.TieringPolicy{
				Name: aws.String("SNAPSHOT_ONLY"), CoolingPeriod: aws.Int64(2),
			},
		})
		restoreNonRoundTrippedSpecFields(desired, ko)
		if ko.Spec.OntapConfiguration.TieringPolicy.CoolingPeriod != nil {
			t.Fatalf("CoolingPeriod = %v, want nil", *ko.Spec.OntapConfiguration.TieringPolicy.CoolingPeriod)
		}
	})
	t.Run("keeps an observed CoolingPeriod when declared (drift stays visible)", func(t *testing.T) {
		desired := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{
			TieringPolicy: &svcapitypes.TieringPolicy{
				Name: aws.String("AUTO"), CoolingPeriod: aws.Int64(10),
			},
		})
		ko := ontapVolume(&svcapitypes.CreateOntapVolumeConfiguration{
			TieringPolicy: &svcapitypes.TieringPolicy{
				Name: aws.String("AUTO"), CoolingPeriod: aws.Int64(20),
			},
		})
		restoreNonRoundTrippedSpecFields(desired, ko)
		if got := ko.Spec.OntapConfiguration.TieringPolicy.CoolingPeriod; got == nil || *got != 20 {
			t.Fatalf("CoolingPeriod = %v, want observed 20", got)
		}
	})
	t.Run("nil inputs", func(t *testing.T) {
		restoreNonRoundTrippedSpecFields(nil, nil)
		restoreNonRoundTrippedSpecFields(ontapVolume(nil), nil)
	})
}

func TestTerminalIfFailed(t *testing.T) {
	withLifecycle := func(l string) *svcapitypes.Volume {
		ko := &svcapitypes.Volume{}
		ko.Status.Lifecycle = aws.String(l)
		return ko
	}
	for _, l := range []string{"CREATED", "AVAILABLE", "CREATING", "PENDING", "MISCONFIGURED", "DELETING"} {
		if err := terminalIfFailed(withLifecycle(l)); err != nil {
			t.Fatalf("%s: got %v, want nil", l, err)
		}
	}
	if err := terminalIfFailed(&svcapitypes.Volume{}); err != nil {
		t.Fatalf("nil lifecycle: got %v", err)
	}

	ko := withLifecycle("FAILED")
	ko.Status.LifecycleTransitionReason = &svcapitypes.LifecycleTransitionReason{
		Message: aws.String("aggregate full"),
	}
	err := terminalIfFailed(ko)
	var term *ackerr.TerminalError
	if !errors.As(err, &term) {
		t.Fatalf("FAILED: got %v, want terminal", err)
	}

	ko.DeletionTimestamp = &metav1.Time{}
	if err := terminalIfFailed(ko); err != nil {
		t.Fatalf("FAILED while deleting: got %v, want nil", err)
	}
}

func TestSetDeletingLifecycle(t *testing.T) {
	r := &resource{ko: &svcapitypes.Volume{}}
	setDeletingLifecycle(r, &svcsdk.DeleteVolumeOutput{Lifecycle: svcsdktypes.VolumeLifecycleDeleting})
	if *r.ko.Status.Lifecycle != "DELETING" {
		t.Fatalf("Lifecycle = %s", *r.ko.Status.Lifecycle)
	}
	r = &resource{ko: &svcapitypes.Volume{}}
	setDeletingLifecycle(r, nil)
	if *r.ko.Status.Lifecycle != "DELETING" {
		t.Fatalf("Lifecycle (nil resp) = %s", *r.ko.Status.Lifecycle)
	}
}

func volumeWithAnnotations(volType string, ann map[string]string) *resource {
	ko := &svcapitypes.Volume{Spec: svcapitypes.VolumeSpec{Type: aws.String(volType)}}
	ko.SetAnnotations(ann)
	return &resource{ko: ko}
}

func TestSetDeleteVolumeConfiguration(t *testing.T) {
	t.Run("no annotations sends nothing", func(t *testing.T) {
		for _, typ := range []string{"ONTAP", "OPENZFS"} {
			input := &svcsdk.DeleteVolumeInput{}
			if err := setDeleteVolumeConfiguration(volumeWithAnnotations(typ, nil), input); err != nil {
				t.Fatal(err)
			}
			if input.OntapConfiguration != nil || input.OpenZFSConfiguration != nil {
				t.Fatalf("%s: got %+v, want no configuration", typ, input)
			}
		}
	})
	t.Run("ONTAP skip-final-backup and bypass", func(t *testing.T) {
		input := &svcsdk.DeleteVolumeInput{}
		r := volumeWithAnnotations("ONTAP", map[string]string{
			svcapitypes.SkipFinalBackupAnnotation:                   "true",
			svcapitypes.BypassSnaplockEnterpriseRetentionAnnotation: "false",
		})
		if err := setDeleteVolumeConfiguration(r, input); err != nil {
			t.Fatal(err)
		}
		c := input.OntapConfiguration
		if c == nil || !*c.SkipFinalBackup || *c.BypassSnaplockEnterpriseRetention {
			t.Fatalf("OntapConfiguration = %+v", c)
		}
		if input.OpenZFSConfiguration != nil {
			t.Fatalf("OpenZFSConfiguration set for ONTAP")
		}
	})
	t.Run("ONTAP ignores OpenZFS options", func(t *testing.T) {
		input := &svcsdk.DeleteVolumeInput{}
		r := volumeWithAnnotations("ONTAP", map[string]string{
			svcapitypes.OpenZFSDeleteOptionsAnnotation: "DELETE_CHILD_VOLUMES_AND_SNAPSHOTS",
		})
		if err := setDeleteVolumeConfiguration(r, input); err != nil {
			t.Fatal(err)
		}
		if input.OntapConfiguration != nil || input.OpenZFSConfiguration != nil {
			t.Fatalf("got %+v, want no configuration", input)
		}
	})
	t.Run("OpenZFS options; skip-final-backup ignored", func(t *testing.T) {
		input := &svcsdk.DeleteVolumeInput{}
		r := volumeWithAnnotations("OPENZFS", map[string]string{
			svcapitypes.OpenZFSDeleteOptionsAnnotation: " DELETE_CHILD_VOLUMES_AND_SNAPSHOTS ",
			svcapitypes.SkipFinalBackupAnnotation:      "true",
		})
		if err := setDeleteVolumeConfiguration(r, input); err != nil {
			t.Fatal(err)
		}
		c := input.OpenZFSConfiguration
		if c == nil || len(c.Options) != 1 ||
			c.Options[0] != svcsdktypes.DeleteOpenZFSVolumeOptionDeleteChildVolumesAndSnapshots {
			t.Fatalf("OpenZFSConfiguration = %+v", c)
		}
		if input.OntapConfiguration != nil {
			t.Fatalf("OntapConfiguration set for OPENZFS")
		}
	})
	t.Run("malformed values are retryable errors", func(t *testing.T) {
		for _, tc := range []struct {
			typ string
			ann map[string]string
		}{
			{"ONTAP", map[string]string{svcapitypes.SkipFinalBackupAnnotation: "yes please"}},
			{"ONTAP", map[string]string{svcapitypes.BypassSnaplockEnterpriseRetentionAnnotation: "maybe"}},
			// A FileSystem-only option must not be accepted for a Volume.
			{"OPENZFS", map[string]string{svcapitypes.OpenZFSDeleteOptionsAnnotation: "DELETE_CHILD_VOLUMES_AND_SNAPSHOTS,BOGUS"}},
		} {
			err := setDeleteVolumeConfiguration(volumeWithAnnotations(tc.typ, tc.ann), &svcsdk.DeleteVolumeInput{})
			if err == nil {
				t.Fatalf("%s %v: got nil error", tc.typ, tc.ann)
			}
			var term *ackerr.TerminalError
			if errors.As(err, &term) {
				t.Fatalf("%s %v: got terminal %v, want retryable", tc.typ, tc.ann, err)
			}
		}
	})
	t.Run("malformed annotation for the other type is ignored", func(t *testing.T) {
		for _, tc := range []struct {
			typ string
			ann map[string]string
		}{
			{"ONTAP", map[string]string{svcapitypes.OpenZFSDeleteOptionsAnnotation: "BOGUS"}},
			{"OPENZFS", map[string]string{svcapitypes.SkipFinalBackupAnnotation: "yes"}},
		} {
			input := &svcsdk.DeleteVolumeInput{}
			if err := setDeleteVolumeConfiguration(volumeWithAnnotations(tc.typ, tc.ann), input); err != nil {
				t.Fatalf("%s %v: got %v, want nil", tc.typ, tc.ann, err)
			}
			if input.OntapConfiguration != nil || input.OpenZFSConfiguration != nil {
				t.Fatalf("%s %v: got %+v, want no configuration", tc.typ, tc.ann, input)
			}
		}
	})
	t.Run("nil type sends nothing", func(t *testing.T) {
		r := &resource{ko: &svcapitypes.Volume{}}
		r.ko.SetAnnotations(map[string]string{svcapitypes.SkipFinalBackupAnnotation: "true"})
		input := &svcsdk.DeleteVolumeInput{}
		if err := setDeleteVolumeConfiguration(r, input); err != nil {
			t.Fatal(err)
		}
		if input.OntapConfiguration != nil {
			t.Fatalf("OntapConfiguration set without a type")
		}
	})
}

func TestBypassSnaplockAnnotationName(t *testing.T) {
	if svcapitypes.BypassSnaplockEnterpriseRetentionAnnotation != "fsx.services.k8s.aws/bypass-snaplock-enterprise-retention" {
		t.Fatalf("annotation = %s", svcapitypes.BypassSnaplockEnterpriseRetentionAnnotation)
	}
}

func TestSetTagsFromAWSSkips(t *testing.T) {
	// No case may call ListTagsForResource (rm has no client).
	rm := &resourceManager{}
	key, value := "k", "v"
	desired := &svcapitypes.Volume{Spec: svcapitypes.VolumeSpec{
		Tags: []*svcapitypes.Tag{{Key: &key, Value: &value}},
	}}
	arn := ackv1alpha1.AWSResourceName("arn:aws:fsx:us-west-2:111122223333:volume/fs-1/fsvol-1")
	withARN := func(ko *svcapitypes.Volume) *svcapitypes.Volume {
		ko.Status.ACKResourceMetadata = &ackv1alpha1.ResourceMetadata{ARN: &arn}
		return ko
	}
	deleting := "DELETING"

	// Being deleted, or DELETING: Describe's nil tags are replaced with a
	// copy of desired's.
	for name, ko := range map[string]*svcapitypes.Volume{
		"being deleted": withARN(&svcapitypes.Volume{
			ObjectMeta: metav1.ObjectMeta{DeletionTimestamp: &metav1.Time{}},
		}),
		"DELETING": withARN(&svcapitypes.Volume{Status: svcapitypes.VolumeStatus{
			Lifecycle: &deleting,
		}}),
	} {
		if err := rm.setTagsFromAWS(context.Background(), desired, ko); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(ko.Spec.Tags) != 1 || *ko.Spec.Tags[0].Key != key || *ko.Spec.Tags[0].Value != value {
			t.Fatalf("%s: tags = %v, want desired's", name, ko.Spec.Tags)
		}
		if &ko.Spec.Tags[0] == &desired.Spec.Tags[0] || ko.Spec.Tags[0] == desired.Spec.Tags[0] {
			t.Fatalf("%s: tags alias desired's", name)
		}
	}

	// No ARN yet: tags are left as they are.
	ko := &svcapitypes.Volume{}
	other := "other"
	ko.Spec.Tags = []*svcapitypes.Tag{{Key: &other}}
	if err := rm.setTagsFromAWS(context.Background(), desired, ko); err != nil {
		t.Fatalf("no ARN: %v", err)
	}
	if len(ko.Spec.Tags) != 1 || *ko.Spec.Tags[0].Key != other {
		t.Fatalf("no ARN: tags changed to %v", ko.Spec.Tags)
	}
}

func TestKeepDesiredTags(t *testing.T) {
	t.Run("nil desired tags", func(t *testing.T) {
		k := "k"
		ko := &svcapitypes.Volume{Spec: svcapitypes.VolumeSpec{
			Tags: []*svcapitypes.Tag{{Key: &k}},
		}}
		keepDesiredTags(&svcapitypes.Volume{}, ko)
		if ko.Spec.Tags != nil {
			t.Fatalf("tags = %v, want nil", ko.Spec.Tags)
		}
	})
	t.Run("copies without aliasing", func(t *testing.T) {
		k, v := "k", "v"
		desired := &svcapitypes.Volume{Spec: svcapitypes.VolumeSpec{
			Tags: []*svcapitypes.Tag{{Key: &k, Value: &v}},
		}}
		ko := &svcapitypes.Volume{}
		keepDesiredTags(desired, ko)
		if len(ko.Spec.Tags) != 1 || *ko.Spec.Tags[0].Key != k || *ko.Spec.Tags[0].Value != v {
			t.Fatalf("tags = %v, want desired's", ko.Spec.Tags)
		}
		*ko.Spec.Tags[0].Value = "changed"
		if *desired.Spec.Tags[0].Value != "v" {
			t.Fatalf("writing the copy changed desired")
		}
		ko.Spec.Tags[0] = nil
		if desired.Spec.Tags[0] == nil {
			t.Fatalf("copy shares desired's slice")
		}
	})
	t.Run("nil inputs", func(t *testing.T) {
		keepDesiredTags(nil, nil)
		keepDesiredTags(nil, &svcapitypes.Volume{})
	})
}

func ozfsResource(cfg *svcapitypes.CreateOpenZFSVolumeConfiguration) *resource {
	return &resource{ko: &svcapitypes.Volume{
		Spec: svcapitypes.VolumeSpec{OpenZFSConfiguration: cfg},
	}}
}

func quota(typ string, id, gib int64) *svcapitypes.OpenZFSUserOrGroupQuota {
	return &svcapitypes.OpenZFSUserOrGroupQuota{
		Type: aws.String(typ), ID: aws.Int64(id), StorageCapacityQuotaGiB: aws.Int64(gib),
	}
}

func clientCfg(c string, opts ...string) *svcapitypes.OpenZFSClientConfiguration {
	return &svcapitypes.OpenZFSClientConfiguration{
		Clients: aws.String(c), Options: aws.StringSlice(opts),
	}
}

func TestNormalizeOpenZFSLists(t *testing.T) {
	// The e2e observation: declared two client configurations and two
	// quotas; Describe returned the clients reversed and the root quotas
	// appended.
	a := ozfsResource(&svcapitypes.CreateOpenZFSVolumeConfiguration{
		NfsExports: []*svcapitypes.OpenZFSNfsExport{{ClientConfigurations: []*svcapitypes.OpenZFSClientConfiguration{
			clientCfg("10.0.0.0/16", "rw", "crossmnt", "no_root_squash"), clientCfg("10.1.0.0/16", "ro"),
		}}},
		UserAndGroupQuotas: []*svcapitypes.OpenZFSUserOrGroupQuota{
			quota("USER", 1001, 5), quota("GROUP", 2001, 8),
		},
	})
	b := ozfsResource(&svcapitypes.CreateOpenZFSVolumeConfiguration{
		NfsExports: []*svcapitypes.OpenZFSNfsExport{{ClientConfigurations: []*svcapitypes.OpenZFSClientConfiguration{
			clientCfg("10.1.0.0/16", "ro"), clientCfg("10.0.0.0/16", "rw", "crossmnt", "no_root_squash"),
		}}},
		UserAndGroupQuotas: []*svcapitypes.OpenZFSUserOrGroupQuota{
			quota("USER", 1001, 5), quota("GROUP", 2001, 8), quota("USER", 0, 0), quota("GROUP", 0, 0),
		},
	})
	if d := newResourceDelta(a, b); len(d.Differences) != 0 {
		t.Fatalf("expected no delta, got %v", d.Differences)
	}
	// latest got a copy, not desired's slice.
	if &b.ko.Spec.OpenZFSConfiguration.UserAndGroupQuotas[0] == &a.ko.Spec.OpenZFSConfiguration.UserAndGroupQuotas[0] ||
		b.ko.Spec.OpenZFSConfiguration.UserAndGroupQuotas[0] == a.ko.Spec.OpenZFSConfiguration.UserAndGroupQuotas[0] {
		t.Fatal("latest shares desired's quota slice")
	}
}

func TestNormalizeOpenZFSListsKeepsRealChanges(t *testing.T) {
	cases := map[string][2][]*svcapitypes.OpenZFSUserOrGroupQuota{
		"changed quota": {
			{quota("USER", 1001, 5)},
			{quota("USER", 1001, 6), quota("USER", 0, 0)},
		},
		"extra observed entry": {
			{quota("USER", 1001, 5)},
			{quota("USER", 1001, 5), quota("USER", 1002, 1)},
		},
		"declared root differs": {
			{quota("USER", 0, 10)},
			{quota("USER", 0, 0)},
		},
		"missing declared entry": {
			{quota("USER", 1001, 5), quota("GROUP", 2001, 8)},
			{quota("USER", 1001, 5), quota("GROUP", 0, 0)},
		},
	}
	for name, c := range cases {
		a := ozfsResource(&svcapitypes.CreateOpenZFSVolumeConfiguration{UserAndGroupQuotas: c[0]})
		b := ozfsResource(&svcapitypes.CreateOpenZFSVolumeConfiguration{UserAndGroupQuotas: c[1]})
		if !newResourceDelta(a, b).DifferentAt("Spec.OpenZFSConfiguration.UserAndGroupQuotas") {
			t.Errorf("%s: expected a UserAndGroupQuotas delta", name)
		}
	}
	a := ozfsResource(&svcapitypes.CreateOpenZFSVolumeConfiguration{NfsExports: []*svcapitypes.OpenZFSNfsExport{{
		ClientConfigurations: []*svcapitypes.OpenZFSClientConfiguration{clientCfg("*", "rw", "crossmnt")},
	}}})
	b := ozfsResource(&svcapitypes.CreateOpenZFSVolumeConfiguration{NfsExports: []*svcapitypes.OpenZFSNfsExport{{
		ClientConfigurations: []*svcapitypes.OpenZFSClientConfiguration{clientCfg("*", "ro")},
	}}})
	if !newResourceDelta(a, b).DifferentAt("Spec.OpenZFSConfiguration.NfsExports") {
		t.Error("expected an NfsExports delta for changed options")
	}
}
