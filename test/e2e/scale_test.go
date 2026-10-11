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
// Two things are asserted, and a third is only recorded:
//
//   - wall time: the median of KUBECTL_NV_SCALE_RUNS runs of each command has
//     to stay under its threshold.
//   - counts: the node count the binary prints has to equal the node count the
//     lane holds. Without this a command that answered in 20ms with an empty
//     list would be the fastest run on record.
//   - findings: KWOK nodes advertise GPUs and carry no operands, so the
//     operand rules fire on every one of them. The count is written to the
//     report; nothing asserts it. A scale lane is not a healthy lane and this
//     suite is not the place to pretend otherwise.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// commandRun is one timed execution of the binary.
type commandRun struct {
	Args       []string `json:"args"`
	DurationMS int64    `json:"durationMs"`
	ExitCode   int      `json:"exitCode"`
	StdoutLen  int      `json:"stdoutBytes"`
}

// commandTiming is every run of one command, plus the two numbers the
// threshold and the report are about.
type commandTiming struct {
	Command     string       `json:"command"`
	Runs        []commandRun `json:"runs"`
	MedianMS    int64        `json:"medianMs"`
	MaxMS       int64        `json:"maxMs"`
	ThresholdMS int64        `json:"thresholdMs"`
	Within      bool         `json:"withinThreshold"`
}

// scaleReport is what the workflow uploads.
type scaleReport struct {
	RecordedAt      string          `json:"recordedAt"`
	Binary          string          `json:"binary"`
	ExpectedFake    int             `json:"expectedFakeNodes"`
	FakeNodes       int             `json:"fakeNodes"`
	RealNodes       int             `json:"realNodes"`
	TotalNodes      int             `json:"totalNodes"`
	HealthNodes     int             `json:"healthSummaryNodes"`
	HealthFindings  int             `json:"healthFindings"`
	HealthCountPath string          `json:"healthCountPath"`
	Timings         []commandTiming `json:"timings"`
}

// reportFile is the one file the workflow reaches for.
const reportFile = "scale-timings.json"

func TestScaleLane(t *testing.T) {
	cfg, err := scaleConfigFrom(os.Getenv)
	if err != nil {
		t.Fatalf("scale configuration: %v", err)
	}
	t.Logf("scale: expecting %d fake nodes, %d runs per command, thresholds list=%s health=%s",
		cfg.Nodes, cfg.Runs, cfg.ListThreshold, cfg.HealthThreshold)

	report := scaleReport{
		RecordedAt:      time.Now().UTC().Format(time.RFC3339),
		Binary:          binaryPath,
		ExpectedFake:    cfg.Nodes,
		HealthCountPath: cfg.HealthCountPath,
		HealthFindings:  -1,
	}
	// The report is written whichever way the assertions go: a run that
	// missed its threshold is exactly the run whose numbers are wanted.
	defer func() {
		if err := writeScaleReport(cfg.ReportDir, report); err != nil {
			t.Errorf("writing the scale report: %v", err)
		} else {
			t.Logf("scale: numbers written to %s", filepath.Join(cfg.ReportDir, reportFile))
		}
	}()

	// -----------------------------------------------------------------
	// list
	// -----------------------------------------------------------------

	listTiming, listOut := timeCommand(t, cfg.Runs, cfg.ListThreshold,
		// Exit 3 is a partial read, which at this size is a real possibility
		// and still produces a list. Anything else is a failure of the run,
		// not a slow run.
		map[int]bool{0: true, 3: true},
		"list", "-o", "json")
	report.Timings = append(report.Timings, listTiming)

	names, err := nodeNames([]byte(listOut))
	if err != nil {
		t.Fatalf("reading the node names out of list -o json: %v", err)
	}
	report.FakeNodes = countWithPrefix(names, scaleNodePrefix)
	report.TotalNodes = len(names)
	report.RealNodes = report.TotalNodes - report.FakeNodes

	if report.FakeNodes != cfg.Nodes {
		t.Errorf("list reported %d nodes named %s*, want %d",
			report.FakeNodes, scaleNodePrefix, cfg.Nodes)
	}
	// The real workers must still be there. A lane whose workers went away
	// while the fake nodes stayed is not the lane this measures.
	if report.RealNodes < 1 {
		t.Errorf("list reported %d nodes that are not %s*, want at least the lane's workers",
			report.RealNodes, scaleNodePrefix)
	}

	// -----------------------------------------------------------------
	// health --summary
	// -----------------------------------------------------------------

	healthTiming, healthOut := timeCommand(t, cfg.Runs, cfg.HealthThreshold,
		// 1 is "findings at or above --fail-on", which this lane produces by
		// construction, and 3 is a partial read.
		map[int]bool{0: true, 1: true, 3: true},
		"health", "--summary", "-o", "json")
	report.Timings = append(report.Timings, healthTiming)

	healthNodes, err := countAtPath([]byte(healthOut), cfg.HealthCountPath)
	if err != nil {
		t.Fatalf("reading %q out of health --summary -o json: %v\nset KUBECTL_NV_SCALE_HEALTH_COUNT_PATH if the field moved\noutput:\n%s",
			cfg.HealthCountPath, err, healthOut)
	}
	report.HealthNodes = healthNodes
	if healthNodes != report.TotalNodes {
		t.Errorf("health --summary counted %d nodes at %q, want %d (the nodes list reported)",
			healthNodes, cfg.HealthCountPath, report.TotalNodes)
	}

	// Recorded, never asserted. See the package comment.
	if n, err := countAtPath([]byte(healthOut), "summary.findings"); err == nil {
		report.HealthFindings = n
	}
	t.Logf("scale: %d fake nodes, %d real nodes, health counted %d, findings %d",
		report.FakeNodes, report.RealNodes, report.HealthNodes, report.HealthFindings)
}

// timeCommand runs the binary n times, returns the timing and the stdout of
// the last run, and fails the test when the median misses the threshold or a
// run exits with a code that is not in want.
func timeCommand(t *testing.T, n int, threshold time.Duration, want map[int]bool, args ...string) (commandTiming, string) {
	t.Helper()

	timing := commandTiming{
		Command:     fmt.Sprint(args),
		ThresholdMS: threshold.Milliseconds(),
	}
	durations := make([]time.Duration, 0, n)
	last := ""

	for i := 0; i < n; i++ {
		start := time.Now()
		res := run(t, args...)
		elapsed := time.Since(start)

		durations = append(durations, elapsed)
		last = res.stdout
		timing.Runs = append(timing.Runs, commandRun{
			Args:       args,
			DurationMS: elapsed.Milliseconds(),
			ExitCode:   res.code,
			StdoutLen:  len(res.stdout),
		})

		if !want[res.code] {
			t.Fatalf("run %d of %v exited %d, want one of %v\nstderr:\n%s",
				i+1, args, res.code, sortedCodes(want), res.stderr)
		}
	}

	median := medianDuration(durations)
	timing.MedianMS = median.Milliseconds()
	timing.MaxMS = maxDuration(durations).Milliseconds()
	timing.Within = median <= threshold

	t.Logf("scale: %v median %v max %v over %d runs (threshold %v)",
		args, median, maxDuration(durations), n, threshold)

	if !timing.Within {
		t.Errorf("%v median %v over %d runs, want at most %v (runs: %v)",
			args, median, n, threshold, durations)
	}

	return timing, last
}

// writeScaleReport puts the numbers where the workflow looks for them.
func writeScaleReport(dir string, report scaleReport) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	raw, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the report: %w", err)
	}
	path := filepath.Join(dir, reportFile)
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}
