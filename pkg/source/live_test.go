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
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newDynamicFake returns a dynamic client that knows the ClusterPolicy list
// kind, which the fake needs before it will serve that resource at all.
func newDynamicFake(t *testing.T, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}: "ClusterPolicyList",
		},
		objects...,
	)
}

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

// fixture is the cluster the fake client serves, plus the two node sets the
// test derived independently of the package under test.
type fixture struct {
	nodes      []corev1.Node
	slices     []resourcev1.ResourceSlice
	wantGPUs   []string
	wantHinted []string
}

// buildFixture lays out 1200 nodes plus three named edge cases.
//
// Membership is capacity or a ResourceSlice; the two NVIDIA labels are hints
// and never make a node a GPU node on their own. Every fourth node is a GPU
// node and the rule that makes it one rotates, so each membership rule is
// exercised with and without a hint label alongside it. Of the rest, half
// carry a hint label and so are hinted, and half carry nothing.
//
// The expected sets are accumulated here by hand so that the assertions do
// not re-run the classifier under test.
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
		switch i % 4 {
		case 0:
			switch (i / 4) % 4 {
			case 0:
				n.Status.Capacity["nvidia.com/gpu"] = resource.MustParse("8")
			case 1:
				// Capacity with a hint label alongside it: still a GPU node,
				// and the hint must not put it in both sets.
				n.Labels["feature.node.kubernetes.io/pci-10de.present"] = "true"
				n.Status.Capacity["nvidia.com/gpu"] = resource.MustParse("8")
			case 2:
				f.slices = append(f.slices, gpuSliceFor(n.Name))
			case 3:
				n.Labels["nvidia.com/gpu.present"] = "true"
				f.slices = append(f.slices, gpuSliceFor(n.Name))
			}
			f.wantGPUs = append(f.wantGPUs, n.Name)
		case 1:
			// A hint and nothing to back it: the device plugin never
			// published anything here.
			n.Labels["nvidia.com/gpu.present"] = "true"
			f.wantHinted = append(f.wantHinted, n.Name)
		case 2:
			// The other hint label, with the extended resource present but
			// at zero, which is what a crash-looping device plugin leaves
			// behind.
			n.Labels["feature.node.kubernetes.io/pci-10de.present"] = "true"
			n.Status.Capacity["nvidia.com/gpu"] = resource.MustParse("0")
			f.wantHinted = append(f.wantHinted, n.Name)
		}
		f.nodes = append(f.nodes, n)
	}

	// The Mokka control plane: the simulated-GPU DaemonSet tolerates every
	// taint, so node-feature-discovery labelled the control plane even though
	// nothing allocatable ever appeared there. It is a hint, not a GPU node.
	controlPlane := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "node-control-plane",
			Labels: map[string]string{
				"node-role.kubernetes.io/control-plane":       "",
				"feature.node.kubernetes.io/pci-10de.present": "true",
				"nvidia.com/gpu.present":                      "true",
			},
		},
	}
	f.nodes = append(f.nodes, controlPlane)
	f.wantHinted = append(f.wantHinted, controlPlane.Name)

	// A node carried only by a ResourceSlice for a foreign DRA driver belongs
	// to neither set, so it is absent from both expectations.
	other := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-other-driver"}}
	f.nodes = append(f.nodes, other)
	f.slices = append(f.slices, resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-other-driver"},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   "other.example.com",
			NodeName: ptr(other.Name),
			Pool:     resourcev1.ResourcePool{Name: other.Name, ResourceSliceCount: 1},
		},
	})

	sort.Strings(f.wantGPUs)
	sort.Strings(f.wantHinted)
	return f
}

func ptr(s string) *string { return &s }

func gpuSliceFor(node string) resourcev1.ResourceSlice {
	return resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: "slice-" + node},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   "gpu.nvidia.com",
			NodeName: ptr(node),
			Pool:     resourcev1.ResourcePool{Name: node, ResourceSliceCount: 1},
		},
	}
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

// assertNames compares a node slice against an expected, already sorted list
// of names.
func assertNames(t *testing.T, what string, got []corev1.Node, want []string) {
	t.Helper()
	gotNames := names(got)
	if len(gotNames) != len(want) {
		t.Fatalf("%s returned %d nodes, want %d (first few: got %v, want %v)",
			what, len(gotNames), len(want), head(gotNames), head(want))
	}
	for i := range want {
		if gotNames[i] != want[i] {
			t.Fatalf("%s[%d] = %q, want %q", what, i, gotNames[i], want[i])
		}
	}
}

func head(s []string) []string {
	if len(s) > 5 {
		return s[:5]
	}
	return s
}

func TestLiveNodesMembershipAndPagination(t *testing.T) {
	f := buildFixture(1200)
	counter := newCallCounter()

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		counter.record("get", action.GetResource().Resource)
		return true, nil, fmt.Errorf("per-object Get is forbidden in this source")
	})
	cs.PrependReactor("list", "nodes", pagingNodeReactor(f.nodes, counter))
	cs.PrependReactor("list", "resourceslices", sliceReactor(f.slices, counter))

	live := NewLive(cs, newDynamicFake(t))
	got, _, err := live.ClassifyNodes(context.Background())
	if err != nil {
		t.Fatalf("ClassifyNodes() error = %v, want nil", err)
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

	assertNames(t, "Nodes()", got, f.wantGPUs)

	for _, n := range names(got) {
		if n == "node-other-driver" {
			t.Errorf("node-other-driver was returned; a ResourceSlice for driver other.example.com must not count")
		}
		if n == "node-control-plane" {
			t.Errorf("node-control-plane was returned; both hint labels with no allocatable GPU is not membership")
		}
	}
}

func TestLiveHintedNodes(t *testing.T) {
	f := buildFixture(1200)
	counter := newCallCounter()

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		counter.record("get", action.GetResource().Resource)
		return true, nil, fmt.Errorf("per-object Get is forbidden in this source")
	})
	cs.PrependReactor("list", "nodes", pagingNodeReactor(f.nodes, counter))
	cs.PrependReactor("list", "resourceslices", sliceReactor(f.slices, counter))

	live := NewLive(cs, newDynamicFake(t))
	_, got, err := live.ClassifyNodes(context.Background())
	if err != nil {
		t.Fatalf("ClassifyNodes() error = %v, want nil", err)
	}

	if n := counter.get("list", "nodes"); n != 3 {
		t.Errorf("list nodes calls = %d, want 3; one paginated list has to serve the whole classification", n)
	}
	if n := counter.get("get", "nodes"); n != 0 {
		t.Errorf("per-object Get calls = %d, want 0", n)
	}

	assertNames(t, "HintedNodes()", got, f.wantHinted)
}

// TestLiveNodesAndHintedAreDisjoint is the invariant the two health rules rest
// on: a node is a GPU node or a hinted node, never both and never neither by
// accident.
func TestLiveNodesAndHintedAreDisjoint(t *testing.T) {
	f := buildFixture(1200)
	counter := newCallCounter()

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", pagingNodeReactor(f.nodes, counter))
	cs.PrependReactor("list", "resourceslices", sliceReactor(f.slices, counter))
	live := NewLive(cs, newDynamicFake(t))

	gpus, hinted, err := live.ClassifyNodes(context.Background())
	if err != nil {
		t.Fatalf("ClassifyNodes() error = %v", err)
	}

	inGPU := map[string]bool{}
	for _, n := range names(gpus) {
		if inGPU[n] {
			t.Fatalf("Nodes() returned %q twice", n)
		}
		inGPU[n] = true
	}
	seenHint := map[string]bool{}
	for _, n := range names(hinted) {
		if seenHint[n] {
			t.Fatalf("HintedNodes() returned %q twice", n)
		}
		seenHint[n] = true
		if inGPU[n] {
			t.Errorf("%q is in both Nodes() and HintedNodes()", n)
		}
	}

	// 300 of the 1200 are GPU nodes by one of the two membership rules; 600
	// carry a hint with nothing behind it, and the control plane makes 601.
	if len(gpus) != 300 {
		t.Errorf("len(Nodes()) = %d, want 300", len(gpus))
	}
	if len(hinted) != 601 {
		t.Errorf("len(HintedNodes()) = %d, want 601", len(hinted))
	}
}

func TestLiveClassificationEdgeCases(t *testing.T) {
	counter := newCallCounter()
	nodes := []corev1.Node{
		// Sorting is checked here too: the input is deliberately unordered.
		{ObjectMeta: metav1.ObjectMeta{Name: "zeta-capacity"},
			Status: corev1.NodeStatus{Capacity: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("4")}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "alpha-slice-only"}},
		{ObjectMeta: metav1.ObjectMeta{
			Name:   "mid-label-only",
			Labels: map[string]string{"nvidia.com/gpu.present": "true"},
		}},
		{ObjectMeta: metav1.ObjectMeta{
			Name:   "beta-both-rules",
			Labels: map[string]string{"feature.node.kubernetes.io/pci-10de.present": "true"},
		},
			Status: corev1.NodeStatus{Capacity: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("1")}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "gamma-plain"}},
	}
	// beta-both-rules is named by a slice as well as carrying capacity, and
	// alpha-slice-only has nothing but the slice.
	slices := []resourcev1.ResourceSlice{
		gpuSliceFor("alpha-slice-only"),
		gpuSliceFor("beta-both-rules"),
		// A second slice for the same node must not duplicate it.
		gpuSliceFor("beta-both-rules"),
	}

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", pagingNodeReactor(nodes, counter))
	cs.PrependReactor("list", "resourceslices", sliceReactor(slices, counter))
	live := NewLive(cs, newDynamicFake(t))

	gpus, hinted, err := live.ClassifyNodes(context.Background())
	if err != nil {
		t.Fatalf("ClassifyNodes() error = %v", err)
	}
	assertNames(t, "Nodes()", gpus, []string{"alpha-slice-only", "beta-both-rules", "zeta-capacity"})
	assertNames(t, "HintedNodes()", hinted, []string{"mid-label-only"})
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

	got, err := NewLive(cs, newDynamicFake(t)).ResourceSlices(context.Background())
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
		// The label is carried because the fake clientset applies the
		// selector to whatever the reactor returns, on top of the reactor's
		// own behaviour.
		pods = append(pods, corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("pod-%04d", i),
			Namespace: "gpu-operator",
			Labels:    map[string]string{"app": "gpu-operator"},
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

	got, err := NewLive(cs, newDynamicFake(t)).
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

// TestLiveNodesAndHintedWrapClassify keeps the two convenience methods honest:
// they must return the halves ClassifyNodes computed, not a second opinion.
func TestLiveNodesAndHintedWrapClassify(t *testing.T) {
	f := buildFixture(40)
	counter := newCallCounter()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", pagingNodeReactor(f.nodes, counter))
	cs.PrependReactor("list", "resourceslices", sliceReactor(f.slices, counter))
	live := NewLive(cs, newDynamicFake(t))

	gpus, hinted, err := live.ClassifyNodes(context.Background())
	if err != nil {
		t.Fatalf("ClassifyNodes() error = %v", err)
	}
	viaNodes, err := live.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes() error = %v", err)
	}
	viaHinted, err := live.HintedNodes(context.Background())
	if err != nil {
		t.Fatalf("HintedNodes() error = %v", err)
	}
	assertNames(t, "Nodes()", viaNodes, names(gpus))
	assertNames(t, "HintedNodes()", viaHinted, names(hinted))
}

// TestLiveHintLabelValueMustBeTrue pins that the hint labels are read for
// their value and not merely for their presence. node-feature-discovery writes
// "true"; a node carrying "false" is a node that was checked and found not to
// have the feature, which is the opposite of a hint.
func TestLiveHintLabelValueMustBeTrue(t *testing.T) {
	counter := newCallCounter()
	nodes := []corev1.Node{
		{ObjectMeta: metav1.ObjectMeta{Name: "hint-false",
			Labels: map[string]string{"nvidia.com/gpu.present": "false"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "hint-empty",
			Labels: map[string]string{"feature.node.kubernetes.io/pci-10de.present": ""}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "hint-one",
			Labels: map[string]string{"nvidia.com/gpu.present": "1"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "hint-true",
			Labels: map[string]string{"nvidia.com/gpu.present": "true"}}},
	}
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", pagingNodeReactor(nodes, counter))
	cs.PrependReactor("list", "resourceslices", sliceReactor(nil, counter))

	gpus, hinted, err := NewLive(cs, newDynamicFake(t)).ClassifyNodes(context.Background())
	if err != nil {
		t.Fatalf("ClassifyNodes() error = %v", err)
	}
	assertNames(t, "Nodes()", gpus, nil)
	assertNames(t, "HintedNodes()", hinted, []string{"hint-true"})
}

// TestLiveDeduplicatesByName covers a list that reports the same node twice,
// which a paginated read can do when the collection changes under it.
func TestLiveDeduplicatesByName(t *testing.T) {
	counter := newCallCounter()
	gpu := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "dup-gpu"},
		Status: corev1.NodeStatus{Capacity: corev1.ResourceList{"nvidia.com/gpu": resource.MustParse("2")}}}
	hint := corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "dup-hint",
		Labels: map[string]string{"nvidia.com/gpu.present": "true"}}}
	nodes := []corev1.Node{gpu, hint, gpu, hint, gpu}

	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", pagingNodeReactor(nodes, counter))
	cs.PrependReactor("list", "resourceslices", sliceReactor(nil, counter))

	gpus, hinted, err := NewLive(cs, newDynamicFake(t)).ClassifyNodes(context.Background())
	if err != nil {
		t.Fatalf("ClassifyNodes() error = %v", err)
	}
	assertNames(t, "Nodes()", gpus, []string{"dup-gpu"})
	assertNames(t, "HintedNodes()", hinted, []string{"dup-hint"})
}

// TestLiveResourceSlicesNotFoundIsEmpty covers a cluster that does not serve
// resource.k8s.io/v1 at all. That is not a denied read and not a broken
// cluster, so it is an empty list and no error.
func TestLiveResourceSlicesNotFoundIsEmpty(t *testing.T) {
	counter := newCallCounter()
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", pagingNodeReactor(twoNodes(), counter))
	cs.PrependReactor("list", "resourceslices", func(action k8stesting.Action) (bool, runtime.Object, error) {
		counter.record("list", "resourceslices")
		return true, nil, apierrors.NewNotFound(
			schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}, "")
	})
	live := NewLive(cs, newDynamicFake(t))

	slices, err := live.ResourceSlices(context.Background())
	if err != nil {
		t.Fatalf("ResourceSlices() error = %v, want nil on a cluster that does not serve the API", err)
	}
	if len(slices) != 0 {
		t.Errorf("ResourceSlices() returned %d items, want 0", len(slices))
	}

	gpus, hinted, err := live.ClassifyNodes(context.Background())
	if err != nil {
		t.Fatalf("ClassifyNodes() error = %v, want nil", err)
	}
	assertNames(t, "Nodes()", gpus, []string{"gpu-1"})
	assertNames(t, "HintedNodes()", hinted, []string{"hinted-1"})
}

// clusterPolicy builds an unstructured ClusterPolicy at the GVK the GPU
// operator serves.
func clusterPolicy(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nvidia.com/v1",
		"kind":       "ClusterPolicy",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"driver": map[string]any{"enabled": true}},
	}}
}

// TestLiveClusterPoliciesPaginates pins both the resource the dynamic client
// is pointed at and the fact that the read follows the continue token. The
// fake only serves the GVR it was told about, so a typo in the group, version
// or resource yields nothing at all.
//
// The pages are counted by the reactor rather than sliced from the request,
// because a cluster-scoped list cannot carry them through the fake:
// testing.NewRootListActionWithOptions assigns the caller's ListOptions and
// then overwrites the field with a fresh value holding only the selectors, so
// Limit and Continue never reach a reactor. The namespaced constructor keeps
// them, which is why the node and pod tests can assert on Limit directly. The
// discrimination here is the item count: a read that stops after one page
// returns 500 of the 1200.
func TestLiveClusterPoliciesPaginates(t *testing.T) {
	counter := newCallCounter()
	var items []unstructured.Unstructured
	for i := 0; i < 1200; i++ {
		items = append(items, *clusterPolicy(fmt.Sprintf("policy-%04d", i)))
	}

	dyn := newDynamicFake(t)
	dyn.PrependReactor("list", "clusterpolicies", func(action k8stesting.Action) (bool, runtime.Object, error) {
		page := counter.get("list", action.GetResource().String())
		counter.record("list", action.GetResource().String())

		start := page * pageLimit
		if start > len(items) {
			start = len(items)
		}
		end := start + pageLimit
		if end > len(items) {
			end = len(items)
		}
		out := &unstructured.UnstructuredList{
			Object: map[string]any{"apiVersion": "nvidia.com/v1", "kind": "ClusterPolicyList"},
			Items:  append([]unstructured.Unstructured(nil), items[start:end]...),
		}
		if end < len(items) {
			out.SetContinue(strconv.Itoa(end))
		}
		return true, out, nil
	})

	got, err := NewLive(fake.NewSimpleClientset(), dyn).ClusterPolicies(context.Background())
	if err != nil {
		t.Fatalf("ClusterPolicies() error = %v, want nil", err)
	}
	if len(got) != 1200 {
		t.Errorf("ClusterPolicies() returned %d objects, want 1200; an unpaginated read stops at %d", len(got), pageLimit)
	}
	const wantGVR = "nvidia.com/v1, Resource=clusterpolicies"
	if n := counter.get("list", wantGVR); n != 3 {
		t.Errorf("list %s calls = %d, want 3; the read must be paginated at the right GVR", wantGVR, n)
	}
	if len(got) > 0 && got[0].GetName() != "policy-0000" {
		t.Errorf("first policy = %q, want policy-0000", got[0].GetName())
	}
}

// TestLiveClusterPoliciesNotFoundIsNotInstalled covers a cluster without the
// GPU operator: the CRD is absent, so the resource does not exist. That is not
// a partial read, and it is not a crash either; the caller decides what to do.
func TestLiveClusterPoliciesNotFoundIsNotInstalled(t *testing.T) {
	dyn := newDynamicFake(t)
	dyn.PrependReactor("list", "clusterpolicies", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(
			schema.GroupResource{Group: "nvidia.com", Resource: "clusterpolicies"}, "")
	})

	got, err := NewLive(fake.NewSimpleClientset(), dyn).ClusterPolicies(context.Background())
	if len(got) != 0 {
		t.Errorf("ClusterPolicies() returned %d objects, want 0", len(got))
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("error = %v, want it to match ErrNotInstalled", err)
	}
	var pe *PartialError
	if errors.As(err, &pe) {
		t.Errorf("error = %v, want a plain error: an absent CRD is not a partial read", err)
	}
}

// TestLiveFullListFallbackOnExpired covers a collection that changes under a
// paginated read. The server drops the continue token with a 410, and the
// pager starts again without one rather than returning half a cluster.
func TestLiveFullListFallbackOnExpired(t *testing.T) {
	f := buildFixture(1200)
	counter := newCallCounter()

	var expired bool
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("list", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		counter.record("list", "nodes")
		list := action.(k8stesting.ListActionImpl)
		// The second page, the first one asked for with a continue token,
		// fails once with the error a dropped watch cache produces.
		if list.ListOptions.Continue != "" && !expired {
			expired = true
			counter.record("expired", "nodes")
			return true, nil, apierrors.NewResourceExpired("continue token expired")
		}
		return pagingNodeReactor(f.nodes, newCallCounter())(action)
	})
	cs.PrependReactor("list", "resourceslices", sliceReactor(f.slices, counter))

	got, _, err := NewLive(cs, newDynamicFake(t)).ClassifyNodes(context.Background())
	if err != nil {
		t.Fatalf("ClassifyNodes() error = %v, want nil: an expired token must fall back to a full list", err)
	}
	if n := counter.get("expired", "nodes"); n != 1 {
		t.Fatalf("the expired page was served %d times, want 1; the test did not exercise the fallback", n)
	}
	assertNames(t, "Nodes()", got, f.wantGPUs)
}
