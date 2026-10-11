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

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// gpuNodeWithExporter is a GPU node whose operator says dcgm-exporter belongs
// on it: the starting point for every case below.
func gpuNodeWithExporter(opts ...nodeOption) health.Input {
	base := []nodeOption{
		withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
		withGPUs(4, 4),
		withDeploy(map[string]string{"dcgm-exporter": "true"}),
	}

	return health.Input{
		Nodes:             []inventory.GPUNode{buildNode("worker-1", append(base, opts...)...)},
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
		checkCommonFields(t, rule, f, inventory.ComponentDCGMExporter)

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

	// This is the difference from NV-DP-001, and it is deliberate.
	// dcgm-exporter is optional: plenty of installs switch it off in the chart
	// values, and this repository's own Mokka lane switches off the standalone
	// host engine, the container toolkit, the mig-manager and the node status
	// exporter that way. An operand nobody asked for must not produce a
	// warning, so the rule needs the label to say it belongs here. The device
	// plugin does not need that, because it is what makes a GPU node a GPU
	// node.
	t.Run("no deploy label means the exporter was never asked for", func(t *testing.T) {
		t.Parallel()

		in := gpuNodeWithExporter()
		in.Nodes[0].Deploy = nil

		wantNone(t, evaluate(rule, in))
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
