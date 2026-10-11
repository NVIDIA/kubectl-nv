//go:build e2e && scale

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

// The scale lane. A Mokka t4 lane carries two real workers; hack/mokka/kwok-nodes.sh
// adds KWOK nodes that advertise simulated GPUs and the GFD labels, and this
// file measures what the built binary costs over the result.
//
// It is behind "e2e && scale" and not "e2e" alone, because the lane it needs
// takes a nightly workflow to build. "make e2e-mokka" must not try to run it.
//
// A timing suite invites one failure above all others: the fast wrong answer.
// An empty list is the quickest list there is, and a paginator that repeats a
// page and drops another returns the right number of the wrong nodes. So every
// run is checked, not the last one; the names have to be the lane's, each one
// once; and the exit code has to be the clean one. The decisions themselves
// live in scale_support_test.go, where they are tested without a cluster.
//
// Findings are recorded and never asserted. KWOK nodes advertise GPUs and
// carry no operands, so the operand rules fire on every one of them. A scale
// lane is not a healthy lane and this suite is not the place to pretend
// otherwise.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// settlePoll is how often the lane is asked whether it has stopped moving.
const settlePoll = 10 * time.Second

// podPageSize bounds one page of the pod census. At two thousand nodes the
// pod list is thousands of objects, and asking for all of them in one response
// is a request the API server serves slowly and the client holds entirely in
// memory. Only the counts are kept.
const podPageSize = 500

func TestScaleLane(t *testing.T) {
	cfg, err := scaleConfigFrom(os.Getenv)
	if err != nil {
		t.Fatalf("scale configuration: %v", err)
	}
	t.Logf("scale: expecting %d fake nodes, %d real and %d hinted, %d runs per command, thresholds list=%s health=%s",
		cfg.Nodes, cfg.RealNodes, cfg.HintedNodes, cfg.Runs, cfg.ListThreshold, cfg.HealthThreshold)

	report := scaleReport{
		RecordedAt: time.Now().UTC().Format(time.RFC3339),
		// Which commit these numbers belong to. A trend read across nightlies
		// is worth nothing if a row cannot be tied back to a tree.
		CommitSHA:      os.Getenv("GITHUB_SHA"),
		Binary:         binaryPath,
		ExpectedFake:   cfg.Nodes,
		ExpectedReal:   cfg.RealNodes,
		ExpectedHinted: cfg.HintedNodes,
		HealthFindings: -1,
	}
	// The report is written whichever way the assertions go, and every timing
	// is appended the moment it exists rather than at the end: a run that
	// missed its threshold, or that died on a bad exit code, is exactly the
	// run whose numbers are wanted.
	defer func() {
		if err := writeScaleReport(cfg.ReportDir, report); err != nil {
			t.Errorf("writing the scale report: %v", err)

			return
		}
		t.Logf("scale: numbers written to %s", filepath.Join(cfg.ReportDir, reportFile))
	}()

	// -----------------------------------------------------------------
	// Let the lane stop moving first
	// -----------------------------------------------------------------

	total, onFake := waitForLaneToSettle(t, cfg.SettleTimeout)
	report.PodsTotal = total
	report.PodsOnFakeNodes = onFake
	t.Logf("scale: lane settled with %d pods, %d of them on fake nodes", total, onFake)

	// -----------------------------------------------------------------
	// list
	// -----------------------------------------------------------------

	listRuns := measure(t, &report, cfg.Runs, cfg.ListThreshold, listExitCodes, "list", "-o", "json")

	// Every run, not the last one. A command that answered correctly once and
	// emptily twice has a median this suite would otherwise be delighted with.
	for i, raw := range listRuns {
		census, err := scaleAssessList([]byte(raw), cfg.Nodes, cfg.RealNodes)
		if err != nil {
			t.Errorf("run %d of list -o json: %v", i+1, err)
		}
		if i == 0 {
			report.FakeNodes = census.Fake
			report.RealNodes = census.Real
			report.TotalNodes = census.Total
		}
	}

	// -----------------------------------------------------------------
	// health --summary
	// -----------------------------------------------------------------

	healthRuns := measure(t, &report, cfg.Runs, cfg.HealthThreshold, healthExitCodes,
		"health", "--summary", "-o", "json")

	// health builds its inventory with IncludeHinted, so it checks the nodes
	// list leaves out as well: the fake nodes, the real workers, and the
	// lane's control plane, which nvml-mock labels and no operand runs on.
	wantNodes := cfg.Nodes + cfg.RealNodes + cfg.HintedNodes
	for i, raw := range healthRuns {
		counted, err := scaleCountAtPath([]byte(raw), healthNodesPath)
		if err != nil {
			t.Errorf("run %d: reading %q out of health --summary -o json: %v\noutput:\n%s",
				i+1, healthNodesPath, err, firstBytes(raw, 2000))

			continue
		}
		if i == 0 {
			report.HealthNodes = counted
		}
		if counted != wantNodes {
			t.Errorf("run %d: health --summary counted %d nodes at %q, want %d (%d fake + %d real + %d hinted)",
				i+1, counted, healthNodesPath, wantNodes, cfg.Nodes, cfg.RealNodes, cfg.HintedNodes)
		}
	}

	// Recorded, never asserted. See the package comment. --summary suppresses
	// the findings in the table and not in the JSON, so they are all here.
	if len(healthRuns) > 0 {
		first := []byte(healthRuns[0])
		for path, into := range map[string]*int{
			healthFindingsPath: &report.HealthFindings,
			"summary.errors":   &report.HealthErrors,
			"summary.warnings": &report.HealthWarnings,
			"summary.infos":    &report.HealthInfos,
		} {
			if n, err := scaleCountAtPath(first, path); err == nil {
				*into = n
			} else {
				t.Logf("scale: could not read %q for the report: %v", path, err)
			}
		}
	}
	t.Logf("scale: %d fake nodes, %d real nodes, health counted %d; findings %d (%d error, %d warning, %d info)",
		report.FakeNodes, report.RealNodes, report.HealthNodes,
		report.HealthFindings, report.HealthErrors, report.HealthWarnings, report.HealthInfos)
}

// measure runs the binary cfg.Runs times, appends the timing to the report
// before judging anything, and returns every run's stdout. The timing is
// appended first on purpose: a run that is about to fail the test is a run
// whose numbers belong in the uploaded artifact.
func measure(t *testing.T, report *scaleReport, runs int, threshold time.Duration, allowed []int, args ...string) []string {
	t.Helper()

	timing := commandTiming{
		Command:     fmt.Sprint(args),
		ThresholdMS: threshold.Milliseconds(),
	}
	durations := make([]time.Duration, 0, runs)
	outs := make([]string, 0, runs)
	bad := make([]string, 0)

	for i := 0; i < runs; i++ {
		start := time.Now()
		res := run(t, args...)
		elapsed := time.Since(start)

		durations = append(durations, elapsed)
		outs = append(outs, res.stdout)
		timing.Runs = append(timing.Runs, commandRun{
			DurationMS: elapsed.Milliseconds(),
			ExitCode:   res.code,
			StdoutLen:  len(res.stdout),
		})

		if !scaleAllows(allowed, res.code) {
			bad = append(bad, fmt.Sprintf("run %d exited %d (want one of %v)\nstderr:\n%s",
				i+1, res.code, allowed, res.stderr))
		}
	}

	stats, err := scaleAssessRuns(durations, threshold)
	timing.MedianMS = stats.Median.Milliseconds()
	timing.MaxMS = stats.Max.Milliseconds()
	timing.Within = stats.Within
	report.Timings = append(report.Timings, timing)

	t.Logf("scale: %v median %v max %v over %d runs (threshold %v)",
		args, stats.Median, stats.Max, len(durations), threshold)

	for _, line := range bad {
		t.Errorf("%v: %s", args, line)
	}
	if err != nil {
		t.Errorf("%v: %v", args, err)
	}

	return outs
}

// waitForLaneToSettle blocks until the lane has stopped changing underneath
// the measurement, and returns the pod counts it settled at.
//
// Two conditions, both needed. Every DaemonSet has to have placed a pod on
// every node it wants one on, and the total pod count has to be the same twice
// in a row. Without the first, the timings include the scheduler still working
// through two thousand nodes; without the second, a DaemonSet that has not yet
// noticed the new nodes reports itself content.
//
// The DaemonSet condition is currentNumberScheduled against
// desiredNumberScheduled and deliberately not numberReady. A fake node's pods
// are answered for by KWOK, and waiting for them to be Ready waits on a
// different controller than the one whose work perturbs the measurement.
func waitForLaneToSettle(t *testing.T, timeout time.Duration) (total, onFake int) {
	t.Helper()

	client := clusterClient(t)
	deadline := time.Now().Add(timeout)
	previous := -1
	var reason string

	for {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		pending, err := listPendingDaemonSets(ctx, client)
		if err == nil {
			total, onFake, err = scaleCountPods(ctx, client)
		}
		cancel()

		if err != nil {
			reason = err.Error()
		} else {
			var done bool
			done, reason = laneSettled(pending, total, previous)
			previous = total
			if done {
				return total, onFake
			}
		}

		if time.Now().After(deadline) {
			t.Fatalf("the lane had not settled after %s: %s", timeout, reason)
		}
		t.Logf("scale: waiting for the lane to settle: %s", reason)
		time.Sleep(settlePoll)
	}
}

// listPendingDaemonSets fetches the DaemonSets and asks scalePendingDaemonSets
// which of them are still placing pods.
func listPendingDaemonSets(ctx context.Context, client kubernetes.Interface) ([]string, error) {
	list, err := client.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing DaemonSets: %w", err)
	}

	return scalePendingDaemonSets(list.Items), nil
}

// scaleCountPods counts every pod in the cluster and how many of them sit on a fake
// node, a page at a time. Only the two counts are kept: the objects are
// thousands and none of them is read twice.
func scaleCountPods(ctx context.Context, client kubernetes.Interface) (total, onFake int, err error) {
	opts := metav1.ListOptions{Limit: podPageSize}
	for {
		page, err := client.CoreV1().Pods("").List(ctx, opts)
		if err != nil {
			return 0, 0, fmt.Errorf("listing pods: %w", err)
		}
		for i := range page.Items {
			total++
			if scaleOnFakeNode(page.Items[i].Spec.NodeName) {
				onFake++
			}
		}
		if page.Continue == "" {
			return total, onFake, nil
		}
		opts.Continue = page.Continue
	}
}

// firstBytes caps an output quoted into a failure message. A health report
// over two thousand nodes is megabytes, and none of it after the first page
// helps anybody see why a field could not be read.
func firstBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return s[:n] + fmt.Sprintf("\n... (%d bytes more)", len(s)-n)
}
