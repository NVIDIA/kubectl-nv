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

package topology

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
)

// The node labels this package groups by. The clique is the GPU operator's,
// the fabric and accelerator labels are Topograph's, and the hostname is
// Kubernetes' own.
const (
	// LabelClique is the NVLink or IMEX clique a node's GPUs belong to.
	LabelClique = "nvidia.com/gpu.clique"
	// LabelSubDomain is the accelerator sub-domain, an optional division
	// inside an accelerator domain, so it is the narrower of the two.
	LabelSubDomain = "accelerator.topograph.run/sub-domain"
	// LabelDomain is the accelerator domain, the NVLink domain a node is in.
	LabelDomain = "accelerator.topograph.run/domain"
	// LabelTierPrefix prefixes one label per fabric tier, the number of the
	// tier following it: fabric.topograph.run/tier-0, tier-1 and so on. Tier
	// 0 is the one closest to the node and the numbers increase outwards.
	LabelTierPrefix = "fabric.topograph.run/tier-"
	// LabelHostname is the node's own name as a label. It is the node level,
	// and the label a Kueue Topology names when it declares one.
	LabelHostname = "kubernetes.io/hostname"
)

// Where a level came from. It is in the output because these have different
// owners, and a reader looking at a level that is wrong needs to know which
// component to go and ask.
const (
	// SourceNode is the implicit level of one node.
	SourceNode = "node"
	// SourceClique is the GPU operator's clique label.
	SourceClique = "clique"
	// SourceSubDomain is Topograph's accelerator sub-domain.
	SourceSubDomain = "accelerator-sub-domain"
	// SourceDomain is Topograph's accelerator domain.
	SourceDomain = "accelerator-domain"
	// SourceTier is one of Topograph's fabric tiers.
	SourceTier = "tier"
	// SourceKueue is a level declared by a Kueue Topology object.
	SourceKueue = "kueue"
	// SourceCluster is the implicit level of the whole cluster.
	SourceCluster = "cluster"
)

// The names of the two implicit levels.
const (
	// LevelNode is the narrowest level there is: one node.
	LevelNode = "node"
	// LevelCluster is the widest: every GPU node the run saw.
	LevelCluster = "cluster"
)

// topographLabels reports whether a label is one Topograph writes. It is what
// decides whether a node counts as partially labelled, which is NV-TOPO-001's
// question and is asked about Topograph's labels alone.
func isTopographLabel(label string) bool {
	return label == LabelDomain ||
		label == LabelSubDomain ||
		strings.HasPrefix(label, LabelTierPrefix)
}

// Group is the set of nodes that share one value of one level's label, with
// what they have between them.
type Group struct {
	// Value is the label value the nodes share, for instance "clique-a".
	Value string `json:"value" yaml:"value"`
	// Nodes are the names of the nodes in the group, sorted.
	Nodes []string `json:"nodes" yaml:"nodes"`
	// NodeCount is how many there are, so that the table does not make a
	// reader count a list only the wide output prints.
	NodeCount int `json:"nodeCount" yaml:"nodeCount"`
	// Allocatable is the nvidia.com/gpu allocatable summed over the group.
	Allocatable int64 `json:"allocatable" yaml:"allocatable"`
	// Requested is what the pods already placed on those nodes asked for.
	Requested int64 `json:"requested" yaml:"requested"`
	// Free is what is left: allocatable minus requested on each node, never
	// below zero, summed. It is 0 while RequestedKnown is false.
	Free int64 `json:"free" yaml:"free"`
	// LargestFreeNode is the most free GPUs on any one node of the group,
	// which is the largest single pod the group can take.
	LargestFreeNode int64 `json:"largestFreeNode" yaml:"largestFreeNode"`
	// RequestedKnown is whether every node in the group had its requested
	// GPUs read. It is false when any did not, and Requested, Free and
	// LargestFreeNode are then 0 and mean nothing: a group reported as
	// entirely free because nobody listed the pods is worse than a group
	// reported as unknown.
	RequestedKnown bool `json:"requestedKnown" yaml:"requestedKnown"`
}

// groupJSON breaks the method set so that MarshalJSON does not recurse.
type groupJSON Group

// MarshalJSON renders a nil node list as an empty array.
func (g Group) MarshalJSON() ([]byte, error) {
	out := groupJSON(g)
	if out.Nodes == nil {
		out.Nodes = []string{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling group %q: %w", g.Value, err)
	}

	return raw, nil
}

// Level is one way of dividing the cluster: one node label, and the groups
// the nodes fall into by its value.
type Level struct {
	// Name is how the level is printed: "clique", "tier-1",
	// "accelerator-domain", or, for a Kueue level, the node label itself.
	// A Kueue level keeps the whole label because the last segment of one is
	// not unique across topologies, and two Topology objects disagreeing
	// about a level called "block" is worse than a long name.
	Name string `json:"name" yaml:"name"`
	// Label is the node label the level reads.
	Label string `json:"label" yaml:"label"`
	// Source is where the level came from: one of SourceClique, SourceTier,
	// SourceDomain or SourceKueue.
	Source string `json:"source" yaml:"source"`
	// Groups are the groups of the level, sorted by value.
	Groups []Group `json:"groups" yaml:"groups"`
}

// levelJSON breaks the method set so that MarshalJSON does not recurse.
type levelJSON Level

// MarshalJSON renders a nil group list as an empty array.
func (l Level) MarshalJSON() ([]byte, error) {
	out := levelJSON(l)
	if out.Groups == nil {
		out.Groups = []Group{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling level %q: %w", l.Name, err)
	}

	return raw, nil
}

// PartialNode is a node carrying some of the cluster's topology labels and
// not the rest. It is the input to NV-TOPO-001: a node half the fabric knows
// about is a node work will be placed on and then run slowly on, and nothing
// else in this output would show it.
type PartialNode struct {
	// Name is the node name.
	Name string `json:"name" yaml:"name"`
	// Present are the topology labels it carries, sorted.
	Present []string `json:"present" yaml:"present"`
	// Missing are the ones the rest of the cluster uses and it does not,
	// sorted.
	Missing []string `json:"missing" yaml:"missing"`
}

// partialNodeJSON breaks the method set so that MarshalJSON does not recurse.
type partialNodeJSON PartialNode

// MarshalJSON renders the nil label lists as empty arrays.
func (p PartialNode) MarshalJSON() ([]byte, error) {
	out := partialNodeJSON(p)
	if out.Present == nil {
		out.Present = []string{}
	}
	if out.Missing == nil {
		out.Missing = []string{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling partial node %q: %w", p.Name, err)
	}

	return raw, nil
}

// KueueStatus says whether the Kueue levels in a report are the cluster's or
// whether there were none to have.
type KueueStatus struct {
	// Installed is whether the cluster served the Kueue topology API.
	Installed bool `json:"installed" yaml:"installed"`
	// Reason says why there are no Kueue levels when there are none: the API
	// is not served, the read was denied, or no ResourceFlavor references a
	// Topology. It is empty when the levels are there.
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// KueueTopology is a Kueue Topology object reduced to what this package
// groups by: its name, and the node labels of its levels in the order the
// object declares them, which runs widest to narrowest.
type KueueTopology struct {
	// Name is the object name, which is what a ResourceFlavor references.
	Name string `json:"name" yaml:"name"`
	// Levels are the node labels, in the declared order.
	Levels []string `json:"levels" yaml:"levels"`
}

// Input is everything Build groups: the GPU nodes, and whatever the cluster
// said about Kueue.
type Input struct {
	// Nodes are the GPU nodes, as pkg/inventory models them.
	Nodes []inventory.GPUNode
	// Kueue is whether the Kueue topology API answered, and why not when it
	// did not.
	Kueue KueueStatus
	// Topologies are the Kueue topologies whose levels belong in the report,
	// in the order they are to be reported.
	Topologies []KueueTopology
}

// levelKey names one level before its groups are counted.
type levelKey struct {
	name   string
	label  string
	source string
	// tier is the number of a Topograph tier, and orders the tiers among
	// themselves.
	tier int
}

// Build groups the nodes by every level the cluster has, narrowest first, and
// names the nodes that carry some of the Topograph labels and not the rest.
func Build(in Input) *TopologyReport {
	report := NewTopologyReport()
	report.Kueue = in.Kueue

	keys := levelsOf(in)
	report.Levels = make([]Level, 0, len(keys))
	for _, key := range keys {
		report.Levels = append(report.Levels, Level{
			Name:   key.name,
			Label:  key.label,
			Source: key.source,
			Groups: key.group(in.Nodes),
		})
	}

	report.PartialNodes = partialNodes(in.Nodes)

	return report
}

// group returns the groups of one level.
func (k levelKey) group(nodes []inventory.GPUNode) []Group {
	switch k.source {
	case SourceNode:
		return groupsPerNode(nodes)
	case SourceCluster:
		return groupsWholeCluster(nodes)
	default:
		return groupsAt(nodes, k.label)
	}
}

// levelsOf collects the levels of a cluster, narrowest first. The order is
// the contract --for is read through, so it is written out here in full:
//
//  1. the node, implicit, one group per node;
//  2. nvidia.com/gpu.clique;
//  3. accelerator.topograph.run/sub-domain;
//  4. accelerator.topograph.run/domain;
//  5. fabric.topograph.run/tier-0 upwards, by number;
//  6. the Kueue levels, from the last declared to the first;
//  7. the cluster, implicit, one group.
//
// Three of those need saying out loud.
//
// The NVLink hierarchy comes before the network one. A clique, a sub-domain
// and a domain are NVLink groupings, and a fabric tier is a switch level;
// Topograph describes them as separate hierarchies, and the NVLink side is
// the tighter interconnect, so a job that fits inside it belongs there rather
// than spread across a switch. A sub-domain is a division inside a domain, so
// it is the narrower of those two.
//
// The Kueue levels are walked backwards because a Topology declares them
// widest first. The CRD refuses any list whose last entry is not
// kubernetes.io/hostname, calling that "the lowest level of topology", and
// the upstream sample declares block, rack, hostname. Reading them forwards
// answers every --for with the widest placement that fits.
//
// The node and the cluster are always there, whatever the cluster is
// labelled with. Without them, a job that fits on one node has no level to be
// answered with, and a cluster with no topology labels at all has no levels
// and no answer.
//
// A label nobody carries is left out: an empty level is a row of nothing in
// the table and a group list nothing can be placed in. A Kueue level naming a
// label one of the levels above already reads is left out too, and the report
// then names that level rather than carrying it twice: a Topology that
// declares kubernetes.io/hostname is declaring the node level, and one that
// declares nvidia.com/gpu.clique is declaring the clique.
func levelsOf(in Input) []levelKey {
	present := labelsInUse(in.Nodes)
	has := func(label string) bool {
		_, found := present[label]
		return found
	}

	// The node level reads the hostname label so that a Kueue level naming it
	// is recognised as an alias, but it groups by the node's name, which
	// every node has whether or not the label is set.
	keys := []levelKey{{name: LevelNode, label: LabelHostname, source: SourceNode}}

	for _, fixed := range []struct {
		name   string
		label  string
		source string
	}{
		{name: "clique", label: LabelClique, source: SourceClique},
		{name: "accelerator-sub-domain", label: LabelSubDomain, source: SourceSubDomain},
		{name: "accelerator-domain", label: LabelDomain, source: SourceDomain},
	} {
		if has(fixed.label) {
			keys = append(keys, levelKey{name: fixed.name, label: fixed.label, source: fixed.source})
		}
	}

	keys = append(keys, tierLevels(present)...)

	seen := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		seen[key.label] = struct{}{}
	}

	for _, topo := range in.Topologies {
		// Backwards: the last level a Topology declares is its narrowest.
		for i := len(topo.Levels) - 1; i >= 0; i-- {
			label := topo.Levels[i]
			if _, done := seen[label]; done {
				continue
			}
			// A Kueue level names an ordinary node label, which lives in the
			// node's own labels rather than in the topology fields, so it is
			// the nodes that are asked and not the topology label set.
			if !anyNodeHas(in.Nodes, label) {
				continue
			}
			seen[label] = struct{}{}
			keys = append(keys, levelKey{name: label, label: label, source: SourceKueue})
		}
	}

	return append(keys, levelKey{name: LevelCluster, source: SourceCluster})
}

// tierLevels returns the fabric tier levels in use, ordered by their number.
func tierLevels(present map[string]struct{}) []levelKey {
	tiers := make([]levelKey, 0, len(present))
	for label := range present {
		if !strings.HasPrefix(label, LabelTierPrefix) {
			continue
		}
		// A tier whose suffix is not a number cannot be placed among the
		// numbered ones, and the far end is the safe end: sorting it as tier
		// 0 would make it the narrowest level in the report and the first
		// answer --for gives.
		number, err := strconv.Atoi(strings.TrimPrefix(label, LabelTierPrefix))
		if err != nil {
			number = math.MaxInt
		}
		tiers = append(tiers, levelKey{
			name:   strings.TrimPrefix(label, "fabric.topograph.run/"),
			label:  label,
			source: SourceTier,
			tier:   number,
		})
	}

	// By number, and then by label so that two unnumbered ones still have an
	// order. A string sort over the labels alone puts tier-10 before tier-2,
	// which reverses two fabric levels.
	sort.Slice(tiers, func(i, j int) bool {
		if tiers[i].tier != tiers[j].tier {
			return tiers[i].tier < tiers[j].tier
		}

		return tiers[i].label < tiers[j].label
	})

	return tiers
}

// labelsInUse collects the label keys at least one node carries, out of the
// two sources a fixed level can come from: the clique field and the Topograph
// labels kept in Topology.
func labelsInUse(nodes []inventory.GPUNode) map[string]struct{} {
	out := make(map[string]struct{}, 4)
	for i := range nodes {
		for label := range labelsOfNode(&nodes[i]) {
			out[label] = struct{}{}
		}
	}

	return out
}

// anyNodeHas reports whether at least one node carries a value for label.
func anyNodeHas(nodes []inventory.GPUNode, label string) bool {
	for i := range nodes {
		if valueAt(&nodes[i], label) != "" {
			return true
		}
	}

	return false
}

// labelsOfNode returns the topology labels of one node, keyed by label.
//
// The clique and the Topograph labels are read from the fields pkg/inventory
// split them into rather than from the raw labels, because those fields are
// the contract: a builder that renames a label has to change them, and a
// reader that went back to the raw labels would quietly disagree with the
// rest of the output.
func labelsOfNode(n *inventory.GPUNode) map[string]string {
	out := make(map[string]string, len(n.Topology)+1)
	if n.Clique != "" {
		out[LabelClique] = n.Clique
	}
	for label, value := range n.Topology {
		if value != "" {
			out[label] = value
		}
	}

	return out
}

// valueAt reads one level's value off a node. A Kueue level names an
// arbitrary node label, which is in the node's own labels rather than in the
// topology fields.
func valueAt(n *inventory.GPUNode, label string) string {
	if value, found := labelsOfNode(n)[label]; found {
		return value
	}

	return n.Labels[label]
}

// groupsPerNode is the implicit node level: one group per node, keyed by its
// name, so that a job of one pod can be answered with the node it fits on.
// It groups by the name rather than by the hostname label because a node
// without that label is still a node.
func groupsPerNode(nodes []inventory.GPUNode) []Group {
	groups := make([]Group, 0, len(nodes))
	for i := range nodes {
		groups = append(groups, groupOf(nodes[i].Name, []*inventory.GPUNode{&nodes[i]}))
	}

	sort.Slice(groups, func(i, j int) bool { return groups[i].Value < groups[j].Value })

	return groups
}

// groupsWholeCluster is the implicit cluster level: every GPU node the run
// saw, in one group. A job that fits nowhere narrower still has to be told
// whether the cluster as a whole can take it.
func groupsWholeCluster(nodes []inventory.GPUNode) []Group {
	if len(nodes) == 0 {
		return []Group{}
	}

	all := make([]*inventory.GPUNode, 0, len(nodes))
	for i := range nodes {
		all = append(all, &nodes[i])
	}

	return []Group{groupOf(LevelCluster, all)}
}

// groupsAt groups the nodes by their value of one label, sorted by value,
// leaving out the nodes that do not carry it.
func groupsAt(nodes []inventory.GPUNode, label string) []Group {
	byValue := map[string][]*inventory.GPUNode{}
	for i := range nodes {
		value := valueAt(&nodes[i], label)
		if value == "" {
			continue
		}
		byValue[value] = append(byValue[value], &nodes[i])
	}

	values := make([]string, 0, len(byValue))
	for value := range byValue {
		values = append(values, value)
	}
	sort.Strings(values)

	groups := make([]Group, 0, len(values))
	for _, value := range values {
		groups = append(groups, groupOf(value, byValue[value]))
	}

	return groups
}

// groupOf sums one group.
//
// Free is the sum of the per-node free counts, each clamped at zero, and not
// the group's allocatable minus its requested. The two differ exactly when a
// node reports more requested than allocatable, which is what a node whose
// allocatable dropped under the pods already on it looks like, and the
// difference matters: that node has nothing to offer, and subtracting its
// overdraft from its neighbours would report a group as fuller than it is.
func groupOf(value string, nodes []*inventory.GPUNode) Group {
	g := Group{Value: value, NodeCount: len(nodes), RequestedKnown: true}

	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
		g.Allocatable += n.Allocatable
		if !n.RequestedKnown {
			g.RequestedKnown = false
			continue
		}
		g.Requested += n.Requested

		free := freeOn(n)
		g.Free += free
		if free > g.LargestFreeNode {
			g.LargestFreeNode = free
		}
	}
	sort.Strings(names)
	g.Nodes = names

	// A group is only as known as its least known node. Half a sum is a
	// number a reader would take for the whole one.
	if !g.RequestedKnown {
		g.Requested = 0
		g.Free = 0
		g.LargestFreeNode = 0
	}

	return g
}

// freeOn is the GPUs one node has left, clamped at zero, and zero while
// nothing is known about what runs on it.
func freeOn(n *inventory.GPUNode) int64 {
	if !n.RequestedKnown {
		return 0
	}

	free := n.Allocatable - n.Requested
	if free < 0 {
		return 0
	}

	return free
}

// partialNodes names the nodes carrying some of the cluster's Topograph
// labels and not the rest.
//
// The set a node is measured against is the Topograph labels in use anywhere
// in the cluster: the fabric tiers, the accelerator domain and its
// sub-domain. That is NV-TOPO-001's question, and it is asked about those
// labels alone. A clique is not one of them, and neither is a Kueue level: a
// fleet of GB200 nodes with cliques beside H100 nodes without them is an
// ordinary mixed cluster, not one where half the nodes lost a label, and
// counting the clique would report every such cluster as broken.
//
// A node carrying none of them is not partial either: it is a node outside
// the fabric entirely, and saying it is missing every label would bury the
// nodes that are missing one.
func partialNodes(nodes []inventory.GPUNode) []PartialNode {
	universe := make([]string, 0, 4)
	for label := range labelsInUse(nodes) {
		if isTopographLabel(label) {
			universe = append(universe, label)
		}
	}
	sort.Strings(universe)

	if len(universe) == 0 {
		return []PartialNode{}
	}

	out := make([]PartialNode, 0, len(nodes))
	for i := range nodes {
		var present, missing []string
		for _, label := range universe {
			if valueAt(&nodes[i], label) != "" {
				present = append(present, label)
				continue
			}
			missing = append(missing, label)
		}

		if len(present) == 0 || len(missing) == 0 {
			continue
		}

		out = append(out, PartialNode{Name: nodes[i].Name, Present: present, Missing: missing})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out
}
