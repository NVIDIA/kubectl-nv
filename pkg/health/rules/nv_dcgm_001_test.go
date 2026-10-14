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

// gpuNodeWithExporter is a GPU node whose deploy label names the exporter and
// whose ClusterPolicy is ready: the starting point for every case below.
func gpuNodeWithExporter(opts ...nodeOption) health.Input {
	base := []nodeOption{
		withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
		withGPUs(4, 4),
		withDeploy(map[string]string{"dcgm-exporter": "true"}),
	}

	return health.Input{
		Nodes:             []inventory.GPUNode{buildNode("worker-1", append(base, opts...)...)},
		ClusterPolicies:   []unstructured.Unstructured{clusterPolicy("cluster-policy", withState("ready"))},
		OperatorInstalled: true,
	}
}

func TestDCGMExporterNotReady(t *testing.T) {
	t.Parallel()

	rule := rules.NewDCGMExporterNotReady()

	if rule.Code() != "NV-DCGM-001" {
		t.Fatalf("Code is %q, want NV-DCGM-001", rule.Code())
	}

	// A warning rather than an error: a node with no dcgm-exporter still runs
	// GPU work, it just runs it unobserved.
	if rule.Severity() != output.SeverityWarning {
		t.Errorf("Severity is %q, want warning", rule.Severity())
	}

	t.Run("no exporter pod where one should run", func(t *testing.T) {
		t.Parallel()

		f := only(t, evaluate(rule, gpuNodeWithExporter()))

		checkFinding(t, rule, f, inventory.ComponentDCGMExporter,
			"the node runs no dcgm-exporter pod and the ClusterPolicy enables it",
			"kubectl nv info worker-1",
		)

		checkEvidence(t, f.Evidence, map[string]string{
			"nvidia.com/gpu.deploy.dcgm-exporter": "true",
		})
	})

	t.Run("an exporter pod that is not ready", func(t *testing.T) {
		t.Parallel()

		in := gpuNodeWithExporter(withOperandPod(inventory.OperandPod{
			Name:      "nvidia-dcgm-exporter-7h2dq",
			Namespace: "gpu-operator",
			Component: inventory.ComponentDCGMExporter,
			Phase:     string(corev1.PodFailed),
			Restarts:  3,
			Ready:     false,
		}))

		f := only(t, evaluate(rule, in))

		checkFinding(t, rule, f, inventory.ComponentDCGMExporter,
			"the dcgm-exporter pod on the node is not ready",
			"kubectl nv info worker-1",
		)

		checkEvidence(t, f.Evidence, map[string]string{
			"nvidia.com/gpu.deploy.dcgm-exporter": "true",
			"operands":                            inventory.ComponentDCGMExporter,
			"pod":                                 "nvidia-dcgm-exporter-7h2dq",
			"phase":                               "Failed",
			"ready":                               "false",
			"restarts":                            "3",
		})
	})

	t.Run("a ready exporter pod", func(t *testing.T) {
		t.Parallel()

		wantNone(t, evaluate(rule, gpuNodeWithExporter(withOperand(inventory.ComponentDCGMExporter, true))))
	})

	// The deploy label says nothing about whether the operand was asked for:
	// the operator writes it true on every node with the NVIDIA PCI label
	// whatever the chart values said. This repository's own lane switches four
	// operands off that way, so without reading the policy the rule would warn
	// on every node of a cluster that chose not to run an exporter.
	t.Run("the ClusterPolicy decides whether the operand should run", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			name      string
			policy    unstructured.Unstructured
			wantCount int
		}{
			{
				name:      "disabled in the policy",
				policy:    clusterPolicy("cluster-policy", withState("ready"), withOperandEnabled("dcgmExporter", false)),
				wantCount: 0,
			},
			{
				name:      "enabled in the policy",
				policy:    clusterPolicy("cluster-policy", withState("ready"), withOperandEnabled("dcgmExporter", true)),
				wantCount: 1,
			},
			{
				name:      "not mentioned in the policy",
				policy:    clusterPolicy("cluster-policy", withState("ready")),
				wantCount: 1,
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				in := gpuNodeWithExporter()
				in.ClusterPolicies = []unstructured.Unstructured{tc.policy}

				got := evaluate(rule, in)
				if len(got) != tc.wantCount {
					t.Errorf("got %d findings, want %d: %+v", len(got), tc.wantCount, got)
				}
			})
		}
	})

	t.Run("the exporter switched off for the node", func(t *testing.T) {
		t.Parallel()

		wantNone(t, evaluate(rule, gpuNodeWithExporter(withDeploy(map[string]string{"dcgm-exporter": "false"}))))
	})

	t.Run("the exporter paused for an upgrade", func(t *testing.T) {
		t.Parallel()

		in := gpuNodeWithExporter(withDeploy(map[string]string{"dcgm-exporter": "paused-for-driver-upgrade"}))

		wantNone(t, evaluate(rule, in))
	})

	t.Run("no operator installed", func(t *testing.T) {
		t.Parallel()

		in := gpuNodeWithExporter()
		in.OperatorInstalled = false

		wantNone(t, evaluate(rule, in))
	})

	t.Run("hinted nodes are not evaluated", func(t *testing.T) {
		t.Parallel()

		node := buildNode("control-plane",
			withDeploy(map[string]string{"dcgm-exporter": "true"}),
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

		bounded := rules.NewDCGMExporterNotReady()
		bounded.MinVersion = "v99.0.0"

		if bounded.Applies("v25.10.0") {
			t.Error("a rule bounded at v99.0.0 applies at v25.10.0, want it skipped")
		}
	})
}
