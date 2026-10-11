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

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

func TestNodePCIWithoutGPULabel(t *testing.T) {
	t.Parallel()

	rule := rules.NewNodePCIWithoutGPULabel()

	if rule.Code() != "NV-NODE-001" {
		t.Fatalf("Code is %q, want NV-NODE-001", rule.Code())
	}

	if rule.Severity() != output.SeverityWarning {
		t.Errorf("Severity is %q, want warning", rule.Severity())
	}

	t.Run("a hinted node with the PCI label and no GPU label", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{"feature.node.kubernetes.io/pci-10de.present": "true"}),
			hinted(),
		)

		f := only(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
		checkCommonFields(t, rule, f, rules.ComponentNode)

		if f.Node != "worker-1" {
			t.Errorf("Node is %q, want worker-1", f.Node)
		}

		checkEvidence(t, f.Evidence, map[string]string{
			"feature.node.kubernetes.io/pci-10de.present": "true",
		})
	})

	t.Run("the operands it did find are the proof", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withLabels(map[string]string{"feature.node.kubernetes.io/pci-10de.present": "true"}),
			withOperand(inventory.ComponentDevicePlugin, true),
			withOperand(inventory.ComponentOperatorValidator, true),
		)

		f := only(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))

		checkEvidence(t, f.Evidence, map[string]string{
			"feature.node.kubernetes.io/pci-10de.present": "true",
			// Sorted and comma joined, as the evidence encodings say.
			"operands": inventory.ComponentDevicePlugin + "," + inventory.ComponentOperatorValidator,
		})
	})

	t.Run("a node with both labels is fine", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withLabels(map[string]string{
			"feature.node.kubernetes.io/pci-10de.present": "true",
			"nvidia.com/gpu.present":                      "true",
		}))

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	t.Run("a node with no PCI label at all is not this rule's business", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1")

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	// The label is only a claim when it says "true". Node feature discovery
	// writes that value and nothing else, so anything else is not a device.
	t.Run("the PCI label only counts at true", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withLabels(map[string]string{
			"feature.node.kubernetes.io/pci-10de.present": "false",
		}))

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	t.Run("the Mokka control plane is left alone", func(t *testing.T) {
		t.Parallel()

		// It carries the PCI label and, on the lane, the GPU label too. Strip
		// the GPU label so that only the operator-targets gate can keep this
		// rule quiet: a test whose node fails two conditions proves neither.
		node := mokkaControlPlane()
		delete(node.Labels, "nvidia.com/gpu.present")

		wantNone(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
	})

	t.Run("a declared range excludes an out-of-range version", func(t *testing.T) {
		t.Parallel()

		if !rule.Applies("v25.10.0") {
			t.Error("the rule does not apply at v25.10.0, want it to")
		}

		if !rule.Applies("") {
			t.Error("an unknown version must mean the rule applies")
		}

		bounded := rules.NewNodePCIWithoutGPULabel()
		bounded.MinVersion = "v99.0.0"

		if bounded.Applies("v25.10.0") {
			t.Error("a rule bounded at v99.0.0 applies at v25.10.0, want it skipped")
		}
	})
}
