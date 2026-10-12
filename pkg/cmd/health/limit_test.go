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

package health_test

import (
	"encoding/json"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	cmdhealth "github.com/NVIDIA/kubectl-nv/pkg/cmd/health"
)

// limitedReport is the shape --limit changes: the findings it keeps, the count
// it leaves behind, and the summary, which is about everything that was found
// rather than about what was printed.
type limitedReport struct {
	Findings []struct {
		Node string `json:"node"`
		Code string `json:"code"`
	} `json:"findings"`
	RemainingItemCount *int64 `json:"remainingItemCount"`
	Summary            struct {
		Errors   int `json:"errors"`
		Warnings int `json:"warnings"`
		Infos    int `json:"infos"`
	} `json:"summary"`
}

// unhealthyCluster is a cluster with more than one finding on it, so that a
// limit has something to leave out.
func unhealthyCluster() cluster {
	c := healthyCluster()
	c.objects = append(c.objects, hintedNode("no-gpus-1"), hintedNode("no-gpus-2"), hintedNode("no-gpus-3"))

	return c
}

// decodeReport reads the JSON report out of what the command printed.
func decodeReport(t *testing.T, raw string) limitedReport {
	t.Helper()

	var report limitedReport
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("decoding the report: %v\n%s", err, raw)
	}

	return report
}

// The flag is on the command itself, registered by pkg/output along with -o
// and --fields, so that every command spells it the same way.
func TestHealthRegistersTheLimitFlag(t *testing.T) {
	t.Parallel()

	c := cmdhealth.NewCommand(&cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()})

	flag := c.Flags().Lookup("limit")
	if flag == nil {
		t.Fatal("health has no --limit")
	}
	if flag.Value.Type() != "int" {
		t.Errorf("--limit is a %s, want an int", flag.Value.Type())
	}
	if flag.DefValue != "0" {
		t.Errorf("--limit defaults to %q, want 0, which prints every finding", flag.DefValue)
	}
}

// --limit is a flag of every list-like command and means the same thing on all
// of them. A health report is a list of findings, and a report with no way to
// ask for fewer is one a caller has to read whole before it can act.
func TestHealthLimitsTheFindings(t *testing.T) {
	t.Parallel()

	whole, _ := run(t, unhealthyCluster(), "-o", "json")
	full := decodeReport(t, whole.out.String())
	if len(full.Findings) < 3 {
		t.Fatalf("the fixture produced %d findings: a limit of 2 would prove nothing", len(full.Findings))
	}

	s, _ := run(t, unhealthyCluster(), "-o", "json", "--limit", "2")
	limited := decodeReport(t, s.out.String())

	if len(limited.Findings) != 2 {
		t.Errorf("--limit 2 printed %d findings", len(limited.Findings))
	}

	// After the sort, not before: the findings a caller gets with a limit have
	// to be the first ones of the report it would get without.
	for i := range limited.Findings {
		if limited.Findings[i] != full.Findings[i] {
			t.Errorf("finding %d is %v, the unlimited report has %v",
				i, limited.Findings[i], full.Findings[i])
		}
	}
}

// The summary is the arithmetic over what was checked, not over what was
// printed. A report saying two findings and summarising two when there were
// nine is a report that says the cluster is in better shape than it is.
func TestHealthLimitDoesNotChangeTheSummary(t *testing.T) {
	t.Parallel()

	whole, _ := run(t, unhealthyCluster(), "-o", "json")
	full := decodeReport(t, whole.out.String())

	s, _ := run(t, unhealthyCluster(), "-o", "json", "--limit", "1")
	limited := decodeReport(t, s.out.String())

	if limited.Summary != full.Summary {
		t.Errorf("the limited summary is %+v, the whole report's is %+v", limited.Summary, full.Summary)
	}
}

// Truncation is reported twice, the way it is on every other list-like
// command: a note on the error stream for a person, and a count in the output
// for a program, which never sees stderr.
func TestHealthReportsItsTruncation(t *testing.T) {
	t.Parallel()

	whole, _ := run(t, unhealthyCluster(), "-o", "json")
	full := decodeReport(t, whole.out.String())

	s, _ := run(t, unhealthyCluster(), "-o", "json", "--limit", "2")
	limited := decodeReport(t, s.out.String())

	if limited.RemainingItemCount == nil {
		t.Fatalf("the truncated report carries no remainingItemCount\n%s", s.out.String())
	}
	if want := int64(len(full.Findings) - 2); *limited.RemainingItemCount != want {
		t.Errorf("remainingItemCount is %d, want %d", *limited.RemainingItemCount, want)
	}

	if !strings.Contains(s.err.String(), "--limit 2") {
		t.Errorf("stderr does not report the truncation: %q", s.err.String())
	}
}

// A report that was not truncated says nothing about truncation. A zero where
// a count would be reads as "nothing left out", which is true, but a key that
// is always there invites a reader to branch on its value rather than on its
// presence.
func TestHealthReportsNoTruncationWhenThereIsNone(t *testing.T) {
	t.Parallel()

	s, _ := run(t, unhealthyCluster(), "-o", "json", "--limit", "500")
	limited := decodeReport(t, s.out.String())

	if limited.RemainingItemCount != nil {
		t.Errorf("remainingItemCount is %d on a report nothing was left out of", *limited.RemainingItemCount)
	}
	if strings.Contains(s.err.String(), "--limit") {
		t.Errorf("stderr reports a truncation that did not happen: %q", s.err.String())
	}
}

// The table is the same report, so it is limited the same way.
func TestHealthLimitsTheTableToo(t *testing.T) {
	t.Parallel()

	s, _ := run(t, unhealthyCluster(), "--limit", "2")

	rows := 0
	for _, line := range strings.Split(strings.TrimSpace(s.out.String()), "\n") {
		if strings.HasPrefix(line, "NODE") || !strings.Contains(line, "NV-") {
			continue
		}
		rows++
	}
	if rows != 2 {
		t.Errorf("the table carries %d finding rows, want 2\n%s", rows, s.out.String())
	}
}
