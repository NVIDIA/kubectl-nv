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
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// callCounter records how many times the fake client saw each verb/resource
// pair. Reactors run on the caller's goroutine, but a mutex keeps the counter
// honest if that ever stops being true.
type callCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCallCounter() *callCounter {
	return &callCounter{counts: map[string]int{}}
}

func (c *callCounter) record(verb, res string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[verb+" "+res]++
}

func (c *callCounter) get(verb, res string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[verb+" "+res]
}

// fixture is the cluster the fake client serves, plus the GPU node names the
// test derived independently of the package under test.
type fixture struct {
	nodes    []corev1.Node
	slices   []resourcev1.ResourceSlice
	wantGPUs []string
}

// buildFixture lays out 1200 nodes. Every fourth node is a GPU node, and the
// rule that makes it one rotates through the four discovery rules, so each
// rule is exercised 75 times. The expected set is accumulated here by hand so
// that the assertion does not re-run the package's own filter.
func buildFixture(total int) fixture {
	f := fixture{}
	for i := 0; i < total; i++ {
		n := corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   fmt.Sprintf("node-%04d", i),
				Labels: map[string]string{"kubernetes.io/os": "linux"},
			},
			Status: corev1.NodeStatus{
				Capacity: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("64")},
			},
		}
		if i%4 == 0 {
			switch (i / 4) % 4 {
			case 0:
				n.Labels["feature.node.kubernetes.io/pci-10de.present"] = "true"
			case 1:
				n.Labels["nvidia.com/gpu.present"] = "true"
			case 2:
				n.Status.Capacity["nvidia.com/gpu"] = resource.MustParse("8")
			case 3:
				name := n.Name
				f.slices = append(f.slices, resourcev1.ResourceSlice{
					ObjectMeta: metav1.ObjectMeta{Name: "slice-" + name},
					Spec: resourcev1.ResourceSliceSpec{
						Driver:   "gpu.nvidia.com",
						NodeName: &name,
						Pool:     resourcev1.ResourcePool{Name: name, ResourceSliceCount: 1},
					},
				})
			}
			f.wantGPUs = append(f.wantGPUs, n.Name)
		}
		f.nodes = append(f.nodes, n)
	}

	// A node that two rules agree on must still be returned once.
	both := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-both-rules",
			Labels: map[string]string{
				"feature.node.kubernetes.io/pci-10de.present": "true",
				"nvidia.com/gpu.present":                      "true",
			},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")},
		},
	}
	f.nodes = append(f.nodes, both)
	f.wantGPUs = append(f.wantGPUs, both.Name)

	// A node carried only by a ResourceSlice for a foreign DRA driver is not a
	// GPU node, so it is deliberately absent from wantGPUs.
	other := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-other-driver"}}
	otherName := other.Name
	f.nodes = append(f.nodes, other)
	f.slices = append(f.slices, resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-other-driver"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   "other.example.com",
			NodeName: &otherName,
			Pool:     resourcev1.ResourcePool{Name: otherName, ResourceSliceCount: 1},
		},
	})

	sort.Strings(f.wantGPUs)
	return f
}

// pagingNodeReactor serves nodes from items honouring Limit and Continue. The
// fake clientset ignores pagination on its own, so the behaviour under test
// only shows up if the reactor implements it.
func pagingNodeReactor(items []corev1.Node, c *callCounter) k8stesting.ReactionFunc {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		c.record("list", "nodes")
		list, ok := action.(k8stesting.ListActionImpl)
		if !ok {
			return true, nil, fmt.Errorf("unexpected action type %T", action)
		}
		start := 0
		if tok := list.ListOptions.Continue; tok != "" {
			v, err := strconv.Atoi(tok)
			if err != nil {
				return true, nil, fmt.Errorf("bad continue token %q: %w", tok, err)
			}
			start = v
		}
		end := len(items)
		if lim := int(list.ListOptions.Limit); lim > 0 && start+lim < end {
			end = start + lim
		}
		out := &corev1.NodeList{Items: append([]corev1.Node(nil), items[start:end]...)}
		if end < len(items) {
			out.Continue = strconv.Itoa(end)
		}
		return true, out, nil
	}
}

func sliceReactor(items []resourcev1.ResourceSlice, c *callCounter) k8stesting.ReactionFunc {
	return func(action k8stesting.Action) (bool, runtime.Object, error) {
		c.record("list", "resourceslices")
		list, ok := action.(k8stesting.ListActionImpl)
		if !ok {
			return true, nil, fmt.Errorf("unexpected action type %T", action)
		}
		start := 0
		if tok := list.ListOptions.Continue; tok != "" {
			v, err := strconv.Atoi(tok)
			if err != nil {
				return true, nil, fmt.Errorf("bad continue token %q: %w", tok, err)
			}
			start = v
		}
		end := len(items)
		if lim := int(list.ListOptions.Limit); lim > 0 && start+lim < end {
			end = start + lim
		}
		out := &resourcev1.ResourceSliceList{Items: append([]resourcev1.ResourceSlice(nil), items[start:end]...)}
		if end < len(items) {
			out.Continue = strconv.Itoa(end)
		}
		return true, out, nil
	}
}

func names(nodes []corev1.Node) []string {
	out := make([]string, 0, len(nodes))
	for i := range nodes {
		out = append(out, nodes[i].Name)
	}
	return out
}

func TestLiveNodesUnionAndPagination(t *testing.T) {
	f := buildFixture(1200)
	counter := newCallCounter()

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		counter.record("get", action.GetResource().Resource)
		return true, nil, fmt.Errorf("per-object Get is forbidden in this source")
	})
	cs.PrependReactor("list", "nodes", pagingNodeReactor(f.nodes, counter))
	cs.PrependReactor("list", "resourceslices", sliceReactor(f.slices, counter))

	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())

	got, err := NewLive(cs, dyn).Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes() error = %v, want nil", err)
	}

	if n := counter.get("list", "nodes"); n != 3 {
		t.Errorf("list nodes calls = %d, want 3 (1202 nodes at a page limit of 500)", n)
	}
	gets := 0
	for _, res := range []string{"nodes", "pods", "resourceslices"} {
		gets += counter.get("get", res)
	}
	if gets != 0 {
		t.Errorf("per-object Get calls = %d, want 0", gets)
	}

	gotNames := names(got)
	if len(gotNames) != len(f.wantGPUs) {
		t.Fatalf("got %d GPU nodes, want %d", len(gotNames), len(f.wantGPUs))
	}
	for i := range gotNames {
		if gotNames[i] != f.wantGPUs[i] {
			t.Fatalf("GPU node %d = %q, want %q", i, gotNames[i], f.wantGPUs[i])
		}
	}

	for _, n := range gotNames {
		if n == "node-other-driver" {
			t.Errorf("node-other-driver was returned; a ResourceSlice for driver other.example.com must not count")
		}
	}
}

func TestLiveNodesSortedAndDeduplicated(t *testing.T) {
	counter := newCallCounter()
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "zeta", Labels: map[string]string{"nvidia.com/gpu.present": "true"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha", Labels: map[string]string{"nvidia.com/gpu.present": "true"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "mid", Labels: map[string]string{"nvidia.com/gpu.present": "true"}}},
	}
	// "mid" is named by two ResourceSlices as well as its label.
	mid := "mid"
	slices := []resourcev1.ResourceSlice{
		{ObjectMeta: metav1.ObjectMeta{Name: "s1"}, Spec: resourcev1.ResourceSliceSpec{Driver: "gpu.nvidia.com", NodeName: &mid}},
		{ObjectMeta: metav1.ObjectMeta{Name: "s2"}, Spec: resourcev1.ResourceSliceSpec{Driver: "gpu.nvidia.com", NodeName: &mid}},
	}

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", pagingNodeReactor(nodes, counter))
	cs.PrependReactor("list", "resourceslices", sliceReactor(slices, counter))

	got, err := NewLive(cs, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())).Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes() error = %v, want nil", err)
	}
	want := []string{"alpha", "mid", "zeta"}
	gotNames := names(got)
	if len(gotNames) != len(want) {
		t.Fatalf("got %v, want %v", gotNames, want)
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("got %v, want %v", gotNames, want)
		}
	}
}

func TestLiveResourceSlicesFilterDriver(t *testing.T) {
	counter := newCallCounter()
	a, b := "n1", "n2"
	slices := []resourcev1.ResourceSlice{
		{ObjectMeta: metav1.ObjectMeta{Name: "keep"}, Spec: resourcev1.ResourceSliceSpec{Driver: "gpu.nvidia.com", NodeName: &a}},
		{ObjectMeta: metav1.ObjectMeta{Name: "drop"}, Spec: resourcev1.ResourceSliceSpec{Driver: "other.example.com", NodeName: &b}},
	}
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "resourceslices", sliceReactor(slices, counter))

	got, err := NewLive(cs, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())).ResourceSlices(context.Background())
	if err != nil {
		t.Fatalf("ResourceSlices() error = %v, want nil", err)
	}
	if len(got) != 1 || got[0].Name != "keep" {
		t.Fatalf("got %v, want exactly [keep]", func() []string {
			out := []string{}
			for i := range got {
				out = append(out, got[i].Name)
			}
			return out
		}())
	}
}

func TestLivePodsPaginates(t *testing.T) {
	counter := newCallCounter()
	var pods []corev1.Pod
	for i := 0; i < 1200; i++ {
		pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("pod-%04d", i),
			Namespace: "gpu-operator",
		}})
	}
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		counter.record("list", "pods")
		list := action.(k8stesting.ListActionImpl)
		if got := list.ListOptions.LabelSelector; got != "app=gpu-operator" {
			return true, nil, fmt.Errorf("label selector = %q, want app=gpu-operator", got)
		}
		start := 0
		if tok := list.ListOptions.Continue; tok != "" {
			start, _ = strconv.Atoi(tok)
		}
		end := len(pods)
		if lim := int(list.ListOptions.Limit); lim > 0 && start+lim < end {
			end = start + lim
		}
		out := &corev1.PodList{Items: append([]corev1.Pod(nil), pods[start:end]...)}
		if end < len(pods) {
			out.Continue = strconv.Itoa(end)
		}
		return true, out, nil
	})

	got, err := NewLive(cs, dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())).
		Pods(context.Background(), "gpu-operator", "app=gpu-operator")
	if err != nil {
		t.Fatalf("Pods() error = %v, want nil", err)
	}
	if len(got) != 1200 {
		t.Errorf("got %d pods, want 1200", len(got))
	}
	if n := counter.get("list", "pods"); n != 3 {
		t.Errorf("list pods calls = %d, want 3", n)
	}
}
