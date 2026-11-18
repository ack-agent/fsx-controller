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
	"errors"
	"testing"

	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	ackcompare "github.com/aws-controllers-k8s/runtime/pkg/compare"
	ackerr "github.com/aws-controllers-k8s/runtime/pkg/errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	svcsdk "github.com/aws/aws-sdk-go-v2/service/fsx"
	svcsdktypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	svcapitypes "github.com/aws-controllers-k8s/fsx-controller/apis/v1alpha1"
)

func svmWithAD(ad *svcapitypes.CreateSvmActiveDirectoryConfiguration) *svcapitypes.StorageVirtualMachine {
	return &svcapitypes.StorageVirtualMachine{
		Spec: svcapitypes.StorageVirtualMachineSpec{ActiveDirectoryConfiguration: ad},
	}
}

func selfManagedAD(pw *ackv1alpha1.SecretKeyReference) *svcapitypes.CreateSvmActiveDirectoryConfiguration {
	return &svcapitypes.CreateSvmActiveDirectoryConfiguration{
		NetBiosName: aws.String("SVM1"),
		SelfManagedActiveDirectoryConfiguration: &svcapitypes.SelfManagedActiveDirectoryConfiguration{
			DomainName: aws.String("corp.example.com"),
			Password:   pw,
		},
	}
}

func TestRestoreADPassword(t *testing.T) {
	ref := &ackv1alpha1.SecretKeyReference{Key: "password"}
	ref.Name = "ad"

	t.Run("restores from desired", func(t *testing.T) {
		ko := svmWithAD(selfManagedAD(nil))
		restoreADPassword(svmWithAD(selfManagedAD(ref)), ko)
		if got := ko.Spec.ActiveDirectoryConfiguration.SelfManagedActiveDirectoryConfiguration.Password; got != ref {
			t.Fatalf("Password = %v, want %v", got, ref)
		}
	})
	t.Run("does not fabricate a block absent from Describe", func(t *testing.T) {
		ko := svmWithAD(nil)
		restoreADPassword(svmWithAD(selfManagedAD(ref)), ko)
		if ko.Spec.ActiveDirectoryConfiguration != nil {
			t.Fatalf("expected nil AD block, got %+v", ko.Spec.ActiveDirectoryConfiguration)
		}
	})
	t.Run("does not fabricate a self-managed block", func(t *testing.T) {
		ko := svmWithAD(&svcapitypes.CreateSvmActiveDirectoryConfiguration{NetBiosName: aws.String("SVM1")})
		restoreADPassword(svmWithAD(selfManagedAD(ref)), ko)
		if ko.Spec.ActiveDirectoryConfiguration.SelfManagedActiveDirectoryConfiguration != nil {
			t.Fatalf("expected nil self-managed block")
		}
	})
	t.Run("desired without AD leaves ko alone", func(t *testing.T) {
		ko := svmWithAD(selfManagedAD(nil))
		restoreADPassword(svmWithAD(nil), ko)
		if ko.Spec.ActiveDirectoryConfiguration.SelfManagedActiveDirectoryConfiguration.Password != nil {
			t.Fatalf("expected nil Password")
		}
	})
	t.Run("nil safe", func(t *testing.T) {
		restoreADPassword(nil, nil)
		restoreADPassword(svmWithAD(nil), nil)
	})
}

func TestRestoreRootVolumeSecurityStyle(t *testing.T) {
	withStyle := func(s *string) *svcapitypes.StorageVirtualMachine {
		return &svcapitypes.StorageVirtualMachine{
			Spec: svcapitypes.StorageVirtualMachineSpec{RootVolumeSecurityStyle: s},
		}
	}
	tests := []struct {
		name     string
		desired  *string
		observed *string
		want     *string
	}{
		{name: "omitted by Describe restores declared", desired: aws.String("UNIX"), want: aws.String("UNIX")},
		{name: "observed value kept", desired: aws.String("UNIX"), observed: aws.String("NTFS"), want: aws.String("NTFS")},
		{name: "observed value kept when undeclared", observed: aws.String("UNIX"), want: aws.String("UNIX")},
		{name: "neither set"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ko := withStyle(tt.observed)
			restoreRootVolumeSecurityStyle(withStyle(tt.desired), ko)
			if aws.ToString(ko.Spec.RootVolumeSecurityStyle) != aws.ToString(tt.want) ||
				(ko.Spec.RootVolumeSecurityStyle == nil) != (tt.want == nil) {
				t.Fatalf("RootVolumeSecurityStyle = %v, want %v", ko.Spec.RootVolumeSecurityStyle, tt.want)
			}
		})
	}
	restoreRootVolumeSecurityStyle(nil, nil)
}

func TestRejectUnexpressibleADChanges(t *testing.T) {
	adDelta := ackcompare.NewDelta()
	adDelta.Add("Spec.ActiveDirectoryConfiguration", nil, nil)
	tagsDelta := ackcompare.NewDelta()
	tagsDelta.Add("Spec.Tags", nil, nil)

	tests := []struct {
		name         string
		desired      *svcapitypes.CreateSvmActiveDirectoryConfiguration
		latest       *svcapitypes.CreateSvmActiveDirectoryConfiguration
		delta        *ackcompare.Delta
		wantTerminal bool
	}{
		{name: "removing the AD block", desired: nil, latest: selfManagedAD(nil), delta: adDelta, wantTerminal: true},
		{name: "removing only the self-managed block",
			desired: &svcapitypes.CreateSvmActiveDirectoryConfiguration{NetBiosName: aws.String("SVM1")},
			latest:  selfManagedAD(nil), delta: adDelta, wantTerminal: true},
		{name: "editing the AD block", desired: selfManagedAD(nil), latest: selfManagedAD(nil), delta: adDelta},
		{name: "adding the AD block", desired: selfManagedAD(nil), latest: nil, delta: adDelta},
		{name: "partial block reported for an SVM without AD", desired: nil,
			latest: &svcapitypes.CreateSvmActiveDirectoryConfiguration{NetBiosName: aws.String("SVM1")},
			delta:  adDelta},
		{name: "no AD delta", desired: nil, latest: selfManagedAD(nil), delta: tagsDelta},
		{name: "nil delta", desired: nil, latest: selfManagedAD(nil), delta: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := rejectUnexpressibleADChanges(
				&resource{svmWithAD(tt.desired)}, &resource{svmWithAD(tt.latest)}, tt.delta,
			)
			if !tt.wantTerminal {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var te *ackerr.TerminalError
			if !errors.As(err, &te) {
				t.Fatalf("expected terminal error, got %T: %v", err, err)
			}
		})
	}
}

func TestTerminalIfFailed(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name         string
		lifecycle    *string
		reason       *svcapitypes.LifecycleTransitionReason
		deleting     bool
		wantTerminal bool
	}{
		{name: "nil lifecycle"},
		{name: "created", lifecycle: aws.String("CREATED")},
		{name: "misconfigured is retryable", lifecycle: aws.String("MISCONFIGURED")},
		{name: "failed", lifecycle: aws.String("FAILED"),
			reason: &svcapitypes.LifecycleTransitionReason{Message: aws.String("boom")}, wantTerminal: true},
		{name: "failed without reason", lifecycle: aws.String("FAILED"), wantTerminal: true},
		{name: "failed while deleting", lifecycle: aws.String("FAILED"), deleting: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ko := &svcapitypes.StorageVirtualMachine{}
			ko.Status.Lifecycle = tt.lifecycle
			ko.Status.LifecycleTransitionReason = tt.reason
			if tt.deleting {
				ko.DeletionTimestamp = &now
			}
			err := terminalIfFailed(ko)
			if !tt.wantTerminal {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var te *ackerr.TerminalError
			if !errors.As(err, &te) {
				t.Fatalf("expected terminal error, got %T: %v", err, err)
			}
		})
	}
	if terminalIfFailed(nil) != nil {
		t.Fatalf("nil ko must not error")
	}
}

func TestSetDeletingLifecycle(t *testing.T) {
	r := &resource{&svcapitypes.StorageVirtualMachine{}}
	setDeletingLifecycle(r, nil)
	if got := aws.ToString(r.ko.Status.Lifecycle); got != "DELETING" {
		t.Fatalf("fallback lifecycle = %q, want DELETING", got)
	}
	setDeletingLifecycle(r, &svcsdk.DeleteStorageVirtualMachineOutput{
		Lifecycle: svcsdktypes.StorageVirtualMachineLifecycleDeleting,
	})
	if got := aws.ToString(r.ko.Status.Lifecycle); got != "DELETING" {
		t.Fatalf("lifecycle = %q, want DELETING", got)
	}
}
