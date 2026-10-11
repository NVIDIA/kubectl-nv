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
	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// deployKeyDevicePlugin is the nvidia.com/gpu.deploy.<key> suffix the operator
// gates the device plugin on.
const deployKeyDevicePlugin = "device-plugin"

// DevicePluginNotReady is NV-DP-001: a GPU node whose device plugin pod is
// missing or not ready.
//
// The device plugin is what puts nvidia.com/gpu into a node's allocatable, so
// a GPU node without a working one is a node on its way to NV-NODE-002. This
// rule is what says why.
//
// Three conditions have to hold before it fires:
//
//   - The operator is installed. It writes the deploy labels, so a cluster
//     without it has none, and a device plugin installed by hand is nothing
//     the operator promised to run.
//   - The ClusterPolicy asks for the operand. The deploy label cannot answer
//     that: the operator writes nvidia.com/gpu.deploy.device-plugin=true on
//     every node carrying the NVIDIA PCI label whatever the policy enables, so
//     reading the label as consent made this rule fire on every node of every
//     cluster that switched the device plugin off in its chart values.
//   - The operand is not switched off or paused for the node.
//   - The node advertises its GPUs through the device plugin at all. A node in
//     pure DRA mode publishes a ResourceSlice and runs a kubelet plugin
//     instead, so a missing device plugin there is the design rather than a
//     fault. A node running both, which is what a migration looks like
//     mid-flight, still needs one.
type DevicePluginNotReady struct{ versionRange }

// NewDevicePluginNotReady returns the rule. Both version bounds are empty: the
// device plugin operand predates every operator version this plugin supports.
func NewDevicePluginNotReady() *DevicePluginNotReady { return &DevicePluginNotReady{} }

// Code implements health.Rule.
func (r *DevicePluginNotReady) Code() string { return "NV-DP-001" }

// Severity implements health.Rule. An error: without this pod the node's GPUs
// are not placeable.
func (r *DevicePluginNotReady) Severity() output.Severity { return output.SeverityError }

// Evaluate implements health.Rule.
//
// Only the GPU nodes are walked. A hinted node advertises no GPUs at all, and
// that is NV-NODE-002's finding rather than this one's.
func (r *DevicePluginNotReady) Evaluate(in health.Input) []output.Finding {
	if !in.OperatorInstalled || !OperandEnabledInPolicy(in, PolicyFieldDevicePlugin) {
		return nil
	}

	var findings []output.Finding

	for _, node := range in.Nodes {
		if node.Mode == inventory.ModeDRA || operandDisabledOn(node, deployKeyDevicePlugin) {
			continue
		}

		pod, found := operandFor(node, inventory.ComponentDevicePlugin)
		if found && pod.Ready {
			continue
		}

		message := "the device plugin pod on the node is not ready"
		if !found {
			message = "the node runs no device plugin pod"
		}

		findings = append(findings, output.Finding{
			Code:      r.Code(),
			Severity:  r.Severity(),
			Node:      node.Name,
			Component: inventory.ComponentDevicePlugin,
			Message:   message,
			Evidence:  operandEvidence(node, deployKeyDevicePlugin, pod, found),
			Next:      infoCommand(node.Name),
		})
	}

	return findings
}

// operandEvidence is the proof behind a missing or unready operand: what the
// operator was told to do with it, what else is running on the node, and the
// pod itself when there is one.
func operandEvidence(node inventory.GPUNode, deployKey string, pod inventory.OperandPod, found bool) map[string]string {
	evidence := map[string]string{}

	setIf(evidence, inventory.DeployLabel(deployKey), node.Deploy[deployKey])
	setIf(evidence, "operands", operandComponents(node))

	if found {
		evidence["pod"] = pod.Name
		evidence["phase"] = pod.Phase
		evidence["ready"] = btoa(pod.Ready)
		evidence["restarts"] = itoa(pod.Restarts)
	}

	return evidence
}
