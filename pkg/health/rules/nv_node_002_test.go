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

package rules_test

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

func TestNodeNoAllocatableGPUs(t *testing.T) {
	t.Parallel()

	rule := rules.NewNodeNoAllocatableGPUs()

	if rule.Code() != "NV-NODE-002" {
		t.Fatalf("Code is %q, want NV-NODE-002", rule.Code())
	}

	if rule.Severity() != output.SeverityError {
		t.Errorf("Severity is %q, want error: a node nothing can be scheduled on is broken", rule.Severity())
	}

	t.Run("a hinted node that advertises nothing", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
			hinted(),
		)

		f := only(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))

		checkFinding(t, rule, f, rules.ComponentNode,
			"the node reports GPUs present and advertises none the scheduler can place",
			"kubectl nv info worker-1",
		)

		if f.Node != "worker-1" {
			t.Errorf("Node is %q, want worker-1", f.Node)
		}

		checkEvidence(t, f.Evidence, map[string]string{
			rules.LabelGPUPresent: "true",
			"capacity":            "0",
			"allocatable":         "0",
			"hinted":              "true",
		})
	})

	// Capacity without allocatable is the device plugin having advertised
	// devices the kubelet will not place anything on. It is the same finding
	// and a different story, so the evidence has to tell them apart.
	t.Run("a GPU node with capacity and nothing allocatable", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
			withGPUs(4, 0),
			withOperand(inventory.ComponentDevicePlugin, false),
		)

		f := only(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))

		checkEvidence(t, f.Evidence, map[string]string{
			rules.LabelGPUPresent: "true",
			"capacity":            "4",
			"allocatable":         "0",
			"hinted":              "false",
			"operands":            inventory.ComponentDevicePlugin,
		})
	})

	t.Run("a healthy GPU node", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
			withGPUs(4, 4),
		)

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	// A node short of some of its GPUs still places work on the rest, which is
	// NV-NODE-003's warning rather than this rule's error. The two partition
	// the space on allocatable being zero, and a rule that fired here as well
	// would report the same node twice at two severities.
	t.Run("a node short of some GPUs belongs to NV-NODE-003", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
			withGPUs(8, 6),
		)

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	// The hint label is what makes this a claim about a node that should have
	// GPUs. Without it the node is just a node.
	t.Run("a node with no gpu.present label", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", hinted())

		wantNone(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
	})

	t.Run("the gpu.present label only counts at true", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "false"}),
			hinted(),
		)

		wantNone(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
	})

	// This is the case NOTES.md records and the reason the gate exists. A
	// healthy Mokka lane has to produce zero findings at warning and above,
	// and this node would otherwise produce an error on every run forever.
	t.Run("the Mokka control plane is left alone", func(t *testing.T) {
		t.Parallel()

		wantNone(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{mokkaControlPlane()}}))
	})

	// A GPU node behind a taint the operands do not tolerate has had them
	// evicted: the hint labels are stale and nothing is coming back. That is
	// the same exclusion the control plane gets, on a node that does have
	// hardware.
	t.Run("a GPU node tainted nvidia.com/gpu NoSchedule with no operands", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
			withTaints(corev1.Taint{Key: "example.com/dedicated", Effect: corev1.TaintEffectNoSchedule}),
			hinted(),
		)

		wantNone(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
	})

	// The same node with the taint the operands do tolerate is still a node
	// the operator targets, so the finding stands. Without this case the one
	// above would pass against a gate that excluded every tainted node.
	t.Run("a GPU node tainted nvidia.com/gpu NoSchedule is still evaluated", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
			withTaints(corev1.Taint{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule}),
			hinted(),
		)

		only(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
	})

	t.Run("a node whose operands were all switched off", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
			withDeploy(map[string]string{"driver": "false", "device-plugin": "false"}),
			hinted(),
		)

		wantNone(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
	})

	// A node that advertises its GPUs through dynamic resource allocation
	// publishes a ResourceSlice and no extended resource, so its allocatable
	// nvidia.com/gpu is legitimately zero. This rule covers the DRA node that
	// publishes no slice, and pkg/source classifies such a node as hinted with
	// mode none, which the case below keeps firing on.
	t.Run("mode decides whether a zero allocatable is a fault", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			mode      string
			wantCount int
		}{
			{mode: inventory.ModeDRA, wantCount: 0},
			{mode: inventory.ModeBoth, wantCount: 1},
			{mode: inventory.ModeNone, wantCount: 1},
		} {
			t.Run(tc.mode, func(t *testing.T) {
				t.Parallel()

				node := buildNode("worker-1",
					withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
					withMode(tc.mode),
					hinted(),
				)

				got := evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}})
				if len(got) != tc.wantCount {
					t.Errorf("mode %q produced %d findings, want %d: %+v", tc.mode, len(got), tc.wantCount, got)
				}
			})
		}
	})

	// A node set up to pass its GPUs through to virtual machines advertises
	// nothing to the container scheduler by design. Reporting it would make
	// the check useless on every cluster running the operator's sandbox
	// workloads.
	t.Run("a node configured for virtual machine workloads", func(t *testing.T) {
		t.Parallel()

		for _, workload := range []string{"vm-passthrough", "vm-vgpu"} {
			t.Run(workload, func(t *testing.T) {
				t.Parallel()

				node := buildNode("worker-1",
					withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
					withWorkloadConfig(workload),
					hinted(),
				)

				wantNone(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
			})
		}
	})

	// The container workload config is the default and must not silence the
	// rule, or the exclusion above would be excluding every node.
	t.Run("a node configured for container workloads is evaluated", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
			withWorkloadConfig("container"),
			hinted(),
		)

		only(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
	})

	// A mixed-strategy MIG node advertises one extended resource per profile
	// and no nvidia.com/gpu at all, so a zero whole-GPU allocatable is what a
	// healthy one looks like. Firing here would report every MIG node in a
	// cluster as broken.
	t.Run("a healthy mixed-strategy MIG node", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
			withMixedMIG(map[string]inventory.ResourceCount{
				"nvidia.com/mig-1g.10gb": {Capacity: 7, Allocatable: 7},
			}),
		)

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	// A MIG node whose profiles are all unplaceable is broken in exactly the
	// way this rule is about, so the exclusion above has to be about having
	// allocatable slices rather than about being a MIG node.
	t.Run("a mixed-strategy MIG node with nothing allocatable", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
			withMixedMIG(map[string]inventory.ResourceCount{
				"nvidia.com/mig-1g.10gb": {Capacity: 7, Allocatable: 0},
			}),
		)

		f := only(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))

		if f.Evidence["mig.allocatable"] != "0" {
			t.Errorf("evidence[mig.allocatable] = %q, want 0: %v", f.Evidence["mig.allocatable"], f.Evidence)
		}
	})

	t.Run("a declared range excludes an out-of-range version", func(t *testing.T) {
		t.Parallel()

		if !rule.Applies("v25.10.0") || !rule.Applies("") {
			t.Error("the rule must apply at a known in-range version and at an unknown one")
		}

		bounded := rules.NewNodeNoAllocatableGPUs()
		bounded.MinVersion = "v99.0.0"

		if bounded.Applies("v25.10.0") {
			t.Error("a rule bounded at v99.0.0 applies at v25.10.0, want it skipped")
		}
	})
}
