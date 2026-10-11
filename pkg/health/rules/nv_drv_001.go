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

package rules

import (
	"time"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The nvidia.com/gpu.deploy.<key> suffixes the operator gates the driver
// operands and the container toolkit on.
const (
	deployKeyDriver       = "driver"
	deployKeyVGPUManager  = "vgpu-manager"
	deployKeyContainerTKT = "container-toolkit"
)

// DriverRestartWindow is how recently a driver pod must have died for a
// restart to count as a fault.
//
// Ten minutes, because the question the window answers is "is this pod dying
// now": a driver that crashed once during this morning's rollout and has been
// up since is not a finding, and a driver in CrashLoopBackOff produces a new
// termination every few minutes at the longest back-off. It is a constant
// rather than a flag because, unlike the upgrade window, it is not a property
// of how a fleet is operated.
const DriverRestartWindow = 10 * time.Minute

// deployKeyForDriver returns the deploy label key gating the component, so
// that the evidence names the label a reader can go and look at.
func deployKeyForDriver(component string) string {
	if component == inventory.ComponentVGPUManager {
		return deployKeyVGPUManager
	}

	return deployKeyDriver
}

// DriverPodNotReady is NV-DRV-001: a driver pod on the node is not ready, or
// is restarting.
//
// The driver is what every other operand is waiting for: without it the
// toolkit validation never passes, the device plugin never advertises
// anything, and the node ends up in NV-NODE-002 with nothing saying why. This
// rule is what says why.
//
// Two conditions, because they are different faults with the same cause:
//
//   - The pod is not ready. A container in CrashLoopBackOff is never ready, so
//     a pod stuck in one fails this test whatever its phase says.
//   - The pod is ready and has restarted inside DriverRestartWindow. This is
//     the driver that comes up, runs for a minute and dies again, which a
//     readiness check sampled at the wrong moment calls healthy. It needs both
//     halves: a restart count on its own cannot tell "restarted ten minutes
//     ago" from "restarted in March", which is why pkg/inventory carries the
//     last termination beside the count.
//
// Which pods are driver pods is pkg/inventory's DriverComponent, the same
// classification pkg/mustgather collects a bug report through. It cannot be
// done on the pod's own "app" label: every DaemonSet the NVIDIADriver CR
// produces carries its generated name there.
//
// Three conditions have to hold before it fires:
//
//   - The operator is installed. It writes the deploy labels, and a driver
//     installed by hand is nothing it promised to run.
//   - The operand is not switched off or paused for the node.
//   - The node runs a driver pod at all. Mokka runs simulated GPUs and no
//     driver, and reporting the absence would make a healthy lane red on every
//     run. A node that is supposed to have one and does not is NV-OP-002's
//     finding.
//
// Both the GPU nodes and the hinted ones are walked. A node whose driver is
// broken stops advertising nvidia.com/gpu and moves into Input.Hinted, so a
// rule reading only the GPU nodes would lose the fault exactly when it
// appeared. See everyNode.
type DriverPodNotReady struct{ versionRange }

// NewDriverPodNotReady returns the rule. Both version bounds are empty: the
// driver DaemonSet predates every operator version this plugin supports.
func NewDriverPodNotReady() *DriverPodNotReady { return &DriverPodNotReady{} }

// Code implements health.Rule.
func (r *DriverPodNotReady) Code() string { return "NV-DRV-001" }

// Severity implements health.Rule. An error: nothing else on the node works
// without the driver. A finding on a node the operator is deliberately
// upgrading lowers itself to info.
func (r *DriverPodNotReady) Severity() output.Severity { return output.SeverityError }

// Evaluate implements health.Rule.
func (r *DriverPodNotReady) Evaluate(in health.Input) []output.Finding {
	if !in.OperatorInstalled {
		return nil
	}

	var findings []output.Finding

	for _, node := range everyNode(in) {
		for _, pod := range node.Operands {
			if !isDriverComponent(pod.Component) {
				continue
			}

			finding, found := r.evaluatePod(in, node, pod)
			if found {
				findings = append(findings, finding)
			}
		}
	}

	return findings
}

// isDriverComponent reports whether a component value names a driver operand.
// pkg/inventory assigns exactly the two constants, so this is a comparison
// against them rather than a second classification.
func isDriverComponent(component string) bool {
	return component == inventory.ComponentDriver || component == inventory.ComponentVGPUManager
}

// evaluatePod is one driver pod's verdict.
func (r *DriverPodNotReady) evaluatePod(
	in health.Input, node inventory.GPUNode, pod inventory.OperandPod,
) (output.Finding, bool) {
	deployKey := deployKeyForDriver(pod.Component)
	if operandDisabledOn(node, deployKey) {
		return output.Finding{}, false
	}

	evidence := operandEvidence(node, deployKey, pod, true)

	message := "the driver pod on the node is not ready"
	if pod.Ready {
		died, recent := insideWindow(pod.LastTermination, in.Now, DriverRestartWindow)
		if pod.Restarts <= 0 || !recent {
			return output.Finding{}, false
		}

		message = "the driver pod on the node is restarting"
		evidence["lastTerminationAt"] = died.UTC().Format(time.RFC3339)
		evidence["lastExitCode"] = itoa(pod.LastTermination.ExitCode)
		evidence["restartWindow"] = DriverRestartWindow.String()
	}

	severity := r.Severity()

	// An upgrade takes the driver down and puts it back on purpose. Reporting
	// an error there would make every rolling driver upgrade in a fleet look
	// like an outage; whether the upgrade itself is wedged is NV-UPG-001's
	// finding.
	if midUpgrade(node) {
		severity = output.SeverityInfo
		evidence[inventory.LabelUpgradeState] = node.UpgradeState
	}

	return output.Finding{
		Code:      r.Code(),
		Severity:  severity,
		Node:      node.Name,
		Component: pod.Component,
		Message:   message,
		Evidence:  evidence,
		Next:      infoCommand(node.Name),
	}, true
}
