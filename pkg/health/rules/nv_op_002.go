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
	"sort"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// OperatorOperandDisabled is NV-OP-002: an operand the operator was told not
// to run on this node, through nvidia.com/gpu.deploy.<operand>=false or
// =paused-for-something.
//
// It reports at info because an operand switched off is usually somebody's
// decision rather than a fault. What it is for is the question underneath most
// of the other findings: a reader looking at a node with no GPUs wants to know
// whether the device plugin failed or was never asked to start, and this is
// the finding that answers it.
//
// It is deliberately not gated on OperatorTargets. That gate's first question
// is whether every deploy label is false, so gating this rule on it would
// silence the rule exactly where it has the most to say.
type OperatorOperandDisabled struct{ versionRange }

// NewOperatorOperandDisabled returns the rule. Both version bounds are empty:
// the deploy labels predate every operator version this plugin supports, and
// the rule reads whatever keys it finds rather than a fixed list.
func NewOperatorOperandDisabled() *OperatorOperandDisabled { return &OperatorOperandDisabled{} }

// Code implements health.Rule.
func (r *OperatorOperandDisabled) Code() string { return "NV-OP-002" }

// Severity implements health.Rule.
func (r *OperatorOperandDisabled) Severity() output.Severity { return output.SeverityInfo }

// Evaluate implements health.Rule.
func (r *OperatorOperandDisabled) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	for _, set := range [][]inventory.GPUNode{in.Nodes, in.Hinted} {
		for _, node := range set {
			findings = append(findings, r.evaluateNode(node)...)
		}
	}

	return findings
}

// evaluateNode reports the disabled operands of one node, in deploy key order.
// Sorted because a Go map is walked in a different order every time, and two
// runs over the same cluster have to produce the same bytes.
func (r *OperatorOperandDisabled) evaluateNode(node inventory.GPUNode) []output.Finding {
	keys := make([]string, 0, len(node.Deploy))
	for key := range node.Deploy {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	var findings []output.Finding

	for _, key := range keys {
		value := node.Deploy[key]
		if !DeployDisabled(value) {
			continue
		}

		// The operator publishes more deploy keys than pkg/inventory maps, and
		// the list grows between versions. An unmapped key is reported under
		// its own name rather than dropped: an operand nobody can name is an
		// operand nobody will go and look at.
		component, ok := inventory.ComponentForDeployKey(key)
		if !ok {
			component = key
		}

		findings = append(findings, output.Finding{
			Code:      r.Code(),
			Severity:  r.Severity(),
			Node:      node.Name,
			Component: component,
			Message:   "the operator is not running this operand on the node",
			Evidence:  map[string]string{inventory.DeployLabel(key): value},
			Next:      infoCommand(node.Name),
		})
	}

	return findings
}
