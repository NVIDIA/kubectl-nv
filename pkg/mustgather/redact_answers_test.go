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

package mustgather_test

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	cmdhealth "github.com/NVIDIA/kubectl-nv/pkg/cmd/health"
	cmdlist "github.com/NVIDIA/kubectl-nv/pkg/cmd/list"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// bundleAnswers is what the two commands say about a bundle, with the node
// names left out. The names are the one thing redaction may change; everything
// else is what it must not.
type bundleAnswers struct {
	listExit    int
	healthExit  int
	nodeCount   int
	gpuTotals   []string
	codes       []string
	summaryLine string
}

// answersFrom runs both commands against a bundle and reads what they said.
func answersFrom(t *testing.T, dir string) bundleAnswers {
	t.Helper()

	listCode, listOut, listErr := runFromBundle(t, cmdlist.NewCommand, dir, "-o", "json")

	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Capacity    int64 `json:"capacity"`
			Allocatable int64 `json:"allocatable"`
			Requested   int64 `json:"requested"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(listOut), &list); err != nil {
		t.Fatalf("list --from-bundle printed no list: %v\nstdout:\n%s\nstderr:\n%s", err, listOut, listErr)
	}

	out := bundleAnswers{listExit: listCode, nodeCount: len(list.Items)}
	for _, item := range list.Items {
		out.gpuTotals = append(out.gpuTotals,
			itoa64(item.Capacity)+"/"+itoa64(item.Allocatable)+"/"+itoa64(item.Requested))
	}

	healthCode, healthOut, healthErr := runFromBundle(t, cmdhealth.NewCommand, dir, "-o", "json")
	out.healthExit = healthCode

	var report struct {
		Kind     string `json:"kind"`
		Findings []struct {
			Code     string `json:"code"`
			Severity string `json:"severity"`
		} `json:"findings"`
		Summary struct {
			Errors         int   `json:"errors"`
			Warnings       int   `json:"warnings"`
			Infos          int   `json:"infos"`
			NodesChecked   int   `json:"nodesChecked"`
			GPUCapacity    int64 `json:"gpuCapacity"`
			GPUAllocatable int64 `json:"gpuAllocatable"`
			GPURequested   int64 `json:"gpuRequested"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(healthOut), &report); err != nil {
		t.Fatalf("health --from-bundle printed no report: %v\nstdout:\n%s\nstderr:\n%s",
			err, healthOut, healthErr)
	}
	if report.Kind != output.KindHealthReport {
		t.Fatalf("health --from-bundle printed a %q, want a %s", report.Kind, output.KindHealthReport)
	}

	for _, f := range report.Findings {
		out.codes = append(out.codes, f.Code+":"+f.Severity)
	}
	sort.Strings(out.codes)
	sort.Strings(out.gpuTotals)
	out.summaryLine = strings.Join([]string{
		itoa64(int64(report.Summary.Errors)), itoa64(int64(report.Summary.Warnings)),
		itoa64(int64(report.Summary.Infos)), itoa64(int64(report.Summary.NodesChecked)),
		itoa64(report.Summary.GPUCapacity), itoa64(report.Summary.GPUAllocatable),
		itoa64(report.Summary.GPURequested),
	}, "/")

	return out
}

// itoa64 renders a count for a comparison key.
func itoa64(n int64) string {
	var digits []byte
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		return "-" + string(digits)
	}

	return string(digits)
}

// The whole point of a pseudonym rather than a blank: a redacted bundle has to
// answer every question the plain one answers, the same way. A redaction that
// rewrote a toleration's key, a boolean whose name holds "token", or the three
// letters of a node called "gpu" would change which pods the operator can
// place and what every node advertises, and a support engineer would be
// debugging a cluster that does not exist.
//
// Both bundles come from a real gather over the same fixture, not from files
// written here: what is compared is what the tool produces.
func TestARedactedBundleGivesTheSameAnswers(t *testing.T) {
	plain := gatherForRoundTrip(t)
	redacted := gatherForRoundTripRedacted(t)

	before := answersFrom(t, plain)
	after := answersFrom(t, redacted)

	if before.listExit != after.listExit {
		t.Errorf("list exits %d on the plain bundle and %d on the redacted one",
			before.listExit, after.listExit)
	}
	if before.healthExit != after.healthExit {
		t.Errorf("health exits %d on the plain bundle and %d on the redacted one",
			before.healthExit, after.healthExit)
	}
	if before.nodeCount != after.nodeCount {
		t.Errorf("the plain bundle holds %d nodes and the redacted one %d",
			before.nodeCount, after.nodeCount)
	}
	if !sameStrings(before.gpuTotals, after.gpuTotals) {
		t.Errorf("the GPU totals changed:\n  plain:    %v\n  redacted: %v",
			before.gpuTotals, after.gpuTotals)
	}
	if !sameStrings(before.codes, after.codes) {
		t.Errorf("the findings changed:\n  plain:    %v\n  redacted: %v", before.codes, after.codes)
	}
	if before.summaryLine != after.summaryLine {
		t.Errorf("the summary changed:\n  plain:    %s\n  redacted: %s",
			before.summaryLine, after.summaryLine)
	}
}

// sameStrings compares two sorted slices.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
