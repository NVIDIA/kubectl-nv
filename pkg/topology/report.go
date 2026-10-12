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

// Package topology groups the GPU nodes of a cluster by the labels that say
// how they are wired together, and answers where a job would fit.
//
// The levels come from three places, and they are reported narrowest first,
// because that is the order the answer to "where does this job fit" is read
// in: the tighter the level, the faster the interconnect between the pods
// placed in it.
//
//  1. nvidia.com/gpu.clique, the NVLink or IMEX clique, written by the GPU
//     operator's feature discovery.
//  2. fabric.topograph.run/tier-<N>, the fabric tiers Topograph discovers,
//     ordered by N, and accelerator.topograph.run/domain, the accelerator
//     domain, which spans them.
//  3. The node labels a Kueue Topology object declares, in the order it
//     declares them, for the clusters where topology aware scheduling is what
//     actually places the work.
//
// The arithmetic per group is deliberately small: how many nodes, how many
// GPUs they advertise, how many are spoken for, and how many are left. The
// last of those is clamped per node rather than per group, and that is the
// one piece of arithmetic here worth arguing about: a node reporting more
// requested than allocatable is a node with nothing to offer, not a node with
// a debt its neighbours pay.
package topology

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// TopologyReport is what the topology command prints.
//
// wire, and output.KindTopologyReport is what a consumer switches on.
//
//nolint:revive // The name repeats the package because it is the kind on the
type TopologyReport struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	// ObjectMeta is the apimachinery type so that the cli-runtime printers
	// can reach the name through meta.Accessor. The report is about a whole
	// cluster and carries no name of its own.
	metav1.ObjectMeta `json:"metadata,omitzero" yaml:"metadata,omitempty"`

	// Levels are the ways the cluster divides, narrowest first.
	Levels []Level `json:"levels" yaml:"levels"`
	// PartialNodes are the nodes missing some of the cluster's topology
	// labels.
	PartialNodes []PartialNode `json:"partialNodes" yaml:"partialNodes"`
	// Kueue says whether the Kueue levels in this report are the cluster's.
	Kueue KueueStatus `json:"kueue" yaml:"kueue"`
	// Fit is the answer to --for, and is absent when nothing was asked.
	//
	// This is the one field here carrying omitempty, for the reason
	// inventory.InitResult.ExitCode carries it: the alternative to an absent
	// key is a null, and a reader that trusted the never-null guarantee
	// breaks on a null and not on a key that is not there.
	Fit *JobFit `json:"fit,omitempty" yaml:"fit,omitempty"`
	// Status carries what the run could not see, and is what the exit code is
	// read from.
	Status output.ReportStatus `json:"status" yaml:"status"`
}

// topologyReportJSON breaks the method set so that MarshalJSON does not
// recurse.
type topologyReportJSON TopologyReport

// MarshalJSON stamps the envelope and renders the nil slices as empty arrays,
// so that a report built by hand comes off the wire like every other one.
func (r TopologyReport) MarshalJSON() ([]byte, error) {
	out := topologyReportJSON(r)
	out.TypeMeta = output.TypeMetaFor(output.KindTopologyReport)

	if out.Levels == nil {
		out.Levels = []Level{}
	}
	if out.PartialNodes == nil {
		out.PartialNodes = []PartialNode{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling the topology report: %w", err)
	}

	return raw, nil
}

// NewTopologyReport returns an empty report stamped with the API version.
// Every report handed to a printer has to come from here: the cli-runtime
// printers read TypeMeta off the struct before they marshal anything, so a
// literal fails at -o json, -o yaml and -o name.
func NewTopologyReport() *TopologyReport {
	return &TopologyReport{
		TypeMeta:     output.TypeMetaFor(output.KindTopologyReport),
		Levels:       []Level{},
		PartialNodes: []PartialNode{},
	}
}

// TableColumns implements output.TableRows. The FITS column is there only
// when a job was asked about, so that the ordinary table is not a column of
// one repeated word.
func (r *TopologyReport) TableColumns(wide bool) []output.Column {
	// Each column names the field it renders, down to the number. The path
	// walker descends into a Level and a Group by their wire shape rather than
	// stopping at their MarshalJSON, so "levels.groups.free" is a path a
	// caller can ask for and this is the column it keeps.
	columns := []output.Column{
		output.OneField("LEVEL", "levels.name"),
		output.OneField("GROUP", "levels.groups.value"),
		output.OneField("NODES", "levels.groups.nodeCount"),
		output.OneField("ALLOCATABLE", "levels.groups.allocatable"),
		// The three that are printed as "unknown" until the requests were
		// read name that fact as well as the number, because a cell showing
		// "unknown" is showing requestedKnown and not only its own field.
		{Header: "REQUESTED", Fields: []string{"levels.groups.requested", "levels.groups.requestedKnown"}},
		{Header: "FREE", Fields: []string{"levels.groups.free", "levels.groups.requestedKnown"}},
		{Header: "LARGEST-FREE", Fields: []string{
			"levels.groups.largestFreeNode", "levels.groups.requestedKnown",
		}},
	}
	if wide {
		columns = append(columns, output.OneField("NODE-NAMES", "levels.groups.nodes"))
	}
	if r.Fit != nil {
		columns = append(columns, output.Column{Header: "FITS", Fields: []string{
			"fit.levels.level", "fit.levels.groups",
		}})
	}

	return columns
}

// TableRows implements output.TableRows, one row per group, in the level
// order of the report.
func (r *TopologyReport) TableRows(wide bool) [][]string {
	fits := r.fitsByLevel()

	rows := make([][]string, 0, len(r.Levels))
	for _, level := range r.Levels {
		for _, group := range level.Groups {
			row := []string{
				level.Name,
				group.Value,
				strconv.Itoa(group.NodeCount),
				strconv.FormatInt(group.Allocatable, 10),
				unknownOr(group.RequestedKnown, group.Requested),
				unknownOr(group.RequestedKnown, group.Free),
				unknownOr(group.RequestedKnown, group.LargestFreeNode),
			}
			if wide {
				row = append(row, strings.Join(group.Nodes, ","))
			}
			if r.Fit != nil {
				row = append(row, r.fitCell(fits, level.Name, group.Value))
			}
			rows = append(rows, row)
		}
	}

	return rows
}

// fitsByLevel indexes the answer so that a row can ask whether its own group
// takes the job.
func (r *TopologyReport) fitsByLevel() map[string]map[string]bool {
	out := map[string]map[string]bool{}
	if r.Fit == nil {
		return out
	}

	for _, level := range r.Fit.Levels {
		groups := make(map[string]bool, len(level.Groups))
		for _, value := range level.Groups {
			groups[value] = true
		}
		out[level.Level] = groups
	}

	return out
}

// unknownOr prints a number, or says the number was never read. A zero there
// would read as "nothing is running on these nodes", which is the one thing
// an unread count does not say.
func unknownOr(known bool, value int64) string {
	if !known {
		return "<unknown>"
	}

	return strconv.FormatInt(value, 10)
}

// fitCell says whether one group takes the job, and marks the rows of the
// narrowest level that does. The answer to --for is one level out of several,
// and a reader scanning a long table should not have to remember the level
// order to find which one it was.
func (r *TopologyReport) fitCell(fits map[string]map[string]bool, level, group string) string {
	if !fits[level][group] {
		return "no"
	}
	if r.Fit != nil && r.Fit.Smallest == level {
		return "yes (smallest)"
	}

	return "yes"
}

// The deep copies are written by hand, as pkg/inventory's are and for the
// same reason: there is no generator in this build, and these are flat types
// whose reference fields are a short, visible list. Every one of them is
// copied here, and report_test.go mutates each through a copy.

// DeepCopyInto copies the receiver into out.
func (g *Group) DeepCopyInto(out *Group) {
	*out = *g

	if g.Nodes != nil {
		out.Nodes = make([]string, len(g.Nodes))
		copy(out.Nodes, g.Nodes)
	}
}

// DeepCopyInto copies the receiver into out.
func (l *Level) DeepCopyInto(out *Level) {
	*out = *l

	if l.Groups != nil {
		out.Groups = make([]Group, len(l.Groups))
		for i := range l.Groups {
			l.Groups[i].DeepCopyInto(&out.Groups[i])
		}
	}
}

// DeepCopyInto copies the receiver into out.
func (p *PartialNode) DeepCopyInto(out *PartialNode) {
	*out = *p

	if p.Present != nil {
		out.Present = make([]string, len(p.Present))
		copy(out.Present, p.Present)
	}
	if p.Missing != nil {
		out.Missing = make([]string, len(p.Missing))
		copy(out.Missing, p.Missing)
	}
}

// DeepCopyInto copies the receiver into out.
func (f *LevelFit) DeepCopyInto(out *LevelFit) {
	*out = *f

	if f.Groups != nil {
		out.Groups = make([]string, len(f.Groups))
		copy(out.Groups, f.Groups)
	}
}

// DeepCopyInto copies the receiver into out.
func (f *JobFit) DeepCopyInto(out *JobFit) {
	*out = *f

	if f.Levels != nil {
		out.Levels = make([]LevelFit, len(f.Levels))
		for i := range f.Levels {
			f.Levels[i].DeepCopyInto(&out.Levels[i])
		}
	}
	if f.Excluded != nil {
		out.Excluded = make([]string, len(f.Excluded))
		copy(out.Excluded, f.Excluded)
	}
}

// DeepCopy returns a copy of the receiver.
func (f *JobFit) DeepCopy() *JobFit {
	if f == nil {
		return nil
	}

	out := new(JobFit)
	f.DeepCopyInto(out)

	return out
}

// DeepCopyInto copies the receiver into out.
func (r *TopologyReport) DeepCopyInto(out *TopologyReport) {
	*out = *r

	out.TypeMeta = r.TypeMeta
	r.ObjectMeta.DeepCopyInto(&out.ObjectMeta)

	if r.Levels != nil {
		out.Levels = make([]Level, len(r.Levels))
		for i := range r.Levels {
			r.Levels[i].DeepCopyInto(&out.Levels[i])
		}
	}
	if r.PartialNodes != nil {
		out.PartialNodes = make([]PartialNode, len(r.PartialNodes))
		for i := range r.PartialNodes {
			r.PartialNodes[i].DeepCopyInto(&out.PartialNodes[i])
		}
	}

	out.Fit = r.Fit.DeepCopy()
	out.Status = copyStatus(r.Status)
}

// copyStatus copies a report status. pkg/output does not give one a deep
// copy of its own, and the three reference fields are copied here rather than
// left shared: a caller that prints a copy while another goroutine appends a
// finding would otherwise race over the same backing array.
func copyStatus(s output.ReportStatus) output.ReportStatus {
	out := s

	if s.Findings != nil {
		out.Findings = make([]output.Finding, len(s.Findings))
		copy(out.Findings, s.Findings)
		for i := range s.Findings {
			if s.Findings[i].Evidence == nil {
				continue
			}
			evidence := make(map[string]string, len(s.Findings[i].Evidence))
			for key, value := range s.Findings[i].Evidence {
				evidence[key] = value
			}
			out.Findings[i].Evidence = evidence
		}
	}

	if s.Unreachable != nil {
		out.Unreachable = make([]output.Unreachable, len(s.Unreachable))
		copy(out.Unreachable, s.Unreachable)
	}

	// The count is a pointer, so a shallow copy would let the printer's copy
	// write a truncation into the report the command still holds.
	if s.RemainingItemCount != nil {
		remaining := *s.RemainingItemCount
		out.RemainingItemCount = &remaining
	}

	return out
}

// LimitItems implements output.Limitable: --limit on a topology report bounds
// its groups, counted across the levels in the order the report carries them.
//
// Across the levels rather than within each, because the levels are ordered
// narrowest first and that is the order the answer to "where does this job
// fit" is read in: a limit takes the tightest groups, which are the ones worth
// having. A level left with no groups is dropped, because a level that lists
// none says the cluster does not divide that way, which would be a different
// answer from "you asked for fewer".
//
// PartialNodes, Kueue and Fit are untouched. They are about the whole report
// rather than one group, and a caller that asked for fewer groups did not ask
// to be told less about where its job fits.
func (r *TopologyReport) LimitItems(n int) int {
	total := 0
	for i := range r.Levels {
		total += len(r.Levels[i].Groups)
	}
	// A limit of zero or less is no limit, which is what the flag's default
	// means. Truncating to nothing here would make a report with no groups out
	// of a caller that passed the flag and then thought better of it.
	if n <= 0 || total <= n {
		return total
	}

	left := n
	kept := make([]Level, 0, len(r.Levels))
	for _, level := range r.Levels {
		if left <= 0 {
			break
		}
		if len(level.Groups) > left {
			level.Groups = level.Groups[:left]
		}
		// A level with no groups is dropped rather than carried: a level that
		// lists none says the cluster does not divide that way, which is a
		// different answer from "you asked for fewer" and one a reader acts
		// on. It happens at a limit landing on a level boundary, and to a
		// level that had none to begin with.
		if len(level.Groups) == 0 {
			continue
		}
		left -= len(level.Groups)
		kept = append(kept, level)
	}
	r.Levels = kept

	remaining := int64(total - n)
	r.Status.RemainingItemCount = &remaining

	return total
}

// DeepCopy returns a copy of the receiver.
func (r *TopologyReport) DeepCopy() *TopologyReport {
	if r == nil {
		return nil
	}

	out := new(TopologyReport)
	r.DeepCopyInto(out)

	return out
}

// DeepCopyObject implements runtime.Object.
func (r *TopologyReport) DeepCopyObject() runtime.Object {
	if c := r.DeepCopy(); c != nil {
		return c
	}

	return nil
}
