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
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// upgNow is the injected clock every case in this file is written against. A
// rule that called time.Now would produce different findings on two runs over
// the same input.
var upgNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// upgradingNode is a worker carrying one upgrade state label.
func upgradingNode(name, state string) inventory.GPUNode {
	return buildNode(name,
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withEveryOperandDeployed(),
		withCount(4),
		withGPUs(4, 4),
		withOperand(inventory.ComponentDevicePlugin, true),
		withUpgradeState(state),
	)
}

// upgradeInput is one node, a clock and whatever events the case gives it.
func upgradeInput(node inventory.GPUNode, events ...corev1.Event) health.Input {
	return health.Input{
		Nodes:             []inventory.GPUNode{node},
		Events:            events,
		OperatorInstalled: true,
		Now:               upgNow,
	}
}

// upgrade-failed is the operator saying the upgrade did not work. It needs no
// timing evidence at all: the state is terminal, not transitional.
func TestUpgradeStuckReportsAFailedUpgrade(t *testing.T) {
	t.Parallel()

	rule := rules.NewUpgradeStuck(30 * time.Minute)
	finding := only(t, evaluate(rule, upgradeInput(upgradingNode("worker-1", "upgrade-failed"))))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"the node's driver upgrade failed",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityError {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityError)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/gpu-driver-upgrade-state": "upgrade-failed",
		"upgradeStuckAfter":                   "30m0s",
	})
}

// The label carries no transition time, so the duration comes off the event
// the upgrade controller recorded when it set the state.
func TestUpgradeStuckReportsAStuckStateWithEventEvidence(t *testing.T) {
	t.Parallel()

	rule := rules.NewUpgradeStuck(30 * time.Minute)
	entered := upgNow.Add(-2 * time.Hour)
	in := upgradeInput(upgradingNode("worker-1", "cordon-required"),
		upgradeEvent("worker-1", "cordon-required", entered))

	finding := only(t, evaluate(rule, in))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"the node has been in driver upgrade state cordon-required for longer than --upgrade-stuck-after",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityWarning {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityWarning)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/gpu-driver-upgrade-state": "cordon-required",
		"since":                               "2026-10-11T10:00:00Z",
		"for":                                 "2h0m0s",
		"upgradeStuckAfter":                   "30m0s",
	})
}

// Without an event there is no evidence that the node has been there long, so
// the finding drops to info rather than claiming a duration nobody measured.
func TestUpgradeStuckDowngradesWithoutEventEvidence(t *testing.T) {
	t.Parallel()

	rule := rules.NewUpgradeStuck(30 * time.Minute)
	finding := only(t, evaluate(rule, upgradeInput(upgradingNode("worker-1", "pod-restart-required"))))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"the node is in driver upgrade state pod-restart-required and no event says since when",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityInfo {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityInfo)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/gpu-driver-upgrade-state": "pod-restart-required",
		"upgradeStuckAfter":                   "30m0s",
	})
}

// An upgrade that has been running for five minutes is an upgrade, not a
// stuck node.
func TestUpgradeStuckStaysQuietInsideTheWindow(t *testing.T) {
	t.Parallel()

	in := upgradeInput(upgradingNode("worker-1", "validation-required"),
		upgradeEvent("worker-1", "validation-required", upgNow.Add(-5*time.Minute)))

	if findings := evaluate(rules.NewUpgradeStuck(30*time.Minute), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The window is the flag's, so raising it has to silence a node the default
// would report.
func TestUpgradeStuckHonoursTheWindow(t *testing.T) {
	t.Parallel()

	in := upgradeInput(upgradingNode("worker-1", "cordon-required"),
		upgradeEvent("worker-1", "cordon-required", upgNow.Add(-2*time.Hour)))

	if findings := evaluate(rules.NewUpgradeStuck(30*time.Minute), in); len(findings) != 1 {
		t.Fatalf("at 30m the node is stuck: got %d findings, want 1", len(findings))
	}

	if findings := evaluate(rules.NewUpgradeStuck(4*time.Hour), in); len(findings) != 0 {
		t.Fatalf("at 4h the node is not stuck yet: got %d findings, want none: %+v", len(findings), findings)
	}
}

// The states that are not stuck states, and the empty label, are the ordinary
// condition of every node in a cluster that is not upgrading.
func TestUpgradeStuckStaysQuietOnTheOtherStates(t *testing.T) {
	t.Parallel()

	for _, state := range []string{"", "upgrade-done", "upgrade-required", "uncordon-required", "drain-required"} {
		t.Run("state="+state, func(t *testing.T) {
			t.Parallel()

			in := upgradeInput(upgradingNode("worker-1", state),
				upgradeEvent("worker-1", state, upgNow.Add(-9*time.Hour)))

			if findings := evaluate(rules.NewUpgradeStuck(30*time.Minute), in); len(findings) != 0 {
				t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
			}
		})
	}
}

// An event about another node says nothing about this one, and taking it
// would date the state from somebody else's upgrade.
func TestUpgradeStuckIgnoresAnEventAboutAnotherNode(t *testing.T) {
	t.Parallel()

	rule := rules.NewUpgradeStuck(30 * time.Minute)
	in := upgradeInput(upgradingNode("worker-1", "cordon-required"),
		upgradeEvent("worker-2", "cordon-required", upgNow.Add(-2*time.Hour)))

	finding := only(t, evaluate(rule, in))
	if finding.Severity != output.SeverityInfo {
		t.Errorf("Severity is %q, want %q: the event names another node, so there is no evidence", finding.Severity, output.SeverityInfo)
	}
}

// The event that dates a state is the one that announced that state. The node
// passed through cordon-required on its way here, and the event saying so is
// older than the one that matters.
func TestUpgradeStuckIgnoresAnEventAboutAnotherState(t *testing.T) {
	t.Parallel()

	rule := rules.NewUpgradeStuck(30 * time.Minute)
	in := upgradeInput(upgradingNode("worker-1", "validation-required"),
		upgradeEvent("worker-1", "cordon-required", upgNow.Add(-9*time.Hour)))

	finding := only(t, evaluate(rule, in))
	if finding.Severity != output.SeverityInfo {
		t.Errorf("Severity is %q, want %q: no event announced validation-required", finding.Severity, output.SeverityInfo)
	}
}

// An event with another reason is not the upgrade controller's. Dating the
// state from the kubelet's last NodeReady would silence every stuck node on a
// healthy cluster.
func TestUpgradeStuckIgnoresAnEventFromAnotherController(t *testing.T) {
	t.Parallel()

	rule := rules.NewUpgradeStuck(30 * time.Minute)
	foreign := upgradeEvent("worker-1", "cordon-required", upgNow.Add(-2*time.Hour))
	foreign.Reason = "NodeReady"

	finding := only(t, evaluate(rule, upgradeInput(upgradingNode("worker-1", "cordon-required"), foreign)))
	if finding.Severity != output.SeverityInfo {
		t.Errorf("Severity is %q, want %q: the event is not the upgrade controller's", finding.Severity, output.SeverityInfo)
	}
}

// The newest event wins: a node that re-entered a state two minutes ago has
// been there two minutes, whatever it did this morning.
func TestUpgradeStuckTakesTheNewestEvent(t *testing.T) {
	t.Parallel()

	in := upgradeInput(upgradingNode("worker-1", "cordon-required"),
		upgradeEvent("worker-1", "cordon-required", upgNow.Add(-9*time.Hour)),
		upgradeEvent("worker-1", "cordon-required", upgNow.Add(-2*time.Minute)))

	if findings := evaluate(rules.NewUpgradeStuck(30*time.Minute), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

func TestUpgradeStuckSkipsHintedNodes(t *testing.T) {
	t.Parallel()

	node := mokkaControlPlane()
	node.UpgradeState = "upgrade-failed"

	in := health.Input{Hinted: []inventory.GPUNode{node}, OperatorInstalled: true, Now: upgNow}

	if findings := evaluate(rules.NewUpgradeStuck(30*time.Minute), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

func TestUpgradeStuckAppliesToEveryOperatorVersion(t *testing.T) {
	t.Parallel()

	rule := rules.NewUpgradeStuck(30 * time.Minute)
	if !rule.Applies("v25.10.0") || !rule.Applies("") {
		t.Error("the rule has to apply to every operator version and to an unknown one")
	}

	bounded := rules.NewUpgradeStuck(30 * time.Minute)
	bounded.MinVersion = "v99.0.0"

	if bounded.Applies("v25.10.0") {
		t.Error("the version gate is not wired: a bounded rule still applies below its minimum")
	}
}

// A zero or negative window is a caller that forgot to set one, and the
// default is what the flag documents.
func TestUpgradeStuckFallsBackToTheDefaultWindow(t *testing.T) {
	t.Parallel()

	in := upgradeInput(upgradingNode("worker-1", "cordon-required"),
		upgradeEvent("worker-1", "cordon-required", upgNow.Add(-10*time.Minute)))

	if findings := evaluate(rules.NewUpgradeStuck(0), in); len(findings) != 0 {
		t.Fatalf("ten minutes is inside the %s default: got %d findings, want none: %+v",
			rules.DefaultUpgradeStuckAfter, len(findings), findings)
	}

	later := upgradeInput(upgradingNode("worker-1", "cordon-required"),
		upgradeEvent("worker-1", "cordon-required", upgNow.Add(-31*time.Minute)))

	finding := only(t, evaluate(rules.NewUpgradeStuck(0), later))
	if got := finding.Evidence["upgradeStuckAfter"]; got != rules.DefaultUpgradeStuckAfter.String() {
		t.Errorf("upgradeStuckAfter is %q, want %q", got, rules.DefaultUpgradeStuckAfter)
	}
}
