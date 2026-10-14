/*
 * Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package source

import (
	"context"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// countLists counts the list calls the clientset was asked to make, per
// resource.
type listCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *listCounter) install(f *k8stesting.Fake) {
	f.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		c.mu.Lock()
		defer c.mu.Unlock()

		if c.n == nil {
			c.n = map[string]int{}
		}

		c.n[action.GetResource().Resource]++

		return false, nil, nil
	})
}

func (c *listCounter) count(resource string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.n[resource]
}

// liveWithCounter builds a Live over fakes, counting every list.
func liveWithCounter(t *testing.T, objects ...runtime.Object) (*Live, *listCounter) {
	t.Helper()

	clientset := fake.NewSimpleClientset(objects...)
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())

	counter := &listCounter{}
	counter.install(&clientset.Fake)

	return NewLive(clientset, dyn), counter
}

// TestLiveListsResourceSlicesOnce pins the cost of the read path. The node
// classification needs the slices to decide membership, and the caller needs
// them again to say which slices belong to which node. Those are one question
// asked twice, and asking the API server twice both doubles the cost and lets
// the two answers describe different moments.
func TestLiveListsResourceSlicesOnce(t *testing.T) {
	t.Parallel()

	node := "worker-1"
	live, counter := liveWithCounter(t,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}},
		&resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1-pool-0"},
			Spec:       resourcev1.ResourceSliceSpec{Driver: DRADriverName, NodeName: &node},
		},
	)

	ctx := t.Context()

	gpu, _, err := live.ClassifyNodes(ctx)
	if err != nil {
		t.Fatalf("ClassifyNodes: %v", err)
	}

	if len(gpu) != 1 {
		t.Fatalf("ClassifyNodes found %d GPU nodes, want 1", len(gpu))
	}

	slices, err := live.ResourceSlices(ctx)
	if err != nil {
		t.Fatalf("ResourceSlices: %v", err)
	}

	if len(slices) != 1 {
		t.Fatalf("ResourceSlices returned %d slices, want 1", len(slices))
	}

	if got := counter.count("resourceslices"); got != 1 {
		t.Errorf("the cluster was asked for resourceslices %d times, want 1", got)
	}
}

// TestLiveSliceSnapshotIsShared pins that both answers describe the same
// moment. A slice that appears between the two reads would otherwise make a
// node a GPU node in one answer and not in the other.
func TestLiveSliceSnapshotIsShared(t *testing.T) {
	t.Parallel()

	node := "worker-1"
	clientset := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}})

	var calls int

	clientset.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		// The second read, were there one, would see a slice the first did not.
		list := &resourcev1.ResourceSliceList{}
		if calls > 1 {
			list.Items = append(list.Items, resourcev1.ResourceSlice{
				ObjectMeta: metav1.ObjectMeta{Name: "appeared-later"},
				Spec:       resourcev1.ResourceSliceSpec{Driver: DRADriverName, NodeName: &node},
			})
		}

		return true, list, nil
	})

	live := NewLive(clientset, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()))
	ctx := t.Context()

	if _, _, err := live.ClassifyNodes(ctx); err != nil {
		t.Fatalf("ClassifyNodes: %v", err)
	}

	slices, err := live.ResourceSlices(ctx)
	if err != nil {
		t.Fatalf("ResourceSlices: %v", err)
	}

	if len(slices) != 0 {
		t.Errorf("ResourceSlices returned %d slices, want the snapshot the classification used", len(slices))
	}
}

// TestLiveSliceFailureIsRememberedOnce pins that a denial is cached too. A
// second attempt would be a second denial, a second audit log entry, and a
// second identical cause in the partial error the user reads.
func TestLiveSliceFailureIsRememberedOnce(t *testing.T) {
	t.Parallel()

	clientset := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-1"}})

	var calls int

	clientset.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++

		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}, "", nil)
	})

	live := NewLive(clientset, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()))
	ctx := context.Background()

	if _, _, err := live.ClassifyNodes(ctx); err == nil {
		t.Fatal("ClassifyNodes returned no error for a denied slice list")
	}

	if _, err := live.ResourceSlices(ctx); err == nil {
		t.Fatal("ResourceSlices returned no error for a denied slice list")
	}

	if calls != 1 {
		t.Errorf("the cluster was asked for resourceslices %d times, want 1", calls)
	}
}
