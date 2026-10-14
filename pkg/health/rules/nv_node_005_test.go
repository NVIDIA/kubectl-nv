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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

var sentinelSince = time.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)

// conditionNode is a worker carrying the conditions the case is about.
func conditionNode(name string, opts ...nodeOption) inventory.GPUNode {
	base := []nodeOption{
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withEveryOperandDeployed(),
		withCount(4),
		withGPUs(4, 4),
		withOperand(inventory.ComponentDevicePlugin, true),
	}

	return buildNode(name, append(base, opts...)...)
}

// at stamps a condition with its transition time, which is what the finding
// reports as "since".
func at(condition corev1.NodeCondition, when time.Time) corev1.NodeCondition {
	condition.LastTransitionTime = metav1.NewTime(when)

	return condition
}

// NVSentinel's platform connector names the condition after the health check
// and the reason after the check plus IsNotHealthy
// (platform-connectors/pkg/connectors/kubernetes/process_node_events.go:193
// and :950, NVSentinel 93fa8222).
func TestNodeConditionOrCordonReportsAnNVSentinelCondition(t *testing.T) {
	t.Parallel()

	rule := rules.NewNodeConditionOrCordon()
	node := conditionNode("worker-1",
		withConditions(at(nvsentinelCondition("GPUXidError", true, "Xid 79 on GPU 3"), sentinelSince)))

	in := health.Input{Nodes: []inventory.GPUNode{node}, OperatorInstalled: true}
	finding := only(t, evaluate(rule, in))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"an NVSentinel health check is reporting the node unhealthy",
		"kubectl nv info worker-1")

	// Informational until the Phase 2 false-positive baseline says how often
	// a healthy fleet produces one of these.
	if finding.Severity != output.SeverityInfo {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityInfo)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"condition": "GPUXidError",
		"status":    "True",
		"reason":    "GPUXidErrorIsNotHealthy",
		"message":   "Xid 79 on GPU 3",
		"since":     "2026-10-11T09:30:00Z",
	})
}

// The same check reporting healthy writes the condition at False with the
// IsHealthy reason, which is the state of every working node once NVSentinel
// has looked at it.
func TestNodeConditionOrCordonStaysQuietOnAHealthyNVSentinelCondition(t *testing.T) {
	t.Parallel()

	node := conditionNode("worker-1",
		withConditions(at(nvsentinelCondition("GPUXidError", false, ""), sentinelSince)))

	in := health.Input{Nodes: []inventory.GPUNode{node}, OperatorInstalled: true}

	if findings := evaluate(rules.NewNodeConditionOrCordon(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// Only what NVSentinel writes is matched. A cluster's own operators put all
// sorts of custom conditions on a node, and reporting every non-standard one
// true would make this rule fire on every node of every cluster that runs
// anything beyond the kubelet.
func TestNodeConditionOrCordonIgnoresAnUnrelatedCustomCondition(t *testing.T) {
	t.Parallel()

	for _, condition := range []corev1.NodeCondition{
		{Type: "FluentBitHealthy", Status: corev1.ConditionTrue, Reason: "AgentRunning"},
		{Type: "MemoryPressure", Status: corev1.ConditionTrue, Reason: "KubeletHasInsufficientMemory"},
		// The right shape at the wrong status: NVSentinel writes IsNotHealthy
		// only alongside True, so a False one is not a fault.
		{Type: "GPUXidError", Status: corev1.ConditionFalse, Reason: "GPUXidErrorIsNotHealthy"},
		// The reason naming another check is not this condition's verdict.
		{Type: "GPUXidError", Status: corev1.ConditionTrue, Reason: "GPURowRemapIsNotHealthy"},
	} {
		t.Run(string(condition.Type)+"/"+condition.Reason+"/"+string(condition.Status), func(t *testing.T) {
			t.Parallel()

			node := conditionNode("worker-1", withConditions(condition))
			in := health.Input{Nodes: []inventory.GPUNode{node}, OperatorInstalled: true}

			if findings := evaluate(rules.NewNodeConditionOrCordon(), in); len(findings) != 0 {
				t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
			}
		})
	}
}

// A cordon is often an operator doing maintenance on purpose, so on its own
// it is information rather than a fault.
func TestNodeConditionOrCordonReportsACordonAtInfo(t *testing.T) {
	t.Parallel()

	rule := rules.NewNodeConditionOrCordon()
	node := conditionNode("worker-1", cordoned())

	in := health.Input{Nodes: []inventory.GPUNode{node}, OperatorInstalled: true}
	finding := only(t, evaluate(rule, in))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"the GPU node is cordoned, so nothing new will be scheduled on it",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityInfo {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityInfo)
	}

	checkEvidence(t, finding.Evidence, map[string]string{"unschedulable": "true"})
}

// A node both cordoned and failing a check has two things wrong with it, and
// collapsing them into one finding loses whichever the reader needed.
func TestNodeConditionOrCordonReportsBoth(t *testing.T) {
	t.Parallel()

	node := conditionNode("worker-1",
		cordoned(),
		withConditions(at(nvsentinelCondition("GPUXidError", true, "Xid 79"), sentinelSince)))

	in := health.Input{Nodes: []inventory.GPUNode{node}, OperatorInstalled: true}
	findings := evaluate(rules.NewNodeConditionOrCordon(), in)

	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}

	if findings[0].Evidence["condition"] != "GPUXidError" {
		t.Errorf("the first finding is %v, want the failing check", findings[0].Evidence)
	}

	if findings[1].Evidence["unschedulable"] != "true" {
		t.Errorf("the second finding is %v, want the cordon", findings[1].Evidence)
	}
}

// Two failing checks on one node are two findings, each naming its own check.
func TestNodeConditionOrCordonReportsEveryFailingCheck(t *testing.T) {
	t.Parallel()

	node := conditionNode("worker-1", withConditions(
		at(nvsentinelCondition("GPURowRemap", true, "pending remap"), sentinelSince),
		at(nvsentinelCondition("GPUXidError", true, "Xid 79"), sentinelSince),
	))

	in := health.Input{Nodes: []inventory.GPUNode{node}, OperatorInstalled: true}
	findings := evaluate(rules.NewNodeConditionOrCordon(), in)

	if len(findings) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
	}

	if findings[0].Evidence["condition"] != "GPURowRemap" || findings[1].Evidence["condition"] != "GPUXidError" {
		t.Errorf("findings name %q and %q, want GPURowRemap then GPUXidError",
			findings[0].Evidence["condition"], findings[1].Evidence["condition"])
	}
}

// A node with no conditions at all, which is what kubectl nv list produces
// and what a bundle without them looks like, is not a node with no faults.
func TestNodeConditionOrCordonStaysQuietOnAHealthyLane(t *testing.T) {
	t.Parallel()

	if findings := evaluate(rules.NewNodeConditionOrCordon(), healthyLane()); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

func TestNodeConditionOrCordonSkipsHintedNodes(t *testing.T) {
	t.Parallel()

	node := mokkaControlPlane()
	cordoned()(&node)
	withConditions(at(nvsentinelCondition("GPUXidError", true, "Xid 79"), sentinelSince))(&node)

	in := health.Input{Hinted: []inventory.GPUNode{node}, OperatorInstalled: true}

	if findings := evaluate(rules.NewNodeConditionOrCordon(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

func TestNodeConditionOrCordonAppliesToEveryOperatorVersion(t *testing.T) {
	t.Parallel()

	rule := rules.NewNodeConditionOrCordon()
	if !rule.Applies("v25.10.0") || !rule.Applies("") {
		t.Error("the rule has to apply to every operator version and to an unknown one")
	}

	bounded := rules.NewNodeConditionOrCordon()
	bounded.MinVersion = "v99.0.0"

	if bounded.Applies("v25.10.0") {
		t.Error("the version gate is not wired: a bounded rule still applies below its minimum")
	}
}
