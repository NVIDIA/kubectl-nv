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

// NodePCIWithoutGPULabel is NV-NODE-001: node feature discovery found an
// NVIDIA PCI device on the node and GPU feature discovery never labelled it.
//
// The two labels come from different places, which is what makes the pair
// meaningful. feature.node.kubernetes.io/pci-10de.present is written by node
// feature discovery, which is not a GPU Operator operand and runs on any node;
// nvidia.com/gpu.present is written by GPU feature discovery, which is one. So
// the first without the second says the hardware is there and the operator has
// not reached it.
type NodePCIWithoutGPULabel struct{ versionRange }

// NewNodePCIWithoutGPULabel returns the rule. Both version bounds are empty:
// the two labels predate every operator version this plugin supports.
func NewNodePCIWithoutGPULabel() *NodePCIWithoutGPULabel { return &NodePCIWithoutGPULabel{} }

// Code implements health.Rule.
func (r *NodePCIWithoutGPULabel) Code() string { return "NV-NODE-001" }

// Severity implements health.Rule. A warning rather than an error: the node
// may be one nobody meant to run GPU work on, and the operator may simply not
// have got there yet.
func (r *NodePCIWithoutGPULabel) Severity() output.Severity { return output.SeverityWarning }

// Evaluate implements health.Rule.
func (r *NodePCIWithoutGPULabel) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	// Both sets: a GPU node missing the GPU label is a node whose feature
	// discovery stopped while its device plugin kept going, which is worth the
	// same finding.
	for _, set := range [][]inventory.GPUNode{in.Nodes, in.Hinted} {
		for _, node := range set {
			if !labelIsTrue(node, LabelPCIVendor) || labelIsTrue(node, LabelGPUPresent) {
				continue
			}

			if !OperatorTargets(node) {
				continue
			}

			evidence := map[string]string{LabelPCIVendor: labelValueTrue}
			setIf(evidence, "operands", operandComponents(node))

			findings = append(findings, output.Finding{
				Code:      r.Code(),
				Severity:  r.Severity(),
				Node:      node.Name,
				Component: ComponentNode,
				Message:   "the node has an NVIDIA PCI device and no " + LabelGPUPresent + " label",
				Evidence:  evidence,
				Next:      infoCommand(node.Name),
			})
		}
	}

	return findings
}
