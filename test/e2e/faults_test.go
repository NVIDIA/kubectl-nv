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
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// This file breaks the lane on purpose, one finding code at a time, and checks
// that `kubectl nv health` says the right thing about each break and goes
// quiet again when it is undone.
//
// It carries the "faults" tag on top of "e2e", so `make e2e-mokka` compiles
// none of it: that lane is the healthy gate and nothing may inject into it.
// `make e2e-faults` brings up its own lane, kubectl-nv-faults, and runs this.
//
// Each case is a pair of calls into hack/mokka/faults, which is where the
// writing to a cluster lives. Nothing here patches anything itself: the
// scripts are what a developer runs by hand and what the agent eval harness
// will reuse, so a reproduction that only existed inside a Go test would be a
// reproduction nobody could run.
//
// The recover half is registered with t.Cleanup before the finding is polled
// for, so a case that fails its assertion still puts the lane back. A case
// that left its fault behind would fail the next case and the healthy gate in
// health_test.go, and the first report would name the wrong code.

// faultsDir is where the injection scripts live, relative to this package.
const faultsDir = "../../hack/mokka/faults"

// pollInterval is how often the report is asked again while a fault takes
// hold or clears. Two seconds: a kubelet status update and an operator
// reconcile are both slower than that, and a tighter loop only costs runs of
// the binary.
const pollInterval = 2 * time.Second

// scriptTimeout bounds one inject or recover. The slowest of them scales a
// Deployment down and waits for its pods, which is seconds on a lane and is
// given minutes here so that a loaded runner is not called a failure.
const scriptTimeout = 5 * time.Minute

// faultCase is one finding code: how to produce it, and what the report has to
// say while it is there.
type faultCase struct {
	// code is the finding code, and script is the file in faultsDir that
	// injects and recovers it. They are kept as two fields rather than one
	// derived from the other so that a rename of either is visible here.
	code   string
	script string

	// severity and component are what the finding has to carry. A rule that
	// started reporting at info what it used to report at error would still
	// produce a finding with the right code, and a test that only matched the
	// code would stay green through it.
	severity  string
	component string

	// clusterScoped findings carry no node. NV-OP-001 is about the
	// ClusterPolicy, which is not about any one node.
	clusterScoped bool

	// evidenceKeys are the keys the finding's evidence has to carry. The
	// values are not pinned: a pod name and a restart count change between
	// runs, and a test that pinned them would be a test about this lane rather
	// than about the rule.
	evidenceKeys []string

	// appear and clear bound the polls. They come from the live run recorded
	// in NOTES.md, with margin: the measured times are a fraction of these,
	// and the margin is what keeps a loaded CI runner from failing a run that
	// is merely slow.
	appear time.Duration
	clear  time.Duration
}

// faultCases is the table, in code order.
//
// Only the eight codes of this milestone's first rule set are here. The eight
// that Track E2 adds come with their own entries when those rules land: an
// entry for a code no rule emits would poll for four minutes and then fail,
// saying nothing about anything.
var faultCases = []faultCase{
	{
		code:         "NV-DCGM-001",
		script:       "nv-dcgm-001.sh",
		severity:     "warning",
		component:    "nvidia-dcgm-exporter",
		evidenceKeys: []string{"nvidia.com/gpu.deploy.dcgm-exporter", "operands", "pod", "phase", "ready", "restarts"},
		appear:       6 * time.Minute,
		clear:        6 * time.Minute,
	},
	{
		code:         "NV-DP-001",
		script:       "nv-dp-001.sh",
		severity:     "error",
		component:    "nvidia-device-plugin-daemonset",
		evidenceKeys: []string{"nvidia.com/gpu.deploy.device-plugin", "operands", "pod", "phase", "ready", "restarts"},
		appear:       6 * time.Minute,
		clear:        6 * time.Minute,
	},
	{
		code:         "NV-NODE-001",
		script:       "nv-node-001.sh",
		severity:     "warning",
		component:    "node",
		evidenceKeys: []string{"feature.node.kubernetes.io/pci-10de.present", "operands"},
		appear:       4 * time.Minute,
		clear:        4 * time.Minute,
	},
	{
		code:         "NV-NODE-002",
		script:       "nv-node-002.sh",
		severity:     "error",
		component:    "node",
		evidenceKeys: []string{"nvidia.com/gpu.present", "capacity", "allocatable", "hinted", "operands"},
		appear:       6 * time.Minute,
		clear:        6 * time.Minute,
	},
	{
		code:         "NV-NODE-003",
		script:       "nv-node-003.sh",
		severity:     "warning",
		component:    "node",
		evidenceKeys: []string{"capacity", "allocatable", "missing"},
		appear:       6 * time.Minute,
		clear:        6 * time.Minute,
	},
	{
		code:         "NV-NODE-004",
		script:       "nv-node-004.sh",
		severity:     "warning",
		component:    "node",
		evidenceKeys: []string{"nvidia.com/gpu.count", "capacity", "expectedCapacity"},
		appear:       4 * time.Minute,
		clear:        4 * time.Minute,
	},
	{
		code:          "NV-OP-001",
		script:        "nv-op-001.sh",
		severity:      "error",
		component:     "gpu-operator",
		clusterScoped: true,
		evidenceKeys:  []string{"name", "status.state"},
		appear:        6 * time.Minute,
		clear:         6 * time.Minute,
	},
	{
		code:         "NV-OP-002",
		script:       "nv-op-002.sh",
		severity:     "info",
		component:    "nvidia-dcgm-exporter",
		evidenceKeys: []string{"nvidia.com/gpu.deploy.dcgm-exporter"},
		appear:       4 * time.Minute,
		clear:        4 * time.Minute,
	},
}

// TestFaultsAreReportedAndRecover is the whole file: inject, see the code,
// recover, see it go.
//
// The cases share one lane and are not parallel. Two faults at once would make
// a failure ambiguous, and two of these scripts write to the same ClusterPolicy
// field, which the scripts themselves refuse rather than race over.
func TestFaultsAreReportedAndRecover(t *testing.T) {
	// The table is checked against the directory before anything is injected.
	// A case whose script was renamed would otherwise fail inside the subtest
	// that needs it, after that subtest had already broken the lane, and an
	// empty table would report a clean run having tested nothing.
	if len(faultCases) == 0 {
		t.Fatal("the fault table is empty; this run would assert nothing")
	}
	for _, tc := range faultCases {
		info, err := os.Stat(filepath.Join(faultsDir, tc.script))
		if err != nil {
			t.Fatalf("%s names %s, which is not there: %v", tc.code, tc.script, err)
		}
		if info.Mode().Perm()&0o111 == 0 {
			t.Fatalf("%s is not executable (mode %04o)", tc.script, info.Mode().Perm())
		}
	}

	node := faultNode(t)
	t.Logf("injecting into %s", node)

	// A dirty lane makes every case below ambiguous: a finding that was
	// already there is not evidence that the injection produced it.
	assertLaneIsClean(t, "before any injection")

	for _, tc := range faultCases {
		t.Run(tc.code, func(t *testing.T) {
			runFaultCase(t, tc, node)
		})
	}
}

// runFaultCase injects one fault, waits for the finding, recovers and waits for
// the lane to go quiet.
func runFaultCase(t *testing.T, tc faultCase, node string) {
	t.Helper()

	recovered := false
	// Registered before the injection rather than after it: a script that
	// failed halfway has still changed the lane, and the recover it needs is
	// the same one.
	t.Cleanup(func() {
		if recovered {
			return
		}
		runFaultScript(t, tc.script, "recover", node)
	})

	injectedAt := time.Now()
	runFaultScript(t, tc.script, "inject", node)

	found := waitForFinding(t, tc, node, injectedAt)
	t.Logf("%s appeared %s after inject: %s", tc.code, time.Since(injectedAt).Round(time.Second), found)

	recoveredAt := time.Now()
	runFaultScript(t, tc.script, "recover", node)
	recovered = true

	waitForClean(t, tc, recoveredAt)
	t.Logf("%s cleared %s after recover", tc.code, time.Since(recoveredAt).Round(time.Second))
}

// waitForFinding polls the report until the case's finding is in it, with the
// severity, the component and the evidence keys the case names.
//
// All four are part of the wait rather than checked after it. A finding's
// evidence is filled from pods and labels that arrive at different moments, so
// a check that ran at the instant the code first appeared would be a check on
// timing; waiting for the whole shape is the same assertion with the race
// removed.
func waitForFinding(t *testing.T, tc faultCase, node string, since time.Time) finding {
	t.Helper()

	deadline := time.Now().Add(tc.appear)
	var last string

	for {
		report, _ := healthJSON(t, "--fail-on", "info")

		match, why := matchFinding(report, tc, node)
		if why == "" {
			return match
		}
		last = why

		if time.Now().After(deadline) {
			t.Fatalf("%s did not appear as expected within %s of inject (%s elapsed): %s\nthe report held:\n%s",
				tc.code, tc.appear, time.Since(since).Round(time.Second), last, formatFindings(report))
		}
		time.Sleep(pollInterval)
	}
}

// matchFinding returns the finding the case is about, or the reason there is
// not one yet. The reason is a sentence rather than a boolean so that a
// timeout says which half of the expectation was not met.
func matchFinding(report healthReport, tc faultCase, node string) (finding, string) {
	var candidates []finding
	for _, f := range report.Findings {
		if f.Code != tc.code {
			continue
		}
		if tc.clusterScoped {
			if f.Node != "" {
				continue
			}
		} else if f.Node != node {
			continue
		}
		candidates = append(candidates, f)
	}

	if len(candidates) == 0 {
		if tc.clusterScoped {
			return finding{}, fmt.Sprintf("no %s finding without a node", tc.code)
		}

		return finding{}, fmt.Sprintf("no %s finding for %s", tc.code, node)
	}

	for _, f := range candidates {
		if f.Severity != tc.severity {
			return finding{}, fmt.Sprintf("%s is at severity %q, want %q", tc.code, f.Severity, tc.severity)
		}
		if f.Component != tc.component {
			return finding{}, fmt.Sprintf("%s names component %q, want %q", tc.code, f.Component, tc.component)
		}
		if missing := missingKeys(f.Evidence, tc.evidenceKeys); len(missing) > 0 {
			return finding{}, fmt.Sprintf("%s is missing evidence %s (it carries %s)",
				tc.code, strings.Join(missing, ", "), strings.Join(evidenceKeys(f.Evidence), ", "))
		}
		// Every finding links to its own catalogue entry, which is the one
		// thing a reader does next and the one field a rule can forget.
		if want := "docs/findings/" + tc.code + ".md"; f.Docs != want {
			return finding{}, fmt.Sprintf("%s links to %q, want %q", tc.code, f.Docs, want)
		}

		return f, ""
	}

	return finding{}, fmt.Sprintf("no usable %s finding", tc.code)
}

// waitForClean polls until the case's code is gone from the whole report and
// nothing at warning or above is left anywhere.
//
// "Anywhere" and not "on that node": a recover that put one node back and left
// the ClusterPolicy patched would pass a per-node check and leave the lane
// broken for the next case.
func waitForClean(t *testing.T, tc faultCase, since time.Time) {
	t.Helper()

	deadline := time.Now().Add(tc.clear)
	// Two clean reports in a row rather than one. A recover that returns
	// before the operand it disturbed is back leaves a gap of a few seconds in
	// which the lane looks clean and then does not, and one clean poll inside
	// that gap would end the wait with the fault still arriving. The scripts
	// wait for their own operand as well; this is the second lock on the same
	// door, and it costs one poll interval per case.
	const cleanRunsWanted = 2
	cleanRuns := 0

	for {
		report, code := healthJSON(t, "--fail-on", "warning")

		var left []string
		for _, f := range report.Findings {
			if f.Code == tc.code {
				left = append(left, f.String())
			}
		}
		remaining := report.atOrAbove(warningRank)
		for _, f := range remaining {
			left = append(left, f.String())
		}

		if len(left) == 0 && code == 0 {
			cleanRuns++
			if cleanRuns >= cleanRunsWanted {
				return
			}
		} else {
			cleanRuns = 0
		}

		if time.Now().After(deadline) {
			t.Fatalf("%s left the lane dirty %s after recover (waited %s): exit %d\n  %s",
				tc.code, time.Since(since).Round(time.Second), tc.clear, code, strings.Join(left, "\n  "))
		}
		time.Sleep(pollInterval)
	}
}

// assertLaneIsClean fails the whole run when the lane is already reporting
// something at warning or above.
func assertLaneIsClean(t *testing.T, when string) {
	t.Helper()

	report, code := healthJSON(t, "--fail-on", "warning")
	if code == 0 && len(report.atOrAbove(warningRank)) == 0 {
		return
	}

	t.Fatalf("the lane is not clean %s (exit %d); every case below would be ambiguous:\n%s",
		when, code, formatFindings(report))
}

// runFaultScript runs one injection script against the lane's own kubeconfig.
//
// The kubeconfig is passed in the environment because that is the only way the
// scripts accept one: there is no fallback to $KUBECONFIG and none to the
// caller's default kubeconfig, which is what stops a script that is handed
// nothing from writing to whichever cluster the developer was last looking at.
func runFaultScript(t *testing.T, script, action, node string) {
	t.Helper()

	path, err := filepath.Abs(filepath.Join(faultsDir, script))
	if err != nil {
		t.Fatalf("resolving %s: %v", script, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), scriptTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, path, action, node)
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = append(os.Environ(), "MOKKA_KUBECONFIG="+kubeconfigPath)

	err = cmd.Run()

	// The scripts' own observation lines are the record of what the lane did
	// and when, so they are logged whether the script succeeded or not.
	if out := strings.TrimSpace(stdout.String()); out != "" {
		t.Logf("%s %s %s:\n%s", script, action, node, out)
	}

	if ctx.Err() != nil {
		t.Fatalf("%s %s %s did not finish within %s\nstderr:\n%s", script, action, node, scriptTimeout, stderr.String())
	}
	if err != nil {
		t.Fatalf("%s %s %s: %v\nstderr:\n%s", script, action, node, err, stderr.String())
	}
}

// faultNode is the worker every case injects into. The first in name order, so
// that two runs of the suite break the same node and a lane left behind by a
// failed run is recognisable.
func faultNode(t *testing.T) string {
	t.Helper()

	workers := workerNodeNames(t)
	if len(workers) == 0 {
		t.Fatal("the lane has no worker nodes to inject into")
	}

	return workers[0]
}

// missingKeys returns the wanted keys the evidence does not carry.
func missingKeys(evidence map[string]string, want []string) []string {
	var missing []string
	for _, key := range want {
		if _, ok := evidence[key]; !ok {
			missing = append(missing, key)
		}
	}

	return missing
}

// evidenceKeys returns the keys of an evidence map, sorted, for a failure
// message that says what was there instead.
func evidenceKeys(evidence map[string]string) []string {
	keys := make([]string, 0, len(evidence))
	for key := range evidence {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

// formatFindings renders a whole report for a failure message.
func formatFindings(report healthReport) string {
	if len(report.Findings) == 0 {
		return "  (no findings) " + report.Summary.String()
	}

	lines := make([]string, 0, len(report.Findings))
	for _, f := range report.Findings {
		lines = append(lines, "  "+f.String())
	}

	return strings.Join(lines, "\n")
}
