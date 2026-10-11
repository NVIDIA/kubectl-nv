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
	"strings"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The nvidia.com/gpu.deploy.<key> suffixes the operator gates the two driver
// operands on.
const (
	deployKeyDriver       = "driver"
	deployKeyVGPUManager  = "vgpu-manager"
	deployKeyContainerTKT = "container-toolkit"
)

// componentDriverToolkit is the component a driver pod selected by
// app.kubernetes.io/component=nvidia-driver arrives under. pkg/inventory takes
// that label when a pod carries no "app" key, so the value is what reaches
// OperandPod.Component verbatim.
const componentDriverToolkit = "nvidia-driver"

// IsDriverComponent reports whether an operand component names a driver pod.
//
// The set is pkg/mustgather's, because a driver this rule does not recognise
// is a driver nobody collects a bug report from either: the two are kept the
// same list by nv_drv_001_test.go, which reads that package's selectors and
// fails on one this function does not claim.
//
// The exact names are not enough on their own. OpenShift builds one driver
// DaemonSet per RHCOS version and names it
// nvidia-driver-daemonset-<version>-<rhcos>, so the "app" label on those pods
// is the base name with a suffix. The suffix has to start with a hyphen: a
// bare prefix match would claim any future operand whose name merely begins
// the same way.
func IsDriverComponent(component string) bool {
	if component == componentDriverToolkit {
		return true
	}

	for _, base := range []string{inventory.ComponentDriver, inventory.ComponentVGPUManager} {
		if component == base || strings.HasPrefix(component, base+"-") {
			return true
		}
	}

	return false
}

// deployKeyForDriver returns the deploy label key gating the component, so
// that the evidence names the label a reader can go and look at.
func deployKeyForDriver(component string) string {
	if component == inventory.ComponentVGPUManager ||
		strings.HasPrefix(component, inventory.ComponentVGPUManager+"-") {
		return deployKeyVGPUManager
	}

	return deployKeyDriver
}

// DriverPodNotReady is NV-DRV-001: a driver pod on the node is not reporting
// ready.
//
// The driver is what every other operand is waiting for: without it the
// toolkit validation never passes, the device plugin never advertises
// anything, and the node ends up in NV-NODE-002 with nothing saying why. This
// rule is what says why.
//
// A crash-looping pod is covered by the same condition rather than by a
// separate one: a container in CrashLoopBackOff is never ready, so a pod stuck
// in it fails the readiness test whatever its phase says.
//
// What this rule deliberately does not report is a pod that is ready now and
// has restarted. inventory.OperandPod carries a restart count and no
// termination time, so "restarted ten minutes ago" and "restarted in March"
// are the same input here, and a rule that fired on a non-zero count would
// report every long lived node in the cluster for ever. The count is in the
// evidence of the findings that do fire, and dating the last termination needs
// a field on the operand model that only pkg/inventory can add.
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
type DriverPodNotReady struct{ versionRange }

// NewDriverPodNotReady returns the rule. Both version bounds are empty: the
// driver DaemonSet predates every operator version this plugin supports.
func NewDriverPodNotReady() *DriverPodNotReady { return &DriverPodNotReady{} }

// Code implements health.Rule.
func (r *DriverPodNotReady) Code() string { return "NV-DRV-001" }

// Severity implements health.Rule. An error: nothing else on the node works
// without the driver.
func (r *DriverPodNotReady) Severity() output.Severity { return output.SeverityError }

// Evaluate implements health.Rule.
func (r *DriverPodNotReady) Evaluate(in health.Input) []output.Finding {
	if !in.OperatorInstalled {
		return nil
	}

	var findings []output.Finding

	for _, node := range in.Nodes {
		for _, pod := range node.Operands {
			if !IsDriverComponent(pod.Component) || pod.Ready {
				continue
			}

			deployKey := deployKeyForDriver(pod.Component)
			if operandDisabledOn(node, deployKey) {
				continue
			}

			findings = append(findings, output.Finding{
				Code:      r.Code(),
				Severity:  r.Severity(),
				Node:      node.Name,
				Component: pod.Component,
				Message:   "the driver pod on the node is not ready",
				Evidence:  operandEvidence(node, deployKey, pod, true),
				Next:      infoCommand(node.Name),
			})
		}
	}

	return findings
}
