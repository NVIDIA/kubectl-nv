//go:build scale

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

// Tests for the arithmetic and the JSON reading the scale suite does. They
// carry the "scale" tag but not "e2e", so "go test -tags scale ./test/e2e/..."
// runs them with no cluster, no binary and no KUBECONFIG: the suite's own
// logic is the thing most likely to be wrong, and it is the only part that can
// be checked without a 2,000 node lane.
package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMedianDuration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   []time.Duration
		want time.Duration
	}{
		{"one sample", []time.Duration{3 * time.Second}, 3 * time.Second},
		// The input is deliberately unsorted: a median that reads the middle
		// index of the caller's slice gets 9s here and fails.
		{"three unsorted", []time.Duration{9 * time.Second, 2 * time.Second, 4 * time.Second}, 4 * time.Second},
		{"three sorted", []time.Duration{2 * time.Second, 4 * time.Second, 9 * time.Second}, 4 * time.Second},
		// An even count averages the two middle samples rather than picking
		// one, so 2s and 4s give 3s and not either input.
		{"four samples", []time.Duration{8 * time.Second, 2 * time.Second, 4 * time.Second, 1 * time.Second}, 3 * time.Second},
		{"empty", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := append([]time.Duration(nil), tc.in...)
			got := medianDuration(in)
			if got != tc.want {
				t.Errorf("medianDuration(%v) = %v, want %v", tc.in, got, tc.want)
			}
			// Reporting must not reorder the caller's samples: the report
			// prints the runs in the order they were measured.
			for i := range in {
				if in[i] != tc.in[i] {
					t.Fatalf("medianDuration sorted its argument in place: %v, was %v", in, tc.in)
				}
			}
		})
	}
}

func TestMaxDuration(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		in   []time.Duration
		want time.Duration
	}{
		{"largest last", []time.Duration{1 * time.Second, 7 * time.Second}, 7 * time.Second},
		{"largest first", []time.Duration{7 * time.Second, 1 * time.Second}, 7 * time.Second},
		{"empty", nil, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := maxDuration(tc.in); got != tc.want {
				t.Errorf("maxDuration(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// env builds a getenv function over a fixed map, so a case says exactly which
// variables are set and the process environment cannot reach the subject.
func env(pairs map[string]string) func(string) string {
	return func(k string) string { return pairs[k] }
}

func TestScaleConfigDefaults(t *testing.T) {
	t.Parallel()

	cfg, err := scaleConfigFrom(env(nil))
	if err != nil {
		t.Fatalf("scaleConfigFrom with nothing set: %v", err)
	}
	if cfg.Nodes != 2000 {
		t.Errorf("Nodes = %d, want 2000", cfg.Nodes)
	}
	if cfg.ListThreshold != 5*time.Second {
		t.Errorf("ListThreshold = %v, want 5s", cfg.ListThreshold)
	}
	if cfg.HealthThreshold != 15*time.Second {
		t.Errorf("HealthThreshold = %v, want 15s", cfg.HealthThreshold)
	}
	if cfg.Runs != 3 {
		t.Errorf("Runs = %d, want 3", cfg.Runs)
	}
	if cfg.HealthCountPath != "summary.nodesChecked" {
		t.Errorf("HealthCountPath = %q, want %q", cfg.HealthCountPath, "summary.nodesChecked")
	}
}

func TestScaleConfigOverrides(t *testing.T) {
	t.Parallel()

	cfg, err := scaleConfigFrom(env(map[string]string{
		"KUBECTL_NV_SCALE_NODES":             "20",
		"KUBECTL_NV_SCALE_LIST_THRESHOLD":    "90s",
		"KUBECTL_NV_SCALE_HEALTH_THRESHOLD":  "2m",
		"KUBECTL_NV_SCALE_RUNS":              "1",
		"KUBECTL_NV_SCALE_HEALTH_COUNT_PATH": "summary.nodes",
		"KUBECTL_NV_SCALE_REPORT_DIR":        "/tmp/somewhere",
	}))
	if err != nil {
		t.Fatalf("scaleConfigFrom with overrides: %v", err)
	}
	if cfg.Nodes != 20 {
		t.Errorf("Nodes = %d, want 20", cfg.Nodes)
	}
	if cfg.ListThreshold != 90*time.Second {
		t.Errorf("ListThreshold = %v, want 90s", cfg.ListThreshold)
	}
	if cfg.HealthThreshold != 2*time.Minute {
		t.Errorf("HealthThreshold = %v, want 2m", cfg.HealthThreshold)
	}
	if cfg.Runs != 1 {
		t.Errorf("Runs = %d, want 1", cfg.Runs)
	}
	if cfg.HealthCountPath != "summary.nodes" {
		t.Errorf("HealthCountPath = %q, want %q", cfg.HealthCountPath, "summary.nodes")
	}
	if cfg.ReportDir != "/tmp/somewhere" {
		t.Errorf("ReportDir = %q, want %q", cfg.ReportDir, "/tmp/somewhere")
	}
}

func TestScaleConfigRejectsBadValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		vars map[string]string
		want string
	}{
		{
			"nodes is not a number",
			map[string]string{"KUBECTL_NV_SCALE_NODES": "many"},
			`KUBECTL_NV_SCALE_NODES="many": want a positive whole number`,
		},
		{
			// Zero fake nodes would make every count assertion below pass
			// against an empty lane, which is the one answer the suite must
			// never accept.
			"nodes is zero",
			map[string]string{"KUBECTL_NV_SCALE_NODES": "0"},
			`KUBECTL_NV_SCALE_NODES="0": want a positive whole number`,
		},
		{
			"nodes is negative",
			map[string]string{"KUBECTL_NV_SCALE_NODES": "-5"},
			`KUBECTL_NV_SCALE_NODES="-5": want a positive whole number`,
		},
		{
			"threshold has no unit",
			map[string]string{"KUBECTL_NV_SCALE_LIST_THRESHOLD": "5"},
			`KUBECTL_NV_SCALE_LIST_THRESHOLD="5": want a Go duration such as 5s`,
		},
		{
			"threshold is not positive",
			map[string]string{"KUBECTL_NV_SCALE_HEALTH_THRESHOLD": "0s"},
			`KUBECTL_NV_SCALE_HEALTH_THRESHOLD="0s": want a positive duration`,
		},
		{
			// An even number of runs would make the median an average of two
			// samples; the report says "median of N runs" and three is what
			// the brief fixed.
			"runs is zero",
			map[string]string{"KUBECTL_NV_SCALE_RUNS": "0"},
			`KUBECTL_NV_SCALE_RUNS="0": want a positive whole number`,
		},
		{
			"count path is empty",
			map[string]string{"KUBECTL_NV_SCALE_HEALTH_COUNT_PATH": "   "},
			`KUBECTL_NV_SCALE_HEALTH_COUNT_PATH="   ": want a dotted JSON path`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := scaleConfigFrom(env(tc.vars))
			if err == nil {
				t.Fatalf("scaleConfigFrom(%v) returned no error, want %q", tc.vars, tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

// listJSON is the shape "kubectl nv list -o json" prints: a GPUNodeList whose
// items each carry metadata.name.
const listJSON = `{
  "apiVersion": "nv.nvidia.com/v1alpha1",
  "kind": "GPUNodeList",
  "items": [
    {"metadata": {"name": "kubectl-nv-scale-worker"}},
    {"metadata": {"name": "kwok-gpu-1"}},
    {"metadata": {"name": "kwok-gpu-2"}}
  ]
}`

func TestNodeNames(t *testing.T) {
	t.Parallel()

	got, err := nodeNames([]byte(listJSON))
	if err != nil {
		t.Fatalf("nodeNames: %v", err)
	}
	want := []string{"kubectl-nv-scale-worker", "kwok-gpu-1", "kwok-gpu-2"}
	if len(got) != len(want) {
		t.Fatalf("nodeNames = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("nodeNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNodeNamesRejectsAnUnnamedItem(t *testing.T) {
	t.Parallel()

	// An item with no metadata.name would otherwise be counted as a node with
	// the empty name, and a list of 2,000 empty names still has length 2,000.
	_, err := nodeNames([]byte(`{"items":[{"metadata":{"name":"a"}},{"metadata":{}}]}`))
	if err == nil {
		t.Fatal("nodeNames accepted an item with no metadata.name")
	}
	const want = `item 1 of the list has no metadata.name`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

func TestNodeNamesRejectsNonJSON(t *testing.T) {
	t.Parallel()

	if _, err := nodeNames([]byte("NAME  PRODUCT\nworker  T4\n")); err == nil {
		t.Fatal("nodeNames accepted table output as JSON")
	}
}

func TestCountWithPrefix(t *testing.T) {
	t.Parallel()

	names := []string{"kwok-gpu-1", "kwok-gpu-20", "worker", "kwok-gpuish"}
	if got := countWithPrefix(names, "kwok-gpu-"); got != 2 {
		t.Errorf("countWithPrefix = %d, want 2", got)
	}
	if got := countWithPrefix(names, "worker"); got != 1 {
		t.Errorf("countWithPrefix = %d, want 1", got)
	}
}

func TestCountAtPath(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		path string
		want int
	}{
		{"a number one level down", `{"summary":{"nodesChecked":2002}}`, "summary.nodesChecked", 2002},
		{"a number at the top", `{"nodes":7}`, "nodes", 7},
		{"an array counts its elements", `{"items":[1,2,3]}`, "items", 3},
		{"zero is a legitimate count", `{"summary":{"nodesChecked":0}}`, "summary.nodesChecked", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, err := countAtPath([]byte(tc.raw), tc.path)
			if err != nil {
				t.Fatalf("countAtPath(%s): %v", tc.path, err)
			}
			if got != tc.want {
				t.Errorf("countAtPath(%s) = %d, want %d", tc.path, got, tc.want)
			}
		})
	}
}

func TestCountAtPathReportsWhatItFoundInstead(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  string
		path string
		want string
	}{
		{
			// The field names of the health summary are not frozen. A path
			// that does not resolve has to stop the suite and say so: the
			// alternative is a count assertion that silently compares nothing.
			"the key is absent",
			`{"summary":{"nodes":1}}`,
			"summary.nodesChecked",
			`no "nodesChecked" under "summary" in the output; "summary" holds: nodes`,
		},
		{
			"the first segment is absent",
			`{"status":"ok"}`,
			"summary.nodesChecked",
			`no "summary" at the top of the output; the output holds: status`,
		},
		{
			"the value is a string",
			`{"summary":{"nodesChecked":"2002"}}`,
			"summary.nodesChecked",
			`"summary.nodesChecked" is a string, want a number or an array`,
		},
		{
			"the value is not a whole number",
			`{"summary":{"nodesChecked":2.5}}`,
			"summary.nodesChecked",
			`"summary.nodesChecked" is 2.5, want a whole number`,
		},
		{
			"a segment is not an object",
			`{"summary":[1,2]}`,
			"summary.nodesChecked",
			`"summary" is not an object, so "nodesChecked" cannot be read from it`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := countAtPath([]byte(tc.raw), tc.path)
			if err == nil {
				t.Fatalf("countAtPath(%s) returned no error, want %q", tc.path, tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestCountAtPathRejectsNonJSON(t *testing.T) {
	t.Parallel()

	// The binary prints a table when -o json is dropped by mistake. That has
	// to be an error naming the decode, not a count of zero.
	_, err := countAtPath([]byte("NAME  GPUS\nworker  4\n"), "summary.nodesChecked")
	if err == nil {
		t.Fatal("countAtPath accepted table output as JSON")
	}
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Errorf("error = %v (%T), want it to wrap a *json.SyntaxError", err, err)
	}
}

// ---------------------------------------------------------------------------
// assessList
// ---------------------------------------------------------------------------
//
// The cases here are the ones a reviewer drove a fake binary through. Each one
// is an answer that is wrong and fast, which is the only kind of wrong answer
// a timing suite is tempted to accept.

// listOf renders a GPUNodeList naming exactly these nodes.
func listOf(names ...string) []byte {
	items := make([]string, 0, len(names))
	for _, n := range names {
		items = append(items, `{"metadata":{"name":"`+n+`"}}`)
	}

	return []byte(`{"kind":"GPUNodeList","items":[` + strings.Join(items, ",") + `]}`)
}

// fakeAndReal is a lane of n fake nodes plus the two workers, in the order
// the binary sorts them.
func fakeAndReal(n int) []byte {
	names := []string{"kubectl-nv-scale-worker", "kubectl-nv-scale-worker2"}
	for i := 1; i <= n; i++ {
		names = append(names, fmt.Sprintf("kwok-gpu-%d", i))
	}

	return listOf(names...)
}

func TestAssessListAcceptsTheLane(t *testing.T) {
	t.Parallel()

	census, err := assessList(fakeAndReal(20), 20, 2)
	if err != nil {
		t.Fatalf("assessList on a correct lane: %v", err)
	}
	if census.Fake != 20 || census.Real != 2 || census.Total != 22 {
		t.Errorf("census = %+v, want {Fake:20 Real:2 Total:22}", census)
	}
}

func TestAssessListRejectsAWrongLane(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		raw  []byte
		want string
	}{
		{
			// The empty run. Instant, and the fastest number this suite
			// would ever record.
			"no nodes at all",
			listOf(),
			"list is missing 3 of the 3 fake nodes: kwok-gpu-1, kwok-gpu-2, kwok-gpu-3",
		},
		{
			// The count is right and the lane is not: kwok-gpu-1 twice,
			// kwok-gpu-3 gone. A test that only counted would pass here.
			"one node named twice and another missing",
			listOf("kubectl-nv-scale-worker", "kubectl-nv-scale-worker2", "kwok-gpu-1", "kwok-gpu-1", "kwok-gpu-2"),
			"list named 1 node(s) more than once: kwok-gpu-1 (2 times)",
		},
		{
			"a fake node missing",
			listOf("kubectl-nv-scale-worker", "kubectl-nv-scale-worker2", "kwok-gpu-1", "kwok-gpu-2"),
			"list is missing 1 of the 3 fake nodes: kwok-gpu-3",
		},
		{
			// Every expected node is there, and one more besides: a lane
			// that was created twice at different counts and deleted once.
			"a fake node from outside the range",
			listOf("kubectl-nv-scale-worker", "kubectl-nv-scale-worker2",
				"kwok-gpu-1", "kwok-gpu-2", "kwok-gpu-3", "kwok-gpu-9"),
			"list names 1 node(s) outside kwok-gpu-1..3: kwok-gpu-9",
		},
		{
			// A lane that lost a worker. "At least one real node" passes
			// here; an equality does not.
			"a worker gone",
			listOf("kubectl-nv-scale-worker", "kwok-gpu-1", "kwok-gpu-2", "kwok-gpu-3"),
			"list names 1 node(s) that are not kwok-gpu-*, want exactly 2",
		},
		{
			"a node that should not be there at all",
			listOf("kubectl-nv-scale-worker", "kubectl-nv-scale-worker2", "somebody-elses-node",
				"kwok-gpu-1", "kwok-gpu-2", "kwok-gpu-3"),
			"list names 3 node(s) that are not kwok-gpu-*, want exactly 2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := assessList(tc.raw, 3, 2)
			if err == nil {
				t.Fatalf("assessList accepted it; want %q", tc.want)
			}
			if err.Error() != tc.want {
				t.Errorf("error = %q, want %q", err.Error(), tc.want)
			}
		})
	}
}

func TestAssessListCapsALongNameList(t *testing.T) {
	t.Parallel()

	// A 2,000 node lane that answered empty must not print 2,000 names.
	_, err := assessList(listOf(), 2000, 2)
	if err == nil {
		t.Fatal("assessList accepted an empty 2,000 node lane")
	}
	const want = "list is missing 2000 of the 2000 fake nodes: kwok-gpu-1, kwok-gpu-10, kwok-gpu-100, kwok-gpu-1000, kwok-gpu-1001 and 1995 more"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}

// ---------------------------------------------------------------------------
// assessRuns
// ---------------------------------------------------------------------------

func TestAssessRunsWithinTheThreshold(t *testing.T) {
	t.Parallel()

	stats, err := assessRuns([]time.Duration{4 * time.Second, 2 * time.Second, 3 * time.Second}, 5*time.Second)
	if err != nil {
		t.Fatalf("assessRuns under the threshold: %v", err)
	}
	if stats.Median != 3*time.Second {
		t.Errorf("Median = %v, want 3s", stats.Median)
	}
	if stats.Max != 4*time.Second {
		t.Errorf("Max = %v, want 4s", stats.Max)
	}
	if !stats.Within {
		t.Error("Within = false, want true")
	}
}

func TestAssessRunsOverTheThreshold(t *testing.T) {
	t.Parallel()

	// The median decides, not the max: 6s, 7s, 8s is over a 5s threshold on
	// every statistic, so a comparison that was silently dropped shows up.
	stats, err := assessRuns([]time.Duration{6 * time.Second, 7 * time.Second, 8 * time.Second}, 5*time.Second)
	if err == nil {
		t.Fatal("assessRuns accepted a median over the threshold")
	}
	const want = "median 7s over 3 runs, want at most 5s (runs: [6s 7s 8s])"
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	// Within travels into the uploaded report, so a false error with a true
	// flag would still tell a reader the run was fine.
	if stats.Within {
		t.Error("Within = true beside an error, want false")
	}
}

func TestAssessRunsOnTheThresholdExactly(t *testing.T) {
	t.Parallel()

	// The threshold is a limit, not a wall to stay under: exactly 5s passes.
	stats, err := assessRuns([]time.Duration{5 * time.Second}, 5*time.Second)
	if err != nil {
		t.Fatalf("assessRuns at exactly the threshold: %v", err)
	}
	if !stats.Within {
		t.Error("Within = false at exactly the threshold, want true")
	}
}

func TestAssessRunsRejectsNoRuns(t *testing.T) {
	t.Parallel()

	// A command that was never run is not a command that was instant.
	_, err := assessRuns(nil, 5*time.Second)
	if err == nil {
		t.Fatal("assessRuns accepted an empty set of runs")
	}
	if err.Error() != "no runs were measured" {
		t.Errorf("error = %q, want %q", err.Error(), "no runs were measured")
	}
}

// ---------------------------------------------------------------------------
// The report
// ---------------------------------------------------------------------------

func TestScaleReportRoundTrip(t *testing.T) {
	t.Parallel()

	dir := filepath.Join(t.TempDir(), "diagnostics", "nested")
	want := scaleReport{
		RecordedAt:      "2026-10-11T12:00:00Z",
		CommitSHA:       "0123456789abcdef",
		Binary:          "/w/bin/kubectl-nv",
		ExpectedFake:    2000,
		ExpectedReal:    2,
		FakeNodes:       2000,
		RealNodes:       2,
		TotalNodes:      2002,
		PodsTotal:       6042,
		PodsOnFakeNodes: 6000,
		HealthNodes:     2002,
		HealthFindings:  4000,
		HealthCountPath: "summary.nodesChecked",
		Timings: []commandTiming{{
			Command:     "[list -o json]",
			Runs:        []commandRun{{DurationMS: 1200, ExitCode: 0, StdoutLen: 99}},
			MedianMS:    1200,
			MaxMS:       1300,
			ThresholdMS: 5000,
			Within:      true,
		}},
	}

	// The directory does not exist yet: the workflow uploads whatever is at
	// this path, and a suite that needed somebody to have made it first would
	// upload nothing on the one run where it mattered.
	if err := writeScaleReport(dir, want); err != nil {
		t.Fatalf("writeScaleReport: %v", err)
	}

	got, err := readScaleReport(dir)
	if err != nil {
		t.Fatalf("reading back what was written: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the report:\n got %+v\nwant %+v", got, want)
	}
}

func TestScaleReportIsValidJSON(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := writeScaleReport(dir, scaleReport{ExpectedFake: 20}); err != nil {
		t.Fatalf("writeScaleReport: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, reportFile))
	if err != nil {
		t.Fatalf("reading the report: %v", err)
	}
	// The workflow uploads this for a person and for whatever reads it next.
	if !json.Valid(raw) {
		t.Errorf("the report is not valid JSON:\n%s", raw)
	}
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		t.Error("the report does not end with a newline")
	}
}

// ---------------------------------------------------------------------------
// The exit codes a measured run may have
// ---------------------------------------------------------------------------

func TestExitCodePolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		allowed []int
		code    int
		want    bool
	}{
		{"list succeeded", listExitCodes, 0, true},
		// The lane kubeconfig is a cluster-admin on a kind cluster. A partial
		// read is not a slow lane, it is a defect, and timing it is timing a
		// command that answered with less than the lane holds.
		{"list read only part of the cluster", listExitCodes, 3, false},
		{"list hit a usage error", listExitCodes, 2, false},
		{"list could not reach the cluster", listExitCodes, 4, false},
		{"list found findings, which it cannot", listExitCodes, 1, false},
		{"health was clean", healthExitCodes, 0, true},
		// Fake nodes carry no operands, so the operand rules fire and health
		// exits 1 on every run of this lane.
		{"health found findings", healthExitCodes, 1, true},
		{"health read only part of the cluster", healthExitCodes, 3, false},
		{"health could not reach the cluster", healthExitCodes, 4, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := allows(tc.allowed, tc.code); got != tc.want {
				t.Errorf("allows(%v, %d) = %v, want %v", tc.allowed, tc.code, got, tc.want)
			}
		})
	}
}

// TestTheReviewersFakeBinaryIsRejected replays the three answers a reviewer's
// fake binary gave that this suite used to accept. Each is fast and wrong,
// which is the pairing a timing suite exists to be suspicious of.
func TestTheReviewersFakeBinaryIsRejected(t *testing.T) {
	t.Parallel()

	const fake, real = 20, 2

	t.Run("two empty runs and one full run", func(t *testing.T) {
		t.Parallel()

		// Checking only the last run passes this: the full list is last and
		// the median is whatever two instant runs make it.
		runs := [][]byte{listOf(), listOf(), fakeAndReal(fake)}
		rejected := 0
		for i, raw := range runs {
			if _, err := assessList(raw, fake, real); err != nil {
				rejected++
				t.Logf("run %d rejected: %v", i+1, err)
			}
		}
		if rejected != 2 {
			t.Errorf("%d of the 3 runs were rejected, want 2 (both empty ones)", rejected)
		}
	})

	t.Run("a full list beside exit 3", func(t *testing.T) {
		t.Parallel()

		// The list itself is perfect, so no amount of looking at the nodes
		// catches this one. The exit code is the whole signal.
		if _, err := assessList(fakeAndReal(fake), fake, real); err != nil {
			t.Fatalf("the list was correct, so assessList should pass it: %v", err)
		}
		if allows(listExitCodes, 3) {
			t.Error("list is allowed to exit 3, so a partial read would be timed and reported as a pass")
		}
	})

	t.Run("one node listed twice and another missing", func(t *testing.T) {
		t.Parallel()

		// 22 names, the right total, the wrong lane.
		names := []string{"kubectl-nv-scale-worker", "kubectl-nv-scale-worker2", "kwok-gpu-1"}
		for i := 1; i <= fake-1; i++ {
			names = append(names, fmt.Sprintf("kwok-gpu-%d", i))
		}
		raw := listOf(names...)

		census, err := assessList(raw, fake, real)
		if err == nil {
			t.Fatalf("assessList accepted a lane with kwok-gpu-1 twice and kwok-gpu-%d missing (census %+v)", fake, census)
		}
		t.Logf("rejected: %v", err)
	})
}
