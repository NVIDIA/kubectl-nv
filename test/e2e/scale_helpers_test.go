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
