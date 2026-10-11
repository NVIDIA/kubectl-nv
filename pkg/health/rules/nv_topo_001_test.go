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

// fabricNode is a worker carrying the Topograph labels the case gives it.
func fabricNode(name string, labels map[string]string) inventory.GPUNode {
	return buildNode(name,
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withEveryOperandDeployed(),
		withCount(4),
		withGPUs(4, 4),
		withOperand(inventory.ComponentDevicePlugin, true),
		withTopology(labels),
	)
}

// A node missing a tier its peers carry is a node work will be placed on and
// then run slowly on, and nothing else in a report shows it.
func TestTopologyLabelsPartialReportsANodeMissingATier(t *testing.T) {
	t.Parallel()

	rule := rules.NewTopologyLabelsPartial()
	in := health.Input{
		Nodes: []inventory.GPUNode{
			fabricNode("worker-1", map[string]string{
				"fabric.topograph.run/tier-0": "sw-a",
				"fabric.topograph.run/tier-1": "sw-top",
			}),
			fabricNode("worker-2", map[string]string{
				"fabric.topograph.run/tier-0": "sw-b",
			}),
		},
		OperatorInstalled: true,
	}

	finding := only(t, evaluate(rule, in))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"the node is missing Topograph labels that other GPU nodes carry",
		"kubectl nv info worker-2")

	if finding.Severity != output.SeverityWarning {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityWarning)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"present": "fabric.topograph.run/tier-0",
		"missing": "fabric.topograph.run/tier-1",
	})
}

// A fleet where every node carries the same labels is a fleet Topograph has
// finished with.
func TestTopologyLabelsPartialStaysQuietWhenEveryNodeAgrees(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		"fabric.topograph.run/tier-0":      "sw-a",
		"accelerator.topograph.run/domain": "nvl-1",
	}

	in := health.Input{
		Nodes: []inventory.GPUNode{
			fabricNode("worker-1", labels),
			fabricNode("worker-2", labels),
		},
		OperatorInstalled: true,
	}

	if findings := evaluate(rules.NewTopologyLabelsPartial(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A cluster Topograph has never run against carries none of the labels, so
// there is no set to be partial against.
func TestTopologyLabelsPartialStaysQuietWithoutAnyTopographLabel(t *testing.T) {
	t.Parallel()

	if findings := evaluate(rules.NewTopologyLabelsPartial(), healthyLane()); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A node carrying none of the labels is outside the fabric entirely rather
// than half inside it, and saying it is missing every label would bury the
// nodes that are missing one. That is pkg/topology's definition, and this
// rule reads the same partial set rather than inventing a second one.
func TestTopologyLabelsPartialStaysQuietOnANodeOutsideTheFabric(t *testing.T) {
	t.Parallel()

	in := health.Input{
		Nodes: []inventory.GPUNode{
			fabricNode("worker-1", map[string]string{"fabric.topograph.run/tier-0": "sw-a"}),
			fabricNode("worker-2", nil),
		},
		OperatorInstalled: true,
	}

	if findings := evaluate(rules.NewTopologyLabelsPartial(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A clique is not a Topograph label. A fleet of GB200 nodes with cliques
// beside H100 nodes without them is an ordinary mixed cluster, and counting
// the clique would report every such cluster as broken.
func TestTopologyLabelsPartialIgnoresTheCliqueLabel(t *testing.T) {
	t.Parallel()

	in := health.Input{
		Nodes: []inventory.GPUNode{
			fabricNode("worker-1", map[string]string{"fabric.topograph.run/tier-0": "sw-a"}),
			func() inventory.GPUNode {
				node := fabricNode("worker-2", map[string]string{"fabric.topograph.run/tier-0": "sw-b"})
				withClique("uuid.1")(&node)

				return node
			}(),
		},
		OperatorInstalled: true,
	}

	if findings := evaluate(rules.NewTopologyLabelsPartial(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// One finding per node missing labels, each naming its own node.
func TestTopologyLabelsPartialReportsEveryPartialNode(t *testing.T) {
	t.Parallel()

	in := health.Input{
		Nodes: []inventory.GPUNode{
			fabricNode("worker-1", map[string]string{
				"fabric.topograph.run/tier-0":      "sw-a",
				"accelerator.topograph.run/domain": "nvl-1",
			}),
			fabricNode("worker-2", map[string]string{"fabric.topograph.run/tier-0": "sw-b"}),
			fabricNode("worker-3", map[string]string{"accelerator.topograph.run/domain": "nvl-2"}),
		},
		OperatorInstalled: true,
	}

	findings := evaluate(rules.NewTopologyLabelsPartial(), in)
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}

	if findings[0].Node != "worker-2" || findings[1].Node != "worker-3" {
		t.Errorf("findings name %q and %q, want worker-2 and worker-3", findings[0].Node, findings[1].Node)
	}
}

func TestTopologyLabelsPartialAppliesToEveryOperatorVersion(t *testing.T) {
	t.Parallel()

	rule := rules.NewTopologyLabelsPartial()
	if !rule.Applies("v25.10.0") || !rule.Applies("") {
		t.Error("the rule has to apply to every operator version and to an unknown one")
	}

	bounded := rules.NewTopologyLabelsPartial()
	bounded.MinVersion = "v99.0.0"

	if bounded.Applies("v25.10.0") {
		t.Error("the version gate is not wired: a bounded rule still applies below its minimum")
	}
}
