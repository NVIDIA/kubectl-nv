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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// componentNFDWorker stands in for the node feature discovery worker pod.
//
// NFD is not a GPU Operator operand: it has no nvidia.com/gpu.deploy key, and
// it runs on every node of the cluster including the control plane. The exact
// label value pkg/inventory reads off such a pod is the inventory builder's
// business; what every test here depends on is only that it is outside the
// set of deploy-gated operand components, which is what the operator-targets
// gate counts.
const componentNFDWorker = "node-feature-discovery-worker"

// nodeOption is one fact about a synthetic node. The rule tests are tables of
// these, so that a case reads as the cluster state it is about rather than as
// twenty lines of struct literal.
type nodeOption func(*inventory.GPUNode)

// buildNode returns a node through the constructor, which is mandatory rather
// than a convenience: the cli-runtime printers read TypeMeta off the object
// before they marshal anything, so a literal is unprintable at -o json.
func buildNode(name string, opts ...nodeOption) inventory.GPUNode {
	node := inventory.NewGPUNode(name)
	for _, opt := range opts {
		opt(node)
	}

	return *node
}

func withLabels(labels map[string]string) nodeOption {
	return func(n *inventory.GPUNode) {
		if n.Labels == nil {
			n.Labels = map[string]string{}
		}

		for k, v := range labels {
			n.Labels[k] = v
		}
	}
}

func withTaints(taints ...corev1.Taint) nodeOption {
	return func(n *inventory.GPUNode) { n.Taints = append(n.Taints, taints...) }
}

// withDeploy sets the nvidia.com/gpu.deploy.<key> labels, keyed by the suffix
// alone, which is how inventory.GPUNode.Deploy is keyed.
func withDeploy(deploy map[string]string) nodeOption {
	return func(n *inventory.GPUNode) {
		if n.Deploy == nil {
			n.Deploy = map[string]string{}
		}

		for k, v := range deploy {
			n.Deploy[k] = v
		}
	}
}

// withEveryOperandDeployed writes nvidia.com/gpu.deploy.<key>=true for every
// key pkg/inventory maps, which is what the operator does to every node
// carrying the NVIDIA PCI label, the control plane included. The labels are
// not a statement that the operands will run there.
func withEveryOperandDeployed() nodeOption {
	return func(n *inventory.GPUNode) {
		if n.Deploy == nil {
			n.Deploy = map[string]string{}
		}

		for _, key := range inventory.DeployKeys() {
			n.Deploy[key] = "true"
		}
	}
}

// withOperand puts one operand pod on the node. The component is the verbatim
// "app" label value, so callers pass an inventory.Component* constant.
func withOperand(component string, ready bool) nodeOption {
	return func(n *inventory.GPUNode) {
		phase := string(corev1.PodRunning)
		if !ready {
			phase = string(corev1.PodPending)
		}

		n.Operands = append(n.Operands, inventory.OperandPod{
			Name:      component + "-abcde",
			Namespace: "gpu-operator",
			Component: component,
			Phase:     phase,
			Ready:     ready,
		})
	}
}

// withOperandPod puts an operand pod on the node with its phase spelled out,
// for the cases where "not ready" is not the same as "pending".
func withOperandPod(pod inventory.OperandPod) nodeOption {
	return func(n *inventory.GPUNode) { n.Operands = append(n.Operands, pod) }
}

func hinted() nodeOption {
	return func(n *inventory.GPUNode) { n.Hinted = true }
}

// withGPUs sets the whole-GPU capacity and allocatable, which is what makes a
// node a GPU node everywhere except under the mixed MIG strategy.
func withGPUs(capacity, allocatable int64) nodeOption {
	return func(n *inventory.GPUNode) {
		n.Capacity, n.Allocatable = capacity, allocatable
		n.Mode = inventory.ModeDevicePlugin
	}
}

// withCount sets nvidia.com/gpu.count, what GPU feature discovery found.
func withCount(count int) nodeOption {
	return func(n *inventory.GPUNode) { n.Count = count }
}

func withSharing(strategy string, replicas int) nodeOption {
	return func(n *inventory.GPUNode) {
		n.Sharing = inventory.Sharing{Strategy: strategy, Replicas: replicas}
	}
}

// withMixedMIG puts the node in the mixed MIG strategy, where it advertises
// one extended resource per profile and no nvidia.com/gpu at all.
func withMixedMIG(resources map[string]inventory.ResourceCount) nodeOption {
	return func(n *inventory.GPUNode) {
		n.MIG.Strategy = "mixed"
		n.MIG.Capable = true
		n.MIG.Resources = resources
	}
}

func withMode(mode string) nodeOption {
	return func(n *inventory.GPUNode) { n.Mode = mode }
}

// withWorkloadConfig sets nvidia.com/gpu.workload.config, which says what kind
// of workload the node's GPUs are set up for: container, vm-passthrough or
// vm-vgpu.
func withWorkloadConfig(value string) nodeOption {
	return func(n *inventory.GPUNode) {
		withLabels(map[string]string{rules.LabelWorkloadConfig: value})(n)
	}
}

// controlPlaneTaint is the taint no GPU Operator operand DaemonSet tolerates
// by default, and so the one that keeps the operands off a control plane.
func controlPlaneTaint() corev1.Taint {
	return corev1.Taint{
		Key:    "node-role.kubernetes.io/control-plane",
		Effect: corev1.TaintEffectNoSchedule,
	}
}

// mokkaControlPlane is the node the node rules must stay quiet about, built
// from what this repository's lane actually produces rather than from what is
// convenient to assert:
//
//   - the simulated-GPU DaemonSet tolerates every taint, so node feature
//     discovery labels the control plane with the NVIDIA PCI label and the
//     operator's feature discovery labels it nvidia.com/gpu.present=true;
//   - the operator writes nvidia.com/gpu.deploy.<key>=true on every node with
//     the PCI label, this one included, so the deploy labels say nothing about
//     whether an operand will run;
//   - no allocatable GPUs, and none coming;
//   - the pods that do run there are the NFD worker and the gpu-operator
//     controller itself, neither of which is a deploy-gated operand;
//   - the control-plane taint, which no operand DaemonSet tolerates.
//
// Every one of those five facts has to be here. A fixture that left the deploy
// labels or the pods out would pass against a gate that only looked at taints,
// and the gate that shipped first did exactly that.
func mokkaControlPlane() inventory.GPUNode {
	return buildNode("kubectl-nv-control-plane",
		withLabels(map[string]string{
			rules.LabelGPUPresent: "true",
			rules.LabelPCIVendor:  "true",
		}),
		withEveryOperandDeployed(),
		withTaints(controlPlaneTaint()),
		withOperand(componentNFDWorker, true),
		withOperand(inventory.ComponentOperator, true),
		hinted(),
	)
}

// mokkaWorker is a healthy t4 worker of the same lane: four simulated GPUs,
// all allocatable, with the operands the lane's chart values enable.
func mokkaWorker(name string) inventory.GPUNode {
	return buildNode(name,
		withLabels(map[string]string{
			rules.LabelGPUPresent: "true",
			rules.LabelPCIVendor:  "true",
		}),
		withEveryOperandDeployed(),
		withCount(4),
		withGPUs(4, 4),
		withOperand(componentNFDWorker, true),
		withOperand(inventory.ComponentDevicePlugin, true),
		withOperand(inventory.ComponentGFD, true),
		withOperand(inventory.ComponentDCGMExporter, true),
		withOperand(inventory.ComponentOperatorValidator, true),
	)
}

// healthyLane is the whole of a healthy Mokka lane: the control plane above
// and two workers, with a ready ClusterPolicy and the operator installed.
func healthyLane() health.Input {
	return health.Input{
		Nodes:             []inventory.GPUNode{mokkaWorker("kubectl-nv-worker"), mokkaWorker("kubectl-nv-worker2")},
		Hinted:            []inventory.GPUNode{mokkaControlPlane()},
		ClusterPolicies:   []unstructured.Unstructured{clusterPolicy("cluster-policy", withState("ready"))},
		OperatorInstalled: true,
		OperatorVersion:   "v26.3.3",
	}
}

// policyOption is one fact about a synthetic ClusterPolicy.
type policyOption func(obj map[string]any)

// clusterPolicy builds an operator ClusterPolicy. With no options it has no
// status and an empty spec, which is what an object the operator has not
// reconciled yet looks like.
func clusterPolicy(name string, opts ...policyOption) unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "nvidia.com/v1",
		"kind":       "ClusterPolicy",
		"metadata":   map[string]any{"name": name},
	}

	for _, opt := range opts {
		opt(obj)
	}

	return unstructured.Unstructured{Object: obj}
}

func withState(state string) policyOption {
	return func(obj map[string]any) {
		obj["status"] = map[string]any{"state": state}
	}
}

// withOperandEnabled sets spec.<field>.enabled, the switch the chart values
// write and the operator reconciles against. Leaving it out means enabled,
// which is the CRD's own default.
func withOperandEnabled(field string, enabled bool) policyOption {
	return func(obj map[string]any) {
		spec, _ := obj["spec"].(map[string]any)
		if spec == nil {
			spec = map[string]any{}
			obj["spec"] = spec
		}

		operand, _ := spec[field].(map[string]any)
		if operand == nil {
			operand = map[string]any{}
			spec[field] = operand
		}

		operand["enabled"] = enabled
	}
}

// withDaemonSetTolerations sets spec.daemonsets.tolerations, which the
// operator puts on every operand DaemonSet in place of their defaults.
func withDaemonSetTolerations(tolerations ...corev1.Toleration) policyOption {
	return func(obj map[string]any) {
		spec, _ := obj["spec"].(map[string]any)
		if spec == nil {
			spec = map[string]any{}
			obj["spec"] = spec
		}

		items := make([]any, 0, len(tolerations))

		for _, toleration := range tolerations {
			item := map[string]any{"key": toleration.Key, "operator": string(toleration.Operator)}
			if toleration.Effect != "" {
				item["effect"] = string(toleration.Effect)
			}

			if toleration.Value != "" {
				item["value"] = toleration.Value
			}

			items = append(items, item)
		}

		spec["daemonsets"] = map[string]any{"tolerations": items}
	}
}

// evaluate runs one rule over an input and returns what it found, so that a
// table case is one line rather than four.
func evaluate(rule health.Rule, in health.Input) []output.Finding {
	return rule.Evaluate(in)
}

// only returns the single finding, failing the test when there is not exactly
// one. A rule that fired twice and a rule that fired once are different
// answers, and an assertion that reads findings[0] hides the difference.
func only(t *testing.T, findings []output.Finding) output.Finding {
	t.Helper()

	if len(findings) != 1 {
		t.Fatalf("got %d findings, want exactly 1: %+v", len(findings), findings)
	}

	return findings[0]
}

// wantNone fails the test when the rule found anything.
func wantNone(t *testing.T, findings []output.Finding) {
	t.Helper()

	if len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// checkEvidence asserts the exact evidence map. Evidence is what proves a
// finding, so a test that only checked the keys would pass on a rule that
// reported the wrong numbers.
func checkEvidence(t *testing.T, got map[string]string, want map[string]string) {
	t.Helper()

	for key, value := range want {
		actual, ok := got[key]
		if !ok {
			t.Errorf("evidence has no %q: %v", key, got)

			continue
		}

		if actual != value {
			t.Errorf("evidence[%q] = %q, want %q", key, actual, value)
		}
	}

	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("evidence carries an unexpected %q = %q", key, got[key])
		}
	}
}

// checkFinding asserts everything a finding has to carry: its rule's code, a
// severity, the component, the exact message and the exact next command.
//
// The message and the command are compared exactly rather than for emptiness.
// A rule that said "the node runs no device plugin pod" where the pod is there
// and crash-looping sends the reader to the wrong place, and a check that only
// asked whether the string was non-empty would not notice.
func checkFinding(t *testing.T, rule health.Rule, f output.Finding, component, message, next string) {
	t.Helper()

	if f.Code != rule.Code() {
		t.Errorf("Code is %q, want %q", f.Code, rule.Code())
	}

	if f.Severity == "" {
		t.Error("Severity is empty")
	}

	if f.Component != component {
		t.Errorf("Component is %q, want %q", f.Component, component)
	}

	if f.Message != message {
		t.Errorf("Message is\n  %q\nwant\n  %q", f.Message, message)
	}

	if f.Next != next {
		t.Errorf("Next is %q, want %q", f.Next, next)
	}

	if len(f.Evidence) == 0 {
		t.Error("Evidence is empty: nothing proves this finding")
	}
}
