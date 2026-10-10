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
	"net"
	"strings"
	"syscall"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// twoNodes is one GPU node by capacity and one hinted node, enough to show
// that a failed secondary read still leaves a usable answer.
func twoNodes() []corev1.Node {
	return []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1"},
			Status: corev1.NodeStatus{Capacity: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("8")}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "hinted-1", Labels: map[string]string{"nvidia.com/gpu.present": "true"}}},
	}
}

func failingSliceReactor(c *callCounter, err error) k8stesting.ReactionFunc {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		c.record("list", "resourceslices")
		return true, nil, err
	}
}

func forbidden(resourceName string) error {
	return apierrors.NewForbidden(
		schema.GroupResource{Group: "resource.k8s.io", Resource: resourceName},
		"", errors.New("not allowed"))
}

// TestForbiddenSecondaryResourceIsPartial covers the RBAC case the exit-code
// contract depends on: a reader denied resourceslices still gets the nodes the
// capacity rule found, the error names the one resource that was lost, and it
// maps to the partial exit code rather than the error one.
func TestForbiddenSecondaryResourceIsPartial(t *testing.T) {
	counter := newCallCounter()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", pagingNodeReactor(twoNodes(), counter))
	cs.PrependReactor("list", "resourceslices", failingSliceReactor(counter, forbidden("resourceslices")))

	gpus, hinted, err := NewLive(cs, newDynamicFake(t)).ClassifyNodes(context.Background())

	assertNames(t, "ClassifyNodes() gpus", gpus, []string{"gpu-1"})
	assertNames(t, "ClassifyNodes() hinted", hinted, []string{"hinted-1"})

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *PartialError", err, err)
	}
	if pe.Resource != "resourceslices" {
		t.Errorf("PartialError.Resource = %q, want resourceslices", pe.Resource)
	}
	if !apierrors.IsForbidden(errors.Unwrap(pe)) {
		t.Errorf("unwrapped error = %v, want a Forbidden API error", errors.Unwrap(pe))
	}
	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Errorf("output.ExitCodeFor(err) = %d, want %d (ExitPartial)", got, output.ExitPartial)
	}
}

func TestLiveResourceSlicesForbiddenIsPartial(t *testing.T) {
	counter := newCallCounter()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "resourceslices", failingSliceReactor(counter, forbidden("resourceslices")))

	got, err := NewLive(cs, newDynamicFake(t)).ResourceSlices(context.Background())
	if len(got) != 0 {
		t.Errorf("ResourceSlices() returned %d items, want 0", len(got))
	}
	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *PartialError", err, err)
	}
	if pe.Resource != "resourceslices" {
		t.Errorf("PartialError.Resource = %q, want resourceslices", pe.Resource)
	}
	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Errorf("output.ExitCodeFor(err) = %d, want %d", got, output.ExitPartial)
	}
}

// TestPlainErrorsExitError walks the failures that are not partial. None of
// them leaves a usable answer, so each has to reach ExitError: reporting a
// clean or partial run off a cluster that was never reached would be a lie.
func TestPlainErrorsExitError(t *testing.T) {
	connRefused := &net.OpError{
		Op: "dial", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 6443},
		Err:  syscall.ECONNREFUSED,
	}
	unauthorized := apierrors.NewUnauthorized("bad token")

	cases := []struct {
		name string
		call func(t *testing.T) error
	}{
		{"node list refused", func(t *testing.T) error {
			counter := newCallCounter()
			cs := fake.NewSimpleClientset()
			cs.PrependReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
				counter.record("list", "nodes")
				return true, nil, connRefused
			})
			_, _, err := NewLive(cs, newDynamicFake(t)).ClassifyNodes(context.Background())
			return err
		}},
		{"node list forbidden", func(t *testing.T) error {
			counter := newCallCounter()
			cs := fake.NewSimpleClientset()
			cs.PrependReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
				counter.record("list", "nodes")
				return true, nil, forbidden("nodes")
			})
			_, _, err := NewLive(cs, newDynamicFake(t)).ClassifyNodes(context.Background())
			return err
		}},
		{"context canceled", func(t *testing.T) error {
			counter := newCallCounter()
			cs := fake.NewSimpleClientset()
			cs.PrependReactor("list", "nodes", pagingNodeReactor(twoNodes(), counter))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			_, _, err := NewLive(cs, newDynamicFake(t)).ClassifyNodes(ctx)
			return err
		}},
		{"slices unauthorized", func(t *testing.T) error {
			counter := newCallCounter()
			cs := fake.NewSimpleClientset()
			cs.PrependReactor("list", "nodes", pagingNodeReactor(twoNodes(), counter))
			cs.PrependReactor("list", "resourceslices", failingSliceReactor(counter, unauthorized))
			_, _, err := NewLive(cs, newDynamicFake(t)).ClassifyNodes(context.Background())
			return err
		}},
		{"slices refused", func(t *testing.T) error {
			counter := newCallCounter()
			cs := fake.NewSimpleClientset()
			cs.PrependReactor("list", "nodes", pagingNodeReactor(twoNodes(), counter))
			cs.PrependReactor("list", "resourceslices", failingSliceReactor(counter, connRefused))
			_, _, err := NewLive(cs, newDynamicFake(t)).ClassifyNodes(context.Background())
			return err
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.call(t)
			if err == nil {
				t.Fatal("error = nil, want a failure")
			}
			var pe *PartialError
			if errors.As(err, &pe) {
				t.Fatalf("error = %v, want a plain error, not *PartialError", err)
			}
			if got := output.ExitCodeFor(err); got != output.ExitError {
				t.Errorf("output.ExitCodeFor(%v) = %d, want %d (ExitError)", err, got, output.ExitError)
			}
		})
	}
}

func TestContextCanceledReachesTheCaller(t *testing.T) {
	counter := newCallCounter()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", pagingNodeReactor(twoNodes(), counter))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, err := NewLive(cs, newDynamicFake(t)).ClassifyNodes(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want it to wrap context.Canceled", err)
	}
	if n := counter.get("list", "nodes"); n != 0 {
		t.Errorf("list nodes calls = %d, want 0: a dead context must not start a list", n)
	}
}

func TestPartialErrorMessageAndCode(t *testing.T) {
	pe := &PartialError{Resource: "resourceslices", Err: errors.New("boom")}
	const want = `could not read resourceslices: boom`
	if got := pe.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
	if got := pe.ExitCode(); got != 3 {
		t.Errorf("ExitCode() = %d, want 3", got)
	}
	if got := output.ExitCodeFor(pe); got != output.ExitPartial {
		t.Errorf("output.ExitCodeFor(pe) = %d, want %d", got, output.ExitPartial)
	}
}

// TestJoinedPartialErrors is how more than one lost resource is reported: one
// PartialError each, joined. Both names survive and the exit code is still
// partial, because joining does not make the run less complete than its worst
// part.
func TestJoinedPartialErrors(t *testing.T) {
	joined := errors.Join(
		&PartialError{Resource: "resourceslices", Err: errors.New("forbidden")},
		&PartialError{Resource: "clusterpolicies", Err: errors.New("forbidden")},
	)
	msg := joined.Error()
	for _, want := range []string{"resourceslices", "clusterpolicies"} {
		if !strings.Contains(msg, want) {
			t.Errorf("joined error %q does not name %q", msg, want)
		}
	}
	var pe *PartialError
	if !errors.As(joined, &pe) {
		t.Fatal("errors.As did not reach a *PartialError through errors.Join")
	}
	if got := output.ExitCodeFor(joined); got != output.ExitPartial {
		t.Errorf("output.ExitCodeFor(joined) = %d, want %d", got, output.ExitPartial)
	}
}
