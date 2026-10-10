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
	"errors"
	"strings"
	"testing"
)

func TestExitCode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		state ReportState
		want  int
	}{
		{
			name:  "no findings",
			state: ReportState{FailOn: SeverityWarning},
			want:  0,
		},
		{
			name:  "info only below an info threshold is still below nothing",
			state: ReportState{Counts: map[Severity]int{SeverityInfo: 4}, FailOn: SeverityWarning},
			want:  0,
		},
		{
			name:  "warning with fail-on error",
			state: ReportState{Counts: map[Severity]int{SeverityWarning: 1}, FailOn: SeverityError},
			want:  0,
		},
		{
			name:  "warning with fail-on warning",
			state: ReportState{Counts: map[Severity]int{SeverityWarning: 1}, FailOn: SeverityWarning},
			want:  1,
		},
		{
			name:  "error with fail-on error",
			state: ReportState{Counts: map[Severity]int{SeverityError: 1}, FailOn: SeverityError},
			want:  1,
		},
		{
			name:  "error with fail-on warning",
			state: ReportState{Counts: map[Severity]int{SeverityError: 1}, FailOn: SeverityWarning},
			want:  1,
		},
		{
			name:  "info with fail-on info",
			state: ReportState{Counts: map[Severity]int{SeverityInfo: 1}, FailOn: SeverityInfo},
			want:  1,
		},
		{
			name:  "zero counts do not count",
			state: ReportState{Counts: map[Severity]int{SeverityError: 0}, FailOn: SeverityError},
			want:  0,
		},
		{
			name:  "empty fail-on defaults to error",
			state: ReportState{Counts: map[Severity]int{SeverityWarning: 3}},
			want:  0,
		},
		{
			name:  "empty fail-on still fails on an error",
			state: ReportState{Counts: map[Severity]int{SeverityError: 1}},
			want:  1,
		},
		{
			name:  "partial with no findings",
			state: ReportState{Partial: true, FailOn: SeverityError},
			want:  3,
		},
		{
			name:  "partial with findings below the threshold",
			state: ReportState{Counts: map[Severity]int{SeverityWarning: 2}, Partial: true, FailOn: SeverityError},
			want:  3,
		},
		{
			// Findings win over partial results: a run that found something at or
			// above the threshold failed, whatever else it could not reach.
			name:  "partial with an error",
			state: ReportState{Counts: map[Severity]int{SeverityError: 1}, Partial: true, FailOn: SeverityError},
			want:  1,
		},
		{
			name:  "an unknown severity never trips the threshold",
			state: ReportState{Counts: map[Severity]int{Severity("fatal"): 9}, FailOn: SeverityInfo},
			want:  0,
		},
		{
			name:  "an unknown threshold is treated as error",
			state: ReportState{Counts: map[Severity]int{SeverityWarning: 1}, FailOn: Severity("bogus")},
			want:  0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := ExitCode(tc.state); got != tc.want {
				t.Errorf("ExitCode(%+v) = %d, want %d", tc.state, got, tc.want)
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
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, tc.got, tc.want)
		}
	}
}

func TestSeverityConstants(t *testing.T) {
	t.Parallel()

	if SeverityError != "error" || SeverityWarning != "warning" || SeverityInfo != "info" {
		t.Errorf("severities = %q/%q/%q, want error/warning/info", SeverityError, SeverityWarning, SeverityInfo)
	}
}

func TestNewReportStateCountsFindingsBySeverity(t *testing.T) {
	t.Parallel()

	findings := []Finding{
		{Node: "node-a", Code: "GPU001", Severity: SeverityError},
		{Node: "node-a", Code: "GPU002", Severity: SeverityWarning},
		{Node: "node-b", Code: "GPU002", Severity: SeverityWarning},
		{Node: "node-b", Code: "GPU003", Severity: SeverityInfo},
	}

	state := NewReportState(findings, true, SeverityWarning)

	if state.Counts[SeverityError] != 1 {
		t.Errorf("error count = %d, want 1", state.Counts[SeverityError])
	}
	if state.Counts[SeverityWarning] != 2 {
		t.Errorf("warning count = %d, want 2", state.Counts[SeverityWarning])
	}
	if state.Counts[SeverityInfo] != 1 {
		t.Errorf("info count = %d, want 1", state.Counts[SeverityInfo])
	}
	if !state.Partial {
		t.Error("state.Partial = false, want true")
	}
	if state.FailOn != SeverityWarning {
		t.Errorf("state.FailOn = %q, want %q", state.FailOn, SeverityWarning)
	}
	if got := ExitCode(state); got != ExitFindings {
		t.Errorf("ExitCode(state) = %d, want %d", got, ExitFindings)
	}
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
