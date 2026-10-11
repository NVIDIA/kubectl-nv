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

package health

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// ReportName is the name a health report carries. A report is about a cluster
// rather than about one named object, and -o name still has to print
// something: "healthreport/cluster" is a line a script can read, where an
// empty name is a line it cannot.
const ReportName = "cluster"

// Summary is the arithmetic a reader looks at when the finding list is longer
// than the screen: how much was found, and over how much.
//
// The GPU totals are the whole-GPU numbers, summed over the nodes that were
// checked. A node running MIG in the mixed strategy advertises one extended
// resource per profile and no nvidia.com/gpu at all, so it contributes zero
// here and its real numbers are in its own mig.resources. Adding MIG slices to
// a GPU count would be adding two different units together.
type Summary struct {
	// Errors, Warnings and Infos count the findings by severity. They count
	// what the report carries, so a --node filtered report counts what is left
	// after the filter.
	Errors int `json:"errors" yaml:"errors"`
	// Warnings is the warning count. See Errors.
	Warnings int `json:"warnings" yaml:"warnings"`
	// Infos is the info count. See Errors.
	Infos int `json:"infos" yaml:"infos"`

	// NodesChecked is how many nodes the rules ran over, the GPU nodes and the
	// hinted ones together. "No findings" over no nodes is not the same answer
	// as "no findings" over forty, and without this number a reader cannot
	// tell them apart.
	NodesChecked int `json:"nodesChecked" yaml:"nodesChecked"`

	// GPUCapacity is the nvidia.com/gpu capacity summed over those nodes.
	GPUCapacity int64 `json:"gpuCapacity" yaml:"gpuCapacity"`
	// GPUAllocatable is the nvidia.com/gpu allocatable summed over them.
	GPUAllocatable int64 `json:"gpuAllocatable" yaml:"gpuAllocatable"`
	// GPURequested is the nvidia.com/gpu requested by the pods on them. It is
	// meaningless unless GPURequestedKnown is set.
	GPURequested int64 `json:"gpuRequested" yaml:"gpuRequested"`
	// GPURequestedKnown is whether every node checked had its requests
	// counted. Reporting zero requested GPUs for a busy cluster because
	// nobody listed the pods would be worse than reporting nothing.
	GPURequestedKnown bool `json:"gpuRequestedKnown" yaml:"gpuRequestedKnown"`
}

// Line renders the summary as the one line printed under the table.
func (s Summary) Line() string {
	var b strings.Builder

	b.WriteString(plural(s.NodesChecked, "node", "nodes"))
	b.WriteString(" checked, ")
	b.WriteString(strconv.FormatInt(s.GPUCapacity, 10))
	b.WriteString(" GPUs capacity, ")
	b.WriteString(strconv.FormatInt(s.GPUAllocatable, 10))
	b.WriteString(" allocatable")

	// Left out rather than printed as zero: an unlisted pod set and an idle
	// cluster both have nothing requested, and only one of them is good news.
	if s.GPURequestedKnown {
		b.WriteString(", ")
		b.WriteString(strconv.FormatInt(s.GPURequested, 10))
		b.WriteString(" requested")
	}

	b.WriteString(": ")
	b.WriteString(plural(s.Errors, "error", "errors"))
	b.WriteString(", ")
	b.WriteString(plural(s.Warnings, "warning", "warnings"))
	b.WriteString(", ")
	b.WriteString(plural(s.Infos, "info", "infos"))

	return b.String()
}

// plural renders a count with the noun that goes with it.
func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}

	return strconv.Itoa(n) + " " + many
}

// HealthReport is the result of one gpu health run: what the rules found, what
// the run could not see, and the arithmetic over what it looked at.
//
// Build one with NewHealthReport. The cli-runtime printers read TypeMeta off
// the object before they marshal anything, so a literal is unprintable at
// -o json, -o yaml and -o name whatever MarshalJSON does; report_test.go keeps
// that statement honest.
type HealthReport struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	// ObjectMeta is the apimachinery type so that the printers can reach the
	// name through meta.Accessor.
	metav1.ObjectMeta `json:"metadata,omitzero" yaml:"metadata,omitempty"`

	// ReportStatus is the findings and what stayed out of reach. It is inline,
	// so a consumer reads report.findings rather than report.status.findings:
	// every command in this contract that reports findings puts them in the
	// same place.
	output.ReportStatus `json:",inline" yaml:",inline"`

	// Summary is the arithmetic over what was checked.
	Summary Summary `json:"summary" yaml:"summary"`
}

// NewHealthReport returns an empty report stamped with the API version and
// carrying the report name. Every report handed to a printer comes from here.
func NewHealthReport() *HealthReport {
	return &HealthReport{
		TypeMeta:     output.TypeMetaFor(output.KindHealthReport),
		ObjectMeta:   metav1.ObjectMeta{Name: ReportName},
		ReportStatus: output.ReportStatus{Findings: []output.Finding{}},
	}
}

// healthReportJSON breaks the method set so that MarshalJSON does not recurse.
//
// It spells the embedded status out as fields rather than embedding
// output.ReportStatus, because the embedded type carries its own MarshalJSON
// and an embedded marshaller is promoted onto the outer struct: a report that
// embedded it would marshal as its status alone, losing the summary and the
// envelope.
type healthReportJSON struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Findings    []output.Finding     `json:"findings"`
	Partial     bool                 `json:"partial,omitempty"`
	Unreachable []output.Unreachable `json:"unreachable,omitempty"`

	Summary Summary `json:"summary"`
}

// MarshalJSON stamps the envelope and renders a nil Findings as an empty
// array. Both guarantees belong on the type: a zero value from a test has to
// come off the wire looking like every other report.
func (r HealthReport) MarshalJSON() ([]byte, error) {
	out := healthReportJSON{
		TypeMeta:    output.TypeMetaFor(output.KindHealthReport),
		ObjectMeta:  r.ObjectMeta,
		Findings:    r.Findings,
		Partial:     r.Partial,
		Unreachable: r.Unreachable,
		Summary:     r.Summary,
	}

	if out.Findings == nil {
		out.Findings = []output.Finding{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling the health report: %w", err)
	}

	return raw, nil
}

// ExitCode maps the report onto the process exit code against the --fail-on
// threshold. It is the report's own answer rather than the caller's, so that
// no command can report findings and exit zero by forgetting to ask.
func (r *HealthReport) ExitCode(failOn output.Severity) int {
	return output.ExitCode(r.ReportStatus, failOn)
}

// noNode is what the table prints where a finding has no node. An empty cell
// would leave the row ambiguous, and a reader scanning a column of node names
// needs every row to have one.
const noNode = "-"

// TableColumns implements output.TableRows.
func (r *HealthReport) TableColumns(wide bool) []output.Column {
	columns := []output.Column{
		{Header: "NODE"},
		{Header: "CODE"},
		{Header: "SEVERITY"},
		{Header: "COMPONENT"},
		{Header: "MESSAGE"},
	}

	// The next command is the whole point of a finding and the widest thing in
	// it, so it waits for -o wide rather than pushing the message off screen.
	if wide {
		columns = append(columns, output.Column{Header: "NEXT"})
	}

	return columns
}

// TableRows implements output.TableRows, one row per finding in the order the
// report carries them.
func (r *HealthReport) TableRows(wide bool) [][]string {
	rows := make([][]string, 0, len(r.Findings))

	for _, f := range r.Findings {
		node := f.Node
		if node == "" {
			node = noNode
		}

		row := []string{node, f.Code, string(f.Severity), f.Component, f.Message}
		if wide {
			row = append(row, f.Next)
		}

		rows = append(rows, row)
	}

	return rows
}

// DeepCopyInto copies the receiver into out.
func (r *HealthReport) DeepCopyInto(out *HealthReport) {
	*out = *r
	out.TypeMeta = r.TypeMeta
	r.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	out.Summary = r.Summary

	if r.Findings != nil {
		out.Findings = make([]output.Finding, len(r.Findings))
		for i, f := range r.Findings {
			out.Findings[i] = f
			// The evidence map is the one reference inside a finding, and a
			// copy that shared it would let a printer's copy rewrite the
			// evidence the report was built from.
			if f.Evidence != nil {
				evidence := make(map[string]string, len(f.Evidence))
				for k, v := range f.Evidence {
					evidence[k] = v
				}
				out.Findings[i].Evidence = evidence
			}
		}
	}

	if r.Unreachable != nil {
		out.Unreachable = make([]output.Unreachable, len(r.Unreachable))
		copy(out.Unreachable, r.Unreachable)
	}
}

// DeepCopy returns a copy of the receiver.
func (r *HealthReport) DeepCopy() *HealthReport {
	if r == nil {
		return nil
	}

	out := new(HealthReport)
	r.DeepCopyInto(out)

	return out
}

// DeepCopyObject implements runtime.Object.
func (r *HealthReport) DeepCopyObject() runtime.Object {
	if c := r.DeepCopy(); c != nil {
		return c
	}

	return nil
}

// CatalogPath is where the catalog entry for a finding code lives in this
// repository. It is the one definition of that path: the engine fills a
// finding's Docs from it, and docs/findings resolves its embedded entries
// through it, so the link in a report and the file behind `explain` cannot
// drift apart.
//
// An empty code has no entry, and an empty Docs is better than a link to
// docs/findings/.md.
func CatalogPath(code string) string {
	if code == "" {
		return ""
	}

	return "docs/findings/" + code + ".md"
}
