//go:build e2e

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

package e2e

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
)

// This file is the healthy-lane gate. It carries the "e2e" tag alone, so it
// runs under `make e2e-mokka`, which injects nothing, as well as under
// `make e2e-faults`, which runs it after the fault subtests have recovered.
// That second run is what makes a recover that quietly did nothing visible: a
// fault left behind shows up here as a warning on a lane that is supposed to
// be clean.
//
// What it asserts is deliberately not "health printed something". The summary
// numbers are compared against the same numbers read from the API server
// through client-go, so the command and the cluster are two independent
// witnesses to the same facts. A report built from a stale cache, a report
// that silently dropped a node and a report that counted the control plane's
// hint labels as GPUs all differ from the cluster, and all three would pass a
// test that only checked the finding list was empty.

// gpuResource is the extended resource the device plugin advertises, and the
// one the health summary's GPU totals are sums of.
const gpuResource = v1.ResourceName("nvidia.com/gpu")

// The hint labels that put a node with no GPU capacity into the report's
// NodesChecked. They are what pkg/source calls a hinted node, and the control
// plane of a Mokka lane carries both: the simulated-GPU DaemonSet tolerates
// every taint, so it labels a node no operand will ever land on.
const (
	pciVendorLabel  = "feature.node.kubernetes.io/pci-10de.present"
	gpuPresentLabel = "nvidia.com/gpu.present"
)

// severityRank orders the severities so that "at warning or above" is one
// comparison rather than a set membership test repeated in every assertion.
var severityRank = map[string]int{"info": 1, "warning": 2, "error": 3}

// warningRank is the threshold the healthy gate is about.
const warningRank = 2

// finding is one entry of a health report, as the JSON carries it.
type finding struct {
	Code      string            `json:"code"`
	Severity  string            `json:"severity"`
	Node      string            `json:"node"`
	Component string            `json:"component"`
	Message   string            `json:"message"`
	Evidence  map[string]string `json:"evidence"`
	Docs      string            `json:"docs"`
	Next      string            `json:"next"`
}

// String renders a finding for a failure message, short enough to read in a
// list and complete enough to act on.
func (f finding) String() string {
	return fmt.Sprintf("%s %s node=%q component=%q evidence=%v: %s",
		f.Code, f.Severity, f.Node, f.Component, f.Evidence, f.Message)
}

// summary is the arithmetic half of a health report.
type summary struct {
	Errors            int   `json:"errors"`
	Warnings          int   `json:"warnings"`
	Infos             int   `json:"infos"`
	NodesChecked      int   `json:"nodesChecked"`
	GPUCapacity       int64 `json:"gpuCapacity"`
	GPUAllocatable    int64 `json:"gpuAllocatable"`
	GPURequested      int64 `json:"gpuRequested"`
	GPURequestedKnown bool  `json:"gpuRequestedKnown"`
}

// healthReport is what `kubectl nv health -o json` prints.
type healthReport struct {
	APIVersion  string    `json:"apiVersion"`
	Kind        string    `json:"kind"`
	Findings    []finding `json:"findings"`
	Partial     bool      `json:"partial"`
	Unreachable []struct {
		Resource string `json:"resource"`
		Reason   string `json:"reason"`
	} `json:"unreachable"`
	Summary summary `json:"summary"`
}

// atOrAbove returns the findings at or above a severity rank.
func (r healthReport) atOrAbove(rank int) []finding {
	var out []finding
	for _, f := range r.Findings {
		if severityRank[f.Severity] >= rank {
			out = append(out, f)
		}
	}

	return out
}

// codes returns the distinct codes in the report, sorted.
func (r healthReport) codes() []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range r.Findings {
		if f.Code == "" || seen[f.Code] {
			continue
		}
		seen[f.Code] = true
		out = append(out, f.Code)
	}
	sort.Strings(out)

	return out
}

// healthJSON runs `kubectl nv health -o json` with extra arguments and decodes
// what it printed.
//
// The exit code is returned rather than asserted: a run that found something
// exits 1 and still prints the report, and the tests that poll for a finding
// need the report more than they need the code.
func healthJSON(t *testing.T, args ...string) (healthReport, int) {
	t.Helper()

	res := run(t, append([]string{"health", "-o", "json"}, args...)...)

	report, err := decodeHealthReport(res.stdout)
	if err != nil {
		t.Fatalf("%v\nexit code %d\nstdout:\n%s\nstderr:\n%s",
			err, res.code, res.stdout, res.stderr)
	}

	return report, res.code
}

// decodeHealthReport turns what the binary printed into a report.
//
// Separate from healthJSON so that the failure has somewhere to be tested. A
// command that printed a usage message, or nothing at all, on the stream a
// consumer parses is a real failure mode and not one a live lane can be asked
// to produce on demand.
func decodeHealthReport(stdout string) (healthReport, error) {
	var report healthReport
	if err := json.Unmarshal([]byte(stdout), &report); err != nil {
		return healthReport{}, fmt.Errorf("decoding the health report: %w", err)
	}

	return report, nil
}

// gateProblems is the healthy gate's whole judgement, as a function of a
// report rather than of a *testing.T.
//
// Pure on purpose. The live gate calls it against a lane, and
// faults_offline_test.go calls it against scripted reports that no lane could
// be made to produce on demand: a partial read, an unknown severity, a report
// that carries a warning and exits 0 anyway. A judgement that lived inside the
// assertions could only ever be tested by breaking a cluster into the right
// shape, which for half of these shapes is not possible at all.
func gateProblems(report healthReport, code int) []string {
	var problems []string

	for _, f := range report.atOrAbove(warningRank) {
		problems = append(problems, "finding on a lane that should be clean: "+f.String())
	}

	// A severity the report carries that this suite does not know how to rank
	// is not a clean report. severityRank answers 0 for one, so atOrAbove
	// above silently passes it: the gate would then be blind to exactly the
	// finding a new severity was added for.
	for _, f := range report.Findings {
		if _, known := severityRank[f.Severity]; !known {
			problems = append(problems,
				fmt.Sprintf("finding %s carries severity %q, which this suite cannot rank", f.Code, f.Severity))
		}
	}

	if code != 0 {
		problems = append(problems,
			fmt.Sprintf("exit code = %d, want 0 with --fail-on warning on a clean lane; the report says %s",
				code, report.Summary))
	}

	if report.Partial {
		problems = append(problems,
			fmt.Sprintf("the report is partial, with %v unreachable; a lane the suite brought up is fully readable",
				report.Unreachable))
	}

	if report.Summary.Errors != 0 || report.Summary.Warnings != 0 {
		problems = append(problems,
			fmt.Sprintf("summary counts %d errors and %d warnings, want 0 and 0",
				report.Summary.Errors, report.Summary.Warnings))
	}

	return problems
}

// laneFacts is what the cluster says about itself, read through client-go and
// out of hack/mokka/lib.sh rather than out of another kubectl-nv command.
type laneFacts struct {
	// gpuNodes advertise nvidia.com/gpu capacity.
	gpuNodes int
	// hinted carry a GPU hint label and no capacity. The control plane of a
	// Mokka lane is one: the simulated-GPU DaemonSet tolerates every taint, so
	// it labels a node no operand will ever land on.
	hinted int
	// capacity and allocatable are the whole-GPU numbers summed over the
	// nodes, as the API server reports them.
	capacity    int64
	allocatable int64
	// fromProfile is the capacity the lane script's own profile table says
	// this lane should have: workers times the profile's device count.
	fromProfile int64
}

// summaryProblems compares the report's arithmetic against the cluster's.
func summaryProblems(report healthReport, facts laneFacts) []string {
	var problems []string

	if report.Summary.GPUCapacity != facts.capacity {
		problems = append(problems, fmt.Sprintf("summary gpuCapacity = %d, the API server says %d",
			report.Summary.GPUCapacity, facts.capacity))
	}
	if report.Summary.GPUAllocatable != facts.allocatable {
		problems = append(problems, fmt.Sprintf("summary gpuAllocatable = %d, the API server says %d",
			report.Summary.GPUAllocatable, facts.allocatable))
	}
	if facts.capacity != facts.fromProfile {
		problems = append(problems, fmt.Sprintf(
			"the cluster advertises %d GPUs and hack/mokka/lib.sh says this profile puts %d on the lane",
			facts.capacity, facts.fromProfile))
	}
	// Equal on a healthy lane, and the first number a reader looks at: a gap
	// between them is a device the scheduler cannot place.
	if facts.allocatable != facts.capacity {
		problems = append(problems, fmt.Sprintf(
			"the cluster has %d GPUs of capacity and %d allocatable; the lane is not healthy",
			facts.capacity, facts.allocatable))
	}
	if want := facts.gpuNodes + facts.hinted; report.Summary.NodesChecked != want {
		problems = append(problems, fmt.Sprintf(
			"summary nodesChecked = %d, want %d (%d with GPU capacity, %d hinted by label)",
			report.Summary.NodesChecked, want, facts.gpuNodes, facts.hinted))
	}
	// The lane runs no GPU workload, so nothing is requested. Reported as
	// unknown instead would mean the pods were never listed, which is a
	// different answer and a worse one: a report that cannot see the pods
	// cannot see the operands either.
	if !report.Summary.GPURequestedKnown {
		problems = append(problems, "the summary says the GPU requests are unknown; the pods were not listed")
	} else if report.Summary.GPURequested != 0 {
		problems = append(problems, fmt.Sprintf(
			"summary gpuRequested = %d; nothing on this lane asks for a GPU", report.Summary.GPURequested))
	}

	return problems
}

// readLaneFacts gathers the cluster's own account of itself.
func readLaneFacts(t *testing.T) laneFacts {
	t.Helper()

	var facts laneFacts
	for _, node := range clusterNodes(t) {
		capacity := node.Status.Capacity[gpuResource]
		allocatable := node.Status.Allocatable[gpuResource]
		facts.capacity += capacity.Value()
		facts.allocatable += allocatable.Value()

		switch {
		case capacity.Value() > 0:
			facts.gpuNodes++
		case node.Labels[pciVendorLabel] == "true" || node.Labels[gpuPresentLabel] == "true":
			facts.hinted++
		}
	}

	facts.fromProfile = int64(len(workerNodeNames(t))) * profileGPUCount(t, laneProfile())

	return facts
}

// TestHealthReportsAHealthyLaneClean is the gate. A Mokka lane that nothing
// has been injected into has to come back with nothing at warning or above.
//
// Under "e2e,faults" this file sorts after faults_test.go, so the Go runner
// reaches it once every injected fault has been recovered. That ordering is
// what makes it the second half of the fault suite as well as the gate for
// e2e-mokka: a recover that quietly did nothing shows up here as a warning on
// a lane that is supposed to be clean. The ordering is a property of the file
// names and is written down because nothing enforces it; a file renamed to
// sort before faults_test.go would still pass, having tested a lane nobody had
// touched yet.
func TestHealthReportsAHealthyLaneClean(t *testing.T) {
	settleLane(t)

	report, code := healthJSON(t, "--fail-on", "warning")

	for _, problem := range gateProblems(report, code) {
		t.Error(problem)
	}
}

// TestHealthSummaryMatchesTheCluster compares the report's arithmetic against
// the API server's own numbers and against the lane script's profile table.
//
// Three witnesses and not two. A summary checked against `kubectl nv list`
// would agree with it whenever both were wrong in the same way, and a capacity
// checked only against the API server agrees with it whenever the device
// plugin advertises the wrong number, which is a fault this suite injects on
// purpose.
func TestHealthSummaryMatchesTheCluster(t *testing.T) {
	settleLane(t)

	report, _ := healthJSON(t)
	facts := readLaneFacts(t)

	if facts.gpuNodes != len(workerNodeNames(t)) {
		t.Errorf("%d node(s) advertise GPUs, want one per worker (%d); the lane did not come up as expected",
			facts.gpuNodes, len(workerNodeNames(t)))
	}
	if facts.capacity <= 0 {
		t.Fatalf("the cluster advertises %d GPUs in total; there is nothing here to check a report against",
			facts.capacity)
	}

	for _, problem := range summaryProblems(report, facts) {
		t.Error(problem)
	}
}

// String renders the summary for a failure message.
func (s summary) String() string {
	return fmt.Sprintf("%d errors, %d warnings, %d infos over %d nodes, %d GPUs capacity, %d allocatable",
		s.Errors, s.Warnings, s.Infos, s.NodesChecked, s.GPUCapacity, s.GPUAllocatable)
}

// TestExplainAnswersForEveryCatalogueCode runs `explain` for every code the
// built binary knows, and for every code the lane's own report carries.
//
// The catalogue list comes out of the binary rather than out of the source
// tree: `explain` prints it when asked for a code that does not exist, which
// is the one place a user is shown what they could have asked for. Reading the
// list from docs/findings instead would pass against a binary that embedded
// none of it.
func TestExplainAnswersForEveryCatalogueCode(t *testing.T) {
	codes := catalogueCodes(t)
	if len(codes) == 0 {
		t.Fatal("the binary lists no finding codes")
	}

	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			res := run(t, "explain", code)
			if res.code != 0 {
				t.Fatalf("explain %s exited %d\nstderr:\n%s", code, res.code, res.stderr)
			}
			// Both streams, which is weaker than it should be and is
			// deliberate for now. cmd/kubectl-nv/main.go points cobra's own
			// output stream at stderr for everything except --help, and
			// explain writes through that stream, so `kubectl nv explain
			// NV-NODE-002 > entry.md` writes an empty file today. The same
			// goes for `version`. Which stream is right is a decision for the
			// owners of main.go and explain.go rather than for this file, and
			// pinning stdout here would turn their open question into a red
			// e2e lane. What this test is for either way is that the built
			// binary HAS the entry: a catalogue that did not reach the binary
			// fails this whichever stream it would have printed on.
			printed := res.stdout + res.stderr
			if !strings.Contains(printed, code) {
				t.Errorf("explain %s printed an entry that does not name the code:\n%s", code, printed)
			}
			if !strings.Contains(printed, "## Reproduction") {
				t.Errorf("explain %s printed an entry with no Reproduction section:\n%s", code, printed)
			}
		})
	}

	// Every code the lane itself reported has to be among them. On a clean
	// lane this is usually empty, which is why the catalogue loop above is the
	// substance of this test and this is the part that only bites when a rule
	// reports a code the binary cannot explain.
	report, _ := healthJSON(t, "--fail-on", "info")
	known := map[string]bool{}
	for _, code := range codes {
		known[code] = true
	}
	for _, code := range report.codes() {
		if !known[code] {
			t.Errorf("the report carries %s, which the binary cannot explain", code)
		}
	}
}

// catalogueCodes asks the binary for a code it does not have, and reads the
// list of codes out of the usage error that comes back.
func catalogueCodes(t *testing.T) []string {
	t.Helper()

	// A code of the right shape and a number no rule will ever use, so the
	// failure is "no such entry" rather than "that is not a code".
	res := run(t, "explain", "NV-NODE-999")
	if res.code != 2 {
		t.Fatalf("explain NV-NODE-999 exited %d, want 2 (a usage error)\nstdout:\n%s\nstderr:\n%s",
			res.code, res.stdout, res.stderr)
	}

	const marker = "the codes are "
	index := strings.Index(res.stderr, marker)
	if index < 0 {
		t.Fatalf("explain did not list the codes it knows:\n%s", res.stderr)
	}

	list := res.stderr[index+len(marker):]
	if end := strings.IndexAny(list, "\n"); end >= 0 {
		list = list[:end]
	}

	var codes []string
	for _, code := range strings.Split(list, ",") {
		code = strings.TrimSpace(code)
		if code != "" {
			codes = append(codes, code)
		}
	}
	sort.Strings(codes)

	return codes
}
