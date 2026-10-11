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

package topology_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// node builds one GPU node for the tests. The labels are split the way
// pkg/inventory's builder splits them: the clique into its own field, the
// Topograph labels into Topology, and everything else into the node's labels,
// which is where a Kueue level is read from.
//
// The node is schedulable and Ready, because that is the ordinary case and
// the tests that care about the other one say so.
func node(name string, allocatable, requested int64, labels map[string]string) inventory.GPUNode {
	n := inventory.NewGPUNode(name)
	n.Allocatable = allocatable
	n.Requested = requested
	n.RequestedKnown = true
	n.Ready = true
	n.Labels = map[string]string{}
	n.Topology = map[string]string{}

	for key, value := range labels {
		n.Labels[key] = value
		switch {
		case key == topology.LabelClique:
			n.Clique = value
		case key == topology.LabelDomain,
			key == topology.LabelSubDomain,
			strings.HasPrefix(key, topology.LabelTierPrefix):
			n.Topology[key] = value
		}
	}

	return *n
}

// levelNames renders the level order of a report, which is what --for walks:
// narrowest first.
func levelNames(r *topology.TopologyReport) []string {
	out := make([]string, 0, len(r.Levels))
	for _, l := range r.Levels {
		out = append(out, l.Name)
	}

	return out
}

func groupValues(l topology.Level) []string {
	out := make([]string, 0, len(l.Groups))
	for _, g := range l.Groups {
		out = append(out, g.Value)
	}

	return out
}

func findLevel(t *testing.T, r *topology.TopologyReport, name string) topology.Level {
	t.Helper()

	for _, l := range r.Levels {
		if l.Name == name {
			return l
		}
	}

	t.Fatalf("report has no level %q, only %v", name, levelNames(r))

	return topology.Level{}
}

func findGroup(t *testing.T, l topology.Level, value string) topology.Group {
	t.Helper()

	for _, g := range l.Groups {
		if g.Value == value {
			return g
		}
	}

	t.Fatalf("level %q has no group %q, only %v", l.Name, value, groupValues(l))

	return topology.Group{}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

// TestLevelsRunNarrowestFirst pins the order --for walks.
//
// The two halves that are easy to get backwards:
//
// The NVLink hierarchy comes before the network one. An accelerator
// sub-domain and domain are NVLink groupings and a fabric tier is a network
// switch level, which Topograph documents as separate hierarchies; the NVLink
// one is the tighter interconnect, so a job that fits inside it should be
// placed there rather than spread across a switch.
//
// The Kueue levels are walked backwards. A Topology declares spec.levels
// widest first: its CRD refuses any list where kubernetes.io/hostname is not
// the last entry, calling it "the lowest level of topology", and the upstream
// sample declares block, rack, hostname. Reading them in the declared order
// puts the widest level first and answers every --for with the widest
// placement that fits.
func TestLevelsRunNarrowestFirst(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		topology.LabelClique:               "clique-a",
		topology.LabelSubDomain:            "nvl1.rack01",
		topology.LabelDomain:               "nvl1",
		topology.LabelTierPrefix + "0":     "switch-1",
		topology.LabelTierPrefix + "1":     "spine-1",
		topology.LabelTierPrefix + "10":    "fabric-1",
		"cloud.example.com/topology-rack":  "rack-1",
		"cloud.example.com/topology-block": "block-1",
		topology.LabelHostname:             "node-a",
	}

	report := topology.Build(topology.Input{
		Nodes: []inventory.GPUNode{node("node-a", 8, 0, labels)},
		Kueue: topology.KueueStatus{Installed: true},
		Topologies: []topology.KueueTopology{{
			Name: "default",
			Levels: []string{
				"cloud.example.com/topology-block",
				"cloud.example.com/topology-rack",
				topology.LabelHostname,
			},
		}},
	})

	want := []string{
		"node",
		"clique",
		"accelerator-sub-domain",
		"accelerator-domain",
		"tier-0",
		"tier-1",
		"tier-10",
		"cloud.example.com/topology-rack",
		"cloud.example.com/topology-block",
		"cluster",
	}
	if got := levelNames(report); !equalStrings(got, want) {
		t.Errorf("level order is\n  %v\nwant\n  %v", got, want)
	}
}

// TestTiersSortByNumberNotByString is the case a string sort gets wrong:
// tier-10 sorts before tier-2 as text and after it as a number, and the
// number is what says which fabric level is wider. Tier 0 is the one nearest
// the node, so it is the narrowest of them.
func TestTiersSortByNumberNotByString(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-a", 8, 0, map[string]string{
			topology.LabelTierPrefix + "10": "fabric-1",
			topology.LabelTierPrefix + "2":  "spine-1",
			topology.LabelTierPrefix + "0":  "switch-1",
		}),
	}})

	want := []string{"node", "tier-0", "tier-2", "tier-10", "cluster"}
	if got := levelNames(report); !equalStrings(got, want) {
		t.Errorf("tier order is %v, want %v", got, want)
	}
}

// TestAnUnnumberedTierSortsAfterTheNumberedOnes: a tier label whose suffix is
// not a number cannot be placed among them, and the far end is the safe end.
// Sorting it as tier 0 would make it the narrowest level in the report and
// the first answer --for gives.
func TestAnUnnumberedTierSortsAfterTheNumberedOnes(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-a", 8, 0, map[string]string{
			topology.LabelTierPrefix + "x": "odd-1",
			topology.LabelTierPrefix + "2": "spine-1",
		}),
	}})

	want := []string{"node", "tier-2", "tier-x", "cluster"}
	if got := levelNames(report); !equalStrings(got, want) {
		t.Errorf("tier order is %v, want %v", got, want)
	}
}

// TestTheNodeAndClusterLevelsAlwaysExist: a cluster with no topology labels
// at all still has two levels, so --for always has an answer. A report whose
// levels were only the labels would answer "it fits nowhere" for a job that
// fits on any node in the cluster.
func TestTheNodeAndClusterLevelsAlwaysExist(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-a", 8, 0, nil),
		node("node-b", 8, 0, nil),
	}})

	if got, want := levelNames(report), []string{"node", "cluster"}; !equalStrings(got, want) {
		t.Fatalf("levels are %v, want %v", got, want)
	}

	nodes := findLevel(t, report, "node")
	if got, want := groupValues(nodes), []string{"node-a", "node-b"}; !equalStrings(got, want) {
		t.Errorf("the node level groups %v, want one group per node %v", got, want)
	}

	cluster := findLevel(t, report, "cluster")
	if len(cluster.Groups) != 1 {
		t.Fatalf("the cluster level has %d groups, want 1", len(cluster.Groups))
	}
	if got, want := cluster.Groups[0].Nodes, []string{"node-a", "node-b"}; !equalStrings(got, want) {
		t.Errorf("the cluster group holds %v, want every node %v", got, want)
	}
}

// TestGroupsCollectTheNodesThatShareALabelValue covers the grouping itself:
// one group per distinct value, holding the nodes that carry it, with the
// groups and the node names both sorted so two runs print the same bytes.
func TestGroupsCollectTheNodesThatShareALabelValue(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-c", 8, 0, map[string]string{topology.LabelClique: "clique-b"}),
		node("node-a", 8, 0, map[string]string{topology.LabelClique: "clique-a"}),
		node("node-b", 8, 0, map[string]string{topology.LabelClique: "clique-a"}),
		node("node-d", 8, 0, nil),
	}})

	clique := findLevel(t, report, "clique")
	if clique.Label != topology.LabelClique {
		t.Errorf("clique level reads label %q, want %q", clique.Label, topology.LabelClique)
	}
	if clique.Source != topology.SourceClique {
		t.Errorf("clique level source is %q, want %q", clique.Source, topology.SourceClique)
	}

	if got, want := groupValues(clique), []string{"clique-a", "clique-b"}; !equalStrings(got, want) {
		t.Fatalf("clique groups are %v, want %v", got, want)
	}

	first := clique.Groups[0]
	if got, want := first.Nodes, []string{"node-a", "node-b"}; !equalStrings(got, want) {
		t.Errorf("clique-a holds %v, want %v", got, want)
	}
	if first.NodeCount != 2 {
		t.Errorf("clique-a counts %d nodes, want 2", first.NodeCount)
	}

	// node-d carries no clique label, so it is in no clique group.
	for _, g := range clique.Groups {
		for _, n := range g.Nodes {
			if n == "node-d" {
				t.Errorf("node-d carries no clique label but landed in group %q", g.Value)
			}
		}
	}
}

// TestGroupsAreSortedByValue pins the order against the order the nodes
// arrived in: a map range is not an order, and a report whose groups move
// between two runs over the same cluster cannot be diffed.
func TestGroupsAreSortedByValue(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-z", 8, 0, map[string]string{topology.LabelClique: "z"}),
		node("node-a", 8, 0, map[string]string{topology.LabelClique: "a"}),
	}})

	if got, want := groupValues(findLevel(t, report, "clique")), []string{"a", "z"}; !equalStrings(got, want) {
		t.Errorf("clique groups are %v, want %v", got, want)
	}
}

// TestGroupArithmetic is the sum the whole command exists for: allocatable,
// requested, free and the largest free on any one node of the group.
func TestGroupArithmetic(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-a", 8, 3, map[string]string{topology.LabelClique: "clique-a"}),
		node("node-b", 8, 1, map[string]string{topology.LabelClique: "clique-a"}),
		node("node-c", 4, 4, map[string]string{topology.LabelClique: "clique-a"}),
	}})

	g := findGroup(t, findLevel(t, report, "clique"), "clique-a")

	if g.Allocatable != 20 {
		t.Errorf("allocatable is %d, want 20", g.Allocatable)
	}
	if g.Requested != 8 {
		t.Errorf("requested is %d, want 8", g.Requested)
	}
	// 5 free on node-a, 7 on node-b, 0 on node-c.
	if g.Free != 12 {
		t.Errorf("free is %d, want 12", g.Free)
	}
	if g.LargestFreeNode != 7 {
		t.Errorf("largest free on one node is %d, want 7", g.LargestFreeNode)
	}
}

// TestFreeClampsAtZeroPerNode is the clamp. A node can report more requested
// than allocatable, which happens whenever allocatable drops under the pods
// already running on it, and the negative must not be allowed to lend
// capacity to the rest of the group.
func TestFreeClampsAtZeroPerNode(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-a", 2, 8, map[string]string{topology.LabelClique: "clique-a"}),
		node("node-b", 8, 0, map[string]string{topology.LabelClique: "clique-a"}),
	}})

	g := findGroup(t, findLevel(t, report, "clique"), "clique-a")

	// Summing the raw difference would give 8 + (2-8) = 2; clamping each node
	// first gives 0 + 8.
	if g.Free != 8 {
		t.Errorf("free is %d, want 8: an over-requested node must contribute 0, not a negative", g.Free)
	}
	if g.LargestFreeNode != 8 {
		t.Errorf("largest free on one node is %d, want 8", g.LargestFreeNode)
	}
}

// TestRequestedUnknownIsNotCountedAsFree covers --no-pods: a node whose
// requested GPUs were never read must not be reported as entirely free, and
// the group says so rather than quietly carrying the wrong number. The node
// is still counted and its allocatable still summed: what is unknown is what
// runs on it, not that it exists.
func TestRequestedUnknownIsNotCountedAsFree(t *testing.T) {
	t.Parallel()

	unknown := node("node-a", 8, 0, map[string]string{topology.LabelClique: "clique-a"})
	unknown.RequestedKnown = false

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{unknown}})

	g := findGroup(t, findLevel(t, report, "clique"), "clique-a")

	if g.RequestedKnown {
		t.Error("group reports requested as known, but the node's was not")
	}
	if g.Free != 0 {
		t.Errorf("free is %d, want 0 while requested is unknown", g.Free)
	}
	if g.NodeCount != 1 {
		t.Errorf("node count is %d, want 1: the node is there whatever is known about its pods", g.NodeCount)
	}
	if g.Allocatable != 8 {
		t.Errorf("allocatable is %d, want 8: what the node advertises was read", g.Allocatable)
	}
}

// TestPartialNodesAreTheOnesMissingATopographLabel feeds E2's NV-TOPO-001,
// which is defined on the Topograph labels: the tiers, the accelerator domain
// and its sub-domain. A node carrying some of them and not the rest is a node
// half the fabric knows about.
func TestPartialNodesAreTheOnesMissingATopographLabel(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("complete", 8, 0, map[string]string{
			topology.LabelDomain:           "nvl1",
			topology.LabelTierPrefix + "0": "switch-1",
		}),
		node("partial", 8, 0, map[string]string{
			topology.LabelDomain: "nvl1",
		}),
		node("none", 8, 0, nil),
	}})

	if len(report.PartialNodes) != 1 {
		t.Fatalf("report names %d partial nodes, want 1: %+v", len(report.PartialNodes), report.PartialNodes)
	}

	p := report.PartialNodes[0]
	if p.Name != "partial" {
		t.Errorf("partial node is %q, want %q", p.Name, "partial")
	}
	if got, want := p.Present, []string{topology.LabelDomain}; !equalStrings(got, want) {
		t.Errorf("present labels are %v, want %v", got, want)
	}
	if got, want := p.Missing, []string{topology.LabelTierPrefix + "0"}; !equalStrings(got, want) {
		t.Errorf("missing labels are %v, want %v", got, want)
	}
}

// TestACliqueIsNotAPartialTopographLabel is the mixed fleet: a GB200 node has
// an NVLink clique and an H100 node does not, and neither is missing a label
// the other has. NV-TOPO-001 is about Topograph's labels, and reporting every
// node of a mixed cluster as partially labelled would make the finding
// useless on exactly the clusters it is for.
func TestACliqueIsNotAPartialTopographLabel(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{
		Nodes: []inventory.GPUNode{
			node("gb200", 8, 0, map[string]string{
				topology.LabelClique:           "clique-a",
				topology.LabelTierPrefix + "0": "switch-1",
			}),
			node("h100", 8, 0, map[string]string{
				topology.LabelTierPrefix + "0": "switch-1",
			}),
		},
		Kueue: topology.KueueStatus{Installed: true},
		Topologies: []topology.KueueTopology{{
			Name:   "default",
			Levels: []string{"cloud.example.com/topology-rack", topology.LabelHostname},
		}},
	})

	if len(report.PartialNodes) != 0 {
		t.Errorf("report names %+v as partially labelled, want none: neither a clique nor a Kueue level is a Topograph label",
			report.PartialNodes)
	}
}

// TestKueueLevelDoesNotRepeatALabelAlreadyCovered: a Kueue Topology is free
// to declare nvidia.com/gpu.clique or kubernetes.io/hostname as one of its
// levels, and the report then carries that level once, under the name it
// already has, rather than twice under two names.
func TestKueueLevelDoesNotRepeatALabelAlreadyCovered(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{
		Nodes: []inventory.GPUNode{node("node-a", 8, 0, map[string]string{
			topology.LabelClique:   "clique-a",
			topology.LabelHostname: "node-a",
		})},
		Kueue: topology.KueueStatus{Installed: true},
		Topologies: []topology.KueueTopology{{
			Name:   "default",
			Levels: []string{topology.LabelClique, topology.LabelHostname},
		}},
	})

	if got, want := levelNames(report), []string{"node", "clique", "cluster"}; !equalStrings(got, want) {
		t.Errorf("levels are %v, want %v: the Kueue levels are aliases of two the report already has", got, want)
	}
}

// TestAKueueLevelNoNodeCarriesIsNotALevel: a Topology can name a label this
// cluster's nodes do not have, and an empty level is a row of nothing and a
// group list nothing can be placed in.
func TestAKueueLevelNoNodeCarriesIsNotALevel(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{
		Nodes: []inventory.GPUNode{node("node-a", 8, 0, nil)},
		Kueue: topology.KueueStatus{Installed: true},
		Topologies: []topology.KueueTopology{{
			Name:   "default",
			Levels: []string{"cloud.example.com/topology-block"},
		}},
	})

	if got, want := levelNames(report), []string{"node", "cluster"}; !equalStrings(got, want) {
		t.Errorf("levels are %v, want %v", got, want)
	}
}

// TestKueueNotInstalledIsReportedNotAnError keeps "Kueue is not here" an
// answer the output carries.
func TestKueueNotInstalledIsReportedNotAnError(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{
		Nodes: []inventory.GPUNode{node("node-a", 8, 0, nil)},
		Kueue: topology.KueueStatus{Installed: false, Reason: "the cluster serves no kueue.x-k8s.io topologies"},
	})

	if report.Kueue.Installed {
		t.Error("report says Kueue is installed")
	}
	if report.Kueue.Reason == "" {
		t.Error("report gives no reason why Kueue levels are missing")
	}
}

// TestReportMarshalsWithTheEnvelopeAndNoNulls is the output contract: the
// kind is stamped and every slice is an array, so a consumer can range over
// the report without a null check.
func TestReportMarshalsWithTheEnvelopeAndNoNulls(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(topology.TopologyReport{})
	if err != nil {
		t.Fatalf("marshalling an empty report: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decoding %s: %v", raw, err)
	}

	if got["kind"] != "TopologyReport" {
		t.Errorf("kind is %v, want TopologyReport", got["kind"])
	}
	if got["apiVersion"] != "kubectl-nv.nvidia.com/v1alpha1" {
		t.Errorf("apiVersion is %v, want kubectl-nv.nvidia.com/v1alpha1", got["apiVersion"])
	}

	for _, key := range []string{"levels", "partialNodes"} {
		value, found := got[key]
		if !found {
			t.Errorf("%q is absent from %s", key, raw)
			continue
		}
		if _, ok := value.([]any); !ok {
			t.Errorf("%q is %#v, want an array", key, value)
		}
	}

	if _, found := got["fit"]; found {
		t.Errorf("fit is present in %s, want it absent when no job was asked about", raw)
	}
}

// TestGroupMarshalsWithoutNullNodes covers the same guarantee one level down,
// where a group built by hand has a nil node list.
func TestGroupMarshalsWithoutNullNodes(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(topology.Group{Value: "clique-a"})
	if err != nil {
		t.Fatalf("marshalling a group: %v", err)
	}

	if !strings.Contains(string(raw), `"nodes":[]`) {
		t.Errorf("group marshalled as %s, want an empty nodes array", raw)
	}
}
