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

package info

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
)

// none is what an empty section says, as kubectl describe does, so that an
// empty list and a section this command forgot to fill read differently.
const none = "<none>"

// describe writes one node as a key and value view grouped the way kubectl
// describe groups a node. The table printers cannot do this: an object with
// forty fields is not a row, and rendering it as one would put the interesting
// half off the right of the terminal.
func describe(w io.Writer, n *inventory.GPUNode) error {
	p := &printer{w: w}

	p.kv("Name", n.Name)
	p.kv("Hinted", strconv.FormatBool(n.Hinted))
	p.kv("Status", status(n))
	p.kv("Product", valueOr(n.Product, none))
	p.kv("Device count", strconv.Itoa(n.Count))
	p.kv("Driver version", valueOr(n.DriverVersion, none))
	p.kv("CUDA version", valueOr(n.CUDAVersion, none))
	p.kv("Mode", valueOr(n.Mode, none))
	p.kv("Clique", valueOr(n.Clique, none))
	p.kv("Upgrade state", valueOr(n.UpgradeState, none))

	p.section("GPUs")
	p.kv("  Capacity", strconv.FormatInt(n.Capacity, 10))
	p.kv("  Allocatable", strconv.FormatInt(n.Allocatable, 10))

	// Reporting 0 for a node nobody counted the pods of would be a claim
	// nobody made, and on a busy node a wrong one.
	requested := "<not counted: the pods were not listed>"
	if n.RequestedKnown {
		requested = strconv.FormatInt(n.Requested, 10)
	}

	p.kv("  Requested", requested)

	p.section("MIG")
	p.kv("  Strategy", valueOr(n.MIG.Strategy, none))
	p.kv("  Capable", strconv.FormatBool(n.MIG.Capable))
	p.kv("  Config", valueOr(n.MIG.Config, none))
	p.kv("  Config state", valueOr(n.MIG.ConfigState, none))

	if len(n.MIG.Resources) > 0 {
		p.section("  Profiles")

		for _, name := range sortedResourceKeys(n.MIG.Resources) {
			count := n.MIG.Resources[name]
			p.kv("    "+name, fmt.Sprintf("%d allocatable / %d capacity", count.Allocatable, count.Capacity))
		}
	}

	p.section("Sharing")
	p.kv("  Strategy", valueOr(n.Sharing.Strategy, none))
	p.kv("  Replicas", strconv.Itoa(n.Sharing.Replicas))

	p.mapSection("Deploy labels", n.Deploy)
	p.mapSection("Topology labels", n.Topology)
	p.mapSection("Labels", n.Labels)

	describeTaints(p, n)
	describeConditions(p, n)
	describeOperands(p, n)
	describeSlices(p, n)

	if p.err != nil {
		return fmt.Errorf("writing the node description: %w", p.err)
	}

	return nil
}

func describeTaints(p *printer, n *inventory.GPUNode) {
	p.section("Taints")

	if len(n.Taints) == 0 {
		p.line("  " + none)

		return
	}

	for i := range n.Taints {
		p.line("  " + n.Taints[i].ToString())
	}
}

func describeConditions(p *printer, n *inventory.GPUNode) {
	p.section("Conditions")

	if len(n.Conditions) == 0 {
		p.line("  " + none)

		return
	}

	for i := range n.Conditions {
		c := &n.Conditions[i]
		p.kv("  "+string(c.Type), strings.TrimSpace(string(c.Status)+"  "+c.Reason))
	}
}

func describeSlices(p *printer, n *inventory.GPUNode) {
	p.section("Resource slices")

	if len(n.ResourceSlices) == 0 {
		p.line("  " + none)

		return
	}

	for _, name := range n.ResourceSlices {
		p.line("  " + name)
	}
}

func describeOperands(p *printer, n *inventory.GPUNode) {
	p.section("Operands")

	if len(n.Operands) == 0 {
		p.line("  " + none)

		return
	}

	for i := range n.Operands {
		o := &n.Operands[i]
		p.kv("  "+o.Name, fmt.Sprintf("%s  %s  ready=%t  restarts=%d",
			valueOr(o.Component, none), o.Phase, o.Ready, o.Restarts))

		for _, image := range o.Images {
			p.line("    image: " + image)
		}

		// Only the validator carries these, and they are its verdict.
		for j := range o.InitContainers {
			p.line("    " + initResult(&o.InitContainers[j]))
		}
	}
}

// initResult renders one init container outcome. A container that has not
// terminated says so: rendering it as "exit 0" would turn a validation still
// running into one that passed.
func initResult(r *inventory.InitResult) string {
	outcome := "not finished"
	if r.ExitCode != nil {
		outcome = "exit " + strconv.FormatInt(int64(*r.ExitCode), 10)
	}

	if r.Reason != "" {
		outcome += " (" + r.Reason + ")"
	}

	return r.Name + ": " + outcome
}

// keyWidth is how wide the key column is. A fixed width rather than a tab
// writer's computed one: the widest line in a description is an operand's
// image, sixty characters of it, and aligning the whole block to that puts
// every value half a terminal away from the key it belongs to. A key longer
// than this pushes its own value one space right and leaves the rest alone.
const keyWidth = 20

// printer writes lines and keeps the first write error, so that the caller
// checks once rather than after every line.
type printer struct {
	w   io.Writer
	err error
}

// kv writes a key and its value in the key column.
func (p *printer) kv(key, value string) {
	p.write(fmt.Sprintf("%-*s %s\n", keyWidth, key+":", value))
}

// section starts a group, as kubectl describe does: a heading whose value is
// the indented lines under it.
func (p *printer) section(name string) {
	p.write(name + ":\n")
}

// line writes a line that is not a key and value pair.
func (p *printer) line(text string) {
	p.write(text + "\n")
}

// mapSection writes a map of labels, sorted, or <none>.
func (p *printer) mapSection(name string, m map[string]string) {
	p.section(name)

	if len(m) == 0 {
		p.line("  " + none)

		return
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	for _, k := range keys {
		p.kv("  "+k, m[k])
	}
}

func (p *printer) write(s string) {
	if p.err != nil {
		return
	}

	_, p.err = io.WriteString(p.w, s)
}

// status reads like the STATUS column of gpu list, so that someone who moved
// from one command to the other does not have to learn a second vocabulary.
func status(n *inventory.GPUNode) string {
	out := "NotReady"
	if n.Ready {
		out = "Ready"
	}

	if n.Unschedulable {
		out += ",SchedulingDisabled"
	}

	return out
}

// valueOr returns fallback when value is empty.
func valueOr(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}

// sortedResourceKeys orders the MIG profile names.
func sortedResourceKeys(m map[string]inventory.ResourceCount) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
