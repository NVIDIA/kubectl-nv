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

// gpuNodeWithOperator is a GPU node on a cluster that has the operator, with a
// ClusterPolicy that enables nothing in particular: the starting point for
// every device plugin case below.
//
// The deploy label is true because the operator writes it true on every node
// with the NVIDIA PCI label, whatever the ClusterPolicy enables. That is the
// point of the enablement cases further down.
func gpuNodeWithOperator(opts ...nodeOption) health.Input {
	base := []nodeOption{
		withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
		withGPUs(4, 4),
		withDeploy(map[string]string{"device-plugin": "true"}),
	}

	return health.Input{
		Nodes:             []inventory.GPUNode{buildNode("worker-1", append(base, opts...)...)},
		ClusterPolicies:   []unstructured.Unstructured{clusterPolicy("cluster-policy", withState("ready"))},
		OperatorInstalled: true,
	}
}

func TestDevicePluginNotReady(t *testing.T) {
	t.Parallel()

	rule := rules.NewDevicePluginNotReady()

	if rule.Code() != "NV-DP-001" {
		t.Fatalf("Code is %q, want NV-DP-001", rule.Code())
	}

	if rule.Severity() != output.SeverityError {
		t.Errorf("Severity is %q, want error", rule.Severity())
	}

	t.Run("no device plugin pod on a GPU node", func(t *testing.T) {
		t.Parallel()

		in := gpuNodeWithOperator(withOperand(inventory.ComponentGFD, true))

		f := only(t, evaluate(rule, in))

		checkFinding(t, rule, f, inventory.ComponentDevicePlugin,
			"the node runs no device plugin pod",
			"kubectl nv info worker-1",
		)

		checkEvidence(t, f.Evidence, map[string]string{
			"nvidia.com/gpu.deploy.device-plugin": "true",
			"operands":                            inventory.ComponentGFD,
		})
	})

	t.Run("a device plugin pod that is not ready", func(t *testing.T) {
		t.Parallel()

		in := gpuNodeWithOperator(withOperandPod(inventory.OperandPod{
			Name:      "nvidia-device-plugin-daemonset-xk2p9",
			Namespace: "gpu-operator",
			Component: inventory.ComponentDevicePlugin,
			Phase:     string(corev1.PodRunning),
			Restarts:  7,
			Ready:     false,
		}))

		f := only(t, evaluate(rule, in))

		// A different message from the missing case on purpose. "Runs no
		// device plugin pod" sends a reader looking for a pod that is right
		// there and crash-looping, which is the wrong place to start.
		checkFinding(t, rule, f, inventory.ComponentDevicePlugin,
			"the device plugin pod on the node is not ready",
			"kubectl nv info worker-1",
		)

		checkEvidence(t, f.Evidence, map[string]string{
			"nvidia.com/gpu.deploy.device-plugin": "true",
			"operands":                            inventory.ComponentDevicePlugin,
			"pod":                                 "nvidia-device-plugin-daemonset-xk2p9",
			"phase":                               "Running",
			"ready":                               "false",
			"restarts":                            "7",
		})
	})

	t.Run("a ready device plugin pod", func(t *testing.T) {
		t.Parallel()

		wantNone(t, evaluate(rule, gpuNodeWithOperator(withOperand(inventory.ComponentDevicePlugin, true))))
	})

	// The deploy label is not consent. The operator writes
	// nvidia.com/gpu.deploy.device-plugin=true on every node carrying the
	// NVIDIA PCI label whatever the ClusterPolicy enables, so the label alone
	// would make this rule fire on every cluster that turned the device plugin
	// off in its chart values.
	t.Run("the ClusterPolicy decides whether the operand should run", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name      string
			policy    unstructured.Unstructured
			wantCount int
		}{
			{
				name:      "disabled in the policy",
				policy:    clusterPolicy("cluster-policy", withState("ready"), withOperandEnabled("devicePlugin", false)),
				wantCount: 0,
			},
			{
				name:      "enabled in the policy",
				policy:    clusterPolicy("cluster-policy", withState("ready"), withOperandEnabled("devicePlugin", true)),
				wantCount: 1,
			},
			{
				// The CRD's own default is enabled, so a policy that never
				// mentions the field means the operand should run.
				name:      "not mentioned in the policy",
				policy:    clusterPolicy("cluster-policy", withState("ready")),
				wantCount: 1,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				in := gpuNodeWithOperator()
				in.ClusterPolicies = []unstructured.Unstructured{tc.policy}

				got := evaluate(rule, in)
				if len(got) != tc.wantCount {
					t.Errorf("got %d findings, want %d: %+v", len(got), tc.wantCount, got)
				}
			})
		}
	})

	// The operator writes the deploy labels, so a cluster without it has none,
	// and a device plugin installed by hand is nothing the operator promised
	// to run. Firing there would report every manually installed cluster as
	// broken.
	t.Run("no operator installed", func(t *testing.T) {
		t.Parallel()

		in := gpuNodeWithOperator()
		in.OperatorInstalled = false

		wantNone(t, evaluate(rule, in))
	})

	t.Run("the device plugin switched off for the node", func(t *testing.T) {
		t.Parallel()

		in := gpuNodeWithOperator(withDeploy(map[string]string{"device-plugin": "false"}))

		wantNone(t, evaluate(rule, in))
	})

	t.Run("the device plugin paused for an upgrade", func(t *testing.T) {
		t.Parallel()

		in := gpuNodeWithOperator(withDeploy(map[string]string{"device-plugin": "paused-for-driver-upgrade"}))

		wantNone(t, evaluate(rule, in))
	})

	// A node that advertises its GPUs through DRA publishes a ResourceSlice
	// and runs no device plugin at all. It is the kubelet plugin that makes
	// its GPUs allocatable, so a missing device plugin there is the design.
	t.Run("a DRA node runs no device plugin", func(t *testing.T) {
		t.Parallel()

		wantNone(t, evaluate(rule, gpuNodeWithOperator(withMode(inventory.ModeDRA))))
	})

	// A node mid-migration runs both, so the device plugin still has to be
	// there.
	t.Run("a node running both modes still needs the device plugin", func(t *testing.T) {
		t.Parallel()

		only(t, evaluate(rule, gpuNodeWithOperator(withMode(inventory.ModeBoth))))
	})

	// Hinted nodes have no GPUs to advertise, so a missing device plugin there
	// is NV-NODE-002's finding rather than this one's.
	t.Run("hinted nodes are not evaluated", func(t *testing.T) {
		t.Parallel()

		node := buildNode("control-plane",
			withDeploy(map[string]string{"device-plugin": "true"}),
			hinted(),
		)

		wantNone(t, evaluate(rule, health.Input{
			Hinted:            []inventory.GPUNode{node},
			OperatorInstalled: true,
		}))
	})

	t.Run("a declared range excludes an out-of-range version", func(t *testing.T) {
		t.Parallel()

		if !rule.Applies("v25.10.0") || !rule.Applies("") {
			t.Error("the rule must apply at a known in-range version and at an unknown one")
		}

		bounded := rules.NewDevicePluginNotReady()
		bounded.MinVersion = "v99.0.0"

		if bounded.Applies("v25.10.0") {
			t.Error("a rule bounded at v99.0.0 applies at v25.10.0, want it skipped")
		}
	})
}
