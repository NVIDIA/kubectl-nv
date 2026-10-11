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

// draModeNode is a worker advertising its GPUs the way the case is about.
func draModeNode(name, mode string, opts ...nodeOption) inventory.GPUNode {
	base := []nodeOption{
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withEveryOperandDeployed(),
		withCount(4),
		withGPUs(4, 4),
		withOperand(inventory.ComponentDevicePlugin, true),
		withMode(mode),
	}

	return buildNode(name, append(base, opts...)...)
}

// Both advertisements on one node is a cluster mid-migration: the device
// plugin and the DRA driver are each handing the scheduler the same physical
// GPUs, so the node can be over-committed twice over.
func TestDevicePluginAndDRABothReportsADoubleAdvertisement(t *testing.T) {
	t.Parallel()

	rule := rules.NewDevicePluginAndDRABoth()
	node := draModeNode("worker-1", inventory.ModeBoth,
		withOperand(inventory.ComponentDRADriver, true),
		// Reversed on purpose: the evidence encoding sorts, and a fixture
		// already in order would pass against a rule that did not.
		withResourceSlices("worker-1-imex.nvidia.com", "worker-1-gpu.nvidia.com"))

	in := health.Input{Nodes: []inventory.GPUNode{node}, OperatorInstalled: true}
	finding := only(t, evaluate(rule, in))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"the node advertises its GPUs through both the device plugin and dynamic resource allocation",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityInfo {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityInfo)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"mode":           "both",
		"capacity":       "4",
		"allocatable":    "4",
		"resourceSlices": "worker-1-gpu.nvidia.com,worker-1-imex.nvidia.com",
	})
}

// Each of the single-mode nodes is a node doing exactly one thing, which is
// the design rather than a fault.
func TestDevicePluginAndDRABothStaysQuietOnASingleMode(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{inventory.ModeDevicePlugin, inventory.ModeDRA, inventory.ModeNone} {
		t.Run("mode="+mode, func(t *testing.T) {
			t.Parallel()

			in := health.Input{
				Nodes:             []inventory.GPUNode{draModeNode("worker-1", mode)},
				OperatorInstalled: true,
			}

			if findings := evaluate(rules.NewDevicePluginAndDRABoth(), in); len(findings) != 0 {
				t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
			}
		})
	}
}

func TestDevicePluginAndDRABothSkipsHintedNodes(t *testing.T) {
	t.Parallel()

	node := mokkaControlPlane()
	withMode(inventory.ModeBoth)(&node)

	in := health.Input{Hinted: []inventory.GPUNode{node}, OperatorInstalled: true}

	if findings := evaluate(rules.NewDevicePluginAndDRABoth(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// There is no GPU Operator floor. The NVIDIA DRA driver installs from a chart
// of its own against any operator version, or none at all, so a node can be in
// both modes whatever the operator is; the operator did not ship a DRA driver
// operand of its own until v26.7.0. A floor read off the operator would
// silence the rule on exactly the clusters that installed the driver
// separately.
func TestDevicePluginAndDRABothAppliesToEveryOperatorVersion(t *testing.T) {
	t.Parallel()

	rule := rules.NewDevicePluginAndDRABoth()

	for _, version := range []string{"v24.9.0", "v25.3.0", "v26.3.3", "v26.10.0", ""} {
		if !rule.Applies(version) {
			t.Errorf("the rule does not apply at operator version %q, and it has to", version)
		}
	}

	bounded := rules.NewDevicePluginAndDRABoth()
	bounded.MinVersion = "v99.0.0"

	if bounded.Applies("v25.10.0") {
		t.Error("the version gate is not wired: a bounded rule still applies below its minimum")
	}
}
