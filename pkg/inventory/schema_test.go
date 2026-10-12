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
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The two kinds this package owns have to be in the registry by the time
// anything links it. Nothing else can put them there: the registry is filled
// at import time, and a kind registered by a command package instead would be
// absent from any binary that does not link that command.
func TestThisPackageRegistersItsKinds(t *testing.T) {
	t.Parallel()

	kinds := output.RegisteredKinds()
	for _, kind := range []string{output.KindGPUNode, output.KindGPUNodeList} {
		if !slices.Contains(kinds, kind) {
			t.Errorf("%s is not registered; registered kinds are %v", kind, kinds)
		}
	}
}

// populatedNode is a node with every field of the kind set to something, so
// that the schema is checked against a full instance rather than against the
// handful of fields a zero value happens to marshal.
func populatedNode(name string) GPUNode {
	return GPUNode{
		TypeMeta: output.TypeMetaFor(output.KindGPUNode),
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Labels:            map[string]string{"nvidia.com/gpu.present": "true"},
			Annotations:       map[string]string{"note": "kept"},
			CreationTimestamp: metav1.NewTime(time.Unix(1760000000, 0).UTC()),
		},
		Product:        "NVIDIA-H100-80GB-HBM3",
		Count:          8,
		Capacity:       8,
		Allocatable:    8,
		Requested:      3,
		RequestedKnown: true,
		DriverVersion:  "550.54.15",
		CUDAVersion:    "12.4",
		Mode:           ModeDevicePlugin,
		MIG: MIG{
			Strategy:    "mixed",
			Config:      "all-1g.10gb",
			ConfigState: "success",
			Capable:     true,
			Resources:   map[string]ResourceCount{"nvidia.com/mig-1g.10gb": {Capacity: 7, Allocatable: 7}},
		},
		Sharing:       Sharing{Strategy: "time-slicing", Replicas: 4},
		UpgradeState:  "upgrade-done",
		Deploy:        map[string]string{"driver": "true"},
		Clique:        "clique-a",
		Topology:      map[string]string{"accelerator.topograph.run/domain": "d1"},
		Ready:         true,
		Unschedulable: false,
		Taints: []corev1.Taint{{
			Key:    "nvidia.com/gpu",
			Value:  "present",
			Effect: corev1.TaintEffectNoSchedule,
		}},
		Conditions: []corev1.NodeCondition{{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  metav1.NewTime(time.Unix(1760000001, 0).UTC()),
			LastTransitionTime: metav1.NewTime(time.Unix(1760000002, 0).UTC()),
			Reason:             "KubeletReady",
			Message:            "kubelet is posting ready status",
		}},
		Operands: []OperandPod{{
			Name:            "nvidia-driver-daemonset-abcde",
			Namespace:       "gpu-operator",
			Component:       ComponentDriver,
			Phase:           string(corev1.PodRunning),
			Restarts:        2,
			Images:          []string{"nvcr.io/nvidia/driver:550.54.15"},
			Ready:           true,
			LastTermination: Termination{Observed: true, ExitCode: 1, Reason: "Error", FinishedAt: "2026-10-01T00:00:00Z"},
			InitContainers: []InitResult{{
				Name:            "toolkit-validation",
				ExitCode:        ptr.To(int32(0)),
				Reason:          "Completed",
				LastTermination: Termination{Observed: true, Reason: "Completed", FinishedAt: "2026-10-01T00:00:00Z"},
			}},
		}},
		ResourceSlices: []string{"node-1-gpu"},
		Hinted:         false,
	}
}

// A schema is only a contract if what the command prints satisfies it. The
// generator reads the Go type, so it cannot catch a field that marshals as
// null where the schema says array; running a sample through the published
// schema can.
func TestAPopulatedGPUNodeSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	node := populatedNode("node-1")
	if err := output.ValidateAgainstSchema(output.KindGPUNode, &node); err != nil {
		t.Fatal(err)
	}
}

// The zero value is the other end of the range: every never-null guarantee in
// MarshalJSON turns a nil into an empty array, and the schema has to accept
// that shape too.
func TestAnEmptyGPUNodeSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	if err := output.ValidateAgainstSchema(output.KindGPUNode, NewGPUNode("node-1")); err != nil {
		t.Fatal(err)
	}
}

func TestAGPUNodeListSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	list := NewGPUNodeList()
	list.Items = append(list.Items, populatedNode("node-1"), populatedNode("node-2"))

	if err := output.ValidateAgainstSchema(output.KindGPUNodeList, list); err != nil {
		t.Fatal(err)
	}
}

func TestAnEmptyGPUNodeListSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	if err := output.ValidateAgainstSchema(output.KindGPUNodeList, NewGPUNodeList()); err != nil {
		t.Fatal(err)
	}
}
