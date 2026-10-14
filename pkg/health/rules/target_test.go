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

// OperatorTargets is the gate that keeps kubectl nv health off nodes the GPU Operator
// was never going to touch, and getting it wrong in either direction is a way
// to make the whole command useless: too permissive and a healthy lane reports
// an error on every run, too strict and a broken node reports clean.
func TestOperatorTargets(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		node inventory.GPUNode
		want bool
	}{
		{
			name: "a plain worker with no taints",
			node: buildNode("worker-1"),
			want: true,
		},
		{
			name: "the Mokka control plane",
			node: mokkaControlPlane(),
			want: false,
		},
		// The question the gate asks about pods is "is a deploy-gated operand
		// running here", not "is any pod running here". NFD runs on every node
		// of the cluster and the operator's own controller runs on one of
		// them, and neither has a nvidia.com/gpu.deploy key. Counting them put
		// the control plane back inside the inventory the node rules read.
		{
			name: "a control plane running NFD and the operator pod",
			node: buildNode("control-plane",
				withTaints(controlPlaneTaint()),
				withOperand(componentNFDWorker, true),
				withOperand(inventory.ComponentOperator, true),
			),
			want: false,
		},
		{
			name: "a control plane that really does run a deploy-gated operand",
			node: buildNode("control-plane",
				withTaints(controlPlaneTaint()),
				withOperand(inventory.ComponentDevicePlugin, true),
			),
			want: true,
		},
		{
			name: "a taint the operand DaemonSets do tolerate",
			node: buildNode("worker-1", withTaints(corev1.Taint{
				Key:    "nvidia.com/gpu",
				Value:  "present",
				Effect: corev1.TaintEffectNoSchedule,
			})),
			want: true,
		},
		// The default toleration is for NoSchedule alone. A NoExecute taint on
		// the same key evicts the operands, so the effect has to be part of the
		// match: a gate that compared keys only would call this node targeted
		// and then report nothing about a node whose operands were all thrown
		// off it.
		{
			name: "the same key at NoExecute is not tolerated",
			node: buildNode("worker-1", withTaints(corev1.Taint{
				Key:    "nvidia.com/gpu",
				Effect: corev1.TaintEffectNoExecute,
			})),
			want: false,
		},
		{
			name: "a node the kubelet tainted as not ready",
			node: buildNode("worker-1", withTaints(corev1.Taint{
				Key:    "node.kubernetes.io/not-ready",
				Effect: corev1.TaintEffectNoExecute,
			})),
			// The DaemonSet controller adds this toleration to every daemon
			// pod, so a node going NotReady does not stop being a node the
			// operator targets. Excluding it would silence every rule on the
			// nodes most likely to be broken.
			want: true,
		},
		{
			name: "a PreferNoSchedule taint blocks nothing",
			node: buildNode("worker-1", withTaints(corev1.Taint{
				Key:    "example.com/spot",
				Effect: corev1.TaintEffectPreferNoSchedule,
			})),
			want: true,
		},
		{
			name: "every operand switched off by label",
			node: buildNode("worker-1", withDeploy(map[string]string{
				"driver":        "false",
				"device-plugin": "false",
				"dcgm-exporter": "paused-for-driver-upgrade",
			})),
			want: false,
		},
		{
			name: "one operand switched off is not all of them",
			node: buildNode("worker-1", withDeploy(map[string]string{
				"driver":        "true",
				"device-plugin": "false",
			})),
			want: true,
		},
		{
			name: "an untolerated NoExecute taint",
			node: buildNode("worker-1", withTaints(corev1.Taint{
				Key:    "example.com/dedicated",
				Value:  "storage",
				Effect: corev1.TaintEffectNoExecute,
			})),
			want: false,
		},
		{
			name: "one untolerated taint among tolerated ones is enough",
			node: buildNode("worker-1", withTaints(
				corev1.Taint{Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule},
				corev1.Taint{Key: "example.com/dedicated", Effect: corev1.TaintEffectNoSchedule},
			)),
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := rules.OperatorTargets(tc.node, nil); got != tc.want {
				t.Errorf("OperatorTargets(%s) = %v, want %v", tc.node.Name, got, tc.want)
			}
		})
	}
}

// The operator replaces every operand DaemonSet's tolerations with
// spec.daemonsets.tolerations when a ClusterPolicy sets them, so a cluster
// that put its GPU nodes behind a custom taint and told the operator about it
// has nodes the operator does target. Reading only the defaults would report
// nothing at all about every node of such a cluster.
func TestOperatorTargetsReadsTheClusterPolicyTolerations(t *testing.T) {
	t.Parallel()

	taint := corev1.Taint{
		Key:    "example.com/gpu-only",
		Value:  "yes",
		Effect: corev1.TaintEffectNoSchedule,
	}
	node := buildNode("worker-1", withTaints(taint))

	if rules.OperatorTargets(node, nil) {
		t.Fatal("the default tolerations tolerate example.com/gpu-only, want them not to")
	}

	policy := clusterPolicy("cluster-policy",
		withState("ready"),
		withDaemonSetTolerations(corev1.Toleration{
			Key:      taint.Key,
			Operator: corev1.TolerationOpExists,
			Effect:   corev1.TaintEffectNoSchedule,
		}),
	)

	tolerations := rules.PolicyTolerations(health.Input{
		ClusterPolicies: []unstructured.Unstructured{policy},
	})

	if len(tolerations) != 1 {
		t.Fatalf("PolicyTolerations returned %d tolerations, want 1: %+v", len(tolerations), tolerations)
	}

	if !rules.OperatorTargets(node, tolerations) {
		t.Error("a node whose taint the ClusterPolicy tolerates is not targeted, want it targeted")
	}
}

// A policy that sets no tolerations must not wipe the defaults out, and a
// policy with a toleration of a shape this code cannot read must not either.
func TestPolicyTolerationsOnAPolicyWithNone(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		input health.Input
	}{
		{name: "no policies at all", input: health.Input{}},
		{
			name: "a policy with no daemonsets stanza",
			input: health.Input{ClusterPolicies: []unstructured.Unstructured{
				clusterPolicy("cluster-policy", withState("ready")),
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := rules.PolicyTolerations(tc.input); len(got) != 0 {
				t.Errorf("PolicyTolerations returned %+v, want nothing", got)
			}

			// The defaults still have to hold.
			node := buildNode("worker-1", withTaints(corev1.Taint{
				Key: "nvidia.com/gpu", Effect: corev1.TaintEffectNoSchedule,
			}))
			if !rules.OperatorTargets(node, rules.PolicyTolerations(tc.input)) {
				t.Error("the default tolerations stopped applying")
			}
		})
	}
}

// The whole point of the gate, stated as the thing a user sees: a healthy lane
// reports nothing worth acting on.
//
// This runs every rule over the lane as it really is, control plane included,
// rather than one rule over one node. A gate can be right about the node a
// unit test hands it and still leave the command crying wolf, and that is
// exactly what shipped first.
func TestAHealthyLaneReportsNothingAtWarningOrAbove(t *testing.T) {
	t.Parallel()

	in := healthyLane()

	var loud []output.Finding

	for _, rule := range rules.All() {
		if !rule.Applies(in.OperatorVersion) {
			continue
		}

		for _, f := range rule.Evaluate(in) {
			if f.Severity == output.SeverityError || f.Severity == output.SeverityWarning {
				loud = append(loud, f)
			}
		}
	}

	if len(loud) != 0 {
		t.Errorf("a healthy lane reported %d findings at warning or above:\n%+v", len(loud), loud)
	}
}

// The deploy labels say what the operator was told to do with each operand.
// "paused-for-driver-upgrade" is not a value anybody guessed: it is the shape
// the operator writes while an upgrade is in flight, and reading it as
// "deployed" would make every rule fire mid-upgrade.
func TestDeployDisabled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		value string
		want  bool
	}{
		{"true", false},
		{"", false},
		{"false", true},
		{"paused-for-driver-upgrade", true},
		{"paused-for-mig-change", true},
		// Only the documented values count. An unknown value is not a claim
		// that the operand is off, and treating it as one would silence the
		// rules on a cluster whose operator writes something new.
		{"TRUE", false},
		{"pause", false},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Parallel()

			if got := rules.DeployDisabled(tc.value); got != tc.want {
				t.Errorf("DeployDisabled(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}
