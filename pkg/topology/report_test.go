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
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// printed renders a report through one of the contract's printers.
func printed(t *testing.T, format string, r *topology.TopologyReport) string {
	t.Helper()

	printer, err := output.NewPrinter(format)
	if err != nil {
		t.Fatalf("building the %s printer: %v", format, err)
	}

	out := &bytes.Buffer{}
	if err := printer.PrintObj(r, out); err != nil {
		t.Fatalf("printing the report as %s: %v", format, err)
	}

	return out.String()
}

// TestReportPrintsThroughEveryFormat is the printer half of the envelope
// contract: the cli-runtime printers read TypeMeta off the struct before they
// marshal anything, so a report that NewTopologyReport did not stamp fails at
// -o json, -o yaml and -o name rather than printing an unstamped object.
func TestReportPrintsThroughEveryFormat(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-a", 8, 2, map[string]string{topology.LabelClique: "clique-a"}),
	}})

	for _, format := range []string{"json", "yaml", "name", "table", "wide"} {
		t.Run(format, func(t *testing.T) {
			t.Parallel()

			if got := printed(t, format, report); got == "" {
				t.Errorf("-o %s printed nothing", format)
			}
		})
	}
}

// headerOf returns the table header as fields, so that a column can be
// matched exactly. Matching on a substring would let LARGEST-FREE stand in
// for FREE, and a report that lost the FREE column would still pass.
func headerOf(t *testing.T, printed string) []string {
	t.Helper()

	lines := strings.Split(printed, "\n")
	if len(lines) == 0 {
		t.Fatal("nothing was printed")
	}

	return strings.Fields(lines[0])
}

func hasField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}

	return false
}

// TestReportTableCarriesTheGroupArithmetic pins the columns a reader sees
// without -o.
func TestReportTableCarriesTheGroupArithmetic(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-a", 8, 2, map[string]string{topology.LabelClique: "clique-a"}),
	}})

	got := printed(t, "table", report)
	header := headerOf(t, got)

	want := []string{"LEVEL", "GROUP", "NODES", "ALLOCATABLE", "REQUESTED", "FREE", "LARGEST-FREE"}
	if !equalStrings(header, want) {
		t.Errorf("the header is\n  %v\nwant\n  %v", header, want)
	}
	if !strings.Contains(got, "clique-a") {
		t.Errorf("the table does not name the group:\n%s", got)
	}
	if hasField(header, "FITS") {
		t.Errorf("the table has a FITS column with no job asked about:\n%s", got)
	}
}

// TestReportTableSaysSoWhenTheRequestedGPUsAreUnknown: an empty cell, or a
// zero, would read as "nothing is running there", which is the one thing a
// cluster nobody listed the pods of does not say.
func TestReportTableSaysSoWhenTheRequestedGPUsAreUnknown(t *testing.T) {
	t.Parallel()

	unknown := node("node-a", 8, 0, map[string]string{topology.LabelClique: "clique-a"})
	unknown.RequestedKnown = false

	got := printed(t, "table", topology.Build(topology.Input{Nodes: []inventory.GPUNode{unknown}}))

	if !strings.Contains(got, "<unknown>") {
		t.Errorf("the table prints a number for GPUs nobody read:\n%s", got)
	}
}

// rowsOf returns the table rows whose first cell is level.
func rowsOf(t *testing.T, printed, level string) [][]string {
	t.Helper()

	var out [][]string
	for _, line := range strings.Split(printed, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == level {
			out = append(out, fields)
		}
	}

	return out
}

// TestReportTableAnswersTheJobPerGroup: with --for, every row says whether
// that group takes the job, and the rows of the narrowest level that takes it
// say which one that is. A reader scanning a long table otherwise has to
// remember the level order to find the answer they asked for.
func TestReportTableAnswersTheJobPerGroup(t *testing.T) {
	t.Parallel()

	nodes := twoCliques()
	report := topology.Build(topology.Input{Nodes: nodes})
	report.Fit = report.FitJob(topology.JobRequest{
		Namespace:  "default",
		Name:       "trainer",
		Pods:       3,
		GPUsPerPod: 4,
	}, nodes)

	got := printed(t, "table", report)

	if !hasField(headerOf(t, got), "FITS") {
		t.Fatalf("the table has no FITS column:\n%s", got)
	}

	// Three pods of four fit under the fabric tier and in no clique.
	for _, row := range rowsOf(t, got, "clique") {
		if last := row[len(row)-1]; last != "no" {
			t.Errorf("a clique row says %q, want no:\n%s", last, got)
		}
	}

	tier := rowsOf(t, got, "tier-0")
	if len(tier) == 0 {
		t.Fatalf("the table has no tier-0 row:\n%s", got)
	}
	for _, row := range tier {
		if last := strings.Join(row[len(row)-2:], " "); last != "yes (smallest)" {
			t.Errorf("the tier-0 row ends %q, want %q:\n%s", last, "yes (smallest)", got)
		}
	}

	// The cluster takes it too, but it is not the answer.
	for _, row := range rowsOf(t, got, "cluster") {
		if last := row[len(row)-1]; last != "yes" {
			t.Errorf("the cluster row says %q, want a plain yes:\n%s", last, got)
		}
	}
}

// TestLevelFitMarshalsWithoutNullGroups covers the never-null guarantee on
// the one type a caller is most likely to build by hand: a level where
// nothing fits.
func TestLevelFitMarshalsWithoutNullGroups(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(topology.LevelFit{Level: "clique"})
	if err != nil {
		t.Fatalf("marshalling a level fit: %v", err)
	}

	if !strings.Contains(string(raw), `"groups":[]`) {
		t.Errorf("the level fit marshalled as %s, want an empty groups array", raw)
	}
}

// TestJobFitMarshalsWithoutNullSlices covers the same for the whole answer.
func TestJobFitMarshalsWithoutNullSlices(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(topology.JobFit{Name: "trainer"})
	if err != nil {
		t.Fatalf("marshalling a job fit: %v", err)
	}

	for _, want := range []string{`"levels":[]`, `"excluded":[]`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the job fit marshalled as %s, want it to carry %s", raw, want)
		}
	}
}

// TestNewTopologyReportStartsEmptyAndStamped.
func TestNewTopologyReportStartsEmptyAndStamped(t *testing.T) {
	t.Parallel()

	r := topology.NewTopologyReport()

	if r.Kind != output.KindTopologyReport {
		t.Errorf("kind is %q, want %q", r.Kind, output.KindTopologyReport)
	}
	if r.APIVersion != output.APIVersion {
		t.Errorf("apiVersion is %q, want %q", r.APIVersion, output.APIVersion)
	}
	if r.Levels == nil || r.PartialNodes == nil {
		t.Error("a new report carries nil slices, want empty ones")
	}
}

// TestReportDeepCopyDoesNotShareItsSlices: the printers and the schema work
// pass reports around, and a shallow copy that compiles is a bug nothing else
// would show.
func TestReportDeepCopyDoesNotShareItsSlices(t *testing.T) {
	t.Parallel()

	nodes := twoCliques()
	nodes[0].Unschedulable = true
	original := topology.Build(topology.Input{Nodes: nodes})
	original.Fit = original.FitJob(topology.JobRequest{Name: "trainer", Pods: 1, GPUsPerPod: 1}, nodes)
	original.PartialNodes = []topology.PartialNode{{Name: "node-a", Present: []string{"a"}, Missing: []string{"b"}}}

	copied, ok := original.DeepCopyObject().(*topology.TopologyReport)
	if !ok {
		t.Fatal("DeepCopyObject did not return a *TopologyReport")
	}

	copied.Levels[0].Groups[0].Nodes[0] = "mutated"
	copied.Levels[0].Groups[0].Value = "mutated"
	copied.PartialNodes[0].Present[0] = "mutated"
	copied.PartialNodes[0].Missing[0] = "mutated"
	copied.Fit.Levels[0].Level = "mutated"
	copied.Fit.Excluded[0] = "mutated"
	copied.Fit.Name = "mutated"

	if original.Levels[0].Groups[0].Nodes[0] == "mutated" {
		t.Error("the group's node names are shared with the copy")
	}
	if original.Levels[0].Groups[0].Value == "mutated" {
		t.Error("the group is shared with the copy")
	}
	if original.PartialNodes[0].Present[0] == "mutated" || original.PartialNodes[0].Missing[0] == "mutated" {
		t.Error("the partial node's labels are shared with the copy")
	}
	if original.Fit.Levels[0].Level == "mutated" || original.Fit.Name == "mutated" {
		t.Error("the fit is shared with the copy")
	}
	if original.Fit.Excluded[0] == "mutated" {
		t.Error("the excluded node names are shared with the copy")
	}
}
