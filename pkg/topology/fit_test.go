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
	"fmt"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// twoCliques is the fixture most of the fit questions are asked against: two
// cliques of four nodes with four GPUs each, under one fabric tier, with half
// of each clique already fully busy.
//
// So each clique has two idle nodes and eight free GPUs, and the tier has
// four idle nodes and sixteen.
func twoCliques() []inventory.GPUNode {
	nodes := make([]inventory.GPUNode, 0, 8)
	for _, clique := range []string{"clique-a", "clique-b"} {
		for i := 1; i <= 4; i++ {
			requested := int64(0)
			if i > 2 {
				requested = 4
			}
			nodes = append(nodes, node(fmt.Sprintf("%s-%d", clique, i), 4, requested, map[string]string{
				topology.LabelClique:           clique,
				topology.LabelTierPrefix + "0": "fabric-1",
			}))
		}
	}

	return nodes
}

// fitFor runs a job question against a report and returns the answer.
func fitFor(t *testing.T, nodes []inventory.GPUNode, pods, gpusPerPod int64) *topology.JobFit {
	t.Helper()

	report := topology.Build(topology.Input{Nodes: nodes})
	fit := report.FitJob(topology.JobRequest{
		Namespace:  "default",
		Name:       "trainer",
		Pods:       pods,
		GPUsPerPod: gpusPerPod,
	}, nodes)
	if fit == nil {
		t.Fatal("FitJob returned nil")
	}

	return fit
}

func fitGroups(t *testing.T, fit *topology.JobFit, level string) []string {
	t.Helper()

	for _, l := range fit.Levels {
		if l.Level == level {
			return l.Groups
		}
	}

	t.Fatalf("the answer has no level %q", level)

	return nil
}

// TestAJobFitsInsideOneClique is the narrow case: eight GPUs over two pods,
// and each clique has two idle nodes of four.
func TestAJobFitsInsideOneClique(t *testing.T) {
	t.Parallel()

	fit := fitFor(t, twoCliques(), 2, 4)

	if got, want := fitGroups(t, fit, "clique"), []string{"clique-a", "clique-b"}; !equalStrings(got, want) {
		t.Errorf("the job fits in %v, want %v", got, want)
	}
	if fit.Smallest != "clique" {
		t.Errorf("the smallest level that fits is %q, want %q", fit.Smallest, "clique")
	}
}

// TestAJobTooBigForACliqueFitsAtTheTier is the same fixture one pod larger:
// twelve GPUs need three idle nodes, which no clique has and the tier does.
func TestAJobTooBigForACliqueFitsAtTheTier(t *testing.T) {
	t.Parallel()

	fit := fitFor(t, twoCliques(), 3, 4)

	if got := fitGroups(t, fit, "clique"); len(got) != 0 {
		t.Errorf("the job fits in cliques %v, want none: a clique has two idle nodes of four", got)
	}
	if got, want := fitGroups(t, fit, "tier-0"), []string{"fabric-1"}; !equalStrings(got, want) {
		t.Errorf("the job fits in tiers %v, want %v", got, want)
	}
	if fit.Smallest != "tier-0" {
		t.Errorf("the smallest level that fits is %q, want %q", fit.Smallest, "tier-0")
	}
}

// TestASinglePodIsAnsweredWithTheNode: one pod needs one node, and the
// narrowest level that holds it is the node itself. Answering "clique" there
// would name a group wider than the placement.
func TestASinglePodIsAnsweredWithTheNode(t *testing.T) {
	t.Parallel()

	fit := fitFor(t, twoCliques(), 1, 1)

	if fit.Smallest != "node" {
		t.Errorf("the smallest level that fits is %q, want %q", fit.Smallest, "node")
	}
	if len(fit.Levels) == 0 || fit.Levels[0].Level != "node" {
		t.Errorf("the answer walks %v, want the node level first", fit.Levels)
	}
}

// TestAnNVLinkDomainIsPreferredToAFabricTier is the scenario the order was
// ruled on. Sixteen nodes of eight GPUs: two NVLink domains of four nodes
// each sit under one fabric leaf, and a job of four pods of eight GPUs fits
// inside one domain and also under the leaf. The domain is the tighter
// interconnect, so that is the answer.
func TestAnNVLinkDomainIsPreferredToAFabricTier(t *testing.T) {
	t.Parallel()

	nodes := make([]inventory.GPUNode, 0, 16)
	for d := 1; d <= 4; d++ {
		for i := 1; i <= 4; i++ {
			nodes = append(nodes, node(fmt.Sprintf("nvl%d-%d", d, i), 8, 0, map[string]string{
				topology.LabelDomain:           fmt.Sprintf("nvl%d", d),
				topology.LabelTierPrefix + "0": "leaf-1",
			}))
		}
	}

	fit := fitFor(t, nodes, 4, 8)

	if fit.Smallest != "accelerator-domain" {
		t.Errorf("the smallest level that fits is %q, want the accelerator domain: it is the tighter interconnect", fit.Smallest)
	}
	if got := fitGroups(t, fit, "accelerator-domain"); len(got) != 4 {
		t.Errorf("the job fits in %v, want all four domains", got)
	}
	if got, want := fitGroups(t, fit, "tier-0"), []string{"leaf-1"}; !equalStrings(got, want) {
		t.Errorf("the job fits in tiers %v, want %v: the leaf holds it too, it is just wider", got, want)
	}
}

// TestTheNarrowestKueueLevelIsTheLastDeclared: a Topology declaring block,
// rack, hostname is declaring them widest first, so a job that fits in a rack
// must be answered with the rack and not with the block that contains it.
func TestTheNarrowestKueueLevelIsTheLastDeclared(t *testing.T) {
	t.Parallel()

	nodes := make([]inventory.GPUNode, 0, 4)
	for i := 1; i <= 4; i++ {
		rack := "rack-1"
		if i > 2 {
			rack = "rack-2"
		}
		nodes = append(nodes, node(fmt.Sprintf("node-%d", i), 4, 0, map[string]string{
			"cloud.example.com/topology-block": "block-1",
			"cloud.example.com/topology-rack":  rack,
		}))
	}

	report := topology.Build(topology.Input{
		Nodes: nodes,
		Kueue: topology.KueueStatus{Installed: true},
		Topologies: []topology.KueueTopology{{
			Name: "default",
			Levels: []string{
				"cloud.example.com/topology-block",
				"cloud.example.com/topology-rack",
			},
		}},
	})

	fit := report.FitJob(topology.JobRequest{Name: "trainer", Pods: 2, GPUsPerPod: 4}, nodes)

	if fit.Smallest != "cloud.example.com/topology-rack" {
		t.Errorf("the smallest level that fits is %q, want the rack: a Topology declares its levels widest first",
			fit.Smallest)
	}
}

// TestAJobThatFitsNowhereSaysSo. With the implicit cluster level, fitting
// nowhere means it does not fit in the whole cluster.
func TestAJobThatFitsNowhereSaysSo(t *testing.T) {
	t.Parallel()

	fit := fitFor(t, twoCliques(), 5, 4)

	if fit.Smallest != "" {
		t.Errorf("the smallest level that fits is %q, want it empty", fit.Smallest)
	}
	for _, l := range fit.Levels {
		if len(l.Groups) != 0 {
			t.Errorf("level %q reports groups %v, want none", l.Level, l.Groups)
		}
	}
}

// TestPodsArePlacedWholeOntoNodes is the bin packing itself, on free GPUs
// that do not divide evenly. Three nodes with three free GPUs each hold three
// pods of two and not four, although the group has nine free GPUs and four
// pods need eight: half a GPU on one node and half on another is not a pod.
func TestPodsArePlacedWholeOntoNodes(t *testing.T) {
	t.Parallel()

	nodes := []inventory.GPUNode{
		node("node-a", 4, 1, map[string]string{topology.LabelClique: "clique-a"}),
		node("node-b", 4, 1, map[string]string{topology.LabelClique: "clique-a"}),
		node("node-c", 4, 1, map[string]string{topology.LabelClique: "clique-a"}),
	}

	if got := fitGroups(t, fitFor(t, nodes, 3, 2), "clique"); !equalStrings(got, []string{"clique-a"}) {
		t.Errorf("three pods of two fit in %v, want clique-a", got)
	}

	if got := fitGroups(t, fitFor(t, nodes, 4, 2), "clique"); len(got) != 0 {
		t.Errorf("four pods of two fit in %v, want none: nine free GPUs over three nodes hold three", got)
	}
}

// TestSeveralPodsShareOneNode: a node with eight free GPUs takes four pods of
// two on its own. An implementation that places one pod per node would answer
// that this group holds two pods, and would send a user looking for hardware
// they already have.
func TestSeveralPodsShareOneNode(t *testing.T) {
	t.Parallel()

	nodes := []inventory.GPUNode{
		node("idle", 8, 0, map[string]string{topology.LabelClique: "clique-a"}),
		node("busy", 2, 1, map[string]string{topology.LabelClique: "clique-a"}),
	}

	// Four pods of two: the idle node holds four on its own, and the busy
	// node's single free GPU holds none.
	if got := fitGroups(t, fitFor(t, nodes, 4, 2), "clique"); !equalStrings(got, []string{"clique-a"}) {
		t.Errorf("four pods of two fit in %v, want clique-a: eight free GPUs on one node hold four", got)
	}

	// Five would need a tenth GPU, and the one free GPU on the busy node
	// cannot hold a pod of two.
	if got := fitGroups(t, fitFor(t, nodes, 5, 2), "clique"); len(got) != 0 {
		t.Errorf("five pods of two fit in %v, want none: nine free GPUs over two nodes hold four pods of two", got)
	}
}

// TestOneHugePodNeedsOneNodeBigEnough: a pod asking for more GPUs than any
// single node has free never fits, whatever the group total is.
func TestOneHugePodNeedsOneNodeBigEnough(t *testing.T) {
	t.Parallel()

	nodes := []inventory.GPUNode{
		node("node-a", 4, 0, map[string]string{topology.LabelClique: "clique-a"}),
		node("node-b", 4, 0, map[string]string{topology.LabelClique: "clique-a"}),
	}

	if got := fitGroups(t, fitFor(t, nodes, 1, 8), "clique"); len(got) != 0 {
		t.Errorf("a pod of eight fits in %v, want none: the biggest node has four free", got)
	}
}

// TestAnOverRequestedNodeOffersNothing is the clamp seen from the fit side: a
// node reporting more requested than allocatable must not cancel out the free
// GPUs of its neighbour.
func TestAnOverRequestedNodeOffersNothing(t *testing.T) {
	t.Parallel()

	nodes := []inventory.GPUNode{
		node("node-a", 2, 8, map[string]string{topology.LabelClique: "clique-a"}),
		node("node-b", 4, 0, map[string]string{topology.LabelClique: "clique-a"}),
	}

	if got := fitGroups(t, fitFor(t, nodes, 1, 4), "clique"); !equalStrings(got, []string{"clique-a"}) {
		t.Errorf("one pod of four fits in %v, want clique-a: node-b has four free", got)
	}
	if got := fitGroups(t, fitFor(t, nodes, 2, 4), "clique"); len(got) != 0 {
		t.Errorf("two pods of four fit in %v, want none: node-a has none free", got)
	}
}

// TestFitSkipsNodesWhoseRequestedGPUsAreUnknown: with --no-pods nothing is
// known about what is running, and answering "it fits" from allocatable alone
// would place a job onto nodes that are full.
func TestFitSkipsNodesWhoseRequestedGPUsAreUnknown(t *testing.T) {
	t.Parallel()

	unknown := node("node-a", 8, 0, map[string]string{topology.LabelClique: "clique-a"})
	unknown.RequestedKnown = false

	if got := fitGroups(t, fitFor(t, []inventory.GPUNode{unknown}, 1, 1), "clique"); len(got) != 0 {
		t.Errorf("a pod fits in %v, want none while nothing is known about what runs there", got)
	}
}

// TestFitLeavesOutNodesNothingCanBePlacedOn: a cordoned node and a node that
// is not Ready both have free GPUs that the scheduler will not use. Counting
// them would answer "it fits" about a placement that will sit Pending, which
// is the question the user was trying to avoid.
func TestFitLeavesOutNodesNothingCanBePlacedOn(t *testing.T) {
	t.Parallel()

	cordoned := node("cordoned", 8, 0, map[string]string{topology.LabelClique: "clique-a"})
	cordoned.Unschedulable = true

	notReady := node("not-ready", 8, 0, map[string]string{topology.LabelClique: "clique-a"})
	notReady.Ready = false

	nodes := []inventory.GPUNode{
		cordoned,
		notReady,
		node("usable", 8, 0, map[string]string{topology.LabelClique: "clique-a"}),
	}

	report := topology.Build(topology.Input{Nodes: nodes})
	fit := report.FitJob(topology.JobRequest{Name: "trainer", Pods: 2, GPUsPerPod: 8}, nodes)

	if got := fitGroups(t, fit, "clique"); len(got) != 0 {
		t.Errorf("two pods of eight fit in %v, want none: only one of the three nodes can take a pod", got)
	}

	if got, want := fit.Excluded, []string{"cordoned", "not-ready"}; !equalStrings(got, want) {
		t.Errorf("the answer leaves out %v, want it to name %v", got, want)
	}

	// The group arithmetic still counts them: they hold GPUs, and a reader
	// asking what the clique has is asking a different question from what can
	// be placed on it right now.
	if g := findGroup(t, findLevel(t, report, "clique"), "clique-a"); g.Allocatable != 24 {
		t.Errorf("the clique reports %d allocatable, want 24: a cordoned node still has its GPUs", g.Allocatable)
	}
}

// TestFitCarriesTheQuestionItAnswered keeps the job, the pod count and the
// GPUs per pod in the output, so that a report read later says what was
// asked.
func TestFitCarriesTheQuestionItAnswered(t *testing.T) {
	t.Parallel()

	fit := fitFor(t, twoCliques(), 2, 4)

	if fit.Namespace != "default" || fit.Name != "trainer" {
		t.Errorf("the answer is about %s/%s, want default/trainer", fit.Namespace, fit.Name)
	}
	if fit.Pods != 2 || fit.GPUsPerPod != 4 {
		t.Errorf("the answer is about %d pods of %d GPUs, want 2 of 4", fit.Pods, fit.GPUsPerPod)
	}
}
