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

package rules

import (
	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// NodeAllocatableBelowCapacity is NV-NODE-003: some of the node's GPUs are
// present and not placeable.
//
// The gap between capacity and allocatable is the kubelet having taken devices
// out of the pool, usually because the device plugin reported them unhealthy,
// or because a reservation holds some back.
//
// Allocatable zero belongs to NV-NODE-002, so the two rules partition the
// space and a node with no placeable GPUs produces one finding rather than
// two. Allocatable above capacity is nonsense rather than a shortfall, and
// reporting a negative count back would be reporting nonsense.
//
// The rule reads the whole-GPU numbers only. A node running MIG in the mixed
// strategy advertises one extended resource per profile and no nvidia.com/gpu,
// so its per-profile shortfall is not covered here; the catalog entry says so.
type NodeAllocatableBelowCapacity struct{ versionRange }

// NewNodeAllocatableBelowCapacity returns the rule. Both version bounds are
// empty: node capacity and allocatable predate every operator version this
// plugin supports.
func NewNodeAllocatableBelowCapacity() *NodeAllocatableBelowCapacity {
	return &NodeAllocatableBelowCapacity{}
}

// Code implements health.Rule.
func (r *NodeAllocatableBelowCapacity) Code() string { return "NV-NODE-003" }

// Severity implements health.Rule. A warning: the node still places work, on
// fewer devices than it has.
func (r *NodeAllocatableBelowCapacity) Severity() output.Severity { return output.SeverityWarning }

// Evaluate implements health.Rule.
//
// Only the GPU nodes are walked. A hinted node has no capacity by definition,
// so there is nothing there to be short of.
func (r *NodeAllocatableBelowCapacity) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	tolerations := PolicyTolerations(in)

	for _, node := range in.Nodes {
		if node.Allocatable <= 0 || node.Allocatable >= node.Capacity {
			continue
		}

		if !OperatorTargets(node, tolerations) {
			continue
		}

		findings = append(findings, output.Finding{
			Code:      r.Code(),
			Severity:  r.Severity(),
			Node:      node.Name,
			Component: ComponentNode,
			Message:   "the node has fewer allocatable GPUs than its capacity",
			Evidence: map[string]string{
				"capacity":    itoa(node.Capacity),
				"allocatable": itoa(node.Allocatable),
				"missing":     itoa(node.Capacity - node.Allocatable),
			},
			Next: infoCommand(node.Name),
		})
	}

	return findings
}
