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
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/NVIDIA/kubectl-nv/pkg/debug"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// none is what an empty field says, as kubectl describe does, so that a field
// the cluster left empty and a field this command forgot to fill read
// differently.
const none = "<none>"

// keyWidth is how wide the key column is. The same figure kubectl nv info
// uses, so that the two commands' output lines up on a reader's screen.
const keyWidth = 16

// The shape of the screen. These are what makes "it fits" a property of the
// renderer rather than of the cluster it happens to be pointed at: a node with
// twenty operands and a gather that was refused every collector produces the
// same number of lines as a healthy one.
//
// summaryLineBudget is the whole screen and is the sum of the parts below:
// eight lines about the node, the operands heading and its lines, the two
// bundle lines, the collector outcomes, and the "Next:" heading with its two
// commands.
const (
	summaryLineBudget = 30
	// maxOperandLines bounds the operand list, counting the line that says how
	// many were left out.
	maxOperandLines = 8
	// maxOutcomeLines bounds the collector outcomes, counting the line that
	// says how many were left out.
	maxOutcomeLines = 8
	// maxLineWidth is how wide any line this file composes may be. It is under
	// a hundred columns so that the summary survives being pasted into a
	// ticket. The bundle directory is the one value that can push a line past
	// it, and it is not truncated: a path a reader cannot copy whole is worse
	// than a long line.
	maxLineWidth = 96
	// maxReasonWidth is how much of one collector's reason is shown. The whole
	// text is in -o json; what belongs here is enough to tell a refusal from a
	// timeout from a cluster that answered something unexpected.
	maxReasonWidth = 60
)

// foldedSkipLabel introduces the collectors that had nothing to collect. They
// are folded into one line because "KubeVirt is not installed" is not news to
// anyone who did not install KubeVirt, and seventeen such lines would push the
// bundle and the next commands off the screen.
const foldedSkipLabel = "  skipped (nothing to collect): "

// ellipsis marks a value this file cut short. Three ASCII dots rather than the
// single character, because the summary is measured in columns and a reader
// may be piping it somewhere that renders the character as two.
const ellipsis = "..."

// summarise writes one node and its bundle as a screen a support engineer
// reads without scrolling.
//
// It is not a table. A report with a node, its operands and a bundle in it is
// not a row, and rendering it as one would put the interesting half off the
// right of the terminal. What it is instead is a fixed, short shape: the node,
// the operands, the bundle, and the commands to run next.
func summarise(w io.Writer, r *debug.Report) error {
	p := &printer{w: w}
	n := &r.Node

	p.kv("Node", n.Name)
	p.kv("Product", valueOr(n.Product, none))
	p.kv("GPUs", gpuCounts(n))
	p.kv("Driver", driverLine(n))
	p.kv("Mode", valueOr(n.Mode, none))
	p.kv("MIG", migLine(n))
	p.kv("Upgrade state", valueOr(n.UpgradeState, none))
	p.kv("Status", status(n, r))

	summariseOperands(p, n)
	summariseBundle(p, &r.Bundle)
	summariseNext(p, n.Name, &r.Bundle)

	if p.err != nil {
		return fmt.Errorf("writing the debug summary: %w", p.err)
	}

	return nil
}

// gpuCounts renders the three numbers a reader compares against each other:
// what is asked for, what is left, and what the node has. A node whose
// requested equals its capacity is a node nothing more can land on, and that
// is visible here and nowhere else in one line.
func gpuCounts(n *inventory.GPUNode) string {
	// Reporting 0 requested for a node nobody counted the pods of would be a
	// claim nobody made, and on a busy node a wrong one.
	requested := "?"
	if n.RequestedKnown {
		requested = strconv.FormatInt(n.Requested, 10)
	}

	return fmt.Sprintf("%s/%d/%d  (requested/allocatable/capacity)",
		requested, n.Allocatable, n.Capacity)
}

// driverLine puts the driver and the CUDA version on one line: a mismatch
// between them is one of the things this command exists to make visible.
func driverLine(n *inventory.GPUNode) string {
	return fmt.Sprintf("%s  (CUDA %s)", valueOr(n.DriverVersion, none), valueOr(n.CUDAVersion, none))
}

// migLine renders the MIG configuration in one line, naming the state the
// mig-manager got to rather than only what it was asked for.
func migLine(n *inventory.GPUNode) string {
	out := valueOr(n.MIG.Strategy, none)
	if n.MIG.Config != "" {
		out += "  config=" + n.MIG.Config
	}

	if n.MIG.ConfigState != "" {
		out += " (" + n.MIG.ConfigState + ")"
	}

	return out
}

// status reads like the STATUS column of kubectl nv list, with the report's
// own verdict on whether it is complete appended, because a summary that is
// missing half the cluster must say so on the line a reader scans first.
func status(n *inventory.GPUNode, r *debug.Report) string {
	out := "NotReady"
	if n.Ready {
		out = "Ready"
	}

	if n.Unschedulable {
		out += ",SchedulingDisabled"
	}

	if r.Status.Partial {
		out += "  (partial: see status.unreachable at -o json)"
	}

	return out
}

// operandColumn is how wide the component column of an operand line is. It
// fits the longest component the GPU Operator and the DRA driver ship, so that
// the states to the right of it line up and a reader can scan the column.
const operandColumn = 34

// summariseOperands writes one line per operand: which operand it is, how it
// is doing, and how often it has restarted.
//
// The component is named rather than the pod. A pod name carries a generated
// suffix and matches nothing a reader can look up; the component is the name
// kubectl nv info, kubectl nv health and the finding catalog all use.
func summariseOperands(p *printer, n *inventory.GPUNode) {
	p.line("Operands:")

	if len(n.Operands) == 0 {
		p.line("  " + none)

		return
	}

	lines := make([]string, 0, len(n.Operands))
	for i := range n.Operands {
		o := &n.Operands[i]
		lines = append(lines, fmt.Sprintf("  %-*s %-9s ready=%-5t restarts=%d",
			operandColumn, valueOr(o.Component, none), o.Phase, o.Ready, o.Restarts))
	}

	for _, line := range bounded(lines, maxOperandLines, "operands") {
		p.line(line)
	}
}

// summariseBundle writes where the bundle is and what is in it.
//
// Every collector that failed or was refused is named with its reason: a
// bundle missing the driver evidence because RBAC refused the exec and one
// missing it because the cluster has no driver pod are the same absence from
// the outside, and the difference is what the user has to act on. The ones
// that simply had nothing to collect are folded into a single line.
func summariseBundle(p *printer, b *debug.Bundle) {
	p.kv("Bundle", b.Directory)
	p.kv("Collectors", fmt.Sprintf("%d ok, %d skipped, %d error  (%d files, %s)",
		b.Collectors.OK, b.Collectors.Skipped, b.Collectors.Error, b.Files, humanBytes(b.Bytes)))

	var named []string
	for _, outcome := range b.Errored {
		named = append(named, outcomeLine("error  ", outcome))
	}

	var folded []string
	for _, outcome := range b.Skipped {
		if actionable(outcome.Reason) {
			named = append(named, outcomeLine("skipped", outcome))

			continue
		}
		folded = append(folded, outcome.Name)
	}

	// The folded line keeps its place whatever else has to go. It accounts for
	// several collectors at once, so dropping it to fit one more individual
	// line would lose the most collectors per line saved.
	limit := maxOutcomeLines
	var tail []string
	if len(folded) > 0 {
		limit--
		tail = []string{foldedLine(folded)}
	}

	for _, line := range append(bounded(named, limit, ""), tail...) {
		p.line(line)
	}
}

// foldedLine names the collectors that had nothing to collect, as many of
// them as fit on one line. A line that silently stopped naming them would read
// as a shorter list rather than as a cut one, so what was left out is counted
// rather than cut off mid-word.
func foldedLine(names []string) string {
	line := foldedSkipLabel + strings.Join(names, ", ")
	if len(line) <= maxLineWidth {
		return line
	}

	for kept := len(names) - 1; kept > 0; kept-- {
		line = fmt.Sprintf("%s%s (+%d more)",
			foldedSkipLabel, strings.Join(names[:kept], ", "), len(names)-kept)
		if len(line) <= maxLineWidth {
			return line
		}
	}

	// Not even one name fits, which takes a collector named longer than the
	// whole line. The count still tells a reader there is something in -o json.
	return truncateLine(fmt.Sprintf("%s%d collectors", foldedSkipLabel, len(names)))
}

// outcomeLine renders one collector that did not simply run.
func outcomeLine(what string, outcome debug.CollectorOutcome) string {
	return truncateLine("  " + what + " " + outcome.Name + ": " + truncate(outcome.Reason, maxReasonWidth))
}

// actionable reports whether a skipped collector's reason is something the
// user can do anything about. Being refused and running out of time are; a
// resource that is not installed is not.
//
// The two markers are the gatherer's own words, and pkg/cmd/debug's tests
// drive a real gather through a denied exec rather than asserting on this
// list, so a change to either one shows up as a refusal that stopped being
// named rather than as a list nobody updated.
func actionable(reason string) bool {
	return strings.HasPrefix(reason, "forbidden") || strings.Contains(reason, "timeout")
}

// summariseNext writes the commands to run after this one. The bundle command
// is offered only when there is a bundle to read: pointing a reader at
// --from-bundle for a directory that holds no index sends them to a command
// that will refuse it.
func summariseNext(p *printer, node string, b *debug.Bundle) {
	p.line("Next:")
	p.line("  kubectl nv health --node " + node)

	if b.Manifest != "" {
		p.line("  kubectl nv health --from-bundle " + b.Directory)
	}
}

// bounded cuts a list down to at most limit lines, the last of which says how
// many were left out and where the rest of them are. what names the things
// being counted, for a list that is not the only one on the screen.
func bounded(lines []string, limit int, what string) []string {
	if len(lines) <= limit {
		return lines
	}

	if what != "" {
		what = " " + what
	}

	out := append([]string(nil), lines[:limit-1]...)

	return append(out, fmt.Sprintf("  %d more%s, see -o json", len(lines)-(limit-1), what))
}

// truncate cuts a value to width, marking that it did.
func truncate(value string, width int) string {
	if len(value) <= width {
		return value
	}

	return value[:width-len(ellipsis)] + ellipsis
}

// truncateLine cuts a whole composed line to the width of the screen. It is
// applied after the reason has already been cut, because a collector name is
// not bounded by anything this package chooses either.
func truncateLine(line string) string {
	return truncate(line, maxLineWidth)
}

// humanBytes renders a size the way a reader compares two bundles.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}

	div, exp := int64(unit), 0
	for size := n / unit; size >= unit; size /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// printer writes lines and keeps the first write error, so that the caller
// checks once rather than after every line.
type printer struct {
	w   io.Writer
	err error
}

// kv writes a key and its value in the key column. Both halves are sanitised:
// a value here comes from a node label, which is written by whoever wrote the
// label.
func (p *printer) kv(key, value string) {
	key = output.SanitizeCell(key)
	p.write(fmt.Sprintf("%-*s %s\n", keyWidth, key+":", output.SanitizeCell(value)))
}

// line writes a line that is not a key and value pair.
func (p *printer) line(text string) {
	p.write(output.SanitizeCell(text) + "\n")
}

func (p *printer) write(s string) {
	if p.err != nil {
		return
	}

	_, p.err = io.WriteString(p.w, s)
}

// valueOr returns fallback when value is empty.
func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}
