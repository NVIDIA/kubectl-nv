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

// Everything the scale suite decides with, kept apart from the suite that
// drives the cluster so that it can be tested without a 2,000 node lane:
// the configuration, the arithmetic, the reading of the binary's JSON, the
// three assertions that can fail the suite, and the report it writes.
//
// It is a _test.go file carrying the scale tag alone, so test/e2e ships no
// non-test code and "go test -tags scale ./test/e2e/..." exercises all of it
// with no cluster, no binary and no KUBECONFIG.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
)

// scaleNodePrefix is the name hack/mokka/kwok-nodes.sh gives every node it
// creates. The two have to agree, and this constant is the statement of that:
// a node counted as fake here is a node that script made.
const scaleNodePrefix = "kwok-gpu-"

// The environment the suite is parameterised by. The defaults are the nightly
// lane; the overrides exist so that the same suite can be run against a 20
// node smoke on a laptop, where a kind cluster sharing a machine with a
// browser is nowhere near a CI runner's timing.
const (
	envNodes           = "KUBECTL_NV_SCALE_NODES"
	envListThreshold   = "KUBECTL_NV_SCALE_LIST_THRESHOLD"
	envHealthThreshold = "KUBECTL_NV_SCALE_HEALTH_THRESHOLD"
	envRuns            = "KUBECTL_NV_SCALE_RUNS"
	envReportDir       = "KUBECTL_NV_SCALE_REPORT_DIR"
	envRealNodes       = "KUBECTL_NV_SCALE_REAL_NODES"
	envHintedNodes     = "KUBECTL_NV_SCALE_HINTED_NODES"
	envSettleTimeout   = "KUBECTL_NV_SCALE_SETTLE_TIMEOUT"
)

// healthNodesPath is where the merged HealthReport carries the node count:
// pkg/health.Summary.NodesChecked, under the inline summary. It is a constant
// and no longer an environment variable, because the field now exists and a
// knob for "where did the field go" is a knob for not noticing that it moved.
const healthNodesPath = "summary.nodesChecked"

// healthFindingsPath is where the findings are. output.ReportStatus is inline
// in HealthReport, so they sit at the top level and not under the summary, and
// --summary only suppresses them in the table: -o json carries the whole
// report either way.
const healthFindingsPath = "findings"

// scaleConfig is the whole of what the suite reads from its environment.
type scaleConfig struct {
	// Nodes is how many fake nodes the lane is expected to hold.
	Nodes int
	// ListThreshold and HealthThreshold bound the median run of each command.
	ListThreshold   time.Duration
	HealthThreshold time.Duration
	// Runs is how many times each command is measured.
	Runs int
	// RealNodes is how many nodes the lane holds that kwok-nodes.sh did not
	// create. On a t4 lane that is the two workers, exactly: the control
	// plane carries no allocatable GPUs and list leaves it out. It is an
	// equality and not a floor, because "at least one" passes against a lane
	// that lost a worker, which is the kind of lane whose timings mean
	// nothing.
	RealNodes int
	// SettleTimeout bounds the wait for the lane to stop moving before
	// anything is measured.
	SettleTimeout time.Duration
	// HintedNodes is how many nodes health counts that list does not. The
	// health command builds its inventory with IncludeHinted, so a node that
	// carries the GPU labels and advertises no allocatable GPUs is checked by
	// the rules and left out of the list. On a Mokka lane that is the control
	// plane: nvml-mock runs there and labels it, and the operands do not
	// tolerate its taint. So health counts one more node than list names, and
	// the suite has to know which.
	HintedNodes int
	// ReportDir is where the numbers are written for the workflow to upload.
	ReportDir string
}

// defaultReportDir is the e2e diagnostics directory the Makefile uses for a
// lane named "scale", relative to the package directory that go test runs the
// test binary in.
var defaultReportDir = filepath.Join("..", "..", "bin", "e2e-diagnostics-scale")

// scaleConfigFrom reads the configuration through the given lookup, which is
// os.Getenv in the suite and a fixed map in the tests. Every value is checked
// here: a threshold that silently fell back to its default because the
// workflow misspelled the unit would turn the nightly into a pass nobody
// earned.
func scaleConfigFrom(getenv func(string) string) (scaleConfig, error) {
	cfg := scaleConfig{
		Nodes:           2000,
		ListThreshold:   5 * time.Second,
		HealthThreshold: 15 * time.Second,
		Runs:            3,
		RealNodes:       2,
		HintedNodes:     1,
		SettleTimeout:   10 * time.Minute,
		ReportDir:       defaultReportDir,
	}

	var err error
	if cfg.Nodes, err = scalePositiveInt(getenv, envNodes, cfg.Nodes); err != nil {
		return scaleConfig{}, err
	}
	if cfg.Runs, err = scalePositiveInt(getenv, envRuns, cfg.Runs); err != nil {
		return scaleConfig{}, err
	}
	if cfg.RealNodes, err = scalePositiveInt(getenv, envRealNodes, cfg.RealNodes); err != nil {
		return scaleConfig{}, err
	}
	if cfg.HintedNodes, err = scaleWholeInt(getenv, envHintedNodes, cfg.HintedNodes); err != nil {
		return scaleConfig{}, err
	}
	if cfg.SettleTimeout, err = scalePositiveDuration(getenv, envSettleTimeout, cfg.SettleTimeout); err != nil {
		return scaleConfig{}, err
	}
	if cfg.ListThreshold, err = scalePositiveDuration(getenv, envListThreshold, cfg.ListThreshold); err != nil {
		return scaleConfig{}, err
	}
	if cfg.HealthThreshold, err = scalePositiveDuration(getenv, envHealthThreshold, cfg.HealthThreshold); err != nil {
		return scaleConfig{}, err
	}

	if raw := getenv(envReportDir); raw != "" {
		cfg.ReportDir = raw
	}

	return cfg, nil
}

// scalePositiveInt reads a whole number above zero, or keeps the default when the
// variable is unset.
func scalePositiveInt(getenv func(string) string, name string, fallback int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s=%q: want a positive whole number", name, raw)
	}

	return n, nil
}

// scaleWholeInt reads a whole number at or above zero. Zero is a legitimate answer
// for a count of hinted nodes: a lane whose control plane carries no GPU
// labels at all has none.
func scaleWholeInt(getenv func(string) string, name string, fallback int) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("%s=%q: want a whole number, zero or above", name, raw)
	}

	return n, nil
}

// scalePositiveDuration reads a Go duration above zero, or keeps the default when
// the variable is unset.
func scalePositiveDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
	raw := getenv(name)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: want a Go duration such as 5s", name, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s=%q: want a positive duration", name, raw)
	}

	return d, nil
}

// scaleMedianDuration is the middle of the samples, or the mean of the two middle
// ones when there is an even number of them. It sorts a copy: the report
// prints the runs in the order they were measured.
func scaleMedianDuration(samples []time.Duration) time.Duration {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}

	return (sorted[mid-1] + sorted[mid]) / 2
}

// scaleMaxDuration is the slowest sample.
func scaleMaxDuration(samples []time.Duration) time.Duration {
	var worst time.Duration
	for _, d := range samples {
		if d > worst {
			worst = d
		}
	}

	return worst
}

// scaleNodeNames reads .items[].metadata.name out of a GPUNodeList. An item with
// no name is an error rather than an empty string: a list of 2,000 empty
// names still has length 2,000, and the count assertion would pass on it.
func scaleNodeNames(raw []byte) ([]string, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, fmt.Errorf("decoding the list output: %w", err)
	}

	names := make([]string, 0, len(list.Items))
	for i, item := range list.Items {
		if item.Metadata.Name == "" {
			return nil, fmt.Errorf("item %d of the list has no metadata.name", i)
		}
		names = append(names, item.Metadata.Name)
	}

	return names, nil
}

// scaleCountWithPrefix counts the names that start with prefix.
func scaleCountWithPrefix(names []string, prefix string) int {
	n := 0
	for _, name := range names {
		if strings.HasPrefix(name, prefix) {
			n++
		}
	}

	return n
}

// scaleCountAtPath reads a count out of JSON at a dotted path. An array counts its
// elements and a number is itself; anything else, including a path that does
// not resolve, is an error naming what was found instead. The health
// summary's field names are not frozen, and a count assertion that quietly
// compared nothing would be worse than a red suite.
func scaleCountAtPath(raw []byte, path string) (int, error) {
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, fmt.Errorf("decoding the output: %w", err)
	}

	segments := strings.Split(path, ".")
	cursor := doc
	for i, segment := range segments {
		object, ok := cursor.(map[string]any)
		if !ok {
			return 0, fmt.Errorf("%q is not an object, so %q cannot be read from it",
				strings.Join(segments[:i], "."), segment)
		}
		next, ok := object[segment]
		if !ok {
			if i == 0 {
				return 0, fmt.Errorf("no %q at the top of the output; the output holds: %s",
					segment, strings.Join(scaleSortedObjectKeys(object), ", "))
			}

			return 0, fmt.Errorf("no %q under %q in the output; %q holds: %s",
				segment, strings.Join(segments[:i], "."), strings.Join(segments[:i], "."),
				strings.Join(scaleSortedObjectKeys(object), ", "))
		}
		cursor = next
	}

	switch value := cursor.(type) {
	case []any:
		return len(value), nil
	case float64:
		if value != float64(int(value)) {
			return 0, fmt.Errorf("%q is %s, want a whole number",
				path, strconv.FormatFloat(value, 'g', -1, 64))
		}

		return int(value), nil
	default:
		return 0, fmt.Errorf("%q is a %s, want a number or an array", path, scaleJSONTypeOf(cursor))
	}
}

// scaleJSONTypeOf names a decoded JSON value the way the error messages read.
func scaleJSONTypeOf(value any) string {
	switch value.(type) {
	case string:
		return "string"
	case bool:
		return "boolean"
	case map[string]any:
		return "object"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", value)
	}
}

// scaleSortedObjectKeys lists an object's keys, so an error message is the same on every
// run and can be compared.
func scaleSortedObjectKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for k := range object {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys
}

// ---------------------------------------------------------------------------
// The three things that can fail the suite
// ---------------------------------------------------------------------------
//
// They live here, away from the cluster, because an assertion nothing tests is
// an assertion nobody knows the shape of. A reviewer handed this suite a fake
// binary that answered instantly with an empty list, with a list that named
// one node twice and dropped another, and with a non-zero exit beside a full
// list. All three passed. Every one of those is a case below now.

// The exit codes the measured commands are allowed. Anything else is a defect
// in the run rather than a slow run, and the suite says so instead of timing
// it.
//
// list must exit 0, and in particular not 3. Exit 3 is a partial read, which
// on this lane means the suite was handed an incomplete list and measured how
// fast it arrived. The lane kubeconfig is a kind cluster-admin, so there is
// nothing the binary may not read: a partial read here is always a defect.
// health may also exit 1, which is "findings at or above --fail-on", and this
// lane produces those by construction.
var (
	listExitCodes   = []int{0}
	healthExitCodes = []int{0, 1}
)

// scaleAllows reports whether code is one the command may exit with.
func scaleAllows(allowed []int, code int) bool {
	for _, want := range allowed {
		if want == code {
			return true
		}
	}

	return false
}

// scaleListCensus is what one run of "list -o json" said the lane holds.
type scaleListCensus struct {
	// Fake is how many nodes were named kwok-gpu-<n>.
	Fake int
	// Real is how many were not.
	Real int
	// Total is every node the run named.
	Total int
}

// scaleAssessList reads one run of "list -o json" and decides whether it named the
// lane. Counting is not enough: the count is right while the names are wrong
// if one node is listed twice and another is missing, which is exactly the
// answer a broken paginator gives. So the fake names have to be the set
// kwok-nodes.sh creates, exactly, each one once.
func scaleAssessList(raw []byte, wantFake, wantReal int) (scaleListCensus, error) {
	names, err := scaleNodeNames(raw)
	if err != nil {
		return scaleListCensus{}, err
	}

	seen := make(map[string]int, len(names))
	for _, name := range names {
		seen[name]++
	}
	repeated := make([]string, 0)
	for name, n := range seen {
		if n > 1 {
			repeated = append(repeated, fmt.Sprintf("%s (%d times)", name, n))
		}
	}
	if len(repeated) > 0 {
		sort.Strings(repeated)

		return scaleListCensus{}, fmt.Errorf("list named %d node(s) more than once: %s",
			len(repeated), scaleFirstN(repeated, 5))
	}

	// The names kwok-nodes.sh creates, by construction: kwok-gpu-1 upwards.
	wanted := make(map[string]bool, wantFake)
	for i := 1; i <= wantFake; i++ {
		wanted[fmt.Sprintf("%s%d", scaleNodePrefix, i)] = true
	}

	census := scaleListCensus{Total: len(names)}
	extra := make([]string, 0)
	for _, name := range names {
		if !strings.HasPrefix(name, scaleNodePrefix) {
			census.Real++
			continue
		}
		census.Fake++
		if !wanted[name] {
			extra = append(extra, name)
			continue
		}
		delete(wanted, name)
	}

	missing := make([]string, 0, len(wanted))
	for name := range wanted {
		missing = append(missing, name)
	}
	sort.Strings(missing)
	sort.Strings(extra)

	switch {
	case len(missing) > 0:
		return census, fmt.Errorf("list is missing %d of the %d fake nodes: %s",
			len(missing), wantFake, scaleFirstN(missing, 5))
	case len(extra) > 0:
		return census, fmt.Errorf("list names %d node(s) outside %s1..%d: %s",
			len(extra), scaleNodePrefix, wantFake, scaleFirstN(extra, 5))
	case census.Real != wantReal:
		return census, fmt.Errorf("list names %d node(s) that are not %s*, want exactly %d",
			census.Real, scaleNodePrefix, wantReal)
	}

	return census, nil
}

// scaleRunStats is the shape of one command's timings.
type scaleRunStats struct {
	Median time.Duration
	Max    time.Duration
	// Within is whether the median met the threshold. It is derived here and
	// never recomputed by a caller: two places deciding one thing is how a
	// report comes to disagree with the test that wrote it.
	Within bool
}

// scaleAssessRuns reduces the measured runs and decides whether they met the
// threshold. No runs at all is an error rather than a zero median: a command
// that was never run is not a command that was instant.
func scaleAssessRuns(durations []time.Duration, threshold time.Duration) (scaleRunStats, error) {
	if len(durations) == 0 {
		return scaleRunStats{}, fmt.Errorf("no runs were measured")
	}

	stats := scaleRunStats{
		Median: scaleMedianDuration(durations),
		Max:    scaleMaxDuration(durations),
	}
	stats.Within = stats.Median <= threshold

	if !stats.Within {
		return stats, fmt.Errorf("median %s over %d runs, want at most %s (runs: %v)",
			stats.Median, len(durations), threshold, durations)
	}

	return stats, nil
}

// scaleFirstN renders at most n of the names, so an error about a 2,000 node lane
// is still something a reader can take in.
func scaleFirstN(names []string, n int) string {
	if len(names) <= n {
		return strings.Join(names, ", ")
	}

	return fmt.Sprintf("%s and %d more", strings.Join(names[:n], ", "), len(names)-n)
}

// ---------------------------------------------------------------------------
// Deciding that the lane has stopped moving
// ---------------------------------------------------------------------------
//
// Measuring a lane that is still placing pods measures the scheduler. Two
// conditions say it has stopped: every DaemonSet has placed a pod on every
// node it wants one on, and the pod count is the same twice running. The
// decisions are here, away from the client that fetches the numbers, so they
// can be driven through the states a 2,000 node lane passes on its way up.

// scalePendingDaemonSets names the DaemonSets that have not yet placed a pod on
// every node they want one on.
//
// The comparison is currentNumberScheduled against desiredNumberScheduled and
// deliberately not numberReady. A fake node's pods are answered for by KWOK's
// pod Stages, so numberReady waits on a different controller than the one
// whose work perturbs a measurement, and on a lane where KWOK is slow it would
// wait for something that never settles.
func scalePendingDaemonSets(items []appsv1.DaemonSet) []string {
	pending := make([]string, 0)
	for i := range items {
		scaleDaemonSet := &items[i]
		if scaleDaemonSet.Status.CurrentNumberScheduled != scaleDaemonSet.Status.DesiredNumberScheduled {
			pending = append(pending, fmt.Sprintf("%s/%s (%d of %d)",
				scaleDaemonSet.Namespace, scaleDaemonSet.Name,
				scaleDaemonSet.Status.CurrentNumberScheduled, scaleDaemonSet.Status.DesiredNumberScheduled))
		}
	}
	sort.Strings(pending)

	return pending
}

// laneSettled decides whether the lane has stopped moving, and when it has not,
// says what is still moving. previous is the pod count from the poll before
// this one, or -1 on the first poll: a lane is never called laneSettled on the
// strength of a single observation, because one count on its own cannot be
// compared with anything.
func laneSettled(pending []string, total, previous int) (bool, string) {
	switch {
	case len(pending) > 0:
		return false, fmt.Sprintf("DaemonSets still placing pods: %s", scaleFirstN(pending, 5))
	case previous < 0:
		return false, fmt.Sprintf("the pod count is %d, with nothing yet to compare it against", total)
	case total != previous:
		return false, fmt.Sprintf("the pod count moved from %d to %d", previous, total)
	default:
		return true, ""
	}
}

// scaleOnFakeNode reports whether a pod is placed on a node kwok-nodes.sh made.
func scaleOnFakeNode(nodeName string) bool {
	return strings.HasPrefix(nodeName, scaleNodePrefix)
}

// ---------------------------------------------------------------------------
// The report the workflow uploads
// ---------------------------------------------------------------------------

// commandRun is one timed execution of the binary.
type commandRun struct {
	DurationMS int64 `json:"durationMs"`
	ExitCode   int   `json:"exitCode"`
	StdoutLen  int   `json:"stdoutBytes"`
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
	CommitSHA       string          `json:"commitSha"`
	Binary          string          `json:"binary"`
	ExpectedFake    int             `json:"expectedFakeNodes"`
	ExpectedReal    int             `json:"expectedRealNodes"`
	FakeNodes       int             `json:"fakeNodes"`
	RealNodes       int             `json:"realNodes"`
	TotalNodes      int             `json:"totalNodes"`
	PodsTotal       int             `json:"podsTotal"`
	PodsOnFakeNodes int             `json:"podsOnFakeNodes"`
	ExpectedHinted  int             `json:"expectedHintedNodes"`
	HealthNodes     int             `json:"healthSummaryNodes"`
	HealthFindings  int             `json:"healthFindings"`
	HealthErrors    int             `json:"healthErrors"`
	HealthWarnings  int             `json:"healthWarnings"`
	HealthInfos     int             `json:"healthInfos"`
	Timings         []commandTiming `json:"timings"`
}

// reportFile is the one file the workflow reaches for.
const reportFile = "scale-timings.json"

// writeScaleReport puts the numbers where the workflow looks for them. It is
// the only artifact this suite produces, so a round trip through it is a test:
// a job that uploads nothing reports nothing, whatever the suite decided.
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

// readScaleReport reads back what writeScaleReport wrote.
func readScaleReport(dir string) (scaleReport, error) {
	raw, err := os.ReadFile(filepath.Join(dir, reportFile))
	if err != nil {
		return scaleReport{}, fmt.Errorf("reading the report: %w", err)
	}
	var report scaleReport
	if err := json.Unmarshal(raw, &report); err != nil {
		return scaleReport{}, fmt.Errorf("decoding the report: %w", err)
	}

	return report, nil
}
