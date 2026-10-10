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

import "fmt"

// The exit codes kubectl-nv uses. They are part of the contract: scripts branch
// on them, so a new meaning needs a new code rather than a new reading of one.
const (
	// ExitOK means the run completed and found nothing at or above --fail-on.
	ExitOK = 0
	// ExitFindings means the run found something at or above --fail-on.
	ExitFindings = 1
	// ExitUsage means the command line or the configuration was wrong.
	ExitUsage = 2
	// ExitPartial means the run completed over less than it was asked for, for
	// instance because RBAC or a denied exec hid part of the cluster. What was
	// missed is listed in the output.
	ExitPartial = 3
)

// Severity is how bad one finding is.
type Severity string

const (
	// SeverityError is a finding that breaks the thing it was found on.
	SeverityError Severity = "error"
	// SeverityWarning is a finding worth acting on but not breaking.
	SeverityWarning Severity = "warning"
	// SeverityInfo is an observation.
	SeverityInfo Severity = "info"
)

// rank orders the severities. An unrecognised severity ranks 0, below every
// threshold, so an unknown value can never make a run fail by accident.
func (s Severity) rank() int {
	switch s {
	case SeverityInfo:
		return 1
	case SeverityWarning:
		return 2
	case SeverityError:
		return 3
	default:
		return 0
	}
}

// ReportState is what the exit code is derived from.
type ReportState struct {
	// Counts is how many findings of each severity the run produced.
	Counts map[Severity]int
	// Partial is set when the run could not see everything it was asked about.
	Partial bool
	// FailOn is the lowest severity that makes the run fail. The zero value
	// means SeverityError, so by default only errors fail a run.
	FailOn Severity
}

// NewReportState counts findings by severity and returns the state the exit
// code is read from.
func NewReportState(findings []Finding, partial bool, failOn Severity) ReportState {
	counts := make(map[Severity]int, 3)
	for _, f := range findings {
		counts[f.Severity]++
	}

	return ReportState{Counts: counts, Partial: partial, FailOn: failOn}
}

// ExitCode maps a report state onto the process exit code.
//
// Findings win over partial results: a run that found something at or above the
// threshold exits ExitFindings even when part of the cluster stayed out of
// reach, because the caller needs to hear about what was found first. A run
// that found nothing but could not see everything exits ExitPartial, so that
// "clean" is never reported for a scan that did not happen.
func ExitCode(state ReportState) int {
	threshold := state.FailOn
	if threshold.rank() == 0 {
		threshold = SeverityError
	}

	for severity, count := range state.Counts {
		if count > 0 && severity.rank() > 0 && severity.rank() >= threshold.rank() {
			return ExitFindings
		}
	}

	if state.Partial {
		return ExitPartial
	}

	return ExitOK
}

// UsageError is a command line or configuration error. It is the one error this
// package maps onto an exit code of its own.
type UsageError struct {
	msg string
}

// NewUsageError returns a UsageError with the formatted message.
func NewUsageError(format string, a ...any) *UsageError {
	return &UsageError{msg: fmt.Sprintf(format, a...)}
}

// Error implements error.
func (e *UsageError) Error() string { return e.msg }

// ExitCode is the code a command exits with after a usage error.
func (e *UsageError) ExitCode() int { return ExitUsage }
