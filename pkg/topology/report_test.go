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

// TestReportTableCarriesTheGroupArithmetic pins the columns a reader sees
// without -o.
func TestReportTableCarriesTheGroupArithmetic(t *testing.T) {
	t.Parallel()

	report := topology.Build(topology.Input{Nodes: []inventory.GPUNode{
		node("node-a", 8, 2, map[string]string{topology.LabelClique: "clique-a"}),
	}})

	got := printed(t, "table", report)

	for _, want := range []string{"LEVEL", "GROUP", "NODES", "ALLOCATABLE", "REQUESTED", "FREE", "LARGEST-FREE"} {
		if !strings.Contains(got, want) {
			t.Errorf("the header has no %q column:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "clique-a") {
		t.Errorf("the table does not name the group:\n%s", got)
	}
	if strings.Contains(got, "FITS") {
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

// TestReportTableAnswersTheJobPerGroup: with --for, every row says whether
// that group takes the job, which is the whole point of asking.
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

	if !strings.Contains(got, "FITS") {
		t.Fatalf("the table has no FITS column:\n%s", got)
	}

	for _, line := range strings.Split(got, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "clique" {
			continue
		}
		if fields[len(fields)-1] != "no" {
			t.Errorf("a clique row says %q, want no: three pods of four do not fit in a clique\n%s", fields[len(fields)-1], got)
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
}
