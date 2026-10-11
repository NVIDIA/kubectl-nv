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
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// twoCliques is the fixture the fit questions are asked against: two cliques
// of four nodes with four GPUs each, under one fabric tier, with half of each
// clique already fully busy.
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
			name := clique + "-" + string(rune('0'+i))
			nodes = append(nodes, node(name, 4, requested, map[string]string{
				topology.LabelClique:           clique,
				topology.LabelTierPrefix + "1": "fabric-1",
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
	if got, want := fitGroups(t, fit, "tier-1"), []string{"fabric-1"}; !equalStrings(got, want) {
		t.Errorf("the job fits in tiers %v, want %v", got, want)
	}
	if fit.Smallest != "tier-1" {
		t.Errorf("the smallest level that fits is %q, want %q", fit.Smallest, "tier-1")
	}
}

// TestSmallestLevelIsTheNarrowestThatFits: a job that fits everywhere must be
// answered with the clique, not with the first level that happens to be
// walked. Placing it as wide as the tier gives up the NVLink domain for
// nothing.
func TestSmallestLevelIsTheNarrowestThatFits(t *testing.T) {
	t.Parallel()

	fit := fitFor(t, twoCliques(), 1, 1)

	if fit.Smallest != "clique" {
		t.Errorf("the smallest level that fits is %q, want %q", fit.Smallest, "clique")
	}
	if len(fit.Levels) == 0 || fit.Levels[0].Level != "clique" {
		t.Errorf("the answer walks %v, want the clique first", fit.Levels)
	}
}

// TestAJobThatFitsNowhereSaysSo.
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
