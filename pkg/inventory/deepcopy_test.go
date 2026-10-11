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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// int32Ptr returns a pointer to v. InitResult.ExitCode is a pointer because
// "no exit code yet" and "exited 0" are different facts.
func int32Ptr(v int32) *int32 { return &v }

// populated is a GPUNode with something in every slice, every map and every
// pointer, so that a shallow copy anywhere has a place to show up.
func populated() GPUNode {
	return GPUNode{
		TypeMeta: output.TypeMetaFor(output.KindGPUNode),
		ObjectMeta: metav1.ObjectMeta{
			Name:   "node-a",
			Labels: map[string]string{"nvidia.com/gpu.present": "true"},
		},
		Deploy:         map[string]string{"driver": "true"},
		Topology:       map[string]string{"fabric.topograph.run/tier-1": "block-1"},
		Taints:         []corev1.Taint{{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}},
		Conditions:     []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		ResourceSlices: []string{"node-a-gpu.nvidia.com-abcde"},
		Operands: []OperandPod{{
			Name:           "nvidia-driver-daemonset-xyz",
			Namespace:      "gpu-operator",
			Images:         []string{"nvcr.io/nvidia/driver:580"},
			InitContainers: []InitResult{{Name: "k8s-driver-manager", ExitCode: int32Ptr(0)}},
		}},
	}
}

func TestGPUNodeDeepCopyIsDeep(t *testing.T) {
	t.Parallel()

	original := populated()
	copied := original.DeepCopy()

	copied.ObjectMeta.Labels["nvidia.com/gpu.present"] = "false"
	copied.Deploy["driver"] = "false"
	copied.Topology["fabric.topograph.run/tier-1"] = "block-9"
	copied.Taints[0].Key = "mutated"
	copied.Conditions[0].Status = corev1.ConditionFalse
	copied.ResourceSlices[0] = "mutated"
	copied.Operands[0].Name = "mutated"
	copied.Operands[0].Images[0] = "mutated"
	copied.Operands[0].InitContainers[0].Name = "mutated"
	*copied.Operands[0].InitContainers[0].ExitCode = 1

	want := populated()

	for _, check := range []struct {
		field string
		got   any
		want  any
	}{
		{"labels", original.ObjectMeta.Labels["nvidia.com/gpu.present"], want.ObjectMeta.Labels["nvidia.com/gpu.present"]},
		{"deploy", original.Deploy["driver"], want.Deploy["driver"]},
		{"topology", original.Topology["fabric.topograph.run/tier-1"], want.Topology["fabric.topograph.run/tier-1"]},
		{"taints", original.Taints[0].Key, want.Taints[0].Key},
		{"conditions", original.Conditions[0].Status, want.Conditions[0].Status},
		{"resourceSlices", original.ResourceSlices[0], want.ResourceSlices[0]},
		{"operands.name", original.Operands[0].Name, want.Operands[0].Name},
		{"operands.images", original.Operands[0].Images[0], want.Operands[0].Images[0]},
		{"initContainers.name", original.Operands[0].InitContainers[0].Name, want.Operands[0].InitContainers[0].Name},
		{"initContainers.exitCode", *original.Operands[0].InitContainers[0].ExitCode, *want.Operands[0].InitContainers[0].ExitCode},
	} {
		if check.got != check.want {
			t.Errorf("mutating the copy changed the original %s: got %v, want %v", check.field, check.got, check.want)
		}
	}
}

func TestGPUNodeListDeepCopyIsDeep(t *testing.T) {
	t.Parallel()

	original := GPUNodeList{
		TypeMeta: output.TypeMetaFor(output.KindGPUNodeList),
		Items:    []GPUNode{populated()},
	}

	copied := original.DeepCopy()
	copied.Items[0].Deploy["driver"] = "false"
	copied.Items[0].Operands[0].Images[0] = "mutated"

	if original.Items[0].Deploy["driver"] != "true" {
		t.Errorf("deploy = %q, want %q", original.Items[0].Deploy["driver"], "true")
	}

	if original.Items[0].Operands[0].Images[0] != "nvcr.io/nvidia/driver:580" {
		t.Errorf("images[0] = %q", original.Items[0].Operands[0].Images[0])
	}
}

// Both types are handed to the cli-runtime printers, which take a
// runtime.Object. A type that does not satisfy it fails at the call site
// instead, which is a worse place to find out.
func TestTypesAreRuntimeObjects(t *testing.T) {
	t.Parallel()

	var (
		node runtime.Object = &GPUNode{}
		list runtime.Object = &GPUNodeList{}
	)

	if node.GetObjectKind() == nil || list.GetObjectKind() == nil {
		t.Fatal("GetObjectKind returned nil")
	}

	if _, ok := node.DeepCopyObject().(*GPUNode); !ok {
		t.Error("GPUNode.DeepCopyObject did not return a *GPUNode")
	}

	if _, ok := list.DeepCopyObject().(*GPUNodeList); !ok {
		t.Error("GPUNodeList.DeepCopyObject did not return a *GPUNodeList")
	}
}
