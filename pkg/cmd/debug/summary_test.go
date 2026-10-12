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

package debug

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/debug"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
)

// render writes one report through the summary and returns its lines.
func render(t *testing.T, r *debug.Report) []string {
	t.Helper()

	var out bytes.Buffer
	if err := summarise(&out, r); err != nil {
		t.Fatalf("summarise: %v", err)
	}

	return strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
}

// reportWith builds a report whose node carries the given operands and whose
// bundle carries the given outcomes. The directory is short so that the width
// assertions are about the lines this command composes rather than about the
// length of a temporary directory's name.
func reportWith(operands []inventory.OperandPod, bundle debug.Bundle) *debug.Report {
	r := debug.NewReport("worker-1")
	r.Node.Operands = operands
	r.Node.Ready = true
	bundle.Directory = "/tmp/bundle"
	if bundle.Manifest == "" {
		bundle.Manifest = "/tmp/bundle/manifest.json"
	}
	r.Bundle = bundle

	return r
}

// sixOperands is a full GPU Operator install on one node.
func sixOperands() []inventory.OperandPod {
	components := []string{
		"gpu-feature-discovery",
		"nvidia-container-toolkit-daemonset",
		"nvidia-dcgm-exporter",
		"nvidia-device-plugin-daemonset",
		"nvidia-driver-daemonset",
		"nvidia-operator-validator",
	}

	out := make([]inventory.OperandPod, 0, len(components))
	for _, c := range components {
		out = append(out, inventory.OperandPod{
			Name: c + "-abcde", Component: c, Phase: "Running", Ready: true,
		})
	}

	return out
}

// TestSummaryFitsOneScreenWithEveryCollectorBroken is the bound by
// construction. The worst case is not a healthy cluster: it is the one where
// every collector has something to say, and that is exactly the run whose
// summary a support engineer most needs to be able to read.
func TestSummaryFitsOneScreenWithEveryCollectorBroken(t *testing.T) {
	t.Parallel()

	names := []string{}
	for _, c := range mustgather.Collectors() {
		names = append(names, c.Name)
	}
	if len(names) != 19 {
		t.Fatalf("the registry has %d collectors, this fixture was written for 19: %v", len(names), names)
	}

	// Every one of them not ok, in the three shapes they come in, each with a
	// reason far longer than any line may be.
	long := strings.Repeat("the API server said no at great length ", 10)
	bundle := debug.Bundle{}
	for i, name := range names {
		switch i % 3 {
		case 0:
			bundle.Errored = append(bundle.Errored, debug.CollectorOutcome{Name: name, Reason: long})
			bundle.Collectors.Error++
		case 1:
			bundle.Skipped = append(bundle.Skipped,
				debug.CollectorOutcome{Name: name, Reason: "forbidden: " + long})
			bundle.Collectors.Skipped++
		case 2:
			bundle.Skipped = append(bundle.Skipped,
				debug.CollectorOutcome{Name: name, Reason: name + " resource(s) not found in the cluster"})
			bundle.Collectors.Skipped++
		}
	}

	lines := render(t, reportWith(sixOperands(), bundle))

	if len(lines) > summaryLineBudget {
		t.Errorf("the summary is %d lines, want at most %d:\n%s",
			len(lines), summaryLineBudget, strings.Join(lines, "\n"))
	}
	for i, line := range lines {
		if len(line) > maxLineWidth {
			t.Errorf("line %d is %d characters, want at most %d:\n%s", i+1, len(line), maxLineWidth, line)
		}
	}

	text := strings.Join(lines, "\n")

	// The refusals and the failures are each named: they are what the user can
	// act on. The absences are folded, because "KubeVirt is not installed" is
	// not news to anyone who did not install KubeVirt.
	if !strings.Contains(text, "more, see -o json") {
		t.Errorf("nothing says how many outcomes were left out:\n%s", text)
	}
	if !strings.Contains(text, foldedSkipLabel) {
		t.Errorf("the absence skips were not folded into one line:\n%s", text)
	}
}

// TestSummaryBoundsTheOperandList covers the other half of the bound. A node
// running a dozen operands must not push the bundle and the next commands off
// the screen, because those are the lines the summary exists for.
func TestSummaryBoundsTheOperandList(t *testing.T) {
	t.Parallel()

	operands := make([]inventory.OperandPod, 0, 20)
	for i := 0; i < 20; i++ {
		operands = append(operands, inventory.OperandPod{
			Name: fmt.Sprintf("operand-%02d", i), Component: fmt.Sprintf("component-%02d", i),
			Phase: "Running", Ready: true,
		})
	}

	lines := render(t, reportWith(operands, debug.Bundle{}))

	if len(lines) > summaryLineBudget {
		t.Errorf("the summary is %d lines, want at most %d:\n%s",
			len(lines), summaryLineBudget, strings.Join(lines, "\n"))
	}

	text := strings.Join(lines, "\n")
	for _, want := range []string{
		"  kubectl nv health --node worker-1",
		"  kubectl nv health --from-bundle /tmp/bundle",
		"more operands, see -o json",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the summary does not carry %q:\n%s", want, text)
		}
	}
}

// TestSummaryNamesTheComponentNotThePodName pins what an operand line says.
// A pod name carries a replica set hash and tells a reader nothing they can
// match against a rule or against kubectl nv info; the component is the name
// the rest of the tool uses.
func TestSummaryNamesTheComponentNotThePodName(t *testing.T) {
	t.Parallel()

	lines := render(t, reportWith([]inventory.OperandPod{
		{Name: "a-pod-name-nobody-can-predict", Component: "nvidia-dcgm-exporter",
			Phase: "Running", Ready: true, Restarts: 2},
		{Name: "another-pod-7f9d", Component: "", Phase: "Pending", Ready: false},
	}, debug.Bundle{}))

	want := []string{
		"  nvidia-dcgm-exporter               Running   ready=true  restarts=2",
		"  <none>                             Pending   ready=false restarts=0",
	}
	for _, line := range want {
		if !contains(lines, line) {
			t.Errorf("the summary has no line %q:\n%s", line, strings.Join(lines, "\n"))
		}
	}
	for _, never := range []string{"a-pod-name-nobody-can-predict", "another-pod-7f9d"} {
		if strings.Contains(strings.Join(lines, "\n"), never) {
			t.Errorf("the summary prints the pod name %q rather than the component", never)
		}
	}
}

// TestSummaryWithoutAnOperand covers the node the operator has not reached.
func TestSummaryWithoutAnOperand(t *testing.T) {
	t.Parallel()

	lines := render(t, reportWith(nil, debug.Bundle{}))

	if !contains(lines, "  <none>") {
		t.Errorf("a node with no operands does not say so:\n%s", strings.Join(lines, "\n"))
	}
}

// TestSummaryWithoutABundleOffersNoBundleCommand covers the gather that could
// not run. Offering --from-bundle for a directory that holds no manifest sends
// the user to a command that will fail, and telling them to send it to support
// sends an empty directory.
func TestSummaryWithoutABundleOffersNoBundleCommand(t *testing.T) {
	t.Parallel()

	r := debug.NewReport("worker-1")
	r.Bundle = debug.Bundle{Directory: "/tmp/bundle"}

	lines := render(t, r)
	text := strings.Join(lines, "\n")

	if !contains(lines, "  kubectl nv health --node worker-1") {
		t.Errorf("the cluster command is missing:\n%s", text)
	}
	if strings.Contains(text, "--from-bundle") {
		t.Errorf("a bundle with no manifest is still offered to --from-bundle:\n%s", text)
	}
}

// hostileReason is a collector reason carrying terminal control: an escape
// that would start a sequence, a bell, and a carriage return that would
// overwrite the line a reader is looking at.
const hostileReason = "forbidden: \x1b[2Jcleared\x07 and \rrewritten\nsecond line\tand a tab"

// TestSummarySanitisesAHostileReason covers the text in this report that
// kubectl-nv did not write. A collector reason is an error message from a
// cluster where anyone with patch on nodes can choose what it says, and it is
// printed into the reader's terminal.
func TestSummarySanitisesAHostileReason(t *testing.T) {
	t.Parallel()

	var out bytes.Buffer
	r := reportWith(nil, debug.Bundle{
		Skipped:    []debug.CollectorOutcome{{Name: "bug-report", Reason: hostileReason}},
		Collectors: debug.CollectorCounts{Skipped: 1},
	})
	if err := summarise(&out, r); err != nil {
		t.Fatalf("summarise: %v", err)
	}

	// The escape, the bell and the carriage return are gone; the newline and
	// the tab became spaces; every other character survived, because a reason
	// that erased itself would hide the collector it is about.
	want := "  skipped bug-report: forbidden: [2Jcleared and rewritten second line and a tab"
	if !contains(strings.Split(out.String(), "\n"), want) {
		t.Errorf("the sanitised line is missing:\nwant %q\ngot\n%s", want, out.String())
	}

	for i, b := range out.Bytes() {
		if b < 0x20 && b != '\n' {
			t.Fatalf("byte %d of the summary is control character %#x", i, b)
		}
	}
}

// TestSummaryTruncatesALongReason keeps one collector's error message from
// taking the screen the summary is supposed to be.
func TestSummaryTruncatesALongReason(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("x", maxReasonWidth*3)
	lines := render(t, reportWith(nil, debug.Bundle{
		Errored:    []debug.CollectorOutcome{{Name: "daemonsets", Reason: long}},
		Collectors: debug.CollectorCounts{Error: 1},
	}))

	want := "  error   daemonsets: " + strings.Repeat("x", maxReasonWidth-3) + "..."
	if !contains(lines, want) {
		t.Errorf("the truncated line is missing:\nwant %q\ngot\n%s", want, strings.Join(lines, "\n"))
	}
}

// contains reports whether lines holds want, compared whole.
func contains(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}

	return false
}
