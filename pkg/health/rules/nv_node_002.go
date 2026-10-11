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

// NodeNoAllocatableGPUs is NV-NODE-002: the node says it has GPUs and the
// scheduler cannot place one on it.
//
// This is the finding gpu health exists for. It covers two shapes of the same
// problem, told apart by the hinted evidence key: a node that advertises no
// nvidia.com/gpu capacity at all, and one that advertises capacity the kubelet
// offers none of.
//
// Two exclusions keep it from crying wolf, and both are load bearing:
//
//   - OperatorTargets. A GPU hint label is written by a DaemonSet and never
//     retracted, so a node can carry nvidia.com/gpu.present=true forever while
//     no operand will ever land on it. On the Mokka lane that node is the
//     control plane, and without this gate a healthy lane reports an error on
//     every run (NOTES.md, "Facts observed").
//   - Mixed-strategy MIG. Such a node advertises one extended resource per
//     profile and no nvidia.com/gpu at all, so zero whole-GPU allocatable is
//     what a healthy one looks like. The exclusion is about having allocatable
//     slices rather than about being a MIG node, so a MIG node whose profiles
//     are all unplaceable is still reported.
type NodeNoAllocatableGPUs struct{ versionRange }

// NewNodeNoAllocatableGPUs returns the rule. Both version bounds are empty:
// node allocatable and the gpu.present label predate every operator version
// this plugin supports.
func NewNodeNoAllocatableGPUs() *NodeNoAllocatableGPUs { return &NodeNoAllocatableGPUs{} }

// Code implements health.Rule.
func (r *NodeNoAllocatableGPUs) Code() string { return "NV-NODE-002" }

// Severity implements health.Rule. An error: a node nothing can be scheduled
// on is broken, whatever its labels say.
func (r *NodeNoAllocatableGPUs) Severity() output.Severity { return output.SeverityError }

// Evaluate implements health.Rule.
func (r *NodeNoAllocatableGPUs) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	for _, set := range [][]inventory.GPUNode{in.Nodes, in.Hinted} {
		for _, node := range set {
			if !labelIsTrue(node, LabelGPUPresent) || node.Allocatable > 0 {
				continue
			}

			if !OperatorTargets(node) {
				continue
			}

			mig := migAllocatable(node)
			if isMixedMIG(node) && mig > 0 {
				continue
			}

			evidence := map[string]string{
				LabelGPUPresent: labelValueTrue,
				"capacity":      itoa(node.Capacity),
				"allocatable":   itoa(node.Allocatable),
				"hinted":        btoa(node.Hinted),
			}

			// Only on a MIG node, where it is the number that proves the node
			// is not healthy-but-partitioned.
			if isMixedMIG(node) {
				evidence["mig.allocatable"] = itoa(mig)
			}

			setIf(evidence, "operands", operandComponents(node))

			findings = append(findings, output.Finding{
				Code:      r.Code(),
				Severity:  r.Severity(),
				Node:      node.Name,
				Component: ComponentNode,
				Message:   "the node reports GPUs present and advertises none the scheduler can place",
				Evidence:  evidence,
				Next:      infoCommand(node.Name),
			})
		}
	}

	return findings
}
