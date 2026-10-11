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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
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
//
// The converse does not hold, and this is the trap: a value of "true" is not a
// claim that the operand will run. The operator writes the deploy labels true
// on every node carrying the NVIDIA PCI label, whatever the ClusterPolicy
// enables, so a rule that wants to know whether an operand was asked for reads
// OperandEnabledInPolicy as well.
func DeployDisabled(value string) bool {
	return value == deployFalse || strings.HasPrefix(value, deployPausedPrefix)
}

// defaultOperandTolerations are the taints a GPU Operator operand DaemonSet
// pod tolerates out of the box, and so the taints that do not keep the
// operator off a node.
//
// Two sources, and both are needed:
//
//   - the operator's own toleration, nvidia.com/gpu with operator Exists and
//     effect NoSchedule, which is what lets the operands land on GPU nodes a
//     cluster has tainted to keep other work off them. NoSchedule and not
//     NoExecute: the same key at NoExecute evicts the operands instead.
//   - the ones the DaemonSet controller adds to every daemon pod: the
//     not-ready and unreachable NoExecute taints, the resource pressure
//     taints, unschedulable, and network-unavailable. Leaving these out would
//     exclude exactly the nodes most likely to be broken, because a node going
//     NotReady gets tainted by its own kubelet.
//
// What is deliberately absent is node-role.kubernetes.io/control-plane. No
// operand DaemonSet tolerates it by default, which is why a control plane
// carrying GPU hint labels is not a node the operator targets.
//
// A ClusterPolicy that sets spec.daemonsets.tolerations adds to this list; see
// PolicyTolerations.
var defaultOperandTolerations = []corev1.Toleration{
	{Key: "nvidia.com/gpu", Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},

	{Key: corev1.TaintNodeNotReady, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	{Key: corev1.TaintNodeUnreachable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
	{Key: corev1.TaintNodeDiskPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodeMemoryPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodePIDPressure, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodeUnschedulable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
	{Key: corev1.TaintNodeNetworkUnavailable, Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
}

// PolicyTolerations returns the tolerations the ClusterPolicy puts on every
// operand DaemonSet, through spec.daemonsets.tolerations.
//
// A cluster that keeps its GPU nodes behind a custom taint tells the operator
// about it there, and the operator replaces the DaemonSets' own tolerations
// with the list. A gate that read only the defaults would decide the operator
// targets no node of such a cluster, and then gpu health would report nothing
// at all about it.
//
// Anything unreadable is skipped rather than guessed at: the CRD is not
// vendored, and a toleration this code cannot parse is one fewer reason to
// call a node targeted, never one more.
func PolicyTolerations(in health.Input) []corev1.Toleration {
	policy, ok := activeClusterPolicy(in.ClusterPolicies)
	if !ok {
		return nil
	}

	items, found, err := unstructured.NestedSlice(policy.Object, "spec", "daemonsets", "tolerations")
	if err != nil || !found {
		return nil
	}

	out := make([]corev1.Toleration, 0, len(items))

	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}

		toleration := corev1.Toleration{
			Key:      stringField(fields, "key"),
			Value:    stringField(fields, "value"),
			Operator: corev1.TolerationOperator(stringField(fields, "operator")),
			Effect:   corev1.TaintEffect(stringField(fields, "effect")),
		}

		// An empty operator means Equal in the Kubernetes API, and spelling it
		// out here keeps toleratedBy from having to know that too.
		if toleration.Operator == "" {
			toleration.Operator = corev1.TolerationOpEqual
		}

		out = append(out, toleration)
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

// stringField reads one string out of an unstructured map, returning an empty
// string for a field that is absent or of another type.
func stringField(fields map[string]any, name string) string {
	value, _ := fields[name].(string)

	return value
}

// OperatorTargets reports whether the GPU Operator is expected to run its
// operands on this node. The node rules are gated on it.
//
// extraTolerations are the ClusterPolicy's own, from PolicyTolerations; a
// caller with no policy to read passes nil.
//
// The gate exists because a GPU hint label is written by a DaemonSet and never
// retracted, so a node can carry nvidia.com/gpu.present=true forever while no
// operand will ever land on it. On this repository's own Mokka lane that is
// the control plane: the simulated-GPU DaemonSet tolerates every taint, so
// node feature discovery labels the control plane, and every GPU Operator
// operand is kept off it by a taint none of them tolerate (NOTES.md, "Facts
// observed"). Without this gate a healthy lane reports an error on every run.
//
// Three questions, in this order:
//
//  1. Did the operator say not to? A node whose nvidia.com/gpu.deploy.* labels
//     are all false or paused is a node the operator was told to leave alone.
//     This comes first because it is an instruction rather than an inference.
//  2. Is a deploy-gated operand already running there? That settles it:
//     whatever the taints say, the pods landed.
//  3. Would every operand be blocked by a taint? A NoSchedule or NoExecute
//     taint that none of the operand tolerations match keeps all of them off.
//
// Question 2 counts deploy-gated operands and nothing else, which is the whole
// difficulty. The pods on a control plane are the node feature discovery
// worker, which runs on every node of the cluster and is not a GPU Operator
// operand, and the gpu-operator controller itself, which is not per-node.
// Neither has a nvidia.com/gpu.deploy key and neither is evidence that an
// operand can run there. Counting any pod at all put the control plane back
// inside the inventory the node rules read, and NV-NODE-002 then reported an
// error on a healthy lane on every run.
//
// Anything else is a node the operator targets. The default is deliberately
// the permissive one: a rule that runs where it should not produces a finding
// a reader can dismiss, where a rule that does not run where it should
// produces a clean report for a broken cluster.
func OperatorTargets(node inventory.GPUNode, extraTolerations []corev1.Toleration) bool {
	if everyOperandDisabled(node.Deploy) {
		return false
	}

	if runsADeployGatedOperand(node) {
		return true
	}

	return !everyOperandTaintedOff(node.Taints, extraTolerations)
}

// deployGatedComponents is the set of operand components the operator gates on
// a nvidia.com/gpu.deploy.<key> label, built from pkg/inventory's own map so
// that the two cannot drift apart.
//
// Built once rather than per call: it is derived from a package level map that
// nothing mutates.
var deployGatedComponents = buildDeployGatedComponents()

func buildDeployGatedComponents() map[string]bool {
	out := map[string]bool{}

	for _, key := range inventory.DeployKeys() {
		if component, ok := inventory.ComponentForDeployKey(key); ok {
			out[component] = true
		}
	}

	return out
}

// runsADeployGatedOperand reports whether one of the pods on the node is an
// operand the operator deploys per node. See OperatorTargets for why anything
// else on the node does not count.
func runsADeployGatedOperand(node inventory.GPUNode) bool {
	for _, pod := range node.Operands {
		if deployGatedComponents[pod.Component] {
			return true
		}
	}

	return false
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
func everyOperandTaintedOff(taints []corev1.Taint, extra []corev1.Toleration) bool {
	for _, taint := range taints {
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}

		if !toleratedBy(taint, defaultOperandTolerations) && !toleratedBy(taint, extra) {
			return true
		}
	}

	return false
}

// toleratedBy reports whether one of the tolerations matches the taint.
//
// The match is written out rather than taken from corev1's ToleratesTaint,
// which takes a logger and would make go-logr a direct dependency of this
// module for a comparison of two strings. It handles the two operators a
// toleration of an operand DaemonSet uses and nothing else: a toleration with
// an operator this does not know tolerates nothing, so an unhandled one makes
// the gate stricter about what counts as tolerated, which produces a finding a
// reader can dismiss rather than a clean report for a broken node.
//
// An empty toleration key matches every key, which is how a cluster writes
// "tolerate everything"; it is only legal with the Exists operator, and that
// is the only combination honoured here.
func toleratedBy(taint corev1.Taint, tolerations []corev1.Toleration) bool {
	for _, toleration := range tolerations {
		// An empty toleration effect matches every effect, which the API
		// treats as "this key, whatever it does to the pod".
		if toleration.Effect != "" && toleration.Effect != taint.Effect {
			continue
		}

		if toleration.Key != "" && toleration.Key != taint.Key {
			continue
		}

		switch toleration.Operator {
		case corev1.TolerationOpExists:
			return true
		case corev1.TolerationOpEqual:
			if toleration.Key != "" && toleration.Value == taint.Value {
				return true
			}
		}
	}

	return false
}
