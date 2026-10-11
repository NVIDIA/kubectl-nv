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

// summarise writes one node and its bundle as a screen a support engineer
// reads without scrolling.
//
// It is not a table. A report with a node, its operands and a bundle in it is
// not a row, and rendering it as one would put the interesting half off the
// right of the terminal. What it is instead is a fixed, short shape: the node,
// one line per operand, the bundle, and the two commands to run next.
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

	p.line("Next:")
	p.line("  kubectl nv health --node " + n.Name)
	p.line("  kubectl nv health --from-bundle " + r.Bundle.Directory)

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
// between them is one of the two things this command exists to make visible.
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

// summariseOperands writes one line per operand: what it is, how it is doing,
// and how often it has restarted.
func summariseOperands(p *printer, n *inventory.GPUNode) {
	p.line("Operands:")

	if len(n.Operands) == 0 {
		p.line("  " + none)

		return
	}

	for i := range n.Operands {
		o := &n.Operands[i]
		p.line(fmt.Sprintf("  %-40s %-10s ready=%t restarts=%d",
			o.Name, o.Phase, o.Ready, o.Restarts))
	}
}

// summariseBundle writes where the bundle is and what is in it. Every
// collector that did not simply run is named with its reason: a bundle missing
// the driver evidence because RBAC refused the exec and one missing it because
// the cluster has no driver pod are the same absence from the outside, and the
// difference is what the user has to act on.
func summariseBundle(p *printer, b *debug.Bundle) {
	p.kv("Bundle", b.Directory)
	p.kv("Collectors", fmt.Sprintf("%d ok, %d skipped, %d error  (%d files, %s)",
		b.Collectors.OK, b.Collectors.Skipped, b.Collectors.Error, b.Files, humanBytes(b.Bytes)))

	for _, outcome := range b.Errored {
		p.line("  error   " + outcome.Name + ": " + outcome.Reason)
	}

	for _, outcome := range b.Skipped {
		p.line("  skipped " + outcome.Name + ": " + outcome.Reason)
	}
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
