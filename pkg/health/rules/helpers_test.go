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
	"sort"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

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

// controlPlaneTaint is the taint no GPU Operator operand DaemonSet tolerates,
// and so the one that keeps the operator off the Mokka lane's control plane.
func controlPlaneTaint() corev1.Taint {
	return corev1.Taint{
		Key:    "node-role.kubernetes.io/control-plane",
		Effect: corev1.TaintEffectNoSchedule,
	}
}

// mokkaControlPlane is the node the node rules must stay quiet about: the
// control plane of this repository's own Mokka lane, labelled by a DaemonSet
// that tolerates every taint, with no allocatable GPUs and no operand that
// will ever land on it.
func mokkaControlPlane() inventory.GPUNode {
	return buildNode("kubectl-nv-control-plane",
		withLabels(map[string]string{
			"nvidia.com/gpu.present":                      "true",
			"feature.node.kubernetes.io/pci-10de.present": "true",
		}),
		withTaints(controlPlaneTaint()),
		hinted(),
	)
}

// clusterPolicy builds an operator ClusterPolicy with the given status.state.
// An empty state leaves the field out, which is what an object the operator
// has not reconciled yet looks like.
func clusterPolicy(name, state string) unstructured.Unstructured {
	obj := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nvidia.com/v1",
		"kind":       "ClusterPolicy",
		"metadata":   map[string]any{"name": name},
	}}

	if state != "" {
		obj.Object["status"] = map[string]any{"state": state}
	}

	return obj
}

// evaluate runs one rule over an input and returns what it found, so that a
// table case is one line rather than four.
func evaluate(rule health.Rule, in health.Input) []output.Finding {
	return rule.Evaluate(in)
}

// nodesFor reduces findings to the nodes they name, sorted, which is what most
// of the rule assertions are actually about.
func nodesFor(findings []output.Finding) []string {
	out := make([]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Node)
	}

	sort.Strings(out)

	return out
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

// checkCommonFields asserts what every finding of every rule has to carry: its
// own code, a severity, a component and a next command. A finding with no next
// command hands the reader a worse version of the problem they already had.
func checkCommonFields(t *testing.T, rule health.Rule, f output.Finding, component string) {
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

	if f.Message == "" {
		t.Error("Message is empty")
	}

	if f.Next == "" {
		t.Error("Next is empty: a finding with no next command is a worse version of the problem")
	}

	if len(f.Evidence) == 0 {
		t.Error("Evidence is empty: nothing proves this finding")
	}
}
