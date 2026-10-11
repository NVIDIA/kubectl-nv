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
	"strings"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// TopologyLabelsPartial is NV-TOPO-001: some GPU nodes carry Topograph labels
// that others are missing.
//
// A node half the fabric knows about is the worst case for a topology aware
// scheduler: it is placeable, so work lands on it, and it is outside the
// groups the placement was computed over, so the work runs across whatever
// link happens to be there. Nothing else in a report shows it.
//
// The partial set is pkg/topology's, computed by the same code that builds
// `kubectl nv topology`'s report rather than by a second definition here. That
// matters for two decisions a second implementation would be free to make
// differently:
//
//   - A clique is not a Topograph label. A fleet of GB200 nodes with cliques
//     beside H100 nodes without them is an ordinary mixed cluster, and
//     counting the clique would report every such cluster as broken.
//   - A node carrying none of the labels is outside the fabric rather than
//     half inside it. Saying it is missing every label would bury the nodes
//     that are missing one, which are the nodes this rule exists for.
//
// Building the whole topology report to read one field of it is a few hundred
// microseconds over a cluster's worth of nodes, and it is what keeps the two
// answers the same answer.
type TopologyLabelsPartial struct{ versionRange }

// NewTopologyLabelsPartial returns the rule. Both version bounds are empty:
// Topograph is not a GPU Operator component, so no operator version decides
// whether its labels mean anything.
func NewTopologyLabelsPartial() *TopologyLabelsPartial { return &TopologyLabelsPartial{} }

// Code implements health.Rule.
func (r *TopologyLabelsPartial) Code() string { return "NV-TOPO-001" }

// Severity implements health.Rule. A warning: the node works, and placement
// against it is being computed from an incomplete picture.
func (r *TopologyLabelsPartial) Severity() output.Severity { return output.SeverityWarning }

// Evaluate implements health.Rule.
func (r *TopologyLabelsPartial) Evaluate(in health.Input) []output.Finding {
	partial := topology.Build(topology.Input{Nodes: in.Nodes}).PartialNodes

	findings := make([]output.Finding, 0, len(partial))

	for i := range partial {
		node := &partial[i]

		findings = append(findings, output.Finding{
			Code:      r.Code(),
			Severity:  r.Severity(),
			Node:      node.Name,
			Component: ComponentNode,
			Message:   "the node is missing Topograph labels that other GPU nodes carry",
			Evidence: map[string]string{
				"present": strings.Join(node.Present, ","),
				"missing": strings.Join(node.Missing, ","),
			},
			Next: infoCommand(node.Name),
		})
	}

	if len(findings) == 0 {
		return nil
	}

	return findings
}
