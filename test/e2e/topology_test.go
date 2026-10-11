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

	// The two cliques, named after the ComputeDomain the lane declares.
	cliqueZero = "00000000-0000-0000-0000-0000000000ab.0"
	cliqueOne  = "00000000-0000-0000-0000-0000000000ab.1"

	// gpusPerNode is the gb200 profile's device count. Four workers of four.
	gpusPerNode = 4
	// nodesPerClique is two, so a clique holds eight GPUs and cannot hold a
	// job that needs three nodes.
	nodesPerClique = 2
)

// The Kueue levels the lane declares, widest first, which is the order Kueue
// itself requires. kubectl nv topology walks them last to first.
var kueueLevels = []string{cliqueLabel, "kubernetes.io/hostname"}

// topologyReport is the JSON contract this lane asserts: the Track G brief's
// description of what "kubectl nv topology -o json" emits. The Go type belongs
// to pkg/topology; this is the wire shape, declared here so that the suite
// stays a black box over the built binary. If the command's output moves, this
// block is the one place that follows it.
type topologyReport struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Levels     []topologyLevel `json:"levels"`
	Fit        *topologyFit    `json:"fit"`
}

// topologyLevel is one grouping key and the groups the nodes fall into under
// it. AliasOf names the earlier level this one repeats, which is how a Kueue
// level that is already in the walk is reported without being walked twice.
type topologyLevel struct {
	Label   string          `json:"label"`
	AliasOf string          `json:"aliasOf"`
	Source  string          `json:"source"`
	Groups  []topologyGroup `json:"groups"`
}

// topologyGroup is the set of nodes that share one value of a level's label,
// with the GPU arithmetic over them.
type topologyGroup struct {
	Value             string   `json:"value"`
	Nodes             []string `json:"nodes"`
	Allocatable       int      `json:"allocatable"`
	Requested         int      `json:"requested"`
	Free              int      `json:"free"`
	LargestFreeOnNode int      `json:"largestFreeOnNode"`
}

// topologyFit is the answer to --for: which levels the job fits at, and the
// narrowest of them.
type topologyFit struct {
	Job           string             `json:"job"`
	Pods          int                `json:"pods"`
	GPUsPerPod    int                `json:"gpusPerPod"`
	SmallestLevel string             `json:"smallestLevel"`
	Levels        []topologyFitLevel `json:"levels"`
}

// topologyFitLevel is one level's verdict for the job.
type topologyFitLevel struct {
	Label  string   `json:"label"`
	Fits   bool     `json:"fits"`
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

	for _, want := range []string{cliqueZero, cliqueOne} {
		group := groupByValue(t, level, want)
		if len(group.Nodes) != nodesPerClique {
			t.Errorf("clique %s holds nodes %v, want %d of them",
				want, group.Nodes, nodesPerClique)
		}
		if group.Allocatable != nodesPerClique*gpusPerNode {
			t.Errorf("clique %s allocatable = %d, want %d",
				want, group.Allocatable, nodesPerClique*gpusPerNode)
		}
		// Nothing is scheduled on this lane: every job it carries is
		// suspended, so requested is zero and free is all of it.
		if group.Requested != 0 {
			t.Errorf("clique %s requested = %d, want 0; something is running on the lane",
				want, group.Requested)
		}
		if group.Free != nodesPerClique*gpusPerNode {
			t.Errorf("clique %s free = %d, want %d",
				want, group.Free, nodesPerClique*gpusPerNode)
		}
		if group.LargestFreeOnNode != gpusPerNode {
			t.Errorf("clique %s largest free on one node = %d, want %d",
				want, group.LargestFreeOnNode, gpusPerNode)
		}
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

// TestJobNeedingEightGPUsFitsInOneClique is the narrow case: two pods of four
// GPUs need two nodes, which is exactly what a clique has. One node cannot
// hold both pods, so the clique is the narrowest level that works and the
// answer discriminates the node level from the clique level.
func TestJobNeedingEightGPUsFitsInOneClique(t *testing.T) {
	name := createSuspendedGPUJob(t, "fit-one-clique", 2, gpusPerNode)

	report := topologyJSON(t, "topology", "--for", "job/"+name,
		"-n", jobNamespace, "-o", "json")

	fit := requireFit(t, report)
	if fit.SmallestLevel != cliqueLabel {
		t.Errorf("smallest level = %q, want %q\nlevels: %s",
			fit.SmallestLevel, cliqueLabel, formatFitLevels(fit.Levels))
	}
	if fit.Pods != 2 || fit.GPUsPerPod != gpusPerNode {
		t.Errorf("fit reports %d pods of %d GPUs, want 2 of %d",
			fit.Pods, fit.GPUsPerPod, gpusPerNode)
	}

	// Exactly one clique holds it: both do, in fact, since both have eight
	// free GPUs on two nodes. What must not happen is the level reporting a
	// group that cannot host the pods.
	level := fitLevelByLabel(t, fit, cliqueLabel)
	if !level.Fits {
		t.Fatalf("level %s does not fit an 8 GPU job: %+v", cliqueLabel, level)
	}
	sort.Strings(level.Groups)
	want := []string{cliqueOne, cliqueZero}
	if strings.Join(level.Groups, ",") != strings.Join(want, ",") {
		t.Errorf("clique groups that fit = %v, want %v", level.Groups, want)
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
	if fit.SmallestLevel != domainLabel {
		t.Errorf("smallest level = %q, want %q\nlevels: %s",
			fit.SmallestLevel, domainLabel, formatFitLevels(fit.Levels))
	}

	// The two levels it must have rejected on the way, named so a failure
	// says which one wrongly accepted.
	for _, label := range []string{cliqueLabel, subDomainLabel} {
		level := fitLevelByLabel(t, fit, label)
		if level.Fits {
			t.Errorf("level %s reports a fit for a job needing 3 nodes, but every %s group has %d: %+v",
				label, label, nodesPerClique, level)
		}
	}

	domain := fitLevelByLabel(t, fit, domainLabel)
	if !domain.Fits {
		t.Fatalf("level %s does not fit a 12 GPU job: %+v", domainLabel, domain)
	}
	if len(domain.Groups) != 1 {
		t.Errorf("accelerator domain groups that fit = %v, want exactly one", domain.Groups)
	}
}

// TestKueueLevelsAppearInTheReport covers the Kueue read. The lane declares a
// Topology whose levels are both already in the walk, so each has to be
// reported as an alias rather than repeated as a level of its own: a report
// that walks kubernetes.io/hostname a second time would group every node on
// its own name and claim a level nobody can schedule at.
func TestKueueLevelsAppearInTheReport(t *testing.T) {
	report := topologyJSON(t, "topology", "-o", "json")

	for _, label := range kueueLevels {
		level := levelByLabel(t, report, label)
		if level.Source != "kueue" && level.AliasOf == "" {
			t.Errorf("Kueue level %s is reported with source %q and aliasOf %q; "+
				"it is neither attributed to Kueue nor marked an alias",
				label, level.Source, level.AliasOf)
		}
	}

	// Both of the lane's Kueue levels are aliases, so neither may appear
	// twice in the walk.
	seen := make(map[string]int)
	for _, level := range report.Levels {
		seen[level.Label]++
	}
	for label, count := range seen {
		if count > 1 {
			t.Errorf("level %s appears %d times; a level that repeats an earlier one is an alias, not a level",
				label, count)
		}
	}
}

// TestTopologyForAMissingJobIsAUsageError pins the code a script branches on.
// A job the caller named and the cluster does not have is a bad argument, not
// a cluster failure, so it is 2 and not 1 or 4.
func TestTopologyForAMissingJobIsAUsageError(t *testing.T) {
	res := run(t, "topology", "--for", "job/no-such-job-on-this-lane",
		"-n", jobNamespace, "-o", "json")

	if res.code != 2 {
		t.Errorf("exit code = %d, want 2\nstdout:\n%s\nstderr:\n%s",
			res.code, res.stdout, res.stderr)
	}
	// A caller parsing stdout must not be handed a partial report.
	if strings.TrimSpace(res.stdout) != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
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

// levelByLabel returns the level with that label, failing with the labels that
// are there when it is absent.
func levelByLabel(t *testing.T, report topologyReport, label string) topologyLevel {
	t.Helper()

	var labels []string
	for _, level := range report.Levels {
		if level.Label == label {
			return level
		}
		labels = append(labels, level.Label)
	}
	t.Fatalf("no level %s in the report; it has %v", label, labels)

	return topologyLevel{}
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

// fitLevelByLabel returns one level's verdict for the job.
func fitLevelByLabel(t *testing.T, fit topologyFit, label string) topologyFitLevel {
	t.Helper()

	for _, level := range fit.Levels {
		if level.Label == label {
			return level
		}
	}
	t.Fatalf("the fit answer says nothing about level %s: %s", label, formatFitLevels(fit.Levels))

	return topologyFitLevel{}
}

// formatFitLevels renders the walk in order, so a failure shows where the
// command stopped rather than only which level it named.
func formatFitLevels(levels []topologyFitLevel) string {
	parts := make([]string, 0, len(levels))
	for _, level := range levels {
		parts = append(parts, fmt.Sprintf("%s fits=%t groups=%v", level.Label, level.Fits, level.Groups))
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
