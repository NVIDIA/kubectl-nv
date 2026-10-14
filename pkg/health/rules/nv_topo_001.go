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
	"strings"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// TopologyLabelsPartial is NV-TOPO-001: Topograph's fabric tiers are on some
// GPU nodes and not on others.
//
// A node the fabric only half knows about is the worst case for a topology
// aware scheduler: it is placeable, so work lands on it, and it is outside the
// groups the placement was computed over, so the work runs across whatever
// link happens to be there. Nothing else in a report shows it.
//
// Two decisions define the rule, and both of them are what separates a useful
// finding from noise:
//
//   - Fabric tiers only, fabric.topograph.run/tier-<N>. The accelerator domain
//     and sub-domain are optional per provider, so a fleet whose provider
//     publishes tiers and no domain is complete, not partial, and counting the
//     domain would report every such cluster. A clique is not a Topograph
//     label at all: a fleet of GB200 nodes with cliques beside H100 nodes
//     without them is an ordinary mixed cluster.
//   - A node carrying none of the tiers its peers carry is reported. That is
//     what "present on some GPU nodes and absent on others" means, and it is
//     the common shape: Topograph reaches a fleet and misses a node, and the
//     node it missed has no tier labels whatsoever rather than some of them.
//
// The second is where this parts company with pkg/topology's PartialNodes,
// which skips a node carrying none of the labels. That is right for the
// topology report, where such a node is shown as being outside the fabric and
// a reader can see it; it is wrong here, where being left out means nothing is
// reported at all. The two therefore keep their own definitions on purpose,
// and this one has its own helper rather than reusing that one.
type TopologyLabelsPartial struct{ versionRange }

// NewTopologyLabelsPartial returns the rule. Both version bounds are empty:
// Topograph is not a GPU Operator component, so no operator version decides
// whether its labels mean anything.
func NewTopologyLabelsPartial() *TopologyLabelsPartial { return &TopologyLabelsPartial{} }

// Code implements health.Rule.
func (r *TopologyLabelsPartial) Code() string { return "NV-TOPO-001" }

// Severity implements health.Rule. Informational until the Phase 2
// false-positive baseline says what a real fleet looks like: the node works,
// and placement against it is being computed from an incomplete picture.
func (r *TopologyLabelsPartial) Severity() output.Severity { return output.SeverityInfo }

// Evaluate implements health.Rule.
//
// Only the GPU nodes are walked. The question is which nodes the fabric
// describes, and a node that advertises no GPUs is not one work is placed on.
func (r *TopologyLabelsPartial) Evaluate(in health.Input) []output.Finding {
	tiers := tiersInUse(in.Nodes)
	if len(tiers) == 0 {
		return nil
	}

	var findings []output.Finding

	for i := range in.Nodes {
		node := &in.Nodes[i]

		var present, missing []string

		for _, tier := range tiers {
			if node.Topology[tier] != "" {
				present = append(present, tier)

				continue
			}

			missing = append(missing, tier)
		}

		if len(missing) == 0 {
			continue
		}

		evidence := map[string]string{"missing": strings.Join(missing, ",")}

		// A node carrying none of them has nothing to list, and an absent key
		// is the encoding for that rather than an empty string.
		setIf(evidence, "present", strings.Join(present, ","))

		findings = append(findings, output.Finding{
			Code:      r.Code(),
			Severity:  r.Severity(),
			Node:      node.Name,
			Component: ComponentNode,
			Message:   "the node is missing Topograph fabric tiers that other GPU nodes carry",
			Evidence:  evidence,
			Next:      infoCommand(node.Name),
		})
	}

	return findings
}

// tiersInUse is every fabric.topograph.run/tier-<N> label any GPU node
// carries, sorted.
//
// Sorted by name rather than by tier number, which is enough here: the result
// is a set to compare nodes against and a sorted, comma joined evidence value,
// not an ordering of the fabric. pkg/topology is where the tiers are ordered
// numerically, because that is where the order means something.
func tiersInUse(nodes []inventory.GPUNode) []string {
	seen := map[string]bool{}

	for i := range nodes {
		for label, value := range nodes[i].Topology {
			if value != "" && strings.HasPrefix(label, inventory.TopologyFabricPrefix) {
				seen[label] = true
			}
		}
	}

	out := make([]string, 0, len(seen))
	for label := range seen {
		out = append(out, label)
	}

	sort.Strings(out)

	return out
}
