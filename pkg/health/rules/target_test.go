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

	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
)

// OperatorTargets is the gate that keeps gpu health off nodes the GPU Operator
// was never going to touch.
//
// The case it exists for is on the Mokka lane and is recorded in NOTES.md: the
// simulated-GPU DaemonSet tolerates every taint, so the control plane carries
// nvidia.com/gpu.present=true and pci-10de.present=true and has no allocatable
// GPUs forever. Every operand DaemonSet is blocked there by a taint none of
// them tolerate, so NV-NODE-002 firing on it would mean a healthy lane never
// reports clean, and a check that cries wolf on every cluster is a check
// nobody reads.
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
			name: "the Mokka control plane: hinted, tainted, no operands",
			node: buildNode("control-plane",
				withLabels(map[string]string{
					"nvidia.com/gpu.present":                      "true",
					"feature.node.kubernetes.io/pci-10de.present": "true",
				}),
				withTaints(corev1.Taint{
					Key:    "node-role.kubernetes.io/control-plane",
					Effect: corev1.TaintEffectNoSchedule,
				}),
				hinted(),
			),
			want: false,
		},
		{
			name: "a control plane that does run the operands anyway",
			node: buildNode("control-plane",
				withTaints(corev1.Taint{
					Key:    "node-role.kubernetes.io/control-plane",
					Effect: corev1.TaintEffectNoSchedule,
				}),
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

			if got := rules.OperatorTargets(tc.node); got != tc.want {
				t.Errorf("OperatorTargets(%s) = %v, want %v", tc.node.Name, got, tc.want)
			}
		})
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
