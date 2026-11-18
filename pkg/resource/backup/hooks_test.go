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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	ackv1alpha1 "github.com/aws-controllers-k8s/runtime/apis/core/v1alpha1"
	ackcompare "github.com/aws-controllers-k8s/runtime/pkg/compare"
	ackerr "github.com/aws-controllers-k8s/runtime/pkg/errors"
	ackmetrics "github.com/aws-controllers-k8s/runtime/pkg/metrics"
	"github.com/aws/aws-sdk-go-v2/aws"
	svcsdk "github.com/aws/aws-sdk-go-v2/service/fsx"
	svcsdktypes "github.com/aws/aws-sdk-go-v2/service/fsx/types"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	svcapitypes "github.com/aws-controllers-k8s/fsx-controller/apis/v1alpha1"
)

const testBackupARN = "arn:aws:fsx:us-west-2:123456789012:backup/backup-0123456789abcdef0"

func deltaAt(paths ...string) *ackcompare.Delta {
	delta := ackcompare.NewDelta()
	for _, p := range paths {
		delta.Add(p, nil, nil)
	}
	return delta
}

func tagList(kv ...string) []*svcapitypes.Tag {
	var out []*svcapitypes.Tag
	for i := 0; i+1 < len(kv); i += 2 {
		out = append(out, &svcapitypes.Tag{Key: aws.String(kv[i]), Value: aws.String(kv[i+1])})
	}
	return out
}

func newBackup(spec svcapitypes.BackupSpec) *resource {
	arn := ackv1alpha1.AWSResourceName(testBackupARN)
	return &resource{ko: &svcapitypes.Backup{
		Spec: spec,
		Status: svcapitypes.BackupStatus{
			ID:                  aws.String("backup-0123456789abcdef0"),
			ACKResourceMetadata: &ackv1alpha1.ResourceMetadata{ARN: &arn},
			Lifecycle:           aws.String("AVAILABLE"),
		},
	}}
}

// fakeFSx records the X-Amz-Target of every call and answers `{}`.
type fakeFSx struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeFSx) handler(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	target := r.Header.Get("X-Amz-Target")
	f.calls = append(f.calls, target[strings.LastIndex(target, ".")+1:])
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	_, _ = w.Write([]byte("{}"))
}

func newTestManager(t *testing.T) (*resourceManager, *fakeFSx) {
	t.Helper()
	fake := &fakeFSx{}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	client := svcsdk.New(svcsdk.Options{
		Region:       "us-west-2",
		BaseEndpoint: aws.String(srv.URL),
		Credentials:  aws.AnonymousCredentials{},
		HTTPClient:   srv.Client(),
	})
	return &resourceManager{sdkapi: client, metrics: ackmetrics.NewMetrics("fsx")}, fake
}

func TestCustomUpdateBackupTagsOnly(t *testing.T) {
	rm, fake := newTestManager(t)
	latest := newBackup(svcapitypes.BackupSpec{
		FileSystemID: aws.String("fs-0123456789abcdef0"),
		Tags:         tagList("a", "initial", "b", "second"),
	})
	desired := newBackup(svcapitypes.BackupSpec{
		FileSystemID: aws.String("fs-0123456789abcdef0"),
		Tags:         tagList("a", "updated", "c", "new"),
	})

	updated, err := rm.customUpdateBackup(
		context.Background(), desired, latest, deltaAt("Spec.Tags"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated == nil || updated.ko == desired.ko {
		t.Fatal("expected a copy of desired")
	}
	if got := strings.Join(fake.calls, ","); got != "UntagResource,TagResource" {
		t.Errorf("calls = %q, want UntagResource,TagResource", got)
	}
	if *updated.ko.Spec.Tags[0].Value != "updated" {
		t.Errorf("returned spec tags should be desired's")
	}
}

func TestCustomUpdateBackupNoTagDeltaSendsNothing(t *testing.T) {
	rm, fake := newTestManager(t)
	r := newBackup(svcapitypes.BackupSpec{Tags: tagList("a", "1")})
	if _, err := rm.customUpdateBackup(
		context.Background(), r, r, ackcompare.NewDelta(),
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(fake.calls) != 0 {
		t.Errorf("expected no API calls, got %v", fake.calls)
	}
}

func TestCustomUpdateBackupRejectsNonTagDeltaFirst(t *testing.T) {
	rm, fake := newTestManager(t)
	latest := newBackup(svcapitypes.BackupSpec{
		FileSystemID: aws.String("fs-0123456789abcdef0"),
		Tags:         tagList("a", "1"),
	})
	desired := newBackup(svcapitypes.BackupSpec{
		FileSystemID: aws.String("fs-0fedcba9876543210"),
		Tags:         tagList("a", "2"),
	})

	_, err := rm.customUpdateBackup(
		context.Background(), desired, latest,
		deltaAt("Spec.Tags", "Spec.FileSystemID"),
	)
	if err == nil {
		t.Fatal("expected a terminal error")
	}
	var terminal *ackerr.TerminalError
	if !errors.As(err, &terminal) {
		t.Fatalf("error is %T, want *ackerr.TerminalError: %v", err, err)
	}
	if !strings.Contains(err.Error(), "Spec.FileSystemID") {
		t.Errorf("error %q does not name the differing path", err.Error())
	}
	if strings.Contains(err.Error(), "Spec.Tags") {
		t.Errorf("error %q should not blame tags", err.Error())
	}
	if len(fake.calls) != 0 {
		t.Errorf("tags were synced before the rejection: %v", fake.calls)
	}
}

func TestRecoverObservedSourceIDs(t *testing.T) {
	const (
		declaredFS  = "fs-0123456789abcdef0"
		observedFS  = "fs-0fedcba9876543210"
		declaredVol = "fsvol-0123456789abcdef0"
		observedVol = "fsvol-0fedcba9876543210"
	)
	resp := func(fs, vol *string) *svcsdk.DescribeBackupsOutput {
		b := svcsdktypes.Backup{}
		if fs != nil {
			b.FileSystem = &svcsdktypes.FileSystem{FileSystemId: fs}
		}
		if vol != nil {
			b.Volume = &svcsdktypes.Volume{VolumeId: vol}
		}
		return &svcsdk.DescribeBackupsOutput{Backups: []svcsdktypes.Backup{b}}
	}
	str := func(p *string) string {
		if p == nil {
			return "<nil>"
		}
		return *p
	}

	tests := []struct {
		name    string
		spec    svcapitypes.BackupSpec
		resp    *svcsdk.DescribeBackupsOutput
		wantFS  string
		wantVol string
	}{
		{
			name:   "declared file system takes the observed ID",
			spec:   svcapitypes.BackupSpec{FileSystemID: aws.String(declaredFS)},
			resp:   resp(aws.String(observedFS), nil),
			wantFS: observedFS, wantVol: "<nil>",
		},
		{
			name:    "declared volume takes the observed ID",
			spec:    svcapitypes.BackupSpec{VolumeID: aws.String(declaredVol)},
			resp:    resp(aws.String(observedFS), aws.String(observedVol)),
			wantFS:  "<nil>",
			wantVol: observedVol,
		},
		{
			name:    "neither declared (adoption) fills both observed IDs",
			spec:    svcapitypes.BackupSpec{},
			resp:    resp(aws.String(observedFS), aws.String(observedVol)),
			wantFS:  observedFS,
			wantVol: observedVol,
		},
		{
			name:   "neither declared, file-system backup fills only FileSystemID",
			spec:   svcapitypes.BackupSpec{},
			resp:   resp(aws.String(observedFS), nil),
			wantFS: observedFS, wantVol: "<nil>",
		},
		{
			name:    "nil nested structs clear declared fields",
			spec:    svcapitypes.BackupSpec{FileSystemID: aws.String(declaredFS), VolumeID: aws.String(declaredVol)},
			resp:    resp(nil, nil),
			wantFS:  "<nil>",
			wantVol: "<nil>",
		},
		{
			name:   "nil nested IDs clear declared fields",
			spec:   svcapitypes.BackupSpec{FileSystemID: aws.String(declaredFS)},
			resp:   &svcsdk.DescribeBackupsOutput{Backups: []svcsdktypes.Backup{{FileSystem: &svcsdktypes.FileSystem{}}}},
			wantFS: "<nil>", wantVol: "<nil>",
		},
		{
			name:   "empty response",
			spec:   svcapitypes.BackupSpec{FileSystemID: aws.String(declaredFS)},
			resp:   &svcsdk.DescribeBackupsOutput{},
			wantFS: declaredFS, wantVol: "<nil>",
		},
		{
			name:   "nil response",
			spec:   svcapitypes.BackupSpec{FileSystemID: aws.String(declaredFS)},
			resp:   nil,
			wantFS: declaredFS, wantVol: "<nil>",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			desired := newBackup(tt.spec)
			ko := desired.ko.DeepCopy()
			recoverObservedSourceIDs(desired, tt.resp, ko)
			if got := str(ko.Spec.FileSystemID); got != tt.wantFS {
				t.Errorf("FileSystemID = %s, want %s", got, tt.wantFS)
			}
			if got := str(ko.Spec.VolumeID); got != tt.wantVol {
				t.Errorf("VolumeID = %s, want %s", got, tt.wantVol)
			}
		})
	}

	// Nil desired/ko must not panic.
	recoverObservedSourceIDs(nil, resp(aws.String(observedFS), nil), &svcapitypes.Backup{})
	recoverObservedSourceIDs(newBackup(svcapitypes.BackupSpec{}), resp(nil, nil), nil)
}

func TestTerminalIfFailed(t *testing.T) {
	withLifecycle := func(lc *string) *svcapitypes.Backup {
		return &svcapitypes.Backup{Status: svcapitypes.BackupStatus{Lifecycle: lc}}
	}
	failedWithDetails := withLifecycle(aws.String("FAILED"))
	failedWithDetails.Status.FailureDetails = &svcapitypes.BackupFailureDetails{
		Message: aws.String("source file system is unavailable"),
	}
	failedDeleting := withLifecycle(aws.String("FAILED"))
	now := metav1.Now()
	failedDeleting.DeletionTimestamp = &now

	tests := []struct {
		name         string
		ko           *svcapitypes.Backup
		wantTerminal bool
		wantMsg      string
	}{
		{name: "nil ko", ko: nil},
		{name: "nil lifecycle", ko: withLifecycle(nil)},
		{name: "available", ko: withLifecycle(aws.String("AVAILABLE"))},
		{name: "creating", ko: withLifecycle(aws.String("CREATING"))},
		{
			name:         "failed with details",
			ko:           failedWithDetails,
			wantTerminal: true,
			wantMsg:      "source file system is unavailable",
		},
		{
			name:         "failed without details",
			ko:           withLifecycle(aws.String("FAILED")),
			wantTerminal: true,
			wantMsg:      "no failure details reported",
		},
		{name: "failed while deleting", ko: failedDeleting},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := terminalIfFailed(tt.ko)
			if !tt.wantTerminal {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			var terminal *ackerr.TerminalError
			if !errors.As(err, &terminal) {
				t.Fatalf("error is %T, want *ackerr.TerminalError: %v", err, err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}
