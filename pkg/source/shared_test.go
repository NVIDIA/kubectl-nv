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
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// sharedNode is a node under time slicing with renameByDefault set: the
// device plugin advertises the replicas under nvidia.com/gpu.shared and
// nothing at all under nvidia.com/gpu.
func sharedNode(name string, shared int64) corev1.Node {
	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"nvidia.com/gpu.present":          "true",
				"nvidia.com/gpu.sharing-strategy": "time-slicing",
				"nvidia.com/gpu.replicas":         "4",
			},
		},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				"nvidia.com/gpu.shared": *resource.NewQuantity(shared, resource.DecimalSI),
			},
			Allocatable: corev1.ResourceList{
				"nvidia.com/gpu.shared": *resource.NewQuantity(shared, resource.DecimalSI),
			},
		},
	}
}

// TestSharedResourceIsMembership pins that a node advertising only
// nvidia.com/gpu.shared is a GPU node.
//
// renameByDefault moves every replica onto that name and leaves nvidia.com/gpu
// at nothing (api/config/v1/consts.go:26 and resources.go:89 in
// k8s-device-plugin, which appends ".shared" to the resource name). Under the
// whole-GPU rule alone such a node classifies as hinted, which puts a working,
// schedulable, time sliced node outside the inventory that every rule reads:
// the same failure the mixed MIG strategy already caused.
func TestSharedResourceIsMembership(t *testing.T) {
	t.Parallel()

	gpus, hinted := classifyNodes([]corev1.Node{sharedNode("ts-1", 16)}, nil)

	if len(gpus) != 1 || gpus[0].Name != "ts-1" {
		t.Errorf("GPU nodes = %v, want ts-1", nodeNamesOf(gpus))
	}

	if len(hinted) != 0 {
		t.Errorf("hinted = %v, want none", nodeNamesOf(hinted))
	}
}

// TestSharedResourceAtZeroIsNotMembership pins the same rule the whole-GPU and
// MIG cases follow: zero placeable devices is not a GPU node, however the node
// is labelled.
func TestSharedResourceAtZeroIsNotMembership(t *testing.T) {
	t.Parallel()

	gpus, hinted := classifyNodes([]corev1.Node{sharedNode("ts-0", 0)}, nil)

	if len(gpus) != 0 {
		t.Errorf("GPU nodes = %v, want none", nodeNamesOf(gpus))
	}

	if len(hinted) != 1 {
		t.Errorf("hinted = %v, want ts-0", nodeNamesOf(hinted))
	}
}

// TestSharedPrefixIsNotMatchedLoosely pins that the match is on the exact
// resource name. A prefix match on "nvidia.com/gpu" would already have
// accepted this node, and a substring match would accept resources that are
// not GPUs at all.
func TestSharedPrefixIsNotMatchedLoosely(t *testing.T) {
	t.Parallel()

	node := corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "impostor", Labels: map[string]string{"nvidia.com/gpu.present": "true"}},
		Status: corev1.NodeStatus{
			Capacity: corev1.ResourceList{
				"nvidia.com/gpu.shared.extra": *resource.NewQuantity(8, resource.DecimalSI),
				"nvidia.com/gpuish":           *resource.NewQuantity(8, resource.DecimalSI),
			},
		},
	}

	gpus, hinted := classifyNodes([]corev1.Node{node}, nil)

	if len(gpus) != 0 {
		t.Errorf("GPU nodes = %v, want none: neither resource is a GPU", nodeNamesOf(gpus))
	}

	if len(hinted) != 1 {
		t.Errorf("hinted = %v, want the impostor", nodeNamesOf(hinted))
	}
}

// nodeNamesOf names a node slice, for the failure messages above.
func nodeNamesOf(nodes []corev1.Node) []string {
	out := make([]string, 0, len(nodes))
	for i := range nodes {
		out = append(out, nodes[i].Name)
	}

	return out
}
