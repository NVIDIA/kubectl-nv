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

// driverPod builds a driver pod of the named component with its readiness and
// restart count spelled out.
func driverPod(component string, ready bool, restarts int32) inventory.OperandPod {
	phase := string(corev1.PodRunning)
	if !ready {
		phase = string(corev1.PodPending)
	}

	return inventory.OperandPod{
		Name:      component + "-abcde",
		Namespace: "gpu-operator",
		Component: component,
		Phase:     phase,
		Ready:     ready,
		Restarts:  restarts,
	}
}

// drvNow is the injected clock every case in this file is written against.
var drvNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// drvInput is one node, the clock and the operator installed.
func drvInput(node inventory.GPUNode) health.Input {
	return health.Input{
		Nodes:             []inventory.GPUNode{node},
		OperatorInstalled: true,
		Now:               drvNow,
	}
}

// driverNode is a worker whose driver pod is in the state the case is about.
func driverNode(name string, pod inventory.OperandPod) inventory.GPUNode {
	return buildNode(name,
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withEveryOperandDeployed(),
		withCount(4),
		withGPUs(4, 4),
		withOperandPod(pod),
		withOperand(inventory.ComponentDevicePlugin, true),
	)
}

func TestDriverPodNotReadyReportsAnUnreadyDriverPod(t *testing.T) {
	t.Parallel()

	rule := rules.NewDriverPodNotReady()
	in := drvInput(driverNode("worker-1", driverPod(inventory.ComponentDriver, false, 7)))

	finding := only(t, evaluate(rule, in))

	checkFinding(t, rule, finding, inventory.ComponentDriver,
		"the driver pod on the node is not ready",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityError {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityError)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/gpu.deploy.driver": "true",
		"operands":                     "nvidia-device-plugin-daemonset,nvidia-driver-daemonset",
		"pod":                          "nvidia-driver-daemonset-abcde",
		"phase":                        "Pending",
		"ready":                        "false",
		"restarts":                     "7",
	})
}

func TestDriverPodNotReadyStaysQuietOnAReadyDriverPod(t *testing.T) {
	t.Parallel()

	in := drvInput(driverNode("worker-1", driverPod(inventory.ComponentDriver, true, 0)))

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// Mokka runs simulated GPUs and no driver pod at all. A node with no driver
// pod is not this finding: reporting one would make every lane run red, and
// the absence of a driver on a node that never had one is not a fault.
func TestDriverPodNotReadyStaysQuietWhenTheNodeRunsNoDriverPod(t *testing.T) {
	t.Parallel()

	if findings := evaluate(rules.NewDriverPodNotReady(), healthyLane()); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A ready pod whose last death is inside the window is a driver that comes
// up, runs for a minute and dies again: a readiness check sampled at the wrong
// moment calls it healthy, and the restart count plus the time of that death
// are what catch it.
func TestDriverPodNotReadyReportsARestartInsideTheWindow(t *testing.T) {
	t.Parallel()

	rule := rules.NewDriverPodNotReady()
	node := driverNode("worker-1", driverPod(inventory.ComponentDriver, true, 12))
	withLastTermination(inventory.ComponentDriver, 1, drvNow.Add(-4*time.Minute))(&node)

	finding := only(t, evaluate(rule, drvInput(node)))

	checkFinding(t, rule, finding, inventory.ComponentDriver,
		"the driver pod on the node is restarting",
		"kubectl nv info worker-1")

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/gpu.deploy.driver": "true",
		"operands":                     "nvidia-device-plugin-daemonset,nvidia-driver-daemonset",
		"pod":                          "nvidia-driver-daemonset-abcde",
		"phase":                        "Running",
		"ready":                        "true",
		"restarts":                     "12",
		"lastTerminationAt":            "2026-10-11T11:56:00Z",
		"lastExitCode":                 "1",
		"restartWindow":                "10m0s",
	})

	if finding.Severity != output.SeverityError {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityError)
	}
}

// A restart outside the window is history. A driver that crashed during this
// morning's rollout and has been up since is not a finding, and a rule firing
// on the count alone would report every long lived node for ever.
func TestDriverPodNotReadyStaysQuietOnARestartOutsideTheWindow(t *testing.T) {
	t.Parallel()

	node := driverNode("worker-1", driverPod(inventory.ComponentDriver, true, 12))
	withLastTermination(inventory.ComponentDriver, 1, drvNow.Add(-3*time.Hour))(&node)

	if findings := evaluate(rules.NewDriverPodNotReady(), drvInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A restart count with no termination recorded cannot be dated, and an
// undatable restart is not reported: the alternative is reading an absent
// time as now, which reports every pod that ever restarted.
func TestDriverPodNotReadyStaysQuietOnAnUndatableRestart(t *testing.T) {
	t.Parallel()

	in := drvInput(driverNode("worker-1", driverPod(inventory.ComponentDriver, true, 12)))

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A termination time this code cannot read is not a time inside the window.
// It arrives as a string off a cluster, and reading an unparsable one as now
// would report every pod that ever restarted on the first cluster whose
// timestamps have a shape this has not seen.
func TestDriverPodNotReadyStaysQuietOnAnUnreadableTerminationTime(t *testing.T) {
	t.Parallel()

	node := driverNode("worker-1", driverPod(inventory.ComponentDriver, true, 12))
	withLastTermination(inventory.ComponentDriver, 1, drvNow.Add(-time.Minute))(&node)

	for i := range node.Operands {
		if node.Operands[i].Component == inventory.ComponentDriver {
			node.Operands[i].LastTermination.FinishedAt = "last Tuesday"
		}
	}

	if findings := evaluate(rules.NewDriverPodNotReady(), drvInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A pod that has never restarted is not restarting, whatever else is dated on
// it. Without the count the window alone would report a pod that terminated
// once and was replaced.
func TestDriverPodNotReadyStaysQuietWithoutARestart(t *testing.T) {
	t.Parallel()

	node := driverNode("worker-1", driverPod(inventory.ComponentDriver, true, 0))
	withLastTermination(inventory.ComponentDriver, 1, drvNow.Add(-1*time.Minute))(&node)

	if findings := evaluate(rules.NewDriverPodNotReady(), drvInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The vGPU manager takes the driver's place on a vGPU host, and it is one of
// the selectors pkg/mustgather collects a bug report through.
func TestDriverPodNotReadyReportsTheVGPUManager(t *testing.T) {
	t.Parallel()

	rule := rules.NewDriverPodNotReady()
	in := drvInput(driverNode("worker-1", driverPod(inventory.ComponentVGPUManager, false, 0)))

	finding := only(t, evaluate(rule, in))

	checkFinding(t, rule, finding, inventory.ComponentVGPUManager,
		"the driver pod on the node is not ready",
		"kubectl nv info worker-1")

	// The vGPU manager is gated on its own deploy label, so the evidence has
	// to name that one and not the driver's.
	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/gpu.deploy.vgpu-manager": "true",
		"operands":                           "nvidia-device-plugin-daemonset,nvidia-vgpu-manager-daemonset",
		"pod":                                "nvidia-vgpu-manager-daemonset-abcde",
		"phase":                              "Pending",
		"ready":                              "false",
		"restarts":                           "0",
	})

	if finding.Severity != output.SeverityError {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityError)
	}
}

// A driver DaemonSet the NVIDIADriver CR produces reaches the rule as
// ComponentDriver, because pkg/inventory classifies it by selector rather than
// by its generated "app" label. This is the pod shape on every modern cluster,
// and before that classification existed the rule could not fire on one at
// all.
func TestDriverPodNotReadyReportsAnNVIDIADriverPod(t *testing.T) {
	t.Parallel()

	pod := driverPod(inventory.ComponentDriver, false, 0)
	pod.Name = "nvidia-gpu-driver-ubuntu22.04-7d9f4c2b1e-abcde"

	finding := only(t, evaluate(rules.NewDriverPodNotReady(), drvInput(driverNode("worker-1", pod))))

	if finding.Component != inventory.ComponentDriver {
		t.Errorf("Component is %q, want %q", finding.Component, inventory.ComponentDriver)
	}

	if finding.Evidence["pod"] != pod.Name {
		t.Errorf("evidence names %q, want the pod's own name %q", finding.Evidence["pod"], pod.Name)
	}
}

// A component whose name merely starts the same way is not a driver. Matching
// on a prefix without the separator would claim any operand a future chart
// names nvidia-driver-daemonsetfoo.
func TestDriverPodNotReadyIgnoresAnUnrelatedComponent(t *testing.T) {
	t.Parallel()

	in := drvInput(driverNode("worker-1", driverPod("nvidia-driver-daemonsetfoo", false, 0)))

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The deploy label is the operator saying it parked the operand on purpose,
// which is NV-OP-002's finding rather than this one's.
func TestDriverPodNotReadyStaysQuietWhileTheDriverIsPaused(t *testing.T) {
	t.Parallel()

	node := driverNode("worker-1", driverPod(inventory.ComponentDriver, false, 0))
	node.Deploy["driver"] = "paused-for-mig-change"

	in := drvInput(node)

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// No GPU Operator means no deploy labels and no operand it promised to run.
func TestDriverPodNotReadyStaysQuietWithoutTheOperator(t *testing.T) {
	t.Parallel()

	in := health.Input{Nodes: []inventory.GPUNode{driverNode("worker-1", driverPod(inventory.ComponentDriver, false, 0))}, Now: drvNow}

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A node whose driver is broken is a hinted node, and this is the case the
// rule exists for.
//
// The driver is what the device plugin waits on, so a node with a broken
// driver never advertises nvidia.com/gpu: it keeps its hint labels and leaves
// Input.Nodes for Input.Hinted. A rule reading only the GPU nodes lost the
// fault at exactly the moment it appeared.
func TestDriverPodNotReadyReportsAHintedWorker(t *testing.T) {
	t.Parallel()

	rule := rules.NewDriverPodNotReady()
	node := driverNode("worker-1", driverPod(inventory.ComponentDriver, false, 7))
	node.Capacity, node.Allocatable, node.Mode = 0, 0, inventory.ModeNone
	hinted()(&node)

	in := health.Input{Hinted: []inventory.GPUNode{node}, OperatorInstalled: true, Now: drvNow}
	finding := only(t, evaluate(rule, in))

	checkFinding(t, rule, finding, inventory.ComponentDriver,
		"the driver pod on the node is not ready",
		"kubectl nv info worker-1")

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/gpu.deploy.driver": "true",
		"operands":                     "nvidia-device-plugin-daemonset,nvidia-driver-daemonset",
		"pod":                          "nvidia-driver-daemonset-abcde",
		"phase":                        "Pending",
		"ready":                        "false",
		"restarts":                     "7",
	})

	if finding.Severity != output.SeverityError {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityError)
	}
}

// Walking the hinted nodes must not reopen the control plane problem. The
// Mokka control plane carries every GPU hint label and every deploy label and
// runs no driver pod at all, so there is nothing here for this rule to match.
func TestDriverPodNotReadyStaysQuietOnTheControlPlane(t *testing.T) {
	t.Parallel()

	in := health.Input{
		Hinted:            []inventory.GPUNode{mokkaControlPlane()},
		OperatorInstalled: true,
		Now:               drvNow,
	}

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// An upgrade takes the driver down and puts it back on purpose. Reporting an
// error there would make every rolling driver upgrade in a fleet look like an
// outage; whether the upgrade itself is wedged is NV-UPG-001's finding.
func TestDriverPodNotReadyDropsToInfoMidUpgrade(t *testing.T) {
	t.Parallel()

	node := driverNode("worker-1", driverPod(inventory.ComponentDriver, false, 0))
	withUpgradeState("pod-restart-required")(&node)

	finding := only(t, evaluate(rules.NewDriverPodNotReady(), drvInput(node)))

	if finding.Severity != output.SeverityInfo {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityInfo)
	}

	if got := finding.Evidence["nvidia.com/gpu-driver-upgrade-state"]; got != "pod-restart-required" {
		t.Errorf("the evidence does not say why the finding was lowered: %v", finding.Evidence)
	}
}

// upgrade-done is not an upgrade in progress. A driver that is not ready on a
// node the controller has finished with is an ordinary error.
func TestDriverPodNotReadyStaysAnErrorAfterTheUpgrade(t *testing.T) {
	t.Parallel()

	node := driverNode("worker-1", driverPod(inventory.ComponentDriver, false, 0))
	withUpgradeState("upgrade-done")(&node)

	finding := only(t, evaluate(rules.NewDriverPodNotReady(), drvInput(node)))

	if finding.Severity != output.SeverityError {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityError)
	}
}

// The vGPU manager is gated on its own deploy label, not the driver's, so a
// paused vGPU manager has to go quiet and a paused driver must not silence it.
func TestDriverPodNotReadyHonoursTheVGPUDeployLabel(t *testing.T) {
	t.Parallel()

	paused := driverNode("worker-1", driverPod(inventory.ComponentVGPUManager, false, 0))
	paused.Deploy["vgpu-manager"] = "paused-for-driver-upgrade"

	if findings := evaluate(rules.NewDriverPodNotReady(), drvInput(paused)); len(findings) != 0 {
		t.Fatalf("a paused vGPU manager is not a finding: got %d: %+v", len(findings), findings)
	}

	other := driverNode("worker-1", driverPod(inventory.ComponentVGPUManager, false, 0))
	other.Deploy["driver"] = "false"

	if findings := evaluate(rules.NewDriverPodNotReady(), drvInput(other)); len(findings) != 1 {
		t.Fatalf("the driver label does not gate the vGPU manager: got %d findings, want 1", len(findings))
	}
}

func TestDriverPodNotReadyAppliesToEveryOperatorVersion(t *testing.T) {
	t.Parallel()

	rule := rules.NewDriverPodNotReady()
	if !rule.Applies("v25.10.0") || !rule.Applies("") {
		t.Error("the rule has to apply to every operator version and to an unknown one")
	}

	bounded := rules.NewDriverPodNotReady()
	bounded.MinVersion = "v99.0.0"

	if bounded.Applies("v25.10.0") {
		t.Error("the version gate is not wired: a bounded rule still applies below its minimum")
	}
}
