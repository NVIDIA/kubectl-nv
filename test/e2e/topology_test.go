//go:build e2e && computedomain

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

// This file is behind a second build tag, computedomain, on top of the suite's
// own e2e tag. It needs a lane the other e2e tests do not have: four workers
// carrying clique and Topograph labels, Kueue installed, and a namespace to
// put jobs in. "make e2e-mokka" builds the two worker lane and compiles with
// -tags e2e only, so this file is not in that binary at all; the compute
// domain lane in hack/mokka/compute-domain adds computedomain.
//
// Everything here drives the built binary, so it compiles without any of the
// packages the binary is built from.

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The lane this file is written against. These values are the ones
// hack/mokka/compute-domain/kind-compute-domain.yaml puts on the nodes and
// hack/mokka/compute-domain/up.sh asserts before the suite runs, so a lane
// that drifted from them fails there rather than here.
const (
	// jobNamespace is created by up.sh and holds nothing but the jobs below.
	jobNamespace = "kubectl-nv-e2e"

	// cliqueLabel is what GPU Feature Discovery writes, and what the lane
	// bakes into every worker so the label is there whether or not GFD
	// resolved the fabric.
	cliqueLabel = "nvidia.com/gpu.clique"
	// subDomainLabel and domainLabel are Topograph v1.0.0's accelerator keys.
	subDomainLabel = "accelerator.topograph.run/sub-domain"
	domainLabel    = "accelerator.topograph.run/domain"
	// tierZeroLabel is Topograph's closest-to-the-node fabric tier.
	tierZeroLabel = "fabric.topograph.run/tier-0"

	// tierOneLabel is the tier above it, which this lane also carries.
	tierOneLabel = "fabric.topograph.run/tier-1"

	// hostnameLabel is Kueue's required last level, and the label the node
	// level of the walk reads so that a Kueue level naming it is recognised.
	hostnameLabel = "kubernetes.io/hostname"

	// The level names the report prints, as against the labels they read.
	// --for answers with one of these, not with a label.
	nodeLevelName      = "node"
	cliqueLevelName    = "clique"
	subDomainLevelName = "accelerator-sub-domain"
	domainLevelName    = "accelerator-domain"
	clusterLevelName   = "cluster"

	// kueueSource marks a level the report took from a Kueue Topology. On
	// this lane no level should carry it: see TestKueueLevelsAppearInTheReport.
	kueueSource = "kueue"

	// The two cliques, named after the ComputeDomain the lane declares.
	cliqueZero = "00000000-0000-0000-0000-0000000000ab.0"
	cliqueOne  = "00000000-0000-0000-0000-0000000000ab.1"

	// domainValue is the accelerator domain that spans both cliques, and so
	// the group a job needing three of the four workers lands in.
	domainValue = "nvl-domain-0"

	// gpusPerNode is the gb200 profile's device count. Four workers of four.
	gpusPerNode = 4
	// nodesPerClique is two, so a clique holds eight GPUs and cannot hold a
	// job that needs three nodes.
	nodesPerClique = 2

	// The lane's cluster, and so the prefix kind derives every worker name
	// from. Fixed rather than discovered: a test that read the node names out
	// of the report and then checked the report against them would pass
	// whatever the report said.
	laneCluster = "kubectl-nv-compute-domain"
)

// The workers in each clique, by name. kind names the first worker
// <cluster>-worker and the rest <cluster>-workerN counting from two;
// hack/mokka/compute-domain/lib.sh puts the first two in clique 0.
var (
	cliqueZeroNodes = []string{laneCluster + "-worker", laneCluster + "-worker2"}
	cliqueOneNodes  = []string{laneCluster + "-worker3", laneCluster + "-worker4"}
	allWorkerNodes  = append(append([]string{}, cliqueZeroNodes...), cliqueOneNodes...)
)

// The wire shape of "kubectl nv topology -o json". It mirrors
// pkg/topology.TopologyReport, declared again here rather than imported so
// that the suite stays a black box over the built binary: a refactor that
// keeps the output keeps these tests green, and a change to the output breaks
// them, which is the point of an end to end suite. When the output moves,
// this block is the one place that follows it.
type topologyReport struct {
	APIVersion   string          `json:"apiVersion"`
	Kind         string          `json:"kind"`
	Levels       []topologyLevel `json:"levels"`
	PartialNodes []partialNode   `json:"partialNodes"`
	Kueue        kueueStatus     `json:"kueue"`
	Fit          *topologyFit    `json:"fit"`
	Status       reportStatus    `json:"status"`
}

// topologyLevel is one way the cluster divides: a name, the node label it
// reads, where the level came from, and the groups the nodes fall into.
//
// There is no alias field. A Kueue level whose node label one of the walk's
// own levels already reads is absorbed into that level and never appears: the
// report names the level once. That is what this lane's Kueue Topology is
// built to exercise, since both of the levels it declares are absorbed.
type topologyLevel struct {
	Name   string          `json:"name"`
	Label  string          `json:"label"`
	Source string          `json:"source"`
	Groups []topologyGroup `json:"groups"`
}

// topologyGroup is the set of nodes that share one value of a level's label,
// with the GPU arithmetic over them. RequestedKnown false means Requested,
// Free and LargestFreeNode are zero and mean nothing.
type topologyGroup struct {
	Value           string   `json:"value"`
	Nodes           []string `json:"nodes"`
	NodeCount       int      `json:"nodeCount"`
	Allocatable     int64    `json:"allocatable"`
	Requested       int64    `json:"requested"`
	Free            int64    `json:"free"`
	LargestFreeNode int64    `json:"largestFreeNode"`
	RequestedKnown  bool     `json:"requestedKnown"`
}

// partialNode is a node carrying some of the cluster's topology labels and
// not the rest. Every worker on this lane carries all of them, so the list is
// empty here and a non-empty one is a lane fault.
type partialNode struct {
	Name    string   `json:"name"`
	Present []string `json:"present"`
	Missing []string `json:"missing"`
}

// kueueStatus says whether the Kueue levels in the report are the cluster's.
type kueueStatus struct {
	Installed bool   `json:"installed"`
	Reason    string `json:"reason"`
}

// reportStatus carries what the run could not see, and is what the exit code
// is read from.
type reportStatus struct {
	Partial     bool          `json:"partial"`
	Unreachable []unreachable `json:"unreachable"`
}

// unreachable is one thing the run could not read, and why.
type unreachable struct {
	Resource string `json:"resource"`
	Node     string `json:"node"`
	Reason   string `json:"reason"`
}

// topologyFit is the answer to --for job/<name>.
type topologyFit struct {
	Namespace  string             `json:"namespace"`
	Name       string             `json:"name"`
	Pods       int64              `json:"pods"`
	GPUsPerPod int64              `json:"gpusPerPod"`
	Levels     []topologyFitLevel `json:"levels"`
	Smallest   string             `json:"smallest"`
	Excluded   []string           `json:"excluded"`
}

// topologyFitLevel is one level's answer: the groups of it the job fits in,
// empty when it fits in none. There is no boolean; the group list is the
// answer.
type topologyFitLevel struct {
	Level  string   `json:"level"`
	Label  string   `json:"label"`
	Groups []string `json:"groups"`
}

// TestTopologyReportsTwoCliquesOfTwoNodes is the shape of the lane as the
// command sees it. It is the assertion the rest of this file rests on: if the
// cliques are not two groups of two with eight free GPUs each, no fit answer
// below means what it claims to.
func TestTopologyReportsTwoCliquesOfTwoNodes(t *testing.T) {
	report := topologyJSON(t, "topology", "-o", "json")

	level := levelByLabel(t, report, cliqueLabel)
	if got := len(level.Groups); got != 2 {
		t.Fatalf("level %s has %d groups, want 2: %+v", cliqueLabel, got, level.Groups)
	}

	for _, clique := range []struct {
		value string
		nodes []string
	}{
		{cliqueZero, cliqueZeroNodes},
		{cliqueOne, cliqueOneNodes},
	} {
		want := clique.value
		group := groupByValue(t, level, want)
		// The exact node names, not their count: two workers from the wrong
		// clique would satisfy a count and would make every fit answer below
		// mean something other than what it claims.
		if got := sortedCopy(group.Nodes); !equalStrings(got, clique.nodes) {
			t.Errorf("clique %s holds nodes %v, want %v", want, got, clique.nodes)
		}
		if group.NodeCount != nodesPerClique {
			t.Errorf("clique %s nodeCount = %d, want %d",
				want, group.NodeCount, nodesPerClique)
		}
		if group.Allocatable != int64(nodesPerClique*gpusPerNode) {
			t.Errorf("clique %s allocatable = %d, want %d",
				want, group.Allocatable, nodesPerClique*gpusPerNode)
		}
		// requestedKnown false would make the three numbers below zero and
		// meaningless, so a report that never listed the pods has to fail
		// here rather than read as a cluster with nothing running on it.
		if !group.RequestedKnown {
			t.Errorf("clique %s reports requestedKnown false; the pod list was not read, so its free GPUs say nothing",
				want)
		}
		// Nothing is scheduled on this lane: every job it carries is
		// suspended, so requested is zero and free is all of it.
		if group.Requested != 0 {
			t.Errorf("clique %s requested = %d, want 0; something is running on the lane",
				want, group.Requested)
		}
		if group.Free != int64(nodesPerClique*gpusPerNode) {
			t.Errorf("clique %s free = %d, want %d",
				want, group.Free, nodesPerClique*gpusPerNode)
		}
		if group.LargestFreeNode != int64(gpusPerNode) {
			t.Errorf("clique %s largest free on one node = %d, want %d",
				want, group.LargestFreeNode, gpusPerNode)
		}
	}

	// Every worker carries the whole label set, so nothing is partial. A node
	// here is a lane fault, and it is the input NV-TOPO-001 will read.
	if len(report.PartialNodes) != 0 {
		t.Errorf("the report lists partially labelled nodes, so the lane is not labelled as it declares: %+v",
			report.PartialNodes)
	}

	// The two cliques must not share a node, or "fits in one clique" below
	// would be satisfiable by a group that is really the whole cluster.
	zero := groupByValue(t, level, cliqueZero)
	one := groupByValue(t, level, cliqueOne)
	for _, node := range zero.Nodes {
		for _, other := range one.Nodes {
			if node == other {
				t.Errorf("node %s is in both cliques", node)
			}
		}
	}
}

// TestTopologyWalksTheLevelsNarrowestFirst pins the order the levels are
// reported in, which is the order --for reads them in and therefore what
// "smallest" means. The lane carries every label the walk knows about, so the
// list below is the whole of it and an assertion on the exact sequence is
// available here in a way it is not on a cluster that carries only some.
func TestTopologyWalksTheLevelsNarrowestFirst(t *testing.T) {
	report := topologyJSON(t, "topology", "-o", "json")

	want := []struct {
		name  string
		label string
	}{
		{nodeLevelName, hostnameLabel},
		{cliqueLevelName, cliqueLabel},
		{subDomainLevelName, subDomainLabel},
		{domainLevelName, domainLabel},
		{"tier-0", tierZeroLabel},
		{"tier-1", tierOneLabel},
		{clusterLevelName, ""},
	}

	if len(report.Levels) != len(want) {
		t.Fatalf("the report has %d levels, want %d: %s",
			len(report.Levels), len(want), formatLevels(report.Levels))
	}
	for i, expected := range want {
		got := report.Levels[i]
		if got.Name != expected.name || got.Label != expected.label {
			t.Errorf("level %d is %q reading %q, want %q reading %q\nfull walk: %s",
				i, got.Name, got.Label, expected.name, expected.label,
				formatLevels(report.Levels))
		}
	}
}

// TestJobNeedingEightGPUsFitsInOneClique is the narrow case: two pods of four
// GPUs need two nodes, which is exactly what a clique has. One node cannot
// hold both pods, so the clique is the narrowest level that works and the
// answer discriminates the node level from the clique level.
func TestJobNeedingEightGPUsFitsInOneClique(t *testing.T) {
	name := createSuspendedGPUJob(t, "fit-one-clique", 2, gpusPerNode)

	report := topologyJSON(t, "topology", "--for", "job/"+name,
		"-n", jobNamespace, "-o", "json")

	fit := requireFit(t, report)
	if fit.Smallest != cliqueLevelName {
		t.Errorf("smallest level = %q, want %q\nlevels: %s",
			fit.Smallest, cliqueLevelName, formatFitLevels(fit.Levels))
	}
	if fit.Namespace != jobNamespace || fit.Name != name {
		t.Errorf("fit is about %s/%s, want %s/%s",
			fit.Namespace, fit.Name, jobNamespace, name)
	}
	if fit.Pods != 2 || fit.GPUsPerPod != int64(gpusPerNode) {
		t.Errorf("fit reports %d pods of %d GPUs, want 2 of %d",
			fit.Pods, fit.GPUsPerPod, gpusPerNode)
	}
	// Nothing on this lane is cordoned or unready, so nothing is left out of
	// the answer. An excluded node would silently shrink every group.
	if len(fit.Excluded) != 0 {
		t.Errorf("the fit excluded nodes %v; on this lane every worker is schedulable",
			fit.Excluded)
	}

	// Both cliques hold it, in fact, since each has eight free GPUs on two
	// nodes. What must not happen is the level naming a group that cannot
	// host the pods, or naming none. The groups are sorted before the
	// comparison, so want is written in sorted order.
	level := fitLevelByName(t, fit, cliqueLevelName)
	want := []string{cliqueZero, cliqueOne}
	if got := sortedCopy(level.Groups); !equalStrings(got, want) {
		t.Errorf("clique groups that fit = %v, want %v", got, want)
	}

	// The node level is narrower than the clique and must have rejected the
	// job: two pods of four GPUs cannot share a node with four.
	node := fitLevelByName(t, fit, nodeLevelName)
	if len(node.Groups) != 0 {
		t.Errorf("the %s level fits a job of two pods of %d GPUs on nodes of %d: %v",
			nodeLevelName, gpusPerNode, gpusPerNode, node.Groups)
	}

	// kubernetes.io/hostname is absorbed into the node level, so the walk
	// carries it once, under that level's name. A second entry for it would
	// be a level nobody can schedule at.
	hostnameLevels := 0
	for _, walked := range fit.Levels {
		if walked.Label == hostnameLabel {
			hostnameLevels++
		}
	}
	if hostnameLevels != 1 {
		t.Errorf("the fit walk reads %s at %d levels, want exactly 1 (the %s level): %s",
			hostnameLabel, hostnameLevels, nodeLevelName, formatFitLevels(fit.Levels))
	}

	// Each of those groups is the two workers the lane put in it, by name.
	for _, clique := range []struct {
		value string
		nodes []string
	}{
		{cliqueZero, cliqueZeroNodes},
		{cliqueOne, cliqueOneNodes},
	} {
		group := groupByValue(t, levelByLabel(t, report, cliqueLabel), clique.value)
		if got := sortedCopy(group.Nodes); !equalStrings(got, clique.nodes) {
			t.Errorf("clique %s holds nodes %v, want %v", clique.value, got, clique.nodes)
		}
	}
}

// TestJobNeedingTwelveGPUsFitsAtTheAcceleratorDomain is the walk itself. Three
// pods of four GPUs need three nodes. No clique has three, and no sub-domain
// has three either, because the lane gives each clique its own sub-domain. The
// accelerator domain holds all four workers, so it is the first level that
// works, which pins the order clique, sub-domain, domain rather than only
// "something wider than a clique".
func TestJobNeedingTwelveGPUsFitsAtTheAcceleratorDomain(t *testing.T) {
	name := createSuspendedGPUJob(t, "fit-accelerator-domain", 3, gpusPerNode)

	report := topologyJSON(t, "topology", "--for", "job/"+name,
		"-n", jobNamespace, "-o", "json")

	fit := requireFit(t, report)
	if fit.Smallest != domainLevelName {
		t.Errorf("smallest level = %q, want %q\nlevels: %s",
			fit.Smallest, domainLevelName, formatFitLevels(fit.Levels))
	}

	// The three levels it must have rejected on the way, named so a failure
	// says which one wrongly accepted. An empty group list is the rejection:
	// there is no boolean to read.
	for _, levelName := range []string{nodeLevelName, cliqueLevelName, subDomainLevelName} {
		level := fitLevelByName(t, fit, levelName)
		if len(level.Groups) != 0 {
			t.Errorf("the %s level fits a job needing 3 nodes, but every one of its groups has at most %d: %v",
				levelName, nodesPerClique, level.Groups)
		}
	}

	if fit.Pods != 3 || fit.GPUsPerPod != int64(gpusPerNode) {
		t.Errorf("fit reports %d pods of %d GPUs, want 3 of %d",
			fit.Pods, fit.GPUsPerPod, gpusPerNode)
	}
	if len(fit.Excluded) != 0 {
		t.Errorf("the fit excluded nodes %v; on this lane every worker is schedulable",
			fit.Excluded)
	}

	// The exact group, not its count: the lane declares one accelerator
	// domain holding all four workers, and that is the only group that can
	// hold three pods.
	domain := fitLevelByName(t, fit, domainLevelName)
	if got := sortedCopy(domain.Groups); !equalStrings(got, []string{domainValue}) {
		t.Errorf("accelerator domain groups that fit = %v, want [%s]", got, domainValue)
	}

	// And it really is all four workers, so "fits at the domain" is not a
	// claim about a group that happens to carry a wider name.
	level := levelByLabel(t, report, domainLabel)
	group := groupByValue(t, level, domainValue)
	if got := sortedCopy(group.Nodes); !equalStrings(got, sortedCopy(allWorkerNodes)) {
		t.Errorf("accelerator domain %s holds nodes %v, want %v",
			domainValue, got, sortedCopy(allWorkerNodes))
	}
}

// TestKueueLevelsAppearInTheReport covers the Kueue read. The lane declares a
// Topology whose levels are both already in the walk, so each has to be
// reported as an alias rather than repeated as a level of its own: a report
// that walks kubernetes.io/hostname a second time would group every node on
// its own name and claim a level nobody can schedule at.
func TestKueueLevelsAppearInTheReport(t *testing.T) {
	report := topologyJSON(t, "topology", "-o", "json")

	// Kueue is installed on this lane and serves a Topology, so the report
	// must say so. An "installed false" here means the read failed and every
	// absorption assertion below would be vacuously true.
	if !report.Kueue.Installed {
		t.Fatalf("the report says Kueue is not installed (reason %q), but the lane installs it",
			report.Kueue.Reason)
	}

	// The absorption. The lane's Topology declares nvidia.com/gpu.clique and
	// kubernetes.io/hostname, and the walk already reads both: the clique
	// level reads the first and the node level reads the second. So each is
	// absorbed into the level that already has it and NO level in this report
	// carries the Kueue source. There is no alias field to check; the absence
	// of a second level for the same label is the whole of it.
	for _, level := range report.Levels {
		if level.Source == kueueSource {
			t.Errorf("level %q (label %s) is reported with source %q, but every level this lane's Topology declares is already read by the walk and must be absorbed into it",
				level.Name, level.Label, kueueSource)
		}
	}

	// Absorbed into these two in particular, so the lane is not passing the
	// check above by having lost the levels altogether.
	byLabel := map[string]topologyLevel{}
	for _, level := range report.Levels {
		byLabel[level.Label] = level
	}
	for _, want := range []struct {
		label  string
		name   string
		source string
	}{
		{hostnameLabel, nodeLevelName, nodeLevelName},
		{cliqueLabel, cliqueLevelName, cliqueLevelName},
	} {
		level, found := byLabel[want.label]
		if !found {
			t.Errorf("no level reads %s, so the Kueue level naming it was dropped rather than absorbed", want.label)
			continue
		}
		if level.Name != want.name || level.Source != want.source {
			t.Errorf("the level reading %s is named %q with source %q, want %q and %q",
				want.label, level.Name, level.Source, want.name, want.source)
		}
	}

	// Each label is read by exactly one level. Two levels over one label is
	// what absorption exists to prevent, and it would offer a level nobody
	// can schedule at.
	byCount := map[string]int{}
	for _, level := range report.Levels {
		if level.Label != "" {
			byCount[level.Label]++
		}
	}
	for label, count := range byCount {
		if count > 1 {
			t.Errorf("label %s is read by %d levels, want exactly 1", label, count)
		}
	}
}

// TestTopologyForAMissingJobIsAUsageError pins the code a script branches on.
// A job the caller named and the cluster does not have is a bad argument, not
// a cluster failure, so it is 2 and not 1 or 4.
func TestTopologyForAMissingJobIsAUsageError(t *testing.T) {
	const missing = "no-such-job-on-this-lane"

	res := run(t, "topology", "--for", "job/"+missing,
		"-n", jobNamespace, "-o", "json")

	if res.code != 2 {
		t.Errorf("exit code = %d, want 2\nstdout:\n%s\nstderr:\n%s",
			res.code, res.stdout, res.stderr)
	}
	// A caller parsing stdout must not be handed a partial report.
	if strings.TrimSpace(res.stdout) != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}

	// Exit 2 alone proves nothing here. A binary with no topology command at
	// all exits 2 on "unknown flag: --for", and so would a binary that lost
	// --for in a refactor. What has to be true is that the command ran, looked
	// for this job, and said it was not there. The message is the one
	// pkg/cmd/topology builds for a NotFound from the Job read.
	wantMessage := fmt.Sprintf("no job %q in namespace %q", missing, jobNamespace)
	if !strings.Contains(res.stderr, wantMessage) {
		t.Errorf("stderr does not say %q; it is:\n%s", wantMessage, res.stderr)
	}

	// The three ways exit 2 can mean "the command line was wrong" rather than
	// "the job was not there". Each of these passing the test above is the
	// failure this check exists to catch.
	for _, forbidden := range []string{"unknown flag", "unknown command", "Usage:"} {
		if strings.Contains(res.stderr, forbidden) {
			t.Errorf("stderr contains %q, so exit 2 is about the command line and not about the job:\n%s",
				forbidden, res.stderr)
		}
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// topologyJSON runs the binary and decodes its stdout, failing with both
// streams when the run did not succeed or the output is not the report.
func topologyJSON(t *testing.T, args ...string) topologyReport {
	t.Helper()

	res := run(t, args...)
	if res.code != 0 {
		t.Fatalf("%v exited %d\nstdout:\n%s\nstderr:\n%s",
			args, res.code, res.stdout, res.stderr)
	}

	assertMatchesSchema(t, output.KindTopologyReport, res.stdout)

	var report topologyReport
	if err := json.Unmarshal([]byte(res.stdout), &report); err != nil {
		t.Fatalf("decoding the output of %v: %v\nstdout:\n%s", args, err, res.stdout)
	}
	if report.Kind != "TopologyReport" {
		t.Fatalf("kind = %q, want TopologyReport\nstdout:\n%s", report.Kind, res.stdout)
	}
	if report.APIVersion != "kubectl-nv.nvidia.com/v1alpha1" {
		t.Fatalf("apiVersion = %q, want kubectl-nv.nvidia.com/v1alpha1", report.APIVersion)
	}

	return report
}

// levelByLabel returns the level that reads that label, failing with the
// labels that are there when it is absent.
func levelByLabel(t *testing.T, report topologyReport, label string) topologyLevel {
	t.Helper()

	var labels []string
	for _, level := range report.Levels {
		if level.Label == label {
			return level
		}
		labels = append(labels, level.Label)
	}
	t.Fatalf("no level reads %s in the report; it reads %v", label, labels)

	return topologyLevel{}
}

// sortedCopy sorts without reordering the caller's slice, so an assertion
// cannot change what a later assertion sees.
func sortedCopy(items []string) []string {
	out := append([]string{}, items...)
	sort.Strings(out)

	return out
}

// equalStrings reports whether two slices hold the same strings in the same
// order.
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

// groupByValue returns the group with that value.
func groupByValue(t *testing.T, level topologyLevel, value string) topologyGroup {
	t.Helper()

	var values []string
	for _, group := range level.Groups {
		if group.Value == value {
			return group
		}
		values = append(values, group.Value)
	}
	t.Fatalf("no group %q under level %s; it has %v", value, level.Label, values)

	return topologyGroup{}
}

// requireFit returns the fit block, failing when --for produced none.
func requireFit(t *testing.T, report topologyReport) topologyFit {
	t.Helper()

	if report.Fit == nil {
		t.Fatal("the report carries no fit block, but it was asked with --for")
	}

	return *report.Fit
}

// fitLevelByName returns one level's answer for the job. Levels are looked up
// by the name the report prints, because that is what Smallest names and what
// the implicit node and cluster levels have instead of a label of their own.
func fitLevelByName(t *testing.T, fit topologyFit, name string) topologyFitLevel {
	t.Helper()

	for _, level := range fit.Levels {
		if level.Level == name {
			return level
		}
	}
	t.Fatalf("the fit answer says nothing about the %s level: %s", name, formatFitLevels(fit.Levels))

	return topologyFitLevel{}
}

// formatLevels renders the report's levels in order, name and label, so a
// failure over one of them shows the whole walk it came from.
func formatLevels(levels []topologyLevel) string {
	parts := make([]string, 0, len(levels))
	for _, level := range levels {
		parts = append(parts, fmt.Sprintf("%s(%s,source=%s)", level.Name, level.Label, level.Source))
	}

	return strings.Join(parts, " -> ")
}

// formatFitLevels renders the walk in order, so a failure shows where the
// command stopped rather than only which level it named.
func formatFitLevels(levels []topologyFitLevel) string {
	parts := make([]string, 0, len(levels))
	for _, level := range levels {
		parts = append(parts, fmt.Sprintf("%s(%s) groups=%v", level.Level, level.Label, level.Groups))
	}

	return strings.Join(parts, "; ")
}

// createSuspendedGPUJob creates a job that will never schedule and returns its
// name. Suspended is what keeps the lane's free GPU count at its full value
// while a job asking for most of them exists: an unsuspended job would have
// its pods admitted, and every arithmetic assertion in this file would then
// depend on how far the scheduler had got.
//
// The name is suffixed so a rerun against a lane that still holds the previous
// run's jobs cannot collide, and the job is deleted when the test ends.
func createSuspendedGPUJob(t *testing.T, prefix string, pods, gpusPerPod int) string {
	t.Helper()

	name := fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
	suspend := true
	parallelism := int32(pods)
	quantity := resource.MustParse(fmt.Sprintf("%d", gpusPerPod))

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: jobNamespace},
		Spec: batchv1.JobSpec{
			Suspend:     &suspend,
			Parallelism: &parallelism,
			Completions: &parallelism,
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{
					RestartPolicy: v1.RestartPolicyNever,
					Containers: []v1.Container{{
						Name: "gpu",
						// The image is never pulled: the job is suspended for
						// its whole life. It is named so the pod template is
						// valid, not so it runs.
						Image:   "registry.k8s.io/pause:3.10",
						Command: []string{"/pause"},
						Resources: v1.ResourceRequirements{
							Limits:   v1.ResourceList{"nvidia.com/gpu": quantity},
							Requests: v1.ResourceList{"nvidia.com/gpu": quantity},
						},
					}},
				},
			},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	client := clusterClient(t)
	if _, err := client.BatchV1().Jobs(jobNamespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		t.Fatalf("creating the suspended job %s/%s: %v", jobNamespace, name, err)
	}

	t.Cleanup(func() {
		deleteCtx, deleteCancel := context.WithTimeout(context.Background(), time.Minute)
		defer deleteCancel()

		policy := metav1.DeletePropagationForeground
		err := client.BatchV1().Jobs(jobNamespace).Delete(deleteCtx, name,
			metav1.DeleteOptions{PropagationPolicy: &policy})
		if err != nil {
			t.Errorf("deleting the job %s/%s: %v", jobNamespace, name, err)
		}
	})

	return name
}
