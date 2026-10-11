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

package inventory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// countingSource is a Source that answers from fixed data and counts what it
// was asked. The counts are the point: a builder that gets the same answer by
// asking once per node is a builder that takes minutes on a real cluster.
type countingSource struct {
	gpu    []corev1.Node
	hinted []corev1.Node
	pods   []corev1.Pod
	slices []resourcev1.ResourceSlice

	// The errors each method returns alongside its data.
	classifyErr error
	podsErr     error
	sliceErr    error

	calls map[string]int
}

func (s *countingSource) count(name string) {
	if s.calls == nil {
		s.calls = map[string]int{}
	}

	s.calls[name]++
}

func (s *countingSource) Nodes(context.Context) ([]corev1.Node, error) {
	s.count("Nodes")

	return s.gpu, s.classifyErr
}

func (s *countingSource) HintedNodes(context.Context) ([]corev1.Node, error) {
	s.count("HintedNodes")

	return s.hinted, s.classifyErr
}

func (s *countingSource) ClassifyNodes(context.Context) ([]corev1.Node, []corev1.Node, error) {
	s.count("ClassifyNodes")

	return s.gpu, s.hinted, s.classifyErr
}

func (s *countingSource) Pods(_ context.Context, namespace, selector string) ([]corev1.Pod, error) {
	s.count("Pods")
	s.count("Pods(" + namespace + "," + selector + ")")

	return s.pods, s.podsErr
}

func (s *countingSource) ResourceSlices(context.Context) ([]resourcev1.ResourceSlice, error) {
	s.count("ResourceSlices")

	return s.slices, s.sliceErr
}

func (s *countingSource) ClusterPolicies(context.Context) ([]unstructured.Unstructured, error) {
	s.count("ClusterPolicies")

	return nil, nil
}

// gpuSlice is a ResourceSlice the NVIDIA DRA driver published for a node.
func gpuSlice(name, node string) resourcev1.ResourceSlice {
	return resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       resourcev1.ResourceSliceSpec{Driver: source.DRADriverName, NodeName: &node},
	}
}

// itemNamed returns the node of that name, failing the test when the list does
// not hold it. Reaching into Items by index would make every assertion below
// depend on the order as well as on what it is about.
func itemNamed(t *testing.T, list *GPUNodeList, name string) *GPUNode {
	t.Helper()

	if list == nil {
		t.Fatalf("no list to look for %q in", name)
	}

	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i]
		}
	}

	t.Fatalf("the list holds no node named %q", name)

	return nil
}

// itemNames lists what the list holds, in the order it holds it.
func itemNames(list *GPUNodeList) []string {
	names := make([]string, 0, len(list.Items))
	for i := range list.Items {
		names = append(names, list.Items[i].Name)
	}

	return names
}

// TestBuildReadsTheClusterOnce is the cost contract of gpu list: one
// classification, one pod list, one slice list, whatever the cluster's size,
// and never a read keyed on a single node.
func TestBuildReadsTheClusterOnce(t *testing.T) {
	t.Parallel()

	const nodeCount = 1200

	src := &countingSource{}
	for i := range nodeCount {
		src.gpu = append(src.gpu, *testNode(fmt.Sprintf("node-%04d", i), nil,
			map[string]string{"nvidia.com/gpu": "8"}, map[string]string{"nvidia.com/gpu": "8"}))
	}

	list, err := Build(context.Background(), src, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if len(list.Items) != nodeCount {
		t.Fatalf("built %d nodes, want %d", len(list.Items), nodeCount)
	}

	want := map[string]int{"ClassifyNodes": 1, "Pods": 1, "ResourceSlices": 1}
	for name, n := range want {
		if src.calls[name] != n {
			t.Errorf("%s called %d times, want %d", name, src.calls[name], n)
		}
	}

	// Nodes and HintedNodes are the halves of ClassifyNodes: calling them as
	// well is a second pass over the cluster describing a second moment.
	for _, name := range []string{"Nodes", "HintedNodes", "ClusterPolicies"} {
		if src.calls[name] != 0 {
			t.Errorf("%s called %d times, want none", name, src.calls[name])
		}
	}

	// The one pod list covers every namespace and filters nothing server side,
	// because the operands and the requested GPUs come out of the same read.
	if src.calls["Pods(,)"] != 1 {
		t.Errorf("the pod list was not the unfiltered all-namespace one: %#v", src.calls)
	}
}

// TestBuildOrdersByName pins the deterministic order, which is what makes two
// runs of gpu list diffable.
func TestBuildOrdersByName(t *testing.T) {
	t.Parallel()

	src := &countingSource{gpu: []corev1.Node{
		*testNode("zeta", nil, map[string]string{"nvidia.com/gpu": "1"}, nil),
		*testNode("alpha", nil, map[string]string{"nvidia.com/gpu": "1"}, nil),
		*testNode("middle", nil, map[string]string{"nvidia.com/gpu": "1"}, nil),
	}}

	list, err := Build(context.Background(), src, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got := itemNames(list)
	want := []string{"alpha", "middle", "zeta"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

// TestBuildPassesAPartialErrorThrough is the exit code 3 contract: the items
// alongside a PartialError are usable, so the builder must return both.
func TestBuildPassesAPartialErrorThrough(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		src  *countingSource
	}{
		{
			name: "the slice list was denied",
			src: &countingSource{
				gpu:      []corev1.Node{*testNode("n1", nil, map[string]string{"nvidia.com/gpu": "1"}, nil)},
				sliceErr: &source.PartialError{Resource: "resourceslices", Err: errors.New("forbidden")},
			},
		},
		{
			name: "the pod list was denied",
			src: &countingSource{
				gpu:     []corev1.Node{*testNode("n1", nil, map[string]string{"nvidia.com/gpu": "1"}, nil)},
				podsErr: &source.PartialError{Resource: "pods", Err: errors.New("forbidden")},
			},
		},
		{
			name: "the classification was partial",
			src: &countingSource{
				gpu:         []corev1.Node{*testNode("n1", nil, map[string]string{"nvidia.com/gpu": "1"}, nil)},
				classifyErr: &source.PartialError{Resource: "resourceslices", Err: errors.New("forbidden")},
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			list, err := Build(context.Background(), tt.src, BuildOptions{})

			var partial *source.PartialError
			if !errors.As(err, &partial) {
				t.Fatalf("Build error = %v, want a source.PartialError", err)
			}

			if list == nil || len(list.Items) != 1 {
				t.Fatalf("Build returned %v items alongside the partial error, want the node it did read", list)
			}
		})
	}
}

// TestBuildPartialPodsLeavesRequestedUnknown is the half-answer that would
// otherwise read as a true one: a node whose pods were only half listed must
// not report the half as its GPU demand.
func TestBuildPartialPodsLeavesRequestedUnknown(t *testing.T) {
	t.Parallel()

	src := &countingSource{
		gpu:     []corev1.Node{*testNode("n1", nil, map[string]string{"nvidia.com/gpu": "8"}, nil)},
		pods:    []corev1.Pod{testPod("p", "n1", corev1.PodRunning, nil, []corev1.Container{gpuContainer("app", 2)})},
		podsErr: &source.PartialError{Resource: "pods", Err: errors.New("forbidden")},
	}

	list, err := Build(context.Background(), src, BuildOptions{})
	if err == nil {
		t.Fatal("Build returned no error, want the pod PartialError")
	}

	node := itemNamed(t, list, "n1")
	if node.RequestedKnown {
		t.Error("RequestedKnown = true after a partial pod list, want false")
	}

	if node.Requested != 0 {
		t.Errorf("Requested = %d after a partial pod list, want 0", node.Requested)
	}
}

// TestBuildFailsOnANodeListThatFailed pins that an outright failure is not
// dressed up as a partial one: with no nodes there is nothing to classify.
func TestBuildFailsOnANodeListThatFailed(t *testing.T) {
	t.Parallel()

	src := &countingSource{classifyErr: errors.New("connection refused")}

	list, err := Build(context.Background(), src, BuildOptions{})
	if err == nil {
		t.Fatal("Build returned no error for a failed node list")
	}

	var partial *source.PartialError
	if errors.As(err, &partial) {
		t.Errorf("Build error = %v, want a plain error rather than a partial one", err)
	}

	if list != nil {
		t.Errorf("Build returned %v, want no list at all", list)
	}
}

// TestBuildNoPods pins --no-pods: the pod list is not made, and the number it
// would have produced is reported as unknown rather than as zero.
func TestBuildNoPods(t *testing.T) {
	t.Parallel()

	src := &countingSource{
		gpu:  []corev1.Node{*testNode("n1", nil, map[string]string{"nvidia.com/gpu": "8"}, nil)},
		pods: []corev1.Pod{testPod("p", "n1", corev1.PodRunning, nil, []corev1.Container{gpuContainer("app", 2)})},
	}

	list, err := Build(context.Background(), src, BuildOptions{NoPods: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if src.calls["Pods"] != 0 {
		t.Errorf("Pods called %d times under --no-pods, want none", src.calls["Pods"])
	}

	if itemNamed(t, list, "n1").RequestedKnown {
		t.Error("RequestedKnown = true under --no-pods, want false")
	}
}

// TestBuildHinted pins that a hinted node is out of the list unless it was
// asked for, and that the count of what was left out is reported.
func TestBuildHinted(t *testing.T) {
	t.Parallel()

	newSource := func() *countingSource {
		return &countingSource{
			gpu: []corev1.Node{*testNode("worker-1", nil, map[string]string{"nvidia.com/gpu": "8"}, nil)},
			hinted: []corev1.Node{
				*testNode("control-plane", map[string]string{"nvidia.com/gpu.present": "true"}, nil, nil),
				*testNode("broken-1", map[string]string{"nvidia.com/gpu.present": "true"}, nil, nil),
			},
		}
	}

	t.Run("left out by default", func(t *testing.T) {
		t.Parallel()

		list, excluded, err := BuildWithHinted(context.Background(), newSource(), BuildOptions{})
		if err != nil {
			t.Fatalf("BuildWithHinted: %v", err)
		}

		if got := itemNames(list); !reflect.DeepEqual(got, []string{"worker-1"}) {
			t.Fatalf("items = %v, want only worker-1", got)
		}

		if excluded != 2 {
			t.Errorf("excluded = %d, want 2", excluded)
		}
	})

	t.Run("included on request", func(t *testing.T) {
		t.Parallel()

		list, excluded, err := BuildWithHinted(context.Background(), newSource(), BuildOptions{IncludeHinted: true})
		if err != nil {
			t.Fatalf("BuildWithHinted: %v", err)
		}

		got := make(map[string]bool, len(list.Items))
		for i := range list.Items {
			got[list.Items[i].Name] = list.Items[i].Hinted
		}

		want := map[string]bool{"broken-1": true, "control-plane": true, "worker-1": false}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("items = %#v, want %#v", got, want)
		}

		if excluded != 0 {
			t.Errorf("excluded = %d, want 0 when the hinted nodes were included", excluded)
		}
	})
}

// TestBuildNodeFilter pins --node, which has to reach a hinted node too: the
// node a user asks about by name is most often the one that went quiet.
func TestBuildNodeFilter(t *testing.T) {
	t.Parallel()

	src := &countingSource{
		gpu: []corev1.Node{
			*testNode("worker-1", nil, map[string]string{"nvidia.com/gpu": "8"}, nil),
			*testNode("worker-2", nil, map[string]string{"nvidia.com/gpu": "8"}, nil),
		},
		hinted: []corev1.Node{*testNode("broken-1", map[string]string{"nvidia.com/gpu.present": "true"}, nil, nil)},
	}

	t.Run("a GPU node", func(t *testing.T) {
		t.Parallel()

		list, err := Build(context.Background(), src, BuildOptions{Node: "worker-2"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		if got := itemNames(list); !reflect.DeepEqual(got, []string{"worker-2"}) {
			t.Fatalf("items = %v, want only worker-2", got)
		}
	})

	t.Run("a hinted node", func(t *testing.T) {
		t.Parallel()

		list, err := Build(context.Background(), src, BuildOptions{Node: "broken-1", IncludeHinted: true})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		if got := itemNames(list); !reflect.DeepEqual(got, []string{"broken-1"}) {
			t.Fatalf("items = %v, want only broken-1", got)
		}

		if !itemNamed(t, list, "broken-1").Hinted {
			t.Error("broken-1 is not marked hinted")
		}
	})

	t.Run("a node the cluster does not have", func(t *testing.T) {
		t.Parallel()

		list, err := Build(context.Background(), src, BuildOptions{Node: "nowhere"})
		if err != nil {
			t.Fatalf("Build: %v", err)
		}

		if got := itemNames(list); len(got) != 0 {
			t.Fatalf("items = %v, want none", got)
		}
	})
}

// TestBuildSelector pins -l. A selector that does not parse is the user's to
// fix and must stop the run rather than quietly match everything.
func TestBuildSelector(t *testing.T) {
	t.Parallel()

	src := &countingSource{gpu: []corev1.Node{
		*testNode("a100-1", map[string]string{"nvidia.com/gpu.product": "A100-SXM4-40GB"},
			map[string]string{"nvidia.com/gpu": "8"}, nil),
		*testNode("t4-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-T4"},
			map[string]string{"nvidia.com/gpu": "4"}, nil),
	}}

	list, err := Build(context.Background(), src, BuildOptions{Selector: "nvidia.com/gpu.product=NVIDIA-T4"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if got := itemNames(list); !reflect.DeepEqual(got, []string{"t4-1"}) {
		t.Fatalf("items = %v, want only t4-1", got)
	}

	if _, err := Build(context.Background(), src, BuildOptions{Selector: "=="}); err == nil {
		t.Error("Build accepted a selector that does not parse")
	}
}

// TestBuildMode walks the four modes end to end, because the slice set reaches
// a node through the builder rather than through the node object.
func TestBuildMode(t *testing.T) {
	t.Parallel()

	src := &countingSource{
		gpu: []corev1.Node{
			*testNode("dp-1", nil, map[string]string{"nvidia.com/gpu": "8"}, nil),
			*testNode("dra-1", nil, nil, nil),
			*testNode("both-1", nil, map[string]string{"nvidia.com/gpu": "8"}, nil),
		},
		hinted: []corev1.Node{*testNode("none-1", map[string]string{"nvidia.com/gpu.present": "true"}, nil, nil)},
		slices: []resourcev1.ResourceSlice{
			gpuSlice("dra-1-pool-0", "dra-1"),
			gpuSlice("both-1-pool-0", "both-1"),
		},
	}

	list, err := Build(context.Background(), src, BuildOptions{IncludeHinted: true, WithDetails: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got := make(map[string]string, len(list.Items))
	for i := range list.Items {
		got[list.Items[i].Name] = list.Items[i].Mode
	}

	want := map[string]string{
		"dp-1":   ModeDevicePlugin,
		"dra-1":  ModeDRA,
		"both-1": ModeBoth,
		"none-1": ModeNone,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("modes = %#v, want %#v", got, want)
	}

	if slices := itemNamed(t, list, "dra-1").ResourceSlices; !reflect.DeepEqual(slices, []string{"dra-1-pool-0"}) {
		t.Errorf("dra-1 ResourceSlices = %v, want its own slice", slices)
	}
}

// TestBuildListIsPrintable pins the D0 review finding: a GPUNodeList literal
// fails at -o json because the cli-runtime printers read TypeMeta off the
// struct before anything is marshalled.
func TestBuildListIsPrintable(t *testing.T) {
	t.Parallel()

	src := &countingSource{gpu: []corev1.Node{
		*testNode("n1", nil, map[string]string{"nvidia.com/gpu": "1"}, nil),
	}}

	list, err := Build(context.Background(), src, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if list.GetObjectKind().GroupVersionKind().Empty() {
		t.Error("the list carries no GroupVersionKind: the printers refuse it")
	}

	if itemNamed(t, list, "n1").GetObjectKind().GroupVersionKind().Empty() {
		t.Error("an item carries no GroupVersionKind: -o name refuses it")
	}
}
