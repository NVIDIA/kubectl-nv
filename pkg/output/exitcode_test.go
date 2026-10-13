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

package output

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// findingsOf builds one finding per severity named.
func findingsOf(severities ...Severity) []Finding {
	findings := make([]Finding, 0, len(severities))
	for i, s := range severities {
		findings = append(findings, Finding{
			Node:     fmt.Sprintf("node-%d", i),
			Code:     fmt.Sprintf("GPU%03d", i),
			Severity: s,
		})
	}

	return findings
}

func TestExitCode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status ReportStatus
		failOn Severity
		want   int
	}{
		{
			name:   "no findings",
			status: ReportStatus{},
			failOn: SeverityWarning,
			want:   0,
		},
		{
			name:   "info below a warning threshold",
			status: ReportStatus{Findings: findingsOf(SeverityInfo, SeverityInfo)},
			failOn: SeverityWarning,
			want:   0,
		},
		{
			name:   "warning with fail-on error",
			status: ReportStatus{Findings: findingsOf(SeverityWarning)},
			failOn: SeverityError,
			want:   0,
		},
		{
			name:   "warning with fail-on warning",
			status: ReportStatus{Findings: findingsOf(SeverityWarning)},
			failOn: SeverityWarning,
			want:   1,
		},
		{
			name:   "error with fail-on error",
			status: ReportStatus{Findings: findingsOf(SeverityError)},
			failOn: SeverityError,
			want:   1,
		},
		{
			name:   "error with fail-on warning",
			status: ReportStatus{Findings: findingsOf(SeverityError)},
			failOn: SeverityWarning,
			want:   1,
		},
		{
			name:   "info with fail-on info",
			status: ReportStatus{Findings: findingsOf(SeverityInfo)},
			failOn: SeverityInfo,
			want:   1,
		},
		{
			name:   "empty fail-on means error",
			status: ReportStatus{Findings: findingsOf(SeverityWarning)},
			failOn: "",
			want:   0,
		},
		{
			name:   "empty fail-on still fails on an error",
			status: ReportStatus{Findings: findingsOf(SeverityError)},
			failOn: "",
			want:   1,
		},
		{
			name:   "partial with no findings",
			status: ReportStatus{Partial: true},
			failOn: SeverityError,
			want:   3,
		},
		{
			name: "partial with what it could not reach listed",
			status: ReportStatus{
				Partial:     true,
				Unreachable: []Unreachable{{Resource: "nodes/node-a/proxy", Node: "node-a", Reason: "forbidden"}},
			},
			failOn: SeverityError,
			want:   3,
		},
		{
			name:   "partial with findings below the threshold",
			status: ReportStatus{Findings: findingsOf(SeverityWarning), Partial: true},
			failOn: SeverityError,
			want:   3,
		},
		{
			// Findings win over partial results: a run that found something at
			// or above the threshold failed, whatever else it could not reach.
			name:   "partial with an error",
			status: ReportStatus{Findings: findingsOf(SeverityError), Partial: true},
			failOn: SeverityError,
			want:   1,
		},
		{
			name:   "an unknown severity on a finding never trips the threshold",
			status: ReportStatus{Findings: findingsOf(Severity("fatal"))},
			failOn: SeverityInfo,
			want:   0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ExitCode(tc.status, tc.failOn); got != tc.want {
				t.Errorf("ExitCode(%+v, %q) = %d, want %d", tc.status, tc.failOn, got, tc.want)
			}
		})
	}
}

func TestExitCodeConstants(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		got  int
		want int
	}{
		{"ExitOK", ExitOK, 0},
		{"ExitFindings", ExitFindings, 1},
		{"ExitUsage", ExitUsage, 2},
		{"ExitPartial", ExitPartial, 3},
		{"ExitError", ExitError, 4},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

// coded is an error from somewhere else that carries its own exit code.
type coded struct{ code int }

func (c coded) Error() string { return "coded" }
func (c coded) ExitCode() int { return c.code }

func TestExitCodeFor(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{name: "nil is success", err: nil, want: ExitOK},
		{name: "a usage error", err: NewUsageError("bad flag"), want: ExitUsage},
		{
			name: "a usage error behind a wrap",
			err:  fmt.Errorf("parsing flags: %w", NewUsageError("bad flag")),
			want: ExitUsage,
		},
		{name: "any error that carries its own code", err: coded{code: 7}, want: 7},
		{
			name: "a coded error behind a wrap",
			err:  fmt.Errorf("listing nodes: %w", coded{code: 3}),
			want: 3,
		},
		{name: "a plain error could not run", err: errors.New("connection refused"), want: ExitError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ExitCodeFor(tc.err); got != tc.want {
				t.Errorf("ExitCodeFor(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestSeverityConstants(t *testing.T) {
	t.Parallel()

	if SeverityError != "error" || SeverityWarning != "warning" || SeverityInfo != "info" {
		t.Errorf("severities = %q/%q/%q, want error/warning/info", SeverityError, SeverityWarning, SeverityInfo)
	}
}

func TestParseSeverity(t *testing.T) {
	t.Parallel()

	t.Run("accepted values", func(t *testing.T) {
		t.Parallel()

		for input, want := range map[string]Severity{
			"":        SeverityError,
			"error":   SeverityError,
			"warning": SeverityWarning,
			"info":    SeverityInfo,
		} {
			got, err := ParseSeverity(input)
			if err != nil {
				t.Errorf("ParseSeverity(%q): %v", input, err)
				continue
			}
			if got != want {
				t.Errorf("ParseSeverity(%q) = %q, want %q", input, got, want)
			}
		}
	})

	// A typo in --fail-on used to be swallowed and quietly read as error. It is
	// the difference between a gate and no gate, so it has to be rejected.
	t.Run("rejected values", func(t *testing.T) {
		t.Parallel()

		for _, input := range []string{"fatal", "Error", "warn", "none", " "} {
			got, err := ParseSeverity(input)
			if err == nil {
				t.Errorf("ParseSeverity(%q) = %q, want an error", input, got)
				continue
			}

			var usage *UsageError
			if !errors.As(err, &usage) {
				t.Errorf("ParseSeverity(%q) error %T, want *UsageError", input, err)
				continue
			}
			if ExitCodeFor(err) != ExitUsage {
				t.Errorf("ParseSeverity(%q) maps to %d, want %d", input, ExitCodeFor(err), ExitUsage)
			}
			if !strings.Contains(err.Error(), input) {
				t.Errorf("error %q does not name the rejected value %q", err.Error(), input)
			}
			for _, valid := range []string{"error", "warning", "info"} {
				if !strings.Contains(err.Error(), valid) {
					t.Errorf("error %q does not list %q", err.Error(), valid)
				}
			}
		}
	})
}

func TestUsageErrorWrapsAndReportsExitUsage(t *testing.T) {
	t.Parallel()

	err := NewUsageError("bad flag %q", "--nope")

	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("errors.As failed for %T", err)
	}
	if usage.ExitCode() != ExitUsage {
		t.Errorf("ExitCode() = %d, want %d", usage.ExitCode(), ExitUsage)
	}
	if !strings.Contains(err.Error(), `bad flag "--nope"`) {
		t.Errorf("Error() = %q, want it to carry the formatted message", err.Error())
	}

	// A plain error is not a usage error: the discrimination has to hold in both
	// directions or the exit code mapping is meaningless.
	if errors.As(errors.New("bad flag"), &usage) {
		t.Error("errors.As matched a plain error")
	}
}

func TestUsageErrorUnwrapsItsCause(t *testing.T) {
	t.Parallel()

	cause := errors.New("no such file")
	err := WrapUsageError(cause, "reading the kubeconfig")

	if !errors.Is(err, cause) {
		t.Error("errors.Is did not find the cause")
	}
	if !strings.Contains(err.Error(), "reading the kubeconfig") {
		t.Errorf("Error() = %q, want the context", err.Error())
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Errorf("Error() = %q, want the cause", err.Error())
	}
	if ExitCodeFor(err) != ExitUsage {
		t.Errorf("ExitCodeFor = %d, want %d", ExitCodeFor(err), ExitUsage)
	}

	// Without a cause there is nothing to unwrap, and Unwrap must not invent one.
	if errors.Unwrap(NewUsageError("bare")) != nil {
		t.Error("a usage error with no cause unwrapped to something")
	}
}

// A consumer that has to special case null before ranging over findings is a
// consumer that will get it wrong.
func TestReportStatusMarshalsFindingsAsAnArray(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(ReportStatus{})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if string(raw) != `{"findings":[]}` {
		t.Errorf("json = %s, want {\"findings\":[]}", raw)
	}

	full, err := json.Marshal(ReportStatus{
		Findings:    []Finding{{Node: "node-a", Code: "GPU001", Severity: SeverityError, Message: "no driver"}},
		Partial:     true,
		Unreachable: []Unreachable{{Resource: "pods/log", Node: "node-a", Reason: "forbidden"}},
	})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	want := `{"findings":[{"code":"GPU001","severity":"error","node":"node-a","message":"no driver","evidence":{}}],` +
		`"partial":true,"unreachable":[{"resource":"pods/log","node":"node-a","reason":"forbidden"}]}`
	if string(full) != want {
		t.Errorf("json  = %s\nwant = %s", full, want)
	}
}
