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
	"reflect"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

func TestOperatorOperandDisabled(t *testing.T) {
	t.Parallel()

	rule := rules.NewOperatorOperandDisabled()

	if rule.Code() != "NV-OP-002" {
		t.Fatalf("Code is %q, want NV-OP-002", rule.Code())
	}

	// Info, not a warning: an operand switched off is usually somebody's
	// decision. A healthy Mokka lane has to produce nothing at warning and
	// above, and this rule fires on perfectly ordinary clusters.
	if rule.Severity() != output.SeverityInfo {
		t.Errorf("Severity is %q, want info", rule.Severity())
	}

	t.Run("an operand switched off", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withDeploy(map[string]string{
			"device-plugin": "false",
			"driver":        "true",
		}))

		f := only(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))

		// The component is the verbatim app label value the operator writes on
		// the pods, not the deploy key: a report that said "device-plugin"
		// where every other finding says "nvidia-device-plugin-daemonset"
		// would be presenting two vocabularies as one.
		checkFinding(t, rule, f, inventory.ComponentDevicePlugin,
			"the operator is not running this operand on the node",
			"kubectl nv info worker-1",
		)

		checkEvidence(t, f.Evidence, map[string]string{
			"nvidia.com/gpu.deploy.device-plugin": "false",
		})
	})

	t.Run("an operand paused for an upgrade", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withDeploy(map[string]string{
			"dcgm-exporter": "paused-for-driver-upgrade",
		}))

		f := only(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
		checkFinding(t, rule, f, inventory.ComponentDCGMExporter,
			"the operator is not running this operand on the node",
			"kubectl nv info worker-1",
		)

		checkEvidence(t, f.Evidence, map[string]string{
			"nvidia.com/gpu.deploy.dcgm-exporter": "paused-for-driver-upgrade",
		})
	})

	// The operator publishes more deploy keys than pkg/inventory maps to a
	// component, and the list grows between versions. An unmapped key must
	// still produce a finding, under its own name.
	t.Run("a deploy key with no component mapped", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withDeploy(map[string]string{"kata-manager": "false"}))

		f := only(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))

		if f.Component != "kata-manager" {
			t.Errorf("Component is %q, want the deploy key verbatim", f.Component)
		}
	})

	t.Run("every operand enabled", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withDeploy(map[string]string{
			"driver":        "true",
			"device-plugin": "true",
		}))

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	// Hinted nodes count: a node with no GPUs because every operand was
	// switched off on it is exactly the case a reader is trying to explain.
	t.Run("hinted nodes are evaluated too", func(t *testing.T) {
		t.Parallel()

		node := buildNode("control-plane", withDeploy(map[string]string{"driver": "false"}), hinted())

		f := only(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))

		if f.Node != "control-plane" {
			t.Errorf("Node is %q, want control-plane", f.Node)
		}
	})

	// This rule is not gated on OperatorTargets. The gate's own first question
	// is whether every deploy label is false, so gating this rule on it would
	// silence the rule exactly where it has the most to say.
	t.Run("a node with every operand switched off still reports each one", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withDeploy(map[string]string{
			"driver":        "false",
			"device-plugin": "false",
		}))

		findings := evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}})
		if len(findings) != 2 {
			t.Fatalf("got %d findings, want one per switched-off operand: %+v", len(findings), findings)
		}
	})

	// Two runs over the same cluster have to produce the same bytes, and a Go
	// map is walked in a different order every time.
	t.Run("findings come out in deploy key order", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withDeploy(map[string]string{
			"vgpu-manager":  "false",
			"driver":        "false",
			"mig-manager":   "false",
			"device-plugin": "false",
		}))

		var components []string
		for _, f := range evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}) {
			components = append(components, f.Component)
		}

		want := []string{
			inventory.ComponentDevicePlugin,
			inventory.ComponentDriver,
			inventory.ComponentMIGManager,
			inventory.ComponentVGPUManager,
		}
		if !reflect.DeepEqual(components, want) {
			t.Errorf("components come out\n%v\nwant\n%v", components, want)
		}
	})

	t.Run("a declared range excludes an out-of-range version", func(t *testing.T) {
		t.Parallel()

		if !rule.Applies("v25.10.0") || !rule.Applies("") {
			t.Error("the rule must apply at a known in-range version and at an unknown one")
		}

		bounded := rules.NewOperatorOperandDisabled()
		bounded.MinVersion = "v99.0.0"

		if bounded.Applies("v25.10.0") {
			t.Error("a rule bounded at v99.0.0 applies at v25.10.0, want it skipped")
		}
	})
}
