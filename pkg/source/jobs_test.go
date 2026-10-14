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

// TestLiveJobGetsTheOneJobItWasAskedAbout: the command knows the namespace
// and the name, so it fetches one object. Listing a namespace to find one job
// needs list RBAC over every job in it and carries back every job in it, both
// of which this command has no use for.
func TestLiveJobGetsTheOneJobItWasAskedAbout(t *testing.T) {
	cs := fake.NewSimpleClientset(batchJob("research", "trainer"), batchJob("other", "trainer"))

	var verbs []string
	cs.PrependReactor("*", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		verbs = append(verbs, action.GetVerb())
		return false, nil, nil
	})

	got, err := NewLive(cs, newDynamicFake(t)).Job(context.Background(), "research", "trainer")
	if err != nil {
		t.Fatalf("Job() error = %v, want nil", err)
	}
	if got == nil {
		t.Fatal("Job() returned no job")
	}
	if got.Namespace != "research" {
		t.Errorf("Job() returned a job in %q, want research", got.Namespace)
	}

	if len(verbs) != 1 || verbs[0] != "get" {
		t.Errorf("Job() issued %v, want exactly one get", verbs)
	}
}

// TestLiveJobNotFoundIsReportedAsNotFound: the command turns this into exit
// 2, so it has to arrive as something apierrors.IsNotFound recognises rather
// than as a nil job and a nil error.
func TestLiveJobNotFoundIsReportedAsNotFound(t *testing.T) {
	cs := fake.NewSimpleClientset()

	got, err := NewLive(cs, newDynamicFake(t)).Job(context.Background(), "research", "absent")
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Job() error = %v, want a NotFound", err)
	}
	if got != nil {
		t.Errorf("Job() returned %v, want nil", got)
	}

	var pe *PartialError
	if errors.As(err, &pe) {
		t.Error("a job that does not exist was reported as a partial read")
	}
}

// TestLiveJobForbiddenIsPartial: a denied job read leaves the topology itself
// readable, so the run is incomplete rather than impossible.
func TestLiveJobForbiddenIsPartial(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "batch", Resource: "jobs"}, "trainer", errors.New("denied"))
	})

	_, err := NewLive(cs, newDynamicFake(t)).Job(context.Background(), "research", "trainer")

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("Job() error = %v (%T), want a PartialError", err, err)
	}
	if pe.Resource != "jobs" {
		t.Errorf("the partial read names %q, want %q", pe.Resource, "jobs")
	}
	if pe.ExitCode() != 3 {
		t.Errorf("exit code is %d, want 3", pe.ExitCode())
	}
}

// TestLiveJobTransportFailureIsAPlainError.
func TestLiveJobTransportFailureIsAPlainError(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	_, err := NewLive(cs, newDynamicFake(t)).Job(context.Background(), "research", "trainer")
	if err == nil {
		t.Fatal("a transport failure was swallowed")
	}

	var pe *PartialError
	if errors.As(err, &pe) {
		t.Errorf("error = %v, want a plain error, not a partial read", err)
	}
	if apierrors.IsNotFound(err) {
		t.Errorf("error = %v, want it not to read as a missing job", err)
	}
}

// TestBundleJobIsPartial: must-gather records the operator's pods and nothing
// of the workload, so a bundle has no job to answer with. Reporting that as
// NotFound would tell the user their job does not exist.
func TestBundleJobIsPartial(t *testing.T) {
	b, err := NewBundle(t.TempDir())
	if err != nil {
		t.Fatalf("opening the bundle: %v", err)
	}

	got, err := b.Job(context.Background(), "research", "trainer")

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("Job() error = %v (%T), want a PartialError", err, err)
	}
	if pe.Resource != "jobs" {
		t.Errorf("the partial read names %q, want %q", pe.Resource, "jobs")
	}
	if got != nil {
		t.Errorf("Job() returned %v, want nil", got)
	}
	if apierrors.IsNotFound(err) {
		t.Error("a bundle reported the job as absent, which it cannot know")
	}
}
