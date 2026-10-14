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
)

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
	// ExitError means the run could not happen: the API server was unreachable,
	// or kubectl-nv hit an internal error. Nothing was checked, so nothing can
	// be concluded from it.
	ExitError = 4
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

// severities lists the severities from least to most severe. The order is the
// ranking, so rank and the --fail-on help text cannot drift apart.
var severities = []Severity{SeverityInfo, SeverityWarning, SeverityError}

// rank orders the severities. An unrecognised severity ranks 0, below every
// threshold, so a finding this package does not understand can never fail a run
// by accident.
func (s Severity) rank() int {
	for i, known := range severities {
		if s == known {
			return i + 1
		}
	}

	return 0
}

// ParseSeverity turns a --fail-on value into a Severity. An empty value means
// SeverityError, the conservative default. Anything else that is not a severity
// is a usage error: a typo in --fail-on is the difference between a gate and no
// gate, so it has to stop the run rather than be read as something else.
func ParseSeverity(value string) (Severity, error) {
	if value == "" {
		return SeverityError, nil
	}

	for _, known := range severities {
		if Severity(value) == known {
			return known, nil
		}
	}

	names := make([]string, 0, len(severities))
	for _, known := range severities {
		names = append(names, string(known))
	}

	return "", NewUsageError("unknown severity %q: want one of %s", value, strings.Join(names, ", "))
}

// Unreachable is one thing a run was not allowed, or not able, to look at.
type Unreachable struct {
	// Resource is what could not be read, for instance "nodes/node-a/proxy".
	Resource string `json:"resource,omitempty" yaml:"resource,omitempty"`
	// Node is the node it belongs to, empty for a cluster scoped resource.
	Node string `json:"node,omitempty" yaml:"node,omitempty"`
	// Reason is why it could not be read, for instance "forbidden".
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// ReportStatus is the outcome of a run: what it found, and what it could not
// see. Every command that reports findings embeds it, and the exit code is read
// from it.
type ReportStatus struct {
	// Findings is what the run found, in the order it should be printed.
	Findings []Finding `json:"findings" yaml:"findings"`
	// Partial is set when the run could not see everything it was asked about.
	Partial bool `json:"partial,omitempty" yaml:"partial,omitempty"`
	// Unreachable lists what Partial is about, so that a reader never has to
	// guess which part of the cluster is missing from the findings.
	Unreachable []Unreachable `json:"unreachable,omitempty" yaml:"unreachable,omitempty"`

	// RemainingItemCount is how many of the repeated part --limit left out. It
	// is the same field a list kind carries in its ListMeta, under the same
	// name, because it answers the same question: a program reading stdout
	// never sees the truncation note on stderr.
	//
	// It is a pointer with omitempty, so a report nothing was left out of
	// carries no such key at all. A zero would be true and would invite a
	// reader to branch on the value rather than on the presence.
	RemainingItemCount *int64 `json:"remainingItemCount,omitempty" yaml:"remainingItemCount,omitempty"`
}

// MarshalJSON renders a nil Findings as an empty array, for the same reason
// List does: ranging over the result must not need a null check first.
func (s ReportStatus) MarshalJSON() ([]byte, error) {
	type statusJSON ReportStatus

	out := statusJSON(s)
	if out.Findings == nil {
		out.Findings = []Finding{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling the report status: %w", err)
	}

	return raw, nil
}

// ExitCode maps a report onto the process exit code. failOn is the threshold
// from --fail-on, as returned by ParseSeverity; its empty value means
// SeverityError.
//
// Findings win over partial results: a run that found something at or above the
// threshold exits ExitFindings even when part of the cluster stayed out of
// reach, because the caller needs to hear about what was found first. A run
// that found nothing but could not see everything exits ExitPartial, so that
// "clean" is never reported for a scan that did not happen.
func ExitCode(status ReportStatus, failOn Severity) int {
	if failOn == "" {
		failOn = SeverityError
	}

	for _, finding := range status.Findings {
		if finding.Severity.rank() >= failOn.rank() {
			return ExitFindings
		}
	}

	if status.Partial {
		return ExitPartial
	}

	return ExitOK
}

// ExitCodeFor maps an error onto the process exit code. Any error that carries
// its own code, this package's UsageError included, decides for itself;
// anything else means the run could not happen, which is ExitError.
func ExitCodeFor(err error) int {
	if err == nil {
		return ExitOK
	}

	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}

	return ExitError
}

// UsageError is a command line or configuration error: something the user can
// fix by typing something else.
type UsageError struct {
	msg string
	// Err is the cause, when there is one to unwrap to.
	Err error
}

// NewUsageError returns a UsageError with the formatted message and no cause.
func NewUsageError(format string, a ...any) *UsageError {
	return &UsageError{msg: fmt.Sprintf(format, a...)}
}

// WrapUsageError returns a UsageError carrying err as its cause.
func WrapUsageError(err error, format string, a ...any) *UsageError {
	return &UsageError{msg: fmt.Sprintf(format, a...), Err: err}
}

// Error implements error.
func (e *UsageError) Error() string {
	if e.Err == nil {
		return e.msg
	}

	return e.msg + ": " + e.Err.Error()
}

// Unwrap returns the cause, so that errors.Is and errors.As reach it.
func (e *UsageError) Unwrap() error { return e.Err }

// ExitCode is the code a command exits with after a usage error.
func (e *UsageError) ExitCode() int { return ExitUsage }
