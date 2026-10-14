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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// debugReportJSON is the DebugReport as a consumer parses it. Only the fields
// this file asserts on are named.
type debugReportJSON struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Node   infoNodeJSON `json:"node"`
	Bundle struct {
		Directory  string `json:"directory"`
		Manifest   string `json:"manifest"`
		Collectors struct {
			OK      int `json:"ok"`
			Skipped int `json:"skipped"`
			Error   int `json:"error"`
		} `json:"collectors"`
		Skipped []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
		Errored []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"errored"`
		Files int   `json:"files"`
		Bytes int64 `json:"bytes"`
	} `json:"bundle"`
	Status struct {
		Partial bool `json:"partial"`
	} `json:"status"`
}

// debugInto runs kubectl nv debug into a fresh directory and returns the
// decoded report, failing the test when the exit code is not the one wanted.
func debugInto(t *testing.T, wantCode int, node string, args ...string) (debugReportJSON, string) {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "artifacts")
	full := append([]string{"debug", node, "--artifacts-dir", dir, "-o", "json"}, args...)
	res := run(t, full...)
	if res.code != wantCode {
		t.Fatalf("kubectl nv debug %v exited %d, want %d\nstderr:\n%s", args, res.code, wantCode, res.stderr)
	}

	var report debugReportJSON
	if err := json.Unmarshal([]byte(res.stdout), &report); err != nil {
		t.Fatalf("decoding the debug report: %v\n%s", err, res.stdout)
	}

	return report, dir
}

// bugReportNames lists the bug reports a bundle holds, by whole name.
func bugReportNames(t *testing.T, dir string) []string {
	t.Helper()

	var reports []string
	for _, name := range bundleNames(t, dir) {
		if strings.HasPrefix(name, "nvidia-bug-report_") {
			reports = append(reports, name)
		}
	}

	return reports
}

// TestDebugAgainstTheLane is the whole command as a support engineer runs it.
// It needs the driver stand-in: the lane installs the GPU Operator with
// driver.enabled=false, so without it there is no pod to exec into and the
// node scope of the exec path is never exercised.
func TestDebugAgainstTheLane(t *testing.T) {
	// Registered before the stand-in is applied, so that it runs after the
	// stand-in has been deleted: cleanups run in reverse. Deleting the
	// daemonset returns as soon as the daemonset is gone, and its pods take a
	// moment longer to terminate. A pod still terminating is still Running, so
	// the next test to gather this namespace collects it and sees a cluster
	// with a driver pod in it. This test leaves the lane as it found it.
	t.Cleanup(func() { waitForNoStandInPods(t) })
	applyStandIn(t)

	workers := workerNodeNames(t)
	if len(workers) < 2 {
		t.Fatalf("the lane has %d worker(s), want at least 2: the node scope is what this test is about", len(workers))
	}
	waitForStandIn(t, len(workers))
	waitForStartedPods(t)

	worker, other := workers[0], workers[1]

	t.Run("it gathers that node and no other", func(t *testing.T) {
		report, dir := debugInto(t, 0, worker)

		if report.Kind != "DebugReport" {
			t.Errorf("kind = %q, want DebugReport", report.Kind)
		}
		if report.Metadata.Name != worker {
			t.Errorf("metadata.name = %q, want %q", report.Metadata.Name, worker)
		}
		if report.Bundle.Directory != dir {
			t.Errorf("bundle.directory = %q, want %q", report.Bundle.Directory, dir)
		}
		if want := filepath.Join(dir, "manifest.json"); report.Bundle.Manifest != want {
			t.Errorf("bundle.manifest = %q, want %q", report.Bundle.Manifest, want)
		}

		// Whole names, not substrings: on this lane
		// kubectl-nv-mokka-worker is a prefix of kubectl-nv-mokka-worker2, so
		// a substring check passes against the wrong node's report.
		got := bugReportNames(t, dir)
		want := []string{"nvidia-bug-report_" + worker + ".log.gz"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the bundle holds the reports %v, want exactly %v", got, want)
		}
		for _, name := range got {
			if name == "nvidia-bug-report_"+other+".log.gz" {
				t.Errorf("the bundle holds %s, which belongs to the node that was not asked for", name)
			}
		}

		// The report's own line about the node it came from, read back out of
		// the gzip the collector copied off the pod.
		if line, wantLine := readGzip(t, filepath.Join(dir, want[0])), bugReportLine(worker); line != wantLine {
			t.Errorf("%s holds %q, want exactly %q", want[0], line, wantLine)
		}
	})

	t.Run("the manifest records the node the gather was narrowed to", func(t *testing.T) {
		_, dir := debugInto(t, 0, worker)

		var m struct {
			Flags struct {
				Node string `json:"node"`
			} `json:"flags"`
		}
		data, rerr := os.ReadFile(filepath.Join(dir, "manifest.json"))
		if rerr != nil {
			t.Fatalf("reading manifest.json: %v", rerr)
		}
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatalf("manifest.json does not parse: %v\n%s", err, data)
		}
		if m.Flags.Node != worker {
			t.Errorf("manifest flags.node = %q, want %q", m.Flags.Node, worker)
		}
	})

	t.Run("the node it reports is the one kubectl nv info reports", func(t *testing.T) {
		report, _ := debugInto(t, 0, worker)
		fromInfo := infoJSON(t, worker)

		// The operand set moves while the lane runs, so the comparison is over
		// the node's stable description rather than over every field. What is
		// being pinned is that debug builds the node exactly as info does
		// rather than describing it a second way.
		if report.Node.Metadata.Name != fromInfo.Metadata.Name {
			t.Errorf("debug reports node %q, info reports %q",
				report.Node.Metadata.Name, fromInfo.Metadata.Name)
		}
		if report.Node.Product != fromInfo.Product {
			t.Errorf("debug reports product %q, info reports %q", report.Node.Product, fromInfo.Product)
		}
		if report.Node.Capacity != fromInfo.Capacity {
			t.Errorf("debug reports capacity %d, info reports %d", report.Node.Capacity, fromInfo.Capacity)
		}
		if report.Node.Mode != fromInfo.Mode {
			t.Errorf("debug reports mode %q, info reports %q", report.Node.Mode, fromInfo.Mode)
		}
		if report.Node.Hinted != fromInfo.Hinted {
			t.Errorf("debug reports hinted=%v, info reports %v", report.Node.Hinted, fromInfo.Hinted)
		}
		if report.Node.Kind != fromInfo.Kind {
			t.Errorf("debug stamps the node as %q, info stamps it as %q", report.Node.Kind, fromInfo.Kind)
		}
		if report.Node.MIG.Strategy != fromInfo.MIG.Strategy {
			t.Errorf("debug reports the MIG strategy %q, info reports %q",
				report.Node.MIG.Strategy, fromInfo.MIG.Strategy)
		}

		// The conditions are compared as a set of type and status pairs: the
		// node's own conditions do not move while the lane is idle, and a
		// report that filled them from somewhere else would not match.
		if got, want := conditionPairs(report.Node), conditionPairs(fromInfo); !reflect.DeepEqual(got, want) {
			t.Errorf("debug reports the conditions %v, info reports %v", got, want)
		}
		// kubectl nv info fills the conditions; so must debug, or it built the
		// node without WithDetails and the report is thinner than it claims.
		if len(report.Node.Conditions) == 0 {
			t.Error("the report's node has no conditions, so it was not built the way info builds it")
		}
	})

	t.Run("--no-exec collects no report and succeeds", func(t *testing.T) {
		report, dir := debugInto(t, 0, worker, "--no-exec")

		if got := bugReportNames(t, dir); len(got) != 0 {
			t.Errorf("the bundle holds %v, but --no-exec was given", got)
		}

		skipped := map[string]string{}
		for _, s := range report.Bundle.Skipped {
			skipped[s.Name] = s.Reason
		}
		for _, collector := range []string{"bug-report", "device-usage"} {
			if got := skipped[collector]; got != "--no-exec" {
				t.Errorf("%s is recorded as %q, want it skipped for --no-exec", collector, got)
			}
		}
		if report.Status.Partial {
			t.Error("status.partial is true, but --no-exec is the user asking for exactly this")
		}
	})

	t.Run("the summary fits one screen", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "artifacts")
		res := run(t, "debug", worker, "--artifacts-dir", dir)
		if res.code != 0 {
			t.Fatalf("kubectl nv debug exited %d, want 0\nstderr:\n%s", res.code, res.stderr)
		}

		lines := strings.Split(strings.TrimRight(res.stdout, "\n"), "\n")
		if len(lines) > 30 {
			t.Errorf("the summary is %d lines, want at most 30:\n%s", len(lines), res.stdout)
		}
		// Compared whole, not as substrings: on this lane
		// kubectl-nv-mokka-worker is a prefix of kubectl-nv-mokka-worker2, so
		// a substring check passes against the other node's command.
		for _, want := range []string{
			"  kubectl nv health --node " + worker,
			"  kubectl nv health --from-bundle " + dir,
		} {
			if !containsLine(lines, want) {
				t.Errorf("the summary has no line %q:\n%s", want, res.stdout)
			}
		}
		if !containsLine(lines, "Node:            "+worker) {
			t.Errorf("the summary does not name %s on its first line:\n%s", worker, res.stdout)
		}
	})
}

// TestDebugOnANodeWithoutGPUs pins the exit code and the message a script
// branches on, which are kubectl nv info's.
func TestDebugOnANodeWithoutGPUs(t *testing.T) {
	res := run(t, "debug", "there-is-no-such-node")

	if res.code != 2 {
		t.Errorf("kubectl nv debug exited %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}

	want := "node there-is-no-such-node has no GPUs and no GPU labels"
	if !strings.Contains(res.stderr, want) {
		t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
	}
}

// containsLine reports whether lines holds want, compared whole.
func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}

	return false
}

// conditionPairs reduces a node's conditions to the type and status pairs two
// readers of the same node must agree on, sorted so that the comparison does
// not depend on the order the API server listed them in.
func conditionPairs(node infoNodeJSON) []string {
	out := make([]string, 0, len(node.Conditions))
	for _, c := range node.Conditions {
		out = append(out, c.Type+"="+c.Status)
	}
	sort.Strings(out)

	return out
}

// TestDebugOnTheControlPlane covers the node a support engineer most often
// asks about: a real node that carries an NVIDIA label and advertises no GPUs.
// That it claims GPUs and has none is the answer, so it is gathered and
// described rather than refused, and the run is a success.
//
// The lane always has one. The simulated-GPU DaemonSet tolerates every taint,
// so node-feature-discovery labels the control plane too.
func TestDebugOnTheControlPlane(t *testing.T) {
	waitForStartedPods(t)

	var controlPlane string
	for _, node := range clusterNodes(t) {
		if _, found := node.Labels[controlPlaneRoleLabel]; found {
			controlPlane = node.Name

			break
		}
	}
	if controlPlane == "" {
		t.Fatal("the lane has no control plane node, which it always has")
	}

	report, dir := debugInto(t, 0, controlPlane)

	if report.Metadata.Name != controlPlane {
		t.Errorf("metadata.name = %q, want %q", report.Metadata.Name, controlPlane)
	}
	if !report.Node.Hinted {
		t.Error("the control plane is not reported as hinted, but it advertises no GPUs")
	}
	if report.Node.Capacity != 0 {
		t.Errorf("capacity = %d, want 0 on the control plane", report.Node.Capacity)
	}

	// A bundle was still written, and it is the control plane's: that is what
	// the user asked for and what they would send to support.
	if report.Bundle.Directory != dir {
		t.Errorf("bundle.directory = %q, want %q", report.Bundle.Directory, dir)
	}
	if want := filepath.Join(dir, "manifest.json"); report.Bundle.Manifest != want {
		t.Errorf("bundle.manifest = %q, want %q", report.Bundle.Manifest, want)
	}

	var m struct {
		Flags struct {
			Node string `json:"node"`
		} `json:"flags"`
	}
	data, rerr := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if rerr != nil {
		t.Fatalf("reading manifest.json: %v", rerr)
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest.json does not parse: %v\n%s", err, data)
	}
	if m.Flags.Node != controlPlane {
		t.Errorf("manifest flags.node = %q, want %q", m.Flags.Node, controlPlane)
	}

	// No driver pod runs on the control plane, so there can be no bug report
	// for it, and that absence has to be a skipped collector with a reason
	// rather than a file nobody can account for.
	if got := bugReportNames(t, dir); len(got) != 0 {
		t.Errorf("the bundle holds %v for a node with no driver pod", got)
	}
}

// standInGoneTimeout bounds the wait for the stand-in's pods to go away.
const standInGoneTimeout = 2 * time.Minute

// waitForNoStandInPods blocks until no pod of the driver stand-in is left in
// the operator namespace, whatever phase it is in.
func waitForNoStandInPods(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(standInGoneTimeout)
	for {
		var left []string
		for _, pod := range operatorPods(t) {
			if strings.HasPrefix(pod.Name, standInDaemonSet) {
				left = append(left, pod.Name+" ("+string(pod.Status.Phase)+")")
			}
		}
		if len(left) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("after %s the driver stand-in still has pods in %s: %s",
				standInGoneTimeout, operatorNamespace, strings.Join(left, ", "))

			return
		}
		time.Sleep(standInPoll)
	}
}
