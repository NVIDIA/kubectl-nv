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

func topoInput(nodes ...inventory.GPUNode) health.Input {
	return health.Input{Nodes: nodes, OperatorInstalled: true}
}

// The shape E3 injects: one worker gets a tier and the other does not. The
// worker without it is the finding, and it carries none of the tiers rather
// than some of them, which is the common way Topograph misses a node.
func TestTopologyLabelsPartialReportsANodeWithNoTierAtAll(t *testing.T) {
	t.Parallel()

	rule := rules.NewTopologyLabelsPartial()
	in := topoInput(
		fabricNode("worker-1", map[string]string{"fabric.topograph.run/tier-0": "sw-a"}),
		fabricNode("worker-2", nil),
	)

	finding := only(t, evaluate(rule, in))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"the node is missing Topograph fabric tiers that other GPU nodes carry",
		"kubectl nv info worker-2")

	if finding.Severity != output.SeverityInfo {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityInfo)
	}

	// present is absent rather than an empty string: the node carries none of
	// them, and an absent key is the encoding for that.
	checkEvidence(t, finding.Evidence, map[string]string{
		"missing": "fabric.topograph.run/tier-0",
	})
}

// A node missing one tier of several is the other half of the same finding.
func TestTopologyLabelsPartialReportsANodeMissingOneTier(t *testing.T) {
	t.Parallel()

	rule := rules.NewTopologyLabelsPartial()
	in := topoInput(
		fabricNode("worker-1", map[string]string{
			"fabric.topograph.run/tier-0": "sw-a",
			"fabric.topograph.run/tier-1": "sw-top",
		}),
		fabricNode("worker-2", map[string]string{"fabric.topograph.run/tier-0": "sw-b"}),
	)

	finding := only(t, evaluate(rule, in))

	checkEvidence(t, finding.Evidence, map[string]string{
		"present": "fabric.topograph.run/tier-0",
		"missing": "fabric.topograph.run/tier-1",
	})

	if finding.Node != "worker-2" {
		t.Errorf("the finding names %q, want worker-2", finding.Node)
	}
}

// A fleet where every node carries the same tiers is a fleet Topograph has
// finished with.
func TestTopologyLabelsPartialStaysQuietWhenEveryNodeAgrees(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		"fabric.topograph.run/tier-0": "sw-a",
		"fabric.topograph.run/tier-1": "sw-top",
	}

	in := topoInput(fabricNode("worker-1", labels), fabricNode("worker-2", labels))

	if findings := evaluate(rules.NewTopologyLabelsPartial(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A cluster Topograph has never run against carries no tiers at all, so there
// is no set to be partial against.
func TestTopologyLabelsPartialStaysQuietWithoutAnyTier(t *testing.T) {
	t.Parallel()

	if findings := evaluate(rules.NewTopologyLabelsPartial(), healthyLane()); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The accelerator domain is optional per provider: a fleet whose provider
// publishes tiers and no domain is complete, not partial, and counting the
// domain would report every such cluster. Only the fabric tiers decide.
func TestTopologyLabelsPartialCountsTiersOnly(t *testing.T) {
	t.Parallel()

	in := topoInput(
		fabricNode("worker-1", map[string]string{
			"fabric.topograph.run/tier-0":      "sw-a",
			"accelerator.topograph.run/domain": "nvl-1",
		}),
		fabricNode("worker-2", map[string]string{"fabric.topograph.run/tier-0": "sw-b"}),
	)

	if findings := evaluate(rules.NewTopologyLabelsPartial(), in); len(findings) != 0 {
		t.Fatalf("the domain is not a tier: got %d findings, want none: %+v", len(findings), findings)
	}
}

// A clique is not a Topograph label. A fleet of GB200 nodes with cliques
// beside H100 nodes without them is an ordinary mixed cluster, and counting
// the clique would report every such cluster as broken.
func TestTopologyLabelsPartialIgnoresTheCliqueLabel(t *testing.T) {
	t.Parallel()

	second := fabricNode("worker-2", map[string]string{"fabric.topograph.run/tier-0": "sw-b"})
	withClique("uuid.1")(&second)

	in := topoInput(fabricNode("worker-1", map[string]string{"fabric.topograph.run/tier-0": "sw-a"}), second)

	if findings := evaluate(rules.NewTopologyLabelsPartial(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// One finding per node missing tiers, each naming its own node, in node
// order so that two runs over the same cluster produce the same bytes.
func TestTopologyLabelsPartialReportsEveryPartialNode(t *testing.T) {
	t.Parallel()

	in := topoInput(
		fabricNode("worker-1", map[string]string{
			"fabric.topograph.run/tier-0": "sw-a",
			"fabric.topograph.run/tier-1": "sw-top",
		}),
		fabricNode("worker-2", map[string]string{"fabric.topograph.run/tier-0": "sw-b"}),
		fabricNode("worker-3", nil),
	)

	findings := evaluate(rules.NewTopologyLabelsPartial(), in)
	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}

	if findings[0].Node != "worker-2" || findings[1].Node != "worker-3" {
		t.Errorf("findings name %q and %q, want worker-2 and worker-3", findings[0].Node, findings[1].Node)
	}

	if got := findings[1].Evidence["missing"]; got != "fabric.topograph.run/tier-0,fabric.topograph.run/tier-1" {
		t.Errorf("worker-3 is missing both tiers, sorted and comma joined; got %q", got)
	}
}

// A label set to an empty string is not a tier the node carries. The node
// would otherwise count as labelled by a value that says nothing.
func TestTopologyLabelsPartialIgnoresAnEmptyTierValue(t *testing.T) {
	t.Parallel()

	in := topoInput(
		fabricNode("worker-1", map[string]string{"fabric.topograph.run/tier-0": "sw-a"}),
		fabricNode("worker-2", map[string]string{"fabric.topograph.run/tier-0": ""}),
	)

	finding := only(t, evaluate(rules.NewTopologyLabelsPartial(), in))
	if finding.Node != "worker-2" {
		t.Errorf("the finding names %q, want worker-2", finding.Node)
	}
}

// Only the GPU nodes are walked. The question is which nodes the fabric
// describes, and a node that advertises no GPUs is not one work is placed on.
func TestTopologyLabelsPartialSkipsHintedNodes(t *testing.T) {
	t.Parallel()

	in := topoInput(fabricNode("worker-1", map[string]string{"fabric.topograph.run/tier-0": "sw-a"}))
	in.Hinted = []inventory.GPUNode{mokkaControlPlane(), fabricNode("worker-9", nil)}

	if findings := evaluate(rules.NewTopologyLabelsPartial(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
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
