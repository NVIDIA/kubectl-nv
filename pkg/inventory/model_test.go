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
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// testNode builds a synthetic node. The quantities are written as the strings
// the API serves, so a test says what the cluster would have said.
func testNode(name string, labels map[string]string, capacity, allocatable map[string]string) *corev1.Node {
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Labels:            labels,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)),
		},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{},
			Allocatable: corev1.ResourceList{},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}

	for k, v := range capacity {
		node.Status.Capacity[corev1.ResourceName(k)] = resource.MustParse(v)
	}

	for k, v := range allocatable {
		node.Status.Allocatable[corev1.ResourceName(k)] = resource.MustParse(v)
	}

	return node
}

// equal fails the test when got and want differ, naming the field.
func equal[T any](t *testing.T, field string, got, want T) {
	t.Helper()

	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", field, got, want)
	}
}

func TestBuildNode(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		node  *corev1.Node
		ctx   nodeContext
		check func(t *testing.T, got *GPUNode)
	}{
		{
			name: "device plugin only",
			node: testNode("dp-1",
				map[string]string{"nvidia.com/gpu.product": "NVIDIA-T4", "nvidia.com/gpu.count": "4"},
				map[string]string{"nvidia.com/gpu": "4"},
				map[string]string{"nvidia.com/gpu": "4"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "Mode", got.Mode, ModeDevicePlugin)
				equal(t, "Capacity", got.Capacity, int64(4))
				equal(t, "Allocatable", got.Allocatable, int64(4))
				equal(t, "Product", got.Product, "NVIDIA-T4")
				equal(t, "Count", got.Count, 4)
			},
		},
		{
			name: "DRA only",
			node: testNode("dra-1", nil, nil, nil),
			ctx:  nodeContext{sliceNames: []string{"dra-1-gpu-pool"}, withDetails: true},
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "Mode", got.Mode, ModeDRA)
				equal(t, "Capacity", got.Capacity, int64(0))
				equal(t, "ResourceSlices", got.ResourceSlices, []string{"dra-1-gpu-pool"})
			},
		},
		{
			name: "device plugin and DRA together",
			node: testNode("both-1", nil,
				map[string]string{"nvidia.com/gpu": "8"},
				map[string]string{"nvidia.com/gpu": "8"}),
			ctx: nodeContext{sliceNames: []string{"both-1-gpu-pool"}},
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "Mode", got.Mode, ModeBoth)
			},
		},
		{
			name: "hinted node advertises neither way",
			node: testNode("hinted-1",
				map[string]string{"nvidia.com/gpu.present": "true"}, nil, nil),
			ctx: nodeContext{hinted: true},
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "Mode", got.Mode, ModeNone)
				equal(t, "Hinted", got.Hinted, true)
			},
		},
		{
			name: "cordoned node",
			node: func() *corev1.Node {
				n := testNode("cordoned-1", nil,
					map[string]string{"nvidia.com/gpu": "1"},
					map[string]string{"nvidia.com/gpu": "1"})
				n.Spec.Unschedulable = true
				n.Spec.Taints = []corev1.Taint{{
					Key:    "node.kubernetes.io/unschedulable",
					Effect: corev1.TaintEffectNoSchedule,
				}}

				return n
			}(),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "Unschedulable", got.Unschedulable, true)
				equal(t, "Ready", got.Ready, true)

				if len(got.Taints) != 1 || got.Taints[0].Key != "node.kubernetes.io/unschedulable" {
					t.Errorf("Taints = %#v, want the unschedulable taint", got.Taints)
				}
			},
		},
		{
			name: "not ready node",
			node: func() *corev1.Node {
				n := testNode("down-1", nil,
					map[string]string{"nvidia.com/gpu": "1"},
					map[string]string{"nvidia.com/gpu": "1"})
				n.Status.Conditions = []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
				}

				return n
			}(),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "Ready", got.Ready, false)
			},
		},
		{
			name: "MIG single",
			node: testNode("mig-single-1",
				map[string]string{
					"nvidia.com/mig.strategy":     "single",
					"nvidia.com/mig.capable":      "true",
					"nvidia.com/mig.config":       "all-1g.5gb",
					"nvidia.com/mig.config.state": "success",
					"nvidia.com/gpu.product":      "A100-SXM4-40GB-MIG-1g.5gb",
				},
				map[string]string{"nvidia.com/gpu": "7"},
				map[string]string{"nvidia.com/gpu": "7"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "MIG.Strategy", got.MIG.Strategy, "single")
				equal(t, "MIG.Config", got.MIG.Config, "all-1g.5gb")
				equal(t, "MIG.ConfigState", got.MIG.ConfigState, "success")
				equal(t, "MIG.Capable", got.MIG.Capable, true)

				if len(got.MIG.Resources) != 0 {
					t.Errorf("MIG.Resources = %#v, want none under the single strategy", got.MIG.Resources)
				}

				equal(t, "Capacity", got.Capacity, int64(7))
				equal(t, "Mode", got.Mode, ModeDevicePlugin)
			},
		},
		{
			name: "MIG mixed advertises one resource per profile",
			node: testNode("mig-mixed-1",
				map[string]string{"nvidia.com/mig.strategy": "mixed", "nvidia.com/mig.capable": "true"},
				map[string]string{"nvidia.com/mig-1g.10gb": "14", "nvidia.com/mig-3g.40gb": "4"},
				map[string]string{"nvidia.com/mig-1g.10gb": "12", "nvidia.com/mig-3g.40gb": "4"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "MIG.Strategy", got.MIG.Strategy, "mixed")
				equal(t, "MIG.Resources", got.MIG.Resources, map[string]ResourceCount{
					"nvidia.com/mig-1g.10gb": {Capacity: 14, Allocatable: 12},
					"nvidia.com/mig-3g.40gb": {Capacity: 4, Allocatable: 4},
				})
				// A mixed node advertises no nvidia.com/gpu at all, and it is
				// still a node the device plugin advertises.
				equal(t, "Capacity", got.Capacity, int64(0))
				equal(t, "Mode", got.Mode, ModeDevicePlugin)
			},
		},
		{
			name: "time sliced with four replicas",
			node: testNode("ts-1",
				map[string]string{
					"nvidia.com/gpu.sharing-strategy": "time-slicing",
					"nvidia.com/gpu.replicas":         "4",
				},
				map[string]string{"nvidia.com/gpu": "16"},
				map[string]string{"nvidia.com/gpu": "16"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "Sharing.Strategy", got.Sharing.Strategy, "time-slicing")
				equal(t, "Sharing.Replicas", got.Sharing.Replicas, 4)
			},
		},
		{
			name: "upgrade state failed",
			node: testNode("up-1",
				map[string]string{"nvidia.com/gpu-driver-upgrade-state": "upgrade-failed"},
				map[string]string{"nvidia.com/gpu": "1"},
				map[string]string{"nvidia.com/gpu": "1"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "UpgradeState", got.UpgradeState, "upgrade-failed")
			},
		},
		{
			name: "no GFD labels",
			node: testNode("bare-1", nil,
				map[string]string{"nvidia.com/gpu": "2"},
				map[string]string{"nvidia.com/gpu": "2"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "Product", got.Product, "")
				equal(t, "Count", got.Count, 0)
				equal(t, "DriverVersion", got.DriverVersion, "")
				equal(t, "CUDAVersion", got.CUDAVersion, "")
				equal(t, "Mode", got.Mode, ModeDevicePlugin)
			},
		},
		{
			name: "newer CUDA label names",
			node: testNode("gfd-new-1",
				map[string]string{
					"nvidia.com/cuda.driver-version.full":  "550.107.02",
					"nvidia.com/cuda.runtime-version.full": "12.5",
				},
				map[string]string{"nvidia.com/gpu": "1"},
				map[string]string{"nvidia.com/gpu": "1"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "DriverVersion", got.DriverVersion, "550.107.02")
				equal(t, "CUDAVersion", got.CUDAVersion, "12.5")
			},
		},
		{
			name: "older CUDA label names",
			node: testNode("gfd-old-1",
				map[string]string{
					"nvidia.com/cuda.driver.major":  "535",
					"nvidia.com/cuda.driver.minor":  "129",
					"nvidia.com/cuda.driver.rev":    "03",
					"nvidia.com/cuda.runtime.major": "12",
					"nvidia.com/cuda.runtime.minor": "2",
				},
				map[string]string{"nvidia.com/gpu": "1"},
				map[string]string{"nvidia.com/gpu": "1"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "DriverVersion", got.DriverVersion, "535.129.03")
				equal(t, "CUDAVersion", got.CUDAVersion, "12.2")
			},
		},
		{
			name: "older driver label without a revision",
			node: testNode("gfd-old-2",
				map[string]string{
					"nvidia.com/cuda.driver.major": "455",
					"nvidia.com/cuda.driver.minor": "06",
					"nvidia.com/cuda.driver.rev":   "",
				},
				map[string]string{"nvidia.com/gpu": "1"},
				map[string]string{"nvidia.com/gpu": "1"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "DriverVersion", got.DriverVersion, "455.06")
			},
		},
		{
			name: "the full label wins over the deprecated parts",
			node: testNode("gfd-both-1",
				map[string]string{
					"nvidia.com/cuda.driver-version.full": "550.107.02",
					"nvidia.com/cuda.driver.major":        "535",
					"nvidia.com/cuda.driver.minor":        "129",
					"nvidia.com/cuda.driver.rev":          "03",
				},
				map[string]string{"nvidia.com/gpu": "1"},
				map[string]string{"nvidia.com/gpu": "1"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "DriverVersion", got.DriverVersion, "550.107.02")
			},
		},
		{
			name: "deploy, clique and topology labels",
			node: testNode("topo-1",
				map[string]string{
					"nvidia.com/gpu.deploy.driver":                "true",
					"nvidia.com/gpu.deploy.device-plugin":         "false",
					"nvidia.com/gpu.clique":                       "7b968a6d-c8aa-45e1-9e07-e1e51be99c31.1",
					"fabric.topograph.run/tier-0":                 "switch-a",
					"fabric.topograph.run/tier-1":                 "switch-b",
					"accelerator.topograph.run/domain":            "domain-1",
					"accelerator.topograph.run/sub-domain":        "rack-3",
					"feature.node.kubernetes.io/pci-10de.present": "true",
				},
				map[string]string{"nvidia.com/gpu": "8"},
				map[string]string{"nvidia.com/gpu": "8"}),
			check: func(t *testing.T, got *GPUNode) {
				equal(t, "Deploy", got.Deploy, map[string]string{"driver": "true", "device-plugin": "false"})
				equal(t, "Clique", got.Clique, "7b968a6d-c8aa-45e1-9e07-e1e51be99c31.1")
				equal(t, "Topology", got.Topology, map[string]string{
					"fabric.topograph.run/tier-0":          "switch-a",
					"fabric.topograph.run/tier-1":          "switch-b",
					"accelerator.topograph.run/domain":     "domain-1",
					"accelerator.topograph.run/sub-domain": "rack-3",
				})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := buildNode(tt.node, tt.ctx)
			if got == nil {
				t.Fatal("buildNode returned nil")
			}
			// Every node goes through the constructor, because the cli-runtime
			// printers refuse one whose TypeMeta is empty.
			equal(t, "Kind", got.Kind, "GPUNode")
			equal(t, "Name", got.Name, tt.node.Name)
			tt.check(t, got)
		})
	}
}

// TestBuildNodeKeepsOnlyTheMetadataAReportNeeds pins the trim: a node's
// annotations and resource version are churn in a golden file and noise in a
// report, and copying the metadata wholesale is the easy way to ship both.
func TestBuildNodeKeepsOnlyTheMetadataAReportNeeds(t *testing.T) {
	t.Parallel()

	node := testNode("meta-1", map[string]string{"nvidia.com/gpu.product": "NVIDIA-T4"},
		map[string]string{"nvidia.com/gpu": "1"}, map[string]string{"nvidia.com/gpu": "1"})
	node.Annotations = map[string]string{"kubectl.kubernetes.io/last-applied-configuration": "{}"}
	node.ResourceVersion = "123456"
	node.UID = "11111111-2222-3333-4444-555555555555"

	got := buildNode(node, nodeContext{})

	if len(got.Annotations) != 0 {
		t.Errorf("Annotations = %#v, want none", got.Annotations)
	}

	equal(t, "ResourceVersion", got.ResourceVersion, "")
	equal(t, "UID", string(got.UID), "")
	equal(t, "Labels", got.Labels, map[string]string{"nvidia.com/gpu.product": "NVIDIA-T4"})
	equal(t, "CreationTimestamp", got.CreationTimestamp, node.CreationTimestamp)
}

// TestBuildNodeDetailsOnlyWithDetails pins what separates list from info: a
// list of nodes with every condition on each one is not a list anyone reads.
func TestBuildNodeDetailsOnlyWithDetails(t *testing.T) {
	t.Parallel()

	node := testNode("cond-1", nil,
		map[string]string{"nvidia.com/gpu": "1"}, map[string]string{"nvidia.com/gpu": "1"})
	node.Status.Conditions = append(node.Status.Conditions, corev1.NodeCondition{
		Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse,
	})

	withoutDetails := buildNode(node, nodeContext{sliceNames: []string{"cond-1-pool"}})
	if len(withoutDetails.Conditions) != 0 {
		t.Errorf("Conditions = %#v, want none without --details", withoutDetails.Conditions)
	}

	if len(withoutDetails.ResourceSlices) != 0 {
		t.Errorf("ResourceSlices = %#v, want none without --details", withoutDetails.ResourceSlices)
	}

	withDetails := buildNode(node, nodeContext{withDetails: true, sliceNames: []string{"cond-1-pool"}})
	if len(withDetails.Conditions) != 2 {
		t.Errorf("Conditions = %#v, want both of the node's", withDetails.Conditions)
	}

	equal(t, "ResourceSlices", withDetails.ResourceSlices, []string{"cond-1-pool"})
}

// TestBuildNodeRequested pins that a node whose pods were never listed says so
// rather than reporting zero requested GPUs for a busy node.
func TestBuildNodeRequested(t *testing.T) {
	t.Parallel()

	node := testNode("req-1", nil,
		map[string]string{"nvidia.com/gpu": "8"}, map[string]string{"nvidia.com/gpu": "8"})

	known := buildNode(node, nodeContext{requested: 6, requestedKnown: true})
	if known.Requested != 6 {
		t.Errorf("Requested = %d, want 6", known.Requested)
	}

	if !known.RequestedKnown {
		t.Error("RequestedKnown = false, want true when the pods were listed")
	}

	unknown := buildNode(node, nodeContext{requested: 6})
	if unknown.Requested != 0 {
		t.Errorf("Requested = %d, want 0 when the pods were not listed", unknown.Requested)
	}

	if unknown.RequestedKnown {
		t.Error("RequestedKnown = true, want false when the pods were not listed")
	}
}
