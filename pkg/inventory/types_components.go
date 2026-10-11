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

package inventory

import "sort"

// The operand vocabulary.
//
// Two different strings name the same operand and they are easy to confuse:
//
//	the component: the verbatim value of the "app" label on the operand's
//	pods, which is what OperandPod.Component holds and what a rule matches on.
//	"nvidia-driver-daemonset".
//
//	the deploy key: the suffix of the nvidia.com/gpu.deploy.<key> node label
//	the operator gates that operand on. "driver".
//
// They are not derivable from each other: the driver is "driver" and
// "nvidia-driver-daemonset", while GFD is "gpu-feature-discovery" both times.
// A command that reported one where the other belongs would be presenting two
// vocabularies as one, so both are written out here and ComponentForDeployKey
// is the only bridge between them.
//
// The constants are the values observed in this repository's own fixtures and
// collection code (pkg/mustgather's driver pod selectors, pkg/source/testdata,
// NOTES.md's record of the Mokka lane) together with the operands the GPU
// Operator and the NVIDIA DRA driver publish. The list is not a closed set:
// OperandPod.Component carries whatever the label said, so an operand nothing
// here names still reaches a reader under its own name.
const (
	// ComponentDriver is the NVIDIA driver DaemonSet.
	ComponentDriver = "nvidia-driver-daemonset"
	// ComponentVGPUManager is the vGPU manager DaemonSet, which takes the
	// driver's place on a vGPU host.
	ComponentVGPUManager = "nvidia-vgpu-manager-daemonset"
	// ComponentContainerToolkit is the container toolkit DaemonSet.
	ComponentContainerToolkit = "nvidia-container-toolkit-daemonset"
	// ComponentDevicePlugin is the device plugin DaemonSet.
	ComponentDevicePlugin = "nvidia-device-plugin-daemonset"
	// ComponentGFD is GPU feature discovery, which writes the nvidia.com/gpu.*
	// labels this package reads almost everything from.
	ComponentGFD = "gpu-feature-discovery"
	// ComponentDCGM is the DCGM host engine.
	ComponentDCGM = "nvidia-dcgm"
	// ComponentDCGMExporter is the DCGM Prometheus exporter.
	ComponentDCGMExporter = "nvidia-dcgm-exporter"
	// ComponentOperatorValidator is the validator, whose verdict is carried in
	// its init container exit codes rather than in its logs. See InitResult.
	ComponentOperatorValidator = "nvidia-operator-validator"
	// ComponentMIGManager is the mig-manager, which applies nvidia.com/mig.config
	// and reports through nvidia.com/mig.config.state.
	ComponentMIGManager = "nvidia-mig-manager"
	// ComponentNodeStatusExporter is the node status exporter.
	ComponentNodeStatusExporter = "nvidia-node-status-exporter"
	// ComponentOperator is the GPU Operator controller itself. It is not a
	// per-node operand, but it carries the same label and a node scoped report
	// that found it still has to name it.
	ComponentOperator = "gpu-operator"
	// ComponentDRADriver is the NVIDIA DRA driver's kubelet plugin, which is
	// what publishes a node's gpu.nvidia.com ResourceSlices. A node whose
	// kubelet plugin is not running advertises nothing under DRA however
	// healthy every GPU Operator pod on it looks.
	ComponentDRADriver = "nvidia-dra-driver-gpu-kubelet-plugin"
	// ComponentIMEXDaemon is the DRA driver's IMEX daemon, which a
	// ComputeDomain is served by.
	ComponentIMEXDaemon = "nvidia-dra-driver-gpu-imex-daemon"
)

// DeployLabelPrefix is the prefix of the per-operand gating labels the GPU
// Operator writes on a node: nvidia.com/gpu.deploy.<key>.
const DeployLabelPrefix = "nvidia.com/gpu.deploy."

// components is the list behind Components, in the order the constants are
// declared above, which is roughly the order the operands come up on a node.
var components = []string{
	ComponentDriver,
	ComponentVGPUManager,
	ComponentContainerToolkit,
	ComponentDevicePlugin,
	ComponentGFD,
	ComponentDCGM,
	ComponentDCGMExporter,
	ComponentOperatorValidator,
	ComponentMIGManager,
	ComponentNodeStatusExporter,
	ComponentOperator,
	ComponentDRADriver,
	ComponentIMEXDaemon,
}

// componentForDeployKey maps a nvidia.com/gpu.deploy.<key> suffix onto the
// component whose pods that label gates. Operands with no deploy label of
// their own, the operator itself and the DRA driver's pods, are absent.
var componentForDeployKey = map[string]string{
	"driver":                ComponentDriver,
	"vgpu-manager":          ComponentVGPUManager,
	"container-toolkit":     ComponentContainerToolkit,
	"device-plugin":         ComponentDevicePlugin,
	"gpu-feature-discovery": ComponentGFD,
	"dcgm":                  ComponentDCGM,
	"dcgm-exporter":         ComponentDCGMExporter,
	"operator-validator":    ComponentOperatorValidator,
	"mig-manager":           ComponentMIGManager,
	"node-status-exporter":  ComponentNodeStatusExporter,
}

// Components returns the operand components this package names, as a fresh
// slice so that a caller may sort or truncate it.
func Components() []string {
	out := make([]string, len(components))
	copy(out, components)

	return out
}

// DeployKeys returns the deploy label keys this package maps, sorted, as a
// fresh slice.
func DeployKeys() []string {
	out := make([]string, 0, len(componentForDeployKey))
	for key := range componentForDeployKey {
		out = append(out, key)
	}

	sort.Strings(out)

	return out
}

// ComponentForDeployKey returns the component a nvidia.com/gpu.deploy.<key>
// label gates. key is the suffix alone, not the whole label name: a caller
// reading a node's labels strips DeployLabelPrefix first, and passing the full
// label here is a mistake that has to be visible rather than a lookup that
// quietly misses.
func ComponentForDeployKey(key string) (string, bool) {
	component, ok := componentForDeployKey[key]

	return component, ok
}

// DeployLabel returns the full node label name for a deploy key.
func DeployLabel(key string) string { return DeployLabelPrefix + key }
