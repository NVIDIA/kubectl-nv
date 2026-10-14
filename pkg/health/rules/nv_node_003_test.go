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

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

func TestNodeAllocatableBelowCapacity(t *testing.T) {
	t.Parallel()

	rule := rules.NewNodeAllocatableBelowCapacity()

	if rule.Code() != "NV-NODE-003" {
		t.Fatalf("Code is %q, want NV-NODE-003", rule.Code())
	}

	if rule.Severity() != output.SeverityWarning {
		t.Errorf("Severity is %q, want warning", rule.Severity())
	}

	t.Run("some devices are not placeable", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withGPUs(8, 6))

		f := only(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))

		checkFinding(t, rule, f, rules.ComponentNode,
			"the node has fewer allocatable GPUs than its capacity",
			"kubectl nv info worker-1",
		)

		checkEvidence(t, f.Evidence, map[string]string{
			"capacity":    "8",
			"allocatable": "6",
			"missing":     "2",
		})
	})

	t.Run("a node with every device placeable", func(t *testing.T) {
		t.Parallel()

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{buildNode("worker-1", withGPUs(8, 8))}}))
	})

	// NV-NODE-002 owns the allocatable-zero case, so the two partition the
	// space and a node with no placeable GPUs produces one finding, not two.
	t.Run("allocatable zero belongs to NV-NODE-002", func(t *testing.T) {
		t.Parallel()

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{buildNode("worker-1", withGPUs(8, 0))}}))
	})

	// Allocatable above capacity is nonsense rather than a shortfall, and
	// reporting "-2 missing" would be reporting nonsense back.
	t.Run("allocatable above capacity is not a shortfall", func(t *testing.T) {
		t.Parallel()

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{buildNode("worker-1", withGPUs(4, 6))}}))
	})

	// A hinted node has no capacity by definition, so there is nothing here to
	// be short of.
	t.Run("hinted nodes are not evaluated", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", hinted())

		wantNone(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
	})

	t.Run("the Mokka control plane is left alone", func(t *testing.T) {
		t.Parallel()

		node := mokkaControlPlane()
		node.Capacity, node.Allocatable = 8, 6

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	t.Run("a declared range excludes an out-of-range version", func(t *testing.T) {
		t.Parallel()

		if !rule.Applies("v25.10.0") || !rule.Applies("") {
			t.Error("the rule must apply at a known in-range version and at an unknown one")
		}

		bounded := rules.NewNodeAllocatableBelowCapacity()
		bounded.MinVersion = "v99.0.0"

		if bounded.Applies("v25.10.0") {
			t.Error("a rule bounded at v99.0.0 applies at v25.10.0, want it skipped")
		}
	})
}
