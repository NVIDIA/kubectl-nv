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

func TestNodeCountDiffersFromCapacity(t *testing.T) {
	t.Parallel()

	rule := rules.NewNodeCountDiffersFromCapacity()

	if rule.Code() != "NV-NODE-004" {
		t.Fatalf("Code is %q, want NV-NODE-004", rule.Code())
	}

	if rule.Severity() != output.SeverityWarning {
		t.Errorf("Severity is %q, want warning", rule.Severity())
	}

	t.Run("feature discovery and the device plugin disagree", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withCount(8), withGPUs(4, 4))

		f := only(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))

		checkFinding(t, rule, f, rules.ComponentNode,
			"the "+rules.LabelGPUCount+" label and the node's GPU capacity disagree",
			"kubectl nv info worker-1",
		)

		checkEvidence(t, f.Evidence, map[string]string{
			rules.LabelGPUCount: "8",
			"capacity":          "4",
			"expectedCapacity":  "8",
		})
	})

	t.Run("they agree", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withCount(4), withGPUs(4, 4))

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	// Time slicing makes the capacity a multiple of the physical count on
	// purpose. A rule that compared the two numbers directly would fire on
	// every node of every time-sliced cluster, which is the fastest way to
	// make a check nobody reads.
	t.Run("time slicing multiplies the capacity legitimately", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withCount(4),
			withGPUs(16, 16),
			withSharing("time-slicing", 4),
		)

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	t.Run("a shared node whose capacity is still wrong", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withCount(4),
			withGPUs(12, 12),
			withSharing("time-slicing", 4),
		)

		f := only(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))

		checkEvidence(t, f.Evidence, map[string]string{
			rules.LabelGPUCount:        "4",
			"capacity":                 "12",
			"expectedCapacity":         "16",
			rules.LabelGPUReplicas:     "4",
			rules.LabelSharingStrategy: "time-slicing",
		})
	})

	// Zero means "GPU feature discovery has not run", which is NV-NODE-001's
	// finding. Reading it as "this node has no GPUs" would make every node
	// without the label report a mismatch.
	t.Run("a node with no count label", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withGPUs(4, 4))

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	// A node advertising its GPUs through dynamic resource allocation alone
	// has no nvidia.com/gpu capacity to compare the count against, so the
	// comparison would report every healthy DRA node in the cluster.
	t.Run("mode decides whether the capacity is comparable", func(t *testing.T) {
		t.Parallel()

		for _, tc := range []struct {
			mode      string
			wantCount int
		}{
			{mode: inventory.ModeDRA, wantCount: 0},
			{mode: inventory.ModeBoth, wantCount: 1},
			{mode: inventory.ModeNone, wantCount: 1},
		} {
			t.Run(tc.mode, func(t *testing.T) {
				t.Parallel()

				node := buildNode("worker-1", withCount(8), withGPUs(4, 4), withMode(tc.mode))

				got := evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}})
				if len(got) != tc.wantCount {
					t.Errorf("mode %q produced %d findings, want %d: %+v", tc.mode, len(got), tc.wantCount, got)
				}
			})
		}
	})

	// A mixed-strategy MIG node advertises one resource per profile and no
	// nvidia.com/gpu, so its capacity is legitimately zero while the count
	// label still reports whole devices.
	t.Run("a mixed-strategy MIG node", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1",
			withCount(4),
			withMixedMIG(map[string]inventory.ResourceCount{
				"nvidia.com/mig-1g.10gb": {Capacity: 28, Allocatable: 28},
			}),
		)

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	t.Run("hinted nodes are not evaluated", func(t *testing.T) {
		t.Parallel()

		node := buildNode("worker-1", withCount(4), hinted())

		wantNone(t, evaluate(rule, health.Input{Hinted: []inventory.GPUNode{node}}))
	})

	t.Run("the Mokka control plane is left alone", func(t *testing.T) {
		t.Parallel()

		node := mokkaControlPlane()
		node.Count, node.Capacity, node.Allocatable, node.Hinted = 8, 4, 4, false
		node.Mode = inventory.ModeDevicePlugin

		wantNone(t, evaluate(rule, health.Input{Nodes: []inventory.GPUNode{node}}))
	})

	t.Run("a declared range excludes an out-of-range version", func(t *testing.T) {
		t.Parallel()

		if !rule.Applies("v25.10.0") || !rule.Applies("") {
			t.Error("the rule must apply at a known in-range version and at an unknown one")
		}

		bounded := rules.NewNodeCountDiffersFromCapacity()
		bounded.MinVersion = "v99.0.0"

		if bounded.Applies("v25.10.0") {
			t.Error("a rule bounded at v99.0.0 applies at v25.10.0, want it skipped")
		}
	})
}
