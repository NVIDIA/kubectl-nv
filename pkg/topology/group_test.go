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
func node(name string, allocatable, requested int64, labels map[string]string) inventory.GPUNode {
	n := inventory.NewGPUNode(name)
	n.Allocatable = allocatable
	n.Requested = requested
	n.RequestedKnown = true
	n.Labels = map[string]string{}
	n.Topology = map[string]string{}

	for key, value := range labels {
		n.Labels[key] = value
		switch {
		case key == topology.LabelClique:
			n.Clique = value
		case key == topology.LabelDomain, strings.HasPrefix(key, topology.LabelTierPrefix):
			n.Topology[key] = value
		}
	}

	return *n
}

// levelNames renders the level order of a report, which is the one thing
// --for depends on: narrowest first.
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

// TestLevelsRunNarrowestFirst pins the order --for walks: the clique, then
// the Topograph tiers by their number, then the accelerator domain, then the
// Kueue levels in the order the Topology object declares them.
func TestLevelsRunNarrowestFirst(t *testing.T) {
	t.Parallel()

	labels := map[string]string{
		topology.LabelClique:               "clique-a",
		topology.LabelTierPrefix + "1":     "switch-1",
		topology.LabelTierPrefix + "2":     "spine-1",
		topology.LabelTierPrefix + "10":    "fabric-1",
		topology.LabelDomain:               "domain-1",
		"cloud.example.com/topology-block": "block-1",
		"kubernetes.io/hostname":           "node-a",
	}

	report := topology.Build(topology.Input{
		Nodes: []inventory.GPUNode{node("node-a", 8, 0, labels)},
		Kueue: topology.KueueStatus{Installed: true},
		Topologies: []topology.KueueTopology{{
			Name:   "default",
			Levels: []string{"cloud.example.com/topology-block", "kubernetes.io/hostname"},
		}},
	})

	want := []string{
		"clique",
		"tier-1",
		"tier-2",
		"tier-10",
		"accelerator-domain",
		"cloud.example.com/topology-block",
		"kubernetes.io/hostname",
	}
	if got := levelNames(report); !equalStrings(got, want) {
		t.Errorf("level order is %v, want %v", got, want)
	}
}

// TestTiersSortByNumberNotByString is the case a string sort gets wrong:
// tier-10 sorts before tier-2 as text and after it as a number, and the
// number is the one that says which fabric level is wider.
func TestTiersSortByNumberNotByString(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-a", 8, 0, map[string]string{
			topology.LabelTierPrefix + "10": "fabric-1",
			topology.LabelTierPrefix + "2":  "spine-1",
		}),
	}})

	want := []string{"tier-2", "tier-10"}
	if got := levelNames(report); !equalStrings(got, want) {
		t.Errorf("tier order is %v, want %v", got, want)
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

	// node-d carries no topology label at all, so it is in no group.
	for _, g := range clique.Groups {
		for _, n := range g.Nodes {
			if n == "node-d" {
				t.Errorf("node-d carries no clique label but landed in group %q", g.Value)
			}
		}
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
// the group says so rather than quietly carrying the wrong number.
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
}

// TestPartialNodesAreTheOnesMissingALevel feeds E2's NV-TOPO-001: a node
// carrying some of the topology labels the cluster uses and not the rest.
func TestPartialNodesAreTheOnesMissingALevel(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("complete", 8, 0, map[string]string{
			topology.LabelClique:           "clique-a",
			topology.LabelTierPrefix + "1": "switch-1",
		}),
		node("partial", 8, 0, map[string]string{
			topology.LabelClique: "clique-a",
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
	if got, want := p.Present, []string{topology.LabelClique}; !equalStrings(got, want) {
		t.Errorf("present labels are %v, want %v", got, want)
	}
	if got, want := p.Missing, []string{topology.LabelTierPrefix + "1"}; !equalStrings(got, want) {
		t.Errorf("missing labels are %v, want %v", got, want)
	}
}

// TestKueueLevelDoesNotRepeatALabelAlreadyCovered: a Kueue Topology is free
// to declare nvidia.com/gpu.clique as one of its levels, and the report must
// then carry that level once rather than twice under two names.
func TestKueueLevelDoesNotRepeatALabelAlreadyCovered(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{
		Nodes: []inventory.GPUNode{node("node-a", 8, 0, map[string]string{
			topology.LabelClique: "clique-a",
		})},
		Kueue: topology.KueueStatus{Installed: true},
		Topologies: []topology.KueueTopology{{
			Name:   "default",
			Levels: []string{topology.LabelClique},
		}},
	})

	if got, want := levelNames(report), []string{"clique"}; !equalStrings(got, want) {
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
