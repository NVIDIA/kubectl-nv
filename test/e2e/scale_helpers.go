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

// The arithmetic and the JSON reading the scale suite does, kept apart from
// the suite so that they can be tested without a 2,000 node lane. Nothing
// here talks to a cluster or to the binary.
package e2e

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
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
	envHealthCountPath = "KUBECTL_NV_SCALE_HEALTH_COUNT_PATH"
	envReportDir       = "KUBECTL_NV_SCALE_REPORT_DIR"
)

// scaleConfig is the whole of what the suite reads from its environment.
type scaleConfig struct {
	// Nodes is how many fake nodes the lane is expected to hold.
	Nodes int
	// ListThreshold and HealthThreshold bound the median run of each command.
	ListThreshold   time.Duration
	HealthThreshold time.Duration
	// Runs is how many times each command is measured.
	Runs int
	// HealthCountPath is the dotted path to the node count in the health
	// summary. It is a variable and not a constant because the summary's
	// field names are younger than this suite: a rename has to be a one line
	// change to the workflow, not a red nightly nobody can explain.
	HealthCountPath string
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
		HealthCountPath: "summary.nodesChecked",
		ReportDir:       defaultReportDir,
	}

	var err error
	if cfg.Nodes, err = positiveInt(getenv, envNodes, cfg.Nodes); err != nil {
		return scaleConfig{}, err
	}
	if cfg.Runs, err = positiveInt(getenv, envRuns, cfg.Runs); err != nil {
		return scaleConfig{}, err
	}
	if cfg.ListThreshold, err = positiveDuration(getenv, envListThreshold, cfg.ListThreshold); err != nil {
		return scaleConfig{}, err
	}
	if cfg.HealthThreshold, err = positiveDuration(getenv, envHealthThreshold, cfg.HealthThreshold); err != nil {
		return scaleConfig{}, err
	}

	if raw := getenv(envHealthCountPath); raw != "" {
		if strings.TrimSpace(raw) == "" {
			return scaleConfig{}, fmt.Errorf("%s=%q: want a dotted JSON path", envHealthCountPath, raw)
		}
		cfg.HealthCountPath = strings.TrimSpace(raw)
	}
	if raw := getenv(envReportDir); raw != "" {
		cfg.ReportDir = raw
	}

	return cfg, nil
}

// positiveInt reads a whole number above zero, or keeps the default when the
// variable is unset.
func positiveInt(getenv func(string) string, name string, fallback int) (int, error) {
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

// positiveDuration reads a Go duration above zero, or keeps the default when
// the variable is unset.
func positiveDuration(getenv func(string) string, name string, fallback time.Duration) (time.Duration, error) {
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

// medianDuration is the middle of the samples, or the mean of the two middle
// ones when there is an even number of them. It sorts a copy: the report
// prints the runs in the order they were measured.
func medianDuration(samples []time.Duration) time.Duration {
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

// maxDuration is the slowest sample.
func maxDuration(samples []time.Duration) time.Duration {
	var worst time.Duration
	for _, d := range samples {
		if d > worst {
			worst = d
		}
	}

	return worst
}

// nodeNames reads .items[].metadata.name out of a GPUNodeList. An item with
// no name is an error rather than an empty string: a list of 2,000 empty
// names still has length 2,000, and the count assertion would pass on it.
func nodeNames(raw []byte) ([]string, error) {
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

// countWithPrefix counts the names that start with prefix.
func countWithPrefix(names []string, prefix string) int {
	n := 0
	for _, name := range names {
		if strings.HasPrefix(name, prefix) {
			n++
		}
	}

	return n
}

// countAtPath reads a count out of JSON at a dotted path. An array counts its
// elements and a number is itself; anything else, including a path that does
// not resolve, is an error naming what was found instead. The health
// summary's field names are not frozen, and a count assertion that quietly
// compared nothing would be worse than a red suite.
func countAtPath(raw []byte, path string) (int, error) {
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
					segment, strings.Join(sortedObjectKeys(object), ", "))
			}

			return 0, fmt.Errorf("no %q under %q in the output; %q holds: %s",
				segment, strings.Join(segments[:i], "."), strings.Join(segments[:i], "."),
				strings.Join(sortedObjectKeys(object), ", "))
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
		return 0, fmt.Errorf("%q is a %s, want a number or an array", path, jsonTypeOf(cursor))
	}
}

// jsonTypeOf names a decoded JSON value the way the error messages read.
func jsonTypeOf(value any) string {
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

// sortedObjectKeys lists an object's keys, so an error message is the same on every
// run and can be compared.
func sortedObjectKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for k := range object {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys
}

// sortedCodes lists the exit codes a run is allowed, for an error message.
func sortedCodes(allowed map[int]bool) []int {
	codes := make([]int, 0, len(allowed))
	for code, ok := range allowed {
		if ok {
			codes = append(codes, code)
		}
	}
	sort.Ints(codes)

	return codes
}
