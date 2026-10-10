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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// TestNodesForbiddenResourceSlicesIsPartial covers the RBAC case the exit-code
// contract depends on: a reader denied resourceslices still gets the nodes the
// label and capacity rules found, and the error says which resource was lost.
func TestNodesForbiddenResourceSlicesIsPartial(t *testing.T) {
	counter := newCallCounter()
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "gpu-1", Labels: map[string]string{"nvidia.com/gpu.present": "true"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "plain-1"}},
	}

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", pagingNodeReactor(nodes, counter))
	cs.PrependReactor("list", "resourceslices", func(action k8stesting.Action) (bool, runtime.Object, error) {
		counter.record("list", "resourceslices")
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"},
			"", errors.New("not allowed"))
	})

	got, err := NewLive(cs, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())).Nodes(context.Background())

	gotNames := names(got)
	if len(gotNames) != 1 || gotNames[0] != "gpu-1" {
		t.Fatalf("nodes = %v, want [gpu-1]", gotNames)
	}

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *PartialError", err, err)
	}
	if len(pe.Resources) != 1 || pe.Resources[0] != "resourceslices" {
		t.Fatalf("PartialError.Resources = %v, want [resourceslices]", pe.Resources)
	}
	if !apierrors.IsForbidden(errors.Unwrap(pe)) {
		t.Errorf("unwrapped error = %v, want a Forbidden API error", errors.Unwrap(pe))
	}
}

func TestPartialErrorMessage(t *testing.T) {
	pe := &PartialError{Resources: []string{"resourceslices"}, Err: errors.New("boom")}
	const want = `could not read resourceslices: boom`
	if got := pe.Error(); got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}

	multi := &PartialError{Resources: []string{"resourceslices", "clusterpolicies"}, Err: errors.New("boom")}
	const wantMulti = `could not read resourceslices, clusterpolicies: boom`
	if got := multi.Error(); got != wantMulti {
		t.Errorf("Error() = %q, want %q", got, wantMulti)
	}
}

// TestNodesListFailureIsFatal pins the other half of the contract: when the
// node list itself cannot be read there is nothing to report, so the result
// is empty and the error names nodes.
func TestNodesListFailureIsFatal(t *testing.T) {
	counter := newCallCounter()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		counter.record("list", "nodes")
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "nodes"}, "", errors.New("not allowed"))
	})

	got, err := NewLive(cs, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())).Nodes(context.Background())
	if err == nil {
		t.Fatal("Nodes() error = nil, want an error when nodes cannot be listed")
	}
	if len(got) != 0 {
		t.Errorf("nodes = %v, want none", names(got))
	}
	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v, want *PartialError", err)
	}
	if len(pe.Resources) != 1 || pe.Resources[0] != "nodes" {
		t.Errorf("PartialError.Resources = %v, want [nodes]", pe.Resources)
	}
}
