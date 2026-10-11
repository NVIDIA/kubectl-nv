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

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
)

// The deploy label values that mean an operand is not supposed to be running.
const (
	// deployFalse is the operator's own "do not deploy this operand here".
	deployFalse = "false"
	// deployPausedPrefix is how the operator parks an operand for the length
	// of something else: "paused-for-driver-upgrade",
	// "paused-for-mig-change". The suffix names the reason and changes between
	// versions, so only the prefix is matched.
	deployPausedPrefix = "paused-"
)

// DeployDisabled reports whether a nvidia.com/gpu.deploy.<operand> label value
// means the operand is not expected to run.
//
// Only the two documented shapes count. An unrecognised value is not a claim
// that the operand is off: reading one as "disabled" would silence the rules
// on the first cluster whose operator writes something this code has not seen,
// and a silenced rule reports clean for the wrong reason.
func DeployDisabled(value string) bool {
	return value == deployFalse || strings.HasPrefix(value, deployPausedPrefix)
}

// operandTolerations are the taints a GPU Operator operand DaemonSet pod
// tolerates, and so the taints that do not keep the operator off a node.
//
// Two sources, and both are needed:
//
//   - the operator's own toleration, nvidia.com/gpu with operator Exists and
//     effect NoSchedule, which is what lets the operands land on GPU nodes a
//     cluster has tainted to keep other work off them.
//   - the ones the DaemonSet controller adds to every daemon pod: the
//     not-ready and unreachable NoExecute taints, the resource pressure
//     taints, unschedulable, and network-unavailable. Leaving these out would
//     exclude exactly the nodes most likely to be broken, because a node going
//     NotReady gets tainted by its own kubelet.
//
// What is deliberately absent is node-role.kubernetes.io/control-plane. No
// operand DaemonSet tolerates it, which is why a control plane carrying GPU
// hint labels is not a node the operator targets.
var operandTolerations = []corev1.Toleration{
	{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},

	{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	{Key: corev1.TaintNodeDiskPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodeMemoryPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodePIDPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodeUnschedulable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodeNetworkUnavailable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
}

// OperatorTargets reports whether the GPU Operator is expected to run its
// operands on this node. The node rules are gated on it.
//
// The gate exists because a GPU hint label is written by a DaemonSet and never
// retracted, so a node can carry nvidia.com/gpu.present=true forever while no
// operand will ever land on it. On this repository's own Mokka lane that is
// the control plane: the simulated-GPU DaemonSet tolerates every taint, so
// node-feature-discovery labels the control plane, and every GPU Operator
// operand is kept off it by a taint none of them tolerate (NOTES.md, "Facts
// observed"). Without this gate a healthy lane reports an error on every run.
//
// Three questions, in this order:
//
//  1. Did the operator say not to? A node whose nvidia.com/gpu.deploy.* labels
//     are all false or paused is a node the operator was told to leave alone.
//     This comes first because it is an instruction rather than an inference.
//  2. Is an operand already running there? That settles it: whatever the
//     taints say, the pods landed. An observation beats a prediction.
//  3. Would every operand be blocked by a taint? A NoSchedule or NoExecute
//     taint that none of the operand tolerations match keeps all of them off.
//
// Anything else is a node the operator targets. The default is deliberately
// the permissive one: a rule that runs where it should not produces a finding
// a reader can dismiss, where a rule that does not run where it should
// produces a clean report for a broken cluster.
func OperatorTargets(node inventory.GPUNode) bool {
	if everyOperandDisabled(node.Deploy) {
		return false
	}

	if len(node.Operands) > 0 {
		return true
	}

	return !everyOperandTaintedOff(node.Taints)
}

// everyOperandDisabled reports whether the deploy labels say the operator was
// told to run nothing here. No deploy labels at all is not an answer: the
// operator writes them, so their absence means it has not reached the node
// rather than that it was switched off.
func everyOperandDisabled(deploy map[string]string) bool {
	if len(deploy) == 0 {
		return false
	}

	for _, value := range deploy {
		if !DeployDisabled(value) {
			return false
		}
	}

	return true
}

// everyOperandTaintedOff reports whether a taint keeps every operand off the
// node. Only NoSchedule and NoExecute keep a pod off; PreferNoSchedule is a
// preference the scheduler may ignore, and treating it as a block would
// exclude spot and overflow nodes that run operands perfectly well.
func everyOperandTaintedOff(taints []corev1.Taint) bool {
	for _, taint := range taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}

		if !toleratedByAnOperand(taint) {
			return true
		}
	}

	return false
}

// toleratedByAnOperand reports whether one of operandTolerations matches the
// taint.
//
// The match is written out rather than taken from corev1's ToleratesTaint,
// which takes a logger and would make go-logr a direct dependency of this
// module for a comparison of two strings. It handles the two operators the
// list above uses and nothing else: a toleration with an operator this does
// not know tolerates nothing, so adding one without extending this function
// makes the gate stricter about what counts as tolerated, which produces a
// finding a reader can dismiss rather than a clean report for a broken node.
func toleratedByAnOperand(taint corev1.Taint) bool {
	for _, toleration := range operandTolerations {
		if toleration.Key != taint.Key || toleration.Effect != taint.Effect {
			continue
		}

		switch toleration.Operator {
		case corev1.TolerationOpExists:
			return true
		case corev1.TolerationOpEqual:
			if toleration.Value == taint.Value {
				return true
			}
		}
	}

	return false
}
