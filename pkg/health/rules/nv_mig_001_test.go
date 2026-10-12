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

// migNode is a worker with whole-GPU capacity, which is what the single and
// none strategies both advertise.
func migNode(name string, opts ...nodeOption) inventory.GPUNode {
	base := []nodeOption{
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withEveryOperandDeployed(),
		withCount(8),
		withGPUs(8, 8),
		withOperand(inventory.ComponentDevicePlugin, true),
		withOperand(inventory.ComponentMIGManager, true),
	}

	return buildNode(name, append(base, opts...)...)
}

func migInput(node inventory.GPUNode) health.Input {
	return health.Input{Nodes: []inventory.GPUNode{node}, OperatorInstalled: true}
}

// failed is the mig-manager saying it could not apply the profile, which is
// the one config state the node is not on its way out of.
func TestMIGConfigNotAppliedReportsAFailedConfig(t *testing.T) {
	t.Parallel()

	rule := rules.NewMIGConfigNotApplied()
	node := migNode("worker-1", withMIG("mixed", "all-1g.10gb", "failed", true))

	finding := only(t, evaluate(rule, migInput(node)))

	checkFinding(t, rule, finding, inventory.ComponentMIGManager,
		"the MIG configuration the node was asked for has not been applied",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityError {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityError)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/mig.config":       "all-1g.10gb",
		"nvidia.com/mig.config.state": "failed",
		"nvidia.com/mig.strategy":     "mixed",
		"nvidia.com/mig.capable":      "true",
	})
}

// pending and rebooting are states a node passes through, so they are a
// warning rather than an error: the reader is being told to look, not that
// the node is lost.
func TestMIGConfigNotAppliedWarnsOnTheTransitionalStates(t *testing.T) {
	t.Parallel()

	for _, state := range []string{"pending", "rebooting", "something-new"} {
		t.Run("state="+state, func(t *testing.T) {
			t.Parallel()

			node := migNode("worker-1", withMIG("single", "all-1g.5gb", state, true))
			finding := only(t, evaluate(rules.NewMIGConfigNotApplied(), migInput(node)))

			if finding.Severity != output.SeverityWarning {
				t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityWarning)
			}

			if got := finding.Evidence["nvidia.com/mig.config.state"]; got != state {
				t.Errorf("evidence state is %q, want %q", got, state)
			}
		})
	}
}

// A config with no state at all is the mig-manager never having picked the
// label up, which is the same absence of a working MIG configuration.
func TestMIGConfigNotAppliedReportsAConfigWithNoState(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1", withMIG("single", "all-1g.5gb", "", true))
	finding := only(t, evaluate(rules.NewMIGConfigNotApplied(), migInput(node)))

	if finding.Severity != output.SeverityWarning {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityWarning)
	}

	if _, present := finding.Evidence["nvidia.com/mig.config.state"]; present {
		t.Errorf("an absent state is left out of the evidence, not written as an empty string: %v", finding.Evidence)
	}
}

// The single strategy publishes every partition as nvidia.com/gpu, so a
// healthy single node advertises whole GPUs and no nvidia.com/mig-* at all.
// E4 runs one of these for real.
func TestMIGConfigNotAppliedStaysQuietOnAHealthySingleNode(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1", withMIG("single", "all-1g.5gb", "success", true))

	if findings := evaluate(rules.NewMIGConfigNotApplied(), migInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The mixed strategy publishes one resource per profile. A healthy
// partitioned node must give no finding: E4 runs a real one on the MIG lane.
func TestMIGConfigNotAppliedStaysQuietOnAHealthyMixedNode(t *testing.T) {
	t.Parallel()

	node := buildNode("worker-1",
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withEveryOperandDeployed(),
		withCount(8),
		withOperand(inventory.ComponentDevicePlugin, true),
		withOperand(inventory.ComponentMIGManager, true),
		withMIG("mixed", "all-1g.10gb", "success", true),
		withMIGResources(map[string]inventory.ResourceCount{
			"nvidia.com/mig-1g.10gb": {Capacity: 56, Allocatable: 56},
		}),
	)

	if findings := evaluate(rules.NewMIGConfigNotApplied(), migInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The Mokka main lane: nvidia.com/mig.capable=true on gb300 with strategy
// none. Nothing was asked for and nothing is wrong.
func TestMIGConfigNotAppliedStaysQuietOnACapableNodeWithStrategyNone(t *testing.T) {
	t.Parallel()

	node := migNode("kubectl-nv-worker", withMIG("none", "", "", true))

	if findings := evaluate(rules.NewMIGConfigNotApplied(), migInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A single-strategy node advertising nvidia.com/mig-* resources is a node
// whose strategy cannot be serving what it advertises: single never publishes
// those names.
func TestMIGConfigNotAppliedReportsSingleAdvertisingMIGResources(t *testing.T) {
	t.Parallel()

	rule := rules.NewMIGConfigNotApplied()
	node := migNode("worker-1",
		withMIG("single", "all-1g.5gb", "success", true),
		withMIGResources(map[string]inventory.ResourceCount{
			"nvidia.com/mig-1g.5gb":  {Capacity: 56, Allocatable: 56},
			"nvidia.com/mig-2g.10gb": {Capacity: 8, Allocatable: 8},
		}),
	)

	finding := only(t, evaluate(rule, migInput(node)))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"the node's MIG strategy and the resources it advertises disagree",
		"kubectl nv info worker-1")

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/mig.strategy":     "single",
		"nvidia.com/mig.config":       "all-1g.5gb",
		"nvidia.com/mig.config.state": "success",
		"nvidia.com/mig.capable":      "true",
		"migResources":                "nvidia.com/mig-1g.5gb,nvidia.com/mig-2g.10gb",
		"capacity":                    "8",
	})

	if finding.Severity != output.SeverityWarning {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityWarning)
	}
}

// A mixed-strategy node that applied a partitioning config and advertises no
// nvidia.com/mig-* resource is advertising nothing a MIG workload can ask
// for.
func TestMIGConfigNotAppliedReportsMixedWithoutMIGResources(t *testing.T) {
	t.Parallel()

	rule := rules.NewMIGConfigNotApplied()
	node := migNode("worker-1", withMIG("mixed", "all-1g.10gb", "success", true))

	finding := only(t, evaluate(rule, migInput(node)))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"the node's MIG strategy and the resources it advertises disagree",
		"kubectl nv info worker-1")

	if _, present := finding.Evidence["migResources"]; present {
		t.Errorf("there are no MIG resources, so the key is left out: %v", finding.Evidence)
	}
}

// all-disabled is the config that asks for no partitions, so a mixed node
// under it advertising no nvidia.com/mig-* is doing exactly what it was told.
func TestMIGConfigNotAppliedStaysQuietOnMixedWithPartitioningDisabled(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1", withMIG("mixed", "all-disabled", "success", true))

	if findings := evaluate(rules.NewMIGConfigNotApplied(), migInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A mixed node nobody has configured has nothing to disagree with.
func TestMIGConfigNotAppliedStaysQuietOnMixedWithNoConfig(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1", withMIG("mixed", "", "", true))

	if findings := evaluate(rules.NewMIGConfigNotApplied(), migInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// Hardware that cannot do MIG cannot be misconfigured for it.
func TestMIGConfigNotAppliedStaysQuietOnANodeThatIsNotMIGCapable(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1", withMIG("mixed", "all-1g.10gb", "success", false))

	if findings := evaluate(rules.NewMIGConfigNotApplied(), migInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A node the mig-manager is reconfiguring advertises its profiles at zero,
// which is not membership, so it is hinted exactly while its configuration is
// the thing that is wrong.
func TestMIGConfigNotAppliedReportsAHintedWorker(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1", withMIG("mixed", "all-1g.10gb", "failed", true))
	node.Capacity, node.Allocatable, node.Mode = 0, 0, inventory.ModeNone
	hinted()(&node)

	in := health.Input{Hinted: []inventory.GPUNode{node}, OperatorInstalled: true}

	finding := only(t, evaluate(rules.NewMIGConfigNotApplied(), in))
	if finding.Node != "worker-1" || finding.Severity != output.SeverityError {
		t.Errorf("finding is %q at %q, want worker-1 at error", finding.Node, finding.Severity)
	}
}

// The Mokka control plane carries no MIG labels, so walking the hinted nodes
// does not put it back into a report.
func TestMIGConfigNotAppliedStaysQuietOnTheControlPlane(t *testing.T) {
	t.Parallel()

	in := health.Input{Hinted: []inventory.GPUNode{mokkaControlPlane()}, OperatorInstalled: true}

	if findings := evaluate(rules.NewMIGConfigNotApplied(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The kubelet does not drop a removed extended resource, it keeps it at zero.
// A node partitioned last week and running whole GPUs today still carries its
// old nvidia.com/mig-1g.10gb entry, and counting that leftover reports a
// working single-strategy node as advertising profiles its strategy does not
// serve.
func TestMIGConfigNotAppliedIgnoresALeftoverZeroMIGResource(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1",
		withMIG("single", "all-1g.5gb", "success", true),
		withMIGResources(map[string]inventory.ResourceCount{
			"nvidia.com/mig-1g.10gb": {Capacity: 0, Allocatable: 0},
		}),
	)

	if findings := evaluate(rules.NewMIGConfigNotApplied(), migInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The mirror of the same rule: a mixed node whose only MIG entries are the
// zeroed leftovers advertises nothing a MIG workload can ask for, so it is
// the finding rather than a node that is quietly fine.
func TestMIGConfigNotAppliedReportsMixedWithOnlyZeroMIGResources(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1",
		withMIG("mixed", "all-1g.10gb", "success", true),
		withMIGResources(map[string]inventory.ResourceCount{
			"nvidia.com/mig-1g.10gb": {Capacity: 0, Allocatable: 0},
		}),
	)

	finding := only(t, evaluate(rules.NewMIGConfigNotApplied(), migInput(node)))

	if got := finding.Message; got != "the node's MIG strategy and the resources it advertises disagree" {
		t.Errorf("Message is %q", got)
	}

	// The zeroed leftover is not a resource the node advertises, so it is not
	// in the evidence either. Listing it would contradict the finding in its
	// own proof: the message says the node advertises no MIG profile and the
	// evidence would name one.
	if _, present := finding.Evidence["migResources"]; present {
		t.Errorf("a zero-capacity profile is not advertised, so it is left out: %v", finding.Evidence)
	}
}

// The same filter on the other branch: a single-strategy node is reported for
// the profiles it really advertises, and a zeroed leftover beside them is not
// one of them.
func TestMIGConfigNotAppliedNamesOnlyTheAdvertisedProfiles(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1",
		withMIG("single", "all-1g.5gb", "success", true),
		withMIGResources(map[string]inventory.ResourceCount{
			"nvidia.com/mig-1g.5gb":  {Capacity: 56, Allocatable: 56},
			"nvidia.com/mig-2g.10gb": {Capacity: 0, Allocatable: 0},
		}),
	)

	finding := only(t, evaluate(rules.NewMIGConfigNotApplied(), migInput(node)))

	if got := finding.Evidence["migResources"]; got != "nvidia.com/mig-1g.5gb" {
		t.Errorf("evidence migResources is %q, want the advertised profile alone", got)
	}
}

// A node the operator was told to leave alone is not one to report a MIG
// configuration on: every deploy label false is the cluster saying so.
func TestMIGConfigNotAppliedStaysQuietWhenEveryOperandIsOff(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1", withMIG("mixed", "all-1g.10gb", "failed", true))
	for key := range node.Deploy {
		node.Deploy[key] = "false"
	}

	if findings := evaluate(rules.NewMIGConfigNotApplied(), migInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A node whose GPUs are handed to virtual machines advertises nothing to the
// container scheduler by design, and MIG is not what is being done to it.
func TestMIGConfigNotAppliedStaysQuietOnAVirtualMachineNode(t *testing.T) {
	t.Parallel()

	node := migNode("worker-1",
		withMIG("mixed", "all-1g.10gb", "failed", true),
		withWorkloadConfig("vm-passthrough"))

	if findings := evaluate(rules.NewMIGConfigNotApplied(), migInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

func TestMIGConfigNotAppliedAppliesToEveryOperatorVersion(t *testing.T) {
	t.Parallel()

	rule := rules.NewMIGConfigNotApplied()
	if !rule.Applies("v25.10.0") || !rule.Applies("") {
		t.Error("the rule has to apply to every operator version and to an unknown one")
	}

	bounded := rules.NewMIGConfigNotApplied()
	bounded.MinVersion = "v99.0.0"

	if bounded.Applies("v25.10.0") {
		t.Error("the version gate is not wired: a bounded rule still applies below its minimum")
	}
}
