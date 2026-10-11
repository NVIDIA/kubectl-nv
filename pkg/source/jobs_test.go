/**
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package source

import (
	"context"
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func batchJob(namespace, name string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
}

// TestLiveJobsReadsOneNamespace: the command asks about the namespace -n
// named, and a job of the same name elsewhere is a different job.
func TestLiveJobsReadsOneNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset(batchJob("research", "trainer"), batchJob("other", "trainer"))

	got, err := NewLive(cs, newDynamicFake(t)).Jobs(context.Background(), "research")
	if err != nil {
		t.Fatalf("Jobs() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("Jobs() returned %d jobs, want 1: %v", len(got), got)
	}
	if got[0].Namespace != "research" {
		t.Errorf("Jobs() returned a job in %q, want research", got[0].Namespace)
	}
}

// TestLiveJobsForbiddenIsPartial: a denied job read leaves the topology
// itself readable, so the run is incomplete rather than impossible.
func TestLiveJobsForbiddenIsPartial(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "batch", Resource: "jobs"}, "", errors.New("denied"))
	})

	_, err := NewLive(cs, newDynamicFake(t)).Jobs(context.Background(), "research")

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("Jobs() error = %v (%T), want a PartialError", err, err)
	}
	if pe.Resource != "jobs" {
		t.Errorf("the partial read names %q, want %q", pe.Resource, "jobs")
	}
}

// TestLiveJobsEmptyNamespaceIsEveryNamespace keeps the Pods convention: an
// empty namespace means the whole cluster.
func TestLiveJobsEmptyNamespaceIsEveryNamespace(t *testing.T) {
	cs := fake.NewSimpleClientset(batchJob("research", "trainer"), batchJob("other", "trainer"))

	got, err := NewLive(cs, newDynamicFake(t)).Jobs(context.Background(), "")
	if err != nil {
		t.Fatalf("Jobs() error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Errorf("Jobs() returned %d jobs, want 2", len(got))
	}
}

// TestBundleJobsArePartial: must-gather records the operator's pods and
// nothing of the workload, so a bundle has no job list to answer from.
func TestBundleJobsArePartial(t *testing.T) {
	b, err := NewBundle(t.TempDir())
	if err != nil {
		t.Fatalf("opening the bundle: %v", err)
	}

	got, err := b.Jobs(context.Background(), "research")

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("Jobs() error = %v (%T), want a PartialError", err, err)
	}
	if pe.Resource != "jobs" {
		t.Errorf("the partial read names %q, want %q", pe.Resource, "jobs")
	}
	if got == nil {
		t.Error("the list is nil, want an empty one a caller can range over")
	}
	if len(got) != 0 {
		t.Errorf("the list holds %d jobs, want 0", len(got))
	}
}
