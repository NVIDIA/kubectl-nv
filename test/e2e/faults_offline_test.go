//go:build e2e && faults

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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file tests the fault suite's own judgement, against reports no cluster
// could be asked to produce on demand.
//
// The live tests in faults_test.go and health_test.go can only be wrong in one
// direction: they pass. A gate that accepted a warning, a matcher that ignored
// the severity, a clean-check that stopped at the first poll, all look exactly
// like a healthy lane from inside a healthy lane. What tells them apart is
// feeding the same functions a report that is wrong in a named way and
// checking they say so, and that is what this does.
//
// It needs no cluster. The decisions are pure functions of a report, and the
// one place a process is involved, `explain`, is driven by a fake binary
// written into the test's own temporary directory.

// offlineNode is the node the scripted reports are about.
const offlineNode = "kubectl-nv-faults-worker"

// scriptedFinding builds a finding with the defaults a healthy NV-NODE-004
// report would carry, so that each case below changes one thing.
func scriptedFinding(mutate func(*finding)) finding {
	f := finding{
		Code:      "NV-NODE-004",
		Severity:  "warning",
		Node:      offlineNode,
		Component: "node",
		Message:   "the nvidia.com/gpu.count label and the node's GPU capacity disagree",
		Evidence: map[string]string{
			"nvidia.com/gpu.count": "5",
			"capacity":             "4",
			"expectedCapacity":     "5",
		},
		Docs: "docs/findings/NV-NODE-004.md",
	}
	if mutate != nil {
		mutate(&f)
	}

	return f
}

// offlineCase is the fault case the matcher tests run against.
func offlineCase() faultCase {
	return faultCase{
		code:         "NV-NODE-004",
		script:       "nv-node-004.sh",
		severity:     "warning",
		component:    "node",
		evidenceKeys: []string{"nvidia.com/gpu.count", "capacity", "expectedCapacity"},
		exact: func(capacity int64) map[string]string {
			return map[string]string{
				"nvidia.com/gpu.count": "5",
				"capacity":             "4",
				"expectedCapacity":     "5",
			}
		},
	}
}

// TestMatchFindingRejectsTheWrongFinding drives the matcher through every way
// a report can carry the right code and still be the wrong answer.
func TestMatchFindingRejectsTheWrongFinding(t *testing.T) {
	tc := offlineCase()
	exact := tc.exact(4)

	cases := []struct {
		name    string
		finding finding
		want    string
	}{
		{
			name:    "a different severity",
			finding: scriptedFinding(func(f *finding) { f.Severity = "info" }),
			want:    "severity",
		},
		{
			name:    "a different component",
			finding: scriptedFinding(func(f *finding) { f.Component = "nvidia-device-plugin-daemonset" }),
			want:    "component",
		},
		{
			name:    "another node",
			finding: scriptedFinding(func(f *finding) { f.Node = offlineNode + "2" }),
			want:    "no NV-NODE-004 finding for " + offlineNode,
		},
		{
			name: "a missing evidence key",
			finding: scriptedFinding(func(f *finding) {
				delete(f.Evidence, "expectedCapacity")
			}),
			want: "missing evidence expectedCapacity",
		},
		{
			name: "evidence keys with empty values",
			finding: scriptedFinding(func(f *finding) {
				for key := range f.Evidence {
					f.Evidence[key] = ""
				}
			}),
			want: "want",
		},
		{
			name: "a count that is not the one the script wrote",
			finding: scriptedFinding(func(f *finding) {
				f.Evidence["nvidia.com/gpu.count"] = "9"
			}),
			want: `evidence nvidia.com/gpu.count = "9"`,
		},
		{
			name:    "a link to another entry",
			finding: scriptedFinding(func(f *finding) { f.Docs = "docs/findings/NV-NODE-003.md" }),
			want:    "links to",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			report := healthReport{Findings: []finding{tt.finding}}
			match, why := matchFinding(report, tc, offlineNode, exact)
			if why == "" {
				t.Fatalf("the matcher accepted %+v", match)
			}
			if !strings.Contains(why, tt.want) {
				t.Errorf("the matcher said %q, which does not mention %q", why, tt.want)
			}
		})
	}
}

// TestMatchFindingTakesTheRightCandidate pins the behaviour a matcher that
// returned on the first candidate's first mismatch would get wrong: a report
// that holds a finding of the right code at the wrong severity AND the one the
// case is waiting for.
func TestMatchFindingTakesTheRightCandidate(t *testing.T) {
	tc := offlineCase()
	wanted := scriptedFinding(nil)
	report := healthReport{Findings: []finding{
		scriptedFinding(func(f *finding) { f.Severity = "info" }),
		wanted,
	}}

	match, why := matchFinding(report, tc, offlineNode, tc.exact(4))
	if why != "" {
		t.Fatalf("the matcher rejected a report that holds the finding: %s", why)
	}
	if match.Severity != wanted.Severity {
		t.Errorf("the matcher returned the %s finding, want the %s one", match.Severity, wanted.Severity)
	}
}

// TestMatchFindingAcceptsTheRightFinding is the control. Without it the cases
// above would pass against a matcher that rejected everything.
func TestMatchFindingAcceptsTheRightFinding(t *testing.T) {
	tc := offlineCase()
	report := healthReport{Findings: []finding{scriptedFinding(nil)}}

	if _, why := matchFinding(report, tc, offlineNode, tc.exact(4)); why != "" {
		t.Errorf("the matcher rejected the finding it was waiting for: %s", why)
	}
}

// TestGateProblemsRejectsADirtyLane drives the healthy gate through the
// reports it exists to refuse.
func TestGateProblemsRejectsADirtyLane(t *testing.T) {
	warning := scriptedFinding(nil)

	cases := []struct {
		name   string
		report healthReport
		code   int
		want   string
	}{
		{
			name:   "a warning, reported and exited on",
			report: healthReport{Findings: []finding{warning}, Summary: summary{Warnings: 1}},
			code:   1,
			want:   "finding on a lane that should be clean",
		},
		{
			name:   "a warning the exit code does not mention",
			report: healthReport{Findings: []finding{warning}, Summary: summary{Warnings: 1}},
			code:   0,
			want:   "finding on a lane that should be clean",
		},
		{
			name: "an error",
			report: healthReport{
				Findings: []finding{scriptedFinding(func(f *finding) { f.Severity = "error" })},
				Summary:  summary{Errors: 1},
			},
			code: 1,
			want: "finding on a lane that should be clean",
		},
		{
			name:   "a summary that counts a warning the findings do not show",
			report: healthReport{Summary: summary{Warnings: 1}},
			code:   0,
			want:   "summary counts 0 errors and 1 warnings",
		},
		{
			name:   "a partial read",
			report: healthReport{Partial: true},
			code:   0,
			want:   "the report is partial",
		},
		{
			name:   "a non-zero exit with nothing in the report",
			report: healthReport{},
			code:   1,
			want:   "exit code = 1",
		},
		{
			name: "a severity this suite cannot rank",
			report: healthReport{
				Findings: []finding{scriptedFinding(func(f *finding) { f.Severity = "critical" })},
			},
			code: 0,
			want: `severity "critical"`,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			problems := gateProblems(tt.report, tt.code)
			if len(problems) == 0 {
				t.Fatal("the gate accepted it")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.want) {
				t.Errorf("the gate said %q, which does not mention %q", problems, tt.want)
			}
		})
	}
}

// TestGateProblemsAcceptsACleanLane is the control for the cases above, and it
// pins the one report that has to pass: nothing found, exit 0.
//
// An info finding passes too. The gate is about warnings and errors, and an
// operand somebody switched off on purpose is reported at info by design.
func TestGateProblemsAcceptsACleanLane(t *testing.T) {
	clean := healthReport{Findings: []finding{}}
	if problems := gateProblems(clean, 0); len(problems) > 0 {
		t.Errorf("the gate refused a clean report: %q", problems)
	}

	info := healthReport{
		Findings: []finding{scriptedFinding(func(f *finding) {
			f.Code = "NV-OP-002"
			f.Severity = "info"
		})},
		Summary: summary{Infos: 1},
	}
	if problems := gateProblems(info, 0); len(problems) > 0 {
		t.Errorf("the gate refused a report carrying only an info finding: %q", problems)
	}
}

// TestSummaryProblemsComparesAgainstTheCluster drives the arithmetic half.
func TestSummaryProblemsComparesAgainstTheCluster(t *testing.T) {
	facts := laneFacts{gpuNodes: 2, hinted: 1, capacity: 8, allocatable: 8, fromProfile: 8}
	good := summary{NodesChecked: 3, GPUCapacity: 8, GPUAllocatable: 8, GPURequestedKnown: true}

	if problems := summaryProblems(healthReport{Summary: good}, facts); len(problems) > 0 {
		t.Fatalf("a summary that matches the cluster was refused: %q", problems)
	}

	cases := []struct {
		name    string
		summary summary
		facts   laneFacts
		want    string
	}{
		{
			name:    "a capacity the cluster does not have",
			summary: summary{NodesChecked: 3, GPUCapacity: 16, GPUAllocatable: 8, GPURequestedKnown: true},
			facts:   facts,
			want:    "gpuCapacity = 16",
		},
		{
			name:    "an allocatable the cluster does not have",
			summary: summary{NodesChecked: 3, GPUCapacity: 8, GPUAllocatable: 4, GPURequestedKnown: true},
			facts:   facts,
			want:    "gpuAllocatable = 4",
		},
		{
			name:    "a node the report did not check",
			summary: summary{NodesChecked: 2, GPUCapacity: 8, GPUAllocatable: 8, GPURequestedKnown: true},
			facts:   facts,
			want:    "nodesChecked = 2",
		},
		{
			name:    "pods that were never listed",
			summary: summary{NodesChecked: 3, GPUCapacity: 8, GPUAllocatable: 8},
			facts:   facts,
			want:    "the GPU requests are unknown",
		},
		{
			name:    "a GPU somebody is holding",
			summary: summary{NodesChecked: 3, GPUCapacity: 8, GPUAllocatable: 8, GPURequested: 1, GPURequestedKnown: true},
			facts:   facts,
			want:    "gpuRequested = 1",
		},
		{
			name:    "a lane that does not match its own profile",
			summary: summary{NodesChecked: 3, GPUCapacity: 8, GPUAllocatable: 8, GPURequestedKnown: true},
			facts:   laneFacts{gpuNodes: 2, hinted: 1, capacity: 8, allocatable: 8, fromProfile: 16},
			want:    "hack/mokka/lib.sh says this profile puts 16",
		},
		{
			name:    "devices the scheduler cannot place",
			summary: summary{NodesChecked: 3, GPUCapacity: 8, GPUAllocatable: 7, GPURequestedKnown: true},
			facts:   laneFacts{gpuNodes: 2, hinted: 1, capacity: 8, allocatable: 7, fromProfile: 8},
			want:    "the lane is not healthy",
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			problems := summaryProblems(healthReport{Summary: tt.summary}, tt.facts)
			if len(problems) == 0 {
				t.Fatal("the comparison accepted it")
			}
			if !strings.Contains(strings.Join(problems, "\n"), tt.want) {
				t.Errorf("the comparison said %q, which does not mention %q", problems, tt.want)
			}
		})
	}
}

// TestDecodeHealthReportRejectsWhatIsNotAReport covers the stream a consumer
// parses. A command that printed a usage message there, or nothing at all,
// would otherwise be read as a report with no findings, which is the one
// wrong answer this whole suite exists to catch.
func TestDecodeHealthReportRejectsWhatIsNotAReport(t *testing.T) {
	for _, stdout := range []string{"", "not json", "kubectl-nv: unknown flag --bogus\n"} {
		if _, err := decodeHealthReport(stdout); err == nil {
			t.Errorf("decoding %q succeeded; a report that is not JSON has to be an error", stdout)
		}
	}

	if _, err := decodeHealthReport(`{"findings":[],"summary":{"nodesChecked":3}}`); err != nil {
		t.Errorf("decoding a report failed: %v", err)
	}
}

// TestCatalogueCodesReadsTheBinarysOwnList drives the one part of the gate
// that runs a process, against a fake binary rather than against a cluster.
//
// What it pins is the parse: `explain` prints the codes it knows in a usage
// error, and a reader that took the whole stderr line, or that stopped at the
// first code, would pass against a binary carrying one entry and fail silently
// against the real one.
func TestCatalogueCodesReadsTheBinarysOwnList(t *testing.T) {
	restore := useFakeBinary(t, `#!/bin/sh
printf 'error: no finding catalogued as "%s": the codes are NV-DP-001, NV-NODE-002, NV-OP-001\n' "$3" >&2
exit 2
`)
	defer restore()

	codes := catalogueCodes(t)
	want := []string{"NV-DP-001", "NV-NODE-002", "NV-OP-001"}
	if len(codes) != len(want) {
		t.Fatalf("catalogueCodes returned %v, want %v", codes, want)
	}
	for i := range want {
		if codes[i] != want[i] {
			t.Errorf("code %d is %q, want %q", i, codes[i], want[i])
		}
	}
}

// useFakeBinary points the suite's runner at a script of the test's own, and
// returns the function that puts the real one back.
//
// The kubeconfig is swapped too, at a file that exists and names nothing: the
// runner passes it to the binary, and the fake never opens it. Nothing here
// reaches a cluster.
func useFakeBinary(t *testing.T, script string) func() {
	t.Helper()

	dir := t.TempDir()
	fake := filepath.Join(dir, "kubectl-nv")
	if err := os.WriteFile(fake, []byte(script), 0o700); err != nil {
		t.Fatalf("writing the fake binary: %v", err)
	}

	kubeconfig := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nclusters: []\n"), 0o600); err != nil {
		t.Fatalf("writing the fake kubeconfig: %v", err)
	}

	oldBinary, oldKubeconfig := binaryPath, kubeconfigPath
	binaryPath, kubeconfigPath = fake, kubeconfig

	return func() { binaryPath, kubeconfigPath = oldBinary, oldKubeconfig }
}

// TestFaultCasesAreWellFormed checks the table itself, before any lane is
// touched by it. A case naming a script that is not there, or a code that does
// not match the script's name, polls for four minutes and then fails saying
// nothing about either mistake.
func TestFaultCasesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, tc := range faultCases {
		if seen[tc.code] {
			t.Errorf("%s has two entries; one of them never runs", tc.code)
		}
		seen[tc.code] = true

		want := strings.ToLower(tc.code) + ".sh"
		if tc.script != want {
			t.Errorf("%s names script %q, want %q", tc.code, tc.script, want)
		}
		if _, known := severityRank[tc.severity]; !known {
			t.Errorf("%s expects severity %q, which this suite cannot rank", tc.code, tc.severity)
		}
		if len(tc.evidenceKeys) == 0 {
			t.Errorf("%s pins no evidence keys", tc.code)
		}
		if tc.exact == nil {
			t.Errorf("%s pins no evidence values", tc.code)
		}
		if tc.appear <= 0 || tc.clear <= 0 {
			t.Errorf("%s has an unbounded wait: appear=%s clear=%s", tc.code, tc.appear, tc.clear)
		}
	}

	// Every script in the directory is in the table. One that is not would be
	// a reproduction nothing runs, which is the state this suite exists to
	// make impossible.
	entries, err := os.ReadDir(faultsDir)
	if err != nil {
		t.Fatalf("reading %s: %v", faultsDir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "nv-") || !strings.HasSuffix(name, ".sh") {
			continue
		}
		code := strings.ToUpper(strings.TrimSuffix(name, ".sh"))
		if !seen[code] {
			t.Errorf("%s is in %s and no fault case runs it", name, faultsDir)
		}
	}
}

// TestEveryFaultCaseHasACatalogueEntry keeps the table and docs/findings in
// step without needing a binary: a code with a reproduction and no entry is a
// finding `kubectl nv explain` cannot answer for.
func TestEveryFaultCaseHasACatalogueEntry(t *testing.T) {
	for _, tc := range faultCases {
		path := filepath.Join("../../docs/findings", tc.code+".md")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s has no catalogue entry: %v", tc.code, err)

			continue
		}
		if !strings.Contains(string(raw), "## Reproduction") {
			t.Errorf("%s has an entry with no Reproduction section", tc.code)
		}
		if !strings.Contains(string(raw), tc.script) {
			t.Errorf("%s's entry does not name %s, so a reader cannot find the script", tc.code, tc.script)
		}
	}
}

// ctkCase is NV-CTK-001's entry in the fault table, looked up rather than
// copied so that these tests judge the case the live suite runs.
func ctkCase(t *testing.T) faultCase {
	t.Helper()

	for _, tc := range faultCases {
		if tc.code == "NV-CTK-001" {
			return tc
		}
	}
	t.Fatal("faultCases has no NV-CTK-001 entry")

	return faultCase{}
}

// ctkFinding is an NV-CTK-001 finding for the toolkit-validation stand-in,
// with the evidence given.
func ctkFinding(evidence map[string]string) finding {
	return finding{
		Code:      "NV-CTK-001",
		Severity:  "error",
		Node:      offlineNode,
		Component: "nvidia-container-toolkit-daemonset",
		Message:   "the toolkit-validation init container of the validator pod exited non-zero",
		Evidence:  evidence,
		Docs:      "docs/findings/NV-CTK-001.md",
	}
}

// TestCTKAcceptsBothShapesTheRuleReports pins the two forms NV-CTK-001 takes on
// a live lane. Between restarts the kubelet reports the init container Waiting
// in CrashLoopBackOff, and the rule reads the exit from the last termination,
// which is when it adds lastTerminationAt. Right after a run, and for as long
// as the kubelet keeps it so, the current state is the termination itself,
// reason Error, and the rule reports that exit with no lastTerminationAt. How
// long the second form lasts is the kubelet's, not the rule's: 11 s to 1m32s
// on the build Mac, and longer than the two minute bound on a CI runner
// (NVIDIA/kubectl-nv#82, 2026-10-14).
func TestCTKAcceptsBothShapesTheRuleReports(t *testing.T) {
	tc := ctkCase(t)

	for name, evidence := range map[string]map[string]string{
		"waiting in CrashLoopBackOff": {
			"pod":               "nv-faults-ctk-validator",
			"initContainer":     "toolkit-validation",
			"exitCode":          "1",
			"reason":            "CrashLoopBackOff",
			"lastTerminationAt": "2026-10-14T11:16:30Z",
		},
		"terminated right after a run": {
			"pod":           "nv-faults-ctk-validator",
			"initContainer": "toolkit-validation",
			"exitCode":      "1",
			"reason":        "Error",
		},
	} {
		t.Run(name, func(t *testing.T) {
			report := healthReport{Findings: []finding{ctkFinding(evidence)}}
			if _, why := matchFinding(report, tc, offlineNode, tc.exact(4)); why != "" {
				t.Errorf("matchFinding rejected the %s shape: %s", name, why)
			}
		})
	}
}

// TestCTKRejectsWhatTheRuleNeverReports holds the two shapes apart: each is
// accepted only whole, so a regression that dropped the last termination from
// the waiting shape, or a finding for an exit that was not a failure, still
// fails.
func TestCTKRejectsWhatTheRuleNeverReports(t *testing.T) {
	tc := ctkCase(t)

	cases := []struct {
		name     string
		evidence map[string]string
		want     string
	}{
		{
			name: "waiting without its termination",
			evidence: map[string]string{
				"pod": "nv-faults-ctk-validator", "initContainer": "toolkit-validation",
				"exitCode": "1", "reason": "CrashLoopBackOff",
			},
			want: `NV-CTK-001 is waiting in CrashLoopBackOff but carries no lastTerminationAt`,
		},
		{
			name: "a termination that also claims a last one",
			evidence: map[string]string{
				"pod": "nv-faults-ctk-validator", "initContainer": "toolkit-validation",
				"exitCode": "1", "reason": "Error", "lastTerminationAt": "2026-10-14T11:16:30Z",
			},
			want: `NV-CTK-001 reports a current termination (reason Error) and a lastTerminationAt, which the rule never sets together`,
		},
		{
			name: "any other reason",
			evidence: map[string]string{
				"pod": "nv-faults-ctk-validator", "initContainer": "toolkit-validation",
				"exitCode": "1", "reason": "Completed",
			},
			want: `NV-CTK-001 evidence reason = "Completed", want CrashLoopBackOff or Error`,
		},
		{
			name: "an exit that was not a failure",
			evidence: map[string]string{
				"pod": "nv-faults-ctk-validator", "initContainer": "toolkit-validation",
				"exitCode": "0", "reason": "Error",
			},
			want: `NV-CTK-001 evidence exitCode = "0", want "1"`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			report := healthReport{Findings: []finding{ctkFinding(c.evidence)}}
			_, why := matchFinding(report, tc, offlineNode, tc.exact(4))
			if why != c.want {
				t.Errorf("matchFinding said %q, want %q", why, c.want)
			}
		})
	}
}
