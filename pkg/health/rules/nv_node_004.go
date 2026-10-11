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
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// NodeCountDiffersFromCapacity is NV-NODE-004: two NVIDIA components counting
// the same devices disagree.
//
// nvidia.com/gpu.count is what GPU feature discovery found on the node; the
// nvidia.com/gpu capacity is what the device plugin advertised to the kubelet.
// When they disagree one of them is working from stale or partial
// information, and the scheduler is believing the capacity.
//
// Three things are accounted for before it fires, and the first is the one
// that would otherwise make this rule useless:
//
//   - Sharing. Time slicing and MPS make the capacity a multiple of the
//     physical count on purpose, so the expected capacity is the count times
//     nvidia.com/gpu.replicas. A rule that compared the two numbers directly
//     would fire on every node of every time-sliced cluster.
//   - Mixed-strategy MIG, whose capacity is legitimately zero because the node
//     advertises one extended resource per profile instead.
//   - A missing count label. Zero there means GPU feature discovery has not
//     run, which is NV-NODE-001's finding, not a disagreement with anything.
type NodeCountDiffersFromCapacity struct{ versionRange }

// NewNodeCountDiffersFromCapacity returns the rule. Both version bounds are
// empty: the count label and the replicas label predate every operator version
// this plugin supports.
func NewNodeCountDiffersFromCapacity() *NodeCountDiffersFromCapacity {
	return &NodeCountDiffersFromCapacity{}
}

// Code implements health.Rule.
func (r *NodeCountDiffersFromCapacity) Code() string { return "NV-NODE-004" }

// Severity implements health.Rule. A warning: the node still works, and one of
// the two numbers a reader relies on is wrong.
func (r *NodeCountDiffersFromCapacity) Severity() output.Severity { return output.SeverityWarning }

// Evaluate implements health.Rule.
func (r *NodeCountDiffersFromCapacity) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	tolerations := PolicyTolerations(in)

	for _, node := range in.Nodes {
		if node.Count <= 0 || isMixedMIG(node) {
			continue
		}

		// A node advertising its GPUs through dynamic resource allocation
		// alone has no nvidia.com/gpu capacity for the count label to
		// disagree with, so comparing the two would report every healthy DRA
		// node in the cluster.
		if node.Mode == inventory.ModeDRA {
			continue
		}

		if !OperatorTargets(node, tolerations) {
			continue
		}

		expected := int64(node.Count) * int64(replicaFactor(node))
		if expected == node.Capacity {
			continue
		}

		evidence := map[string]string{
			LabelGPUCount:      itoa(node.Count),
			"capacity":         itoa(node.Capacity),
			"expectedCapacity": itoa(expected),
		}

		// Only reported where sharing is what makes the expected capacity
		// differ from the count. On a node that shares nothing these keys
		// would be two more numbers to read past.
		if replicaFactor(node) > 1 {
			evidence[LabelGPUReplicas] = itoa(node.Sharing.Replicas)
			setIf(evidence, LabelSharingStrategy, node.Sharing.Strategy)
		}

		findings = append(findings, output.Finding{
			Code:      r.Code(),
			Severity:  r.Severity(),
			Node:      node.Name,
			Component: ComponentNode,
			Message:   "the " + LabelGPUCount + " label and the node's GPU capacity disagree",
			Evidence:  evidence,
			Next:      infoCommand(node.Name),
		})
	}

	return findings
}

// replicaFactor is how many times each physical device is advertised. It is 1
// on a node that shares nothing, which is also what an absent or nonsensical
// nvidia.com/gpu.replicas label reads as: a factor below one would make the
// expected capacity smaller than the device count, which no sharing strategy
// does.
func replicaFactor(node inventory.GPUNode) int {
	if node.Sharing.Replicas < 1 {
		return 1
	}

	return node.Sharing.Replicas
}
