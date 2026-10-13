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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// nodeWithCapacity builds a node advertising exactly the resources given.
func nodeWithCapacity(name string, capacity map[corev1.ResourceName]string) corev1.Node {
	list := corev1.ResourceList{}
	for key, value := range capacity {
		list[key] = resource.MustParse(value)
	}

	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     corev1.NodeStatus{Capacity: list},
	}
}

// A node running MIG in the mixed strategy advertises one extended resource
// per profile, nvidia.com/mig-1g.10gb and friends, and no nvidia.com/gpu at
// all. Under the capacity rule alone it classified as hinted, which put every
// mixed-MIG node outside the inventory: the exact nodes a MIG rule is about.
//
// MIG slices are GPU capacity, so they count as membership.
func TestMixedMIGNodeIsAGPUNode(t *testing.T) {
	t.Parallel()

	nodes := []corev1.Node{
		nodeWithCapacity("mixed-mig", map[corev1.ResourceName]string{
			"nvidia.com/mig-1g.10gb": "7",
			"nvidia.com/mig-2g.20gb": "3",
			corev1.ResourceCPU:       "64",
		}),
	}

	gpus, hinted := classifyNodes(nodes, nil)

	if len(gpus) != 1 || len(hinted) != 0 {
		t.Fatalf("classifyNodes = %d GPU and %d hinted nodes, want 1 and 0", len(gpus), len(hinted))
	}

	if gpus[0].Name != "mixed-mig" {
		t.Errorf("GPU node = %q, want %q", gpus[0].Name, "mixed-mig")
	}
}

// The single strategy keeps advertising nvidia.com/gpu, so it must not have
// regressed while the mixed one was being fixed.
func TestSingleStrategyMIGNodeIsStillAGPUNode(t *testing.T) {
	t.Parallel()

	nodes := []corev1.Node{
		nodeWithCapacity("single-mig", map[corev1.ResourceName]string{"nvidia.com/gpu": "7"}),
	}

	gpus, hinted := classifyNodes(nodes, nil)

	if len(gpus) != 1 || len(hinted) != 0 {
		t.Fatalf("classifyNodes = %d GPU and %d hinted nodes, want 1 and 0", len(gpus), len(hinted))
	}

	if gpus[0].Name != "single-mig" {
		t.Errorf("GPU node = %q, want %q", gpus[0].Name, "single-mig")
	}
}

// Zero is not membership, for the MIG resources exactly as for nvidia.com/gpu.
// A node whose mig-manager is reconfiguring briefly advertises its profiles at
// zero, and reporting it as a GPU node with no GPUs reads as a healthy node.
func TestZeroMIGCapacityIsNotAGPUNode(t *testing.T) {
	t.Parallel()

	nodes := []corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "draining-mig",
				Labels: map[string]string{LabelGPUPresent: labelValueTrue},
			},
			Status: corev1.NodeStatus{Capacity: corev1.ResourceList{
				"nvidia.com/mig-1g.10gb": resource.MustParse("0"),
				"nvidia.com/gpu":         resource.MustParse("0"),
			}},
		},
	}

	gpus, hinted := classifyNodes(nodes, nil)

	if len(gpus) != 0 {
		t.Errorf("GPU nodes = %d, want 0: zero capacity is not membership", len(gpus))
	}

	if len(hinted) != 1 || hinted[0].Name != "draining-mig" {
		t.Errorf("hinted = %v, want one draining-mig", hinted)
	}
}

// The prefix has to be the full nvidia.com/mig- and nothing shorter. A node
// advertising an unrelated resource whose name merely contains "mig" is not a
// GPU node, and neither is one advertising nvidia.com/mig with no profile.
func TestMIGPrefixDoesNotOverReach(t *testing.T) {
	t.Parallel()

	nodes := []corev1.Node{
		nodeWithCapacity("other-vendor", map[corev1.ResourceName]string{"example.com/mig-1g.10gb": "4"}),
		nodeWithCapacity("migration-resource", map[corev1.ResourceName]string{"nvidia.com/migration": "4"}),
		nodeWithCapacity("bare-mig", map[corev1.ResourceName]string{"nvidia.com/mig": "4"}),
	}

	gpus, hinted := classifyNodes(nodes, nil)

	if len(gpus) != 0 {
		t.Errorf("GPU nodes = %v, want none: the prefix matched something it should not", gpus)
	}

	if len(hinted) != 0 {
		t.Errorf("hinted = %v, want none", hinted)
	}
}

// A mixed-MIG node with no hint label at all is still a GPU node: membership
// is read from capacity, and the labels never decide it.
func TestMixedMIGNodeNeedsNoHintLabel(t *testing.T) {
	t.Parallel()

	nodes := []corev1.Node{
		nodeWithCapacity("unlabelled-mig", map[corev1.ResourceName]string{"nvidia.com/mig-3g.40gb": "2"}),
	}

	gpus, _ := classifyNodes(nodes, nil)

	if len(gpus) != 1 || gpus[0].Name != "unlabelled-mig" {
		t.Errorf("GPU nodes = %v, want one unlabelled-mig", gpus)
	}
}

// The three membership rules are alternatives, and a node satisfying more than
// one appears once.
func TestMembershipRulesAreAlternatives(t *testing.T) {
	t.Parallel()

	nodes := []corev1.Node{
		nodeWithCapacity("by-gpu", map[corev1.ResourceName]string{"nvidia.com/gpu": "8"}),
		nodeWithCapacity("by-mig", map[corev1.ResourceName]string{"nvidia.com/mig-1g.10gb": "7"}),
		nodeWithCapacity("by-slice", nil),
		nodeWithCapacity("by-all-three", map[corev1.ResourceName]string{
			"nvidia.com/gpu":         "8",
			"nvidia.com/mig-1g.10gb": "7",
		}),
	}

	fromSlices := map[string]struct{}{"by-slice": {}, "by-all-three": {}}

	gpus, hinted := classifyNodes(nodes, fromSlices)

	var got []string
	for _, n := range gpus {
		got = append(got, n.Name)
	}

	want := "by-all-three by-gpu by-mig by-slice"
	if strings.Join(got, " ") != want {
		t.Errorf("GPU nodes = %q, want %q", strings.Join(got, " "), want)
	}

	if len(hinted) != 0 {
		t.Errorf("hinted = %v, want none", hinted)
	}
}

// isGPUNode is the capacity half of the rule on its own, which is what the
// bundle reader applies when it has no slices to go on.
func TestIsGPUNodeCapacityRule(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		capacity map[corev1.ResourceName]string
		want     bool
	}{
		{"whole gpus", map[corev1.ResourceName]string{"nvidia.com/gpu": "1"}, true},
		{"mig profile", map[corev1.ResourceName]string{"nvidia.com/mig-1g.10gb": "1"}, true},
		{"several mig profiles", map[corev1.ResourceName]string{
			"nvidia.com/mig-1g.10gb": "0",
			"nvidia.com/mig-7g.80gb": "1",
		}, true},
		{"zero gpus", map[corev1.ResourceName]string{"nvidia.com/gpu": "0"}, false},
		{"zero mig", map[corev1.ResourceName]string{"nvidia.com/mig-1g.10gb": "0"}, false},
		{"no nvidia resources", map[corev1.ResourceName]string{corev1.ResourceCPU: "64"}, false},
		{"nothing at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			node := nodeWithCapacity("n", tc.capacity)
			if got := isGPUNode(&node); got != tc.want {
				t.Errorf("isGPUNode(%v) = %v, want %v", tc.capacity, got, tc.want)
			}
		})
	}
}
