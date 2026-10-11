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

// Package debug holds the report "kubectl nv debug <node>" prints: one node as
// the inventory sees it, next to the must-gather bundle collected for it.
//
// The type lives here rather than in the command package so that a reader of
// the output contract finds it where the other payload types are, and so that
// nothing in it depends on cobra.
//
// Build every value through NewReport. The cli-runtime printers behind -o
// json, -o yaml and -o name read TypeMeta off the struct and refuse one whose
// GroupVersionKind is empty, and they do it before any marshalling, so
// MarshalJSON stamping the envelope does not reach them.
package debug

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The prefixes a collector status carries when it did not simply run. They are
// the gatherer's own spelling; pkg/cmd/debug's tests drive a real gather over
// a fake cluster, so a change to either one is caught rather than guessed at.
const (
	statusSkipped = "skipped:"
	statusError   = "error:"
)

// CollectorCounts is how the gather's collectors ended, one count per status.
// A skipped collector is counted apart from a successful one on purpose: a
// bundle whose driver evidence was refused and one that holds it must not add
// up to the same number.
type CollectorCounts struct {
	// OK is how many collectors ran and wrote what they planned to.
	OK int `json:"ok" yaml:"ok"`
	// Skipped is how many had nothing to do, or were kept out by a flag, or
	// were refused.
	Skipped int `json:"skipped" yaml:"skipped"`
	// Error is how many failed.
	Error int `json:"error" yaml:"error"`
}

// CollectorOutcome names one collector that did not simply run, and why.
type CollectorOutcome struct {
	// Name is the collector's name in the gather's registry.
	Name string `json:"name" yaml:"name"`
	// Reason is what the manifest recorded after the status prefix, for
	// instance "--no-exec" or "forbidden: ...".
	Reason string `json:"reason" yaml:"reason"`
}

// Bundle is the must-gather this run collected, summarised. The bundle itself
// stays on disk; this says where it is and what is in it, so that a reader can
// tell a complete bundle from one that is missing the part they need without
// unpacking it.
type Bundle struct {
	// Directory is where the bundle was written. It is what the user sends.
	Directory string `json:"directory" yaml:"directory"`
	// Manifest is the path of the bundle's own index, empty when the gather
	// failed before it could write one.
	Manifest string `json:"manifest" yaml:"manifest"`
	// Collectors is how the gather's collectors ended.
	Collectors CollectorCounts `json:"collectors" yaml:"collectors"`
	// Skipped names the collectors that did not run, in the order they were
	// run in, each with its reason.
	Skipped []CollectorOutcome `json:"skipped" yaml:"skipped"`
	// Errored names the collectors that failed, in the same order.
	Errored []CollectorOutcome `json:"errored" yaml:"errored"`
	// Files is how many files the bundle indexes, not counting the index.
	Files int `json:"files" yaml:"files"`
	// Bytes is what those files add up to.
	Bytes int64 `json:"bytes" yaml:"bytes"`
}

// bundleJSON breaks the method set so that MarshalJSON does not recurse.
type bundleJSON Bundle

// MarshalJSON renders the nil slices as empty arrays.
func (b Bundle) MarshalJSON() ([]byte, error) {
	out := bundleJSON(b)
	if out.Skipped == nil {
		out.Skipped = []CollectorOutcome{}
	}

	if out.Errored == nil {
		out.Errored = []CollectorOutcome{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling the bundle summary: %w", err)
	}

	return raw, nil
}

// Report is everything kubectl nv debug gathered about one node: the node as
// kubectl nv info builds it, the bundle collected for it, and what the run
// could not see.
//
// There are no findings in it. Deciding whether the node is healthy is
// kubectl nv health's job, and a second command answering the same question
// its own way is a second answer to disagree with.
type Report struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	// ObjectMeta is the apimachinery type so that the cli-runtime printers can
	// reach the name through meta.Accessor. The name is the node's.
	metav1.ObjectMeta `json:"metadata,omitzero" yaml:"metadata,omitempty"`

	// Node is the node, built exactly as kubectl nv info builds it.
	Node inventory.GPUNode `json:"node" yaml:"node"`
	// Bundle is the must-gather collected for that node.
	Bundle Bundle `json:"bundle" yaml:"bundle"`
	// Status is what the run could not see, from the inventory read and from
	// the gather both. The exit code is read from it.
	Status output.ReportStatus `json:"status" yaml:"status"`
}

// reportJSON breaks the method set so that MarshalJSON does not recurse.
type reportJSON Report

// MarshalJSON stamps the envelope. The guarantee belongs on the type rather
// than on the constructor: a zero value from a test still has to come off the
// wire looking like every other report.
func (r Report) MarshalJSON() ([]byte, error) {
	out := reportJSON(r)
	out.TypeMeta = output.TypeMetaFor(output.KindDebugReport)

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling the debug report for %q: %w", r.Name, err)
	}

	return raw, nil
}

// NewReport returns a report for one node, stamped with the API version and
// carrying the node's name. Every report handed to a printer comes from here.
func NewReport(node string) *Report {
	return &Report{
		TypeMeta:   output.TypeMetaFor(output.KindDebugReport),
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Node:       *inventory.NewGPUNode(node),
		Bundle: Bundle{
			Skipped: []CollectorOutcome{},
			Errored: []CollectorOutcome{},
		},
		Status: output.ReportStatus{Findings: []output.Finding{}},
	}
}

// SummariseBundle reads the gather's own index into the summary this report
// carries. m is nil when the gather failed before it could write one, which is
// still a directory the user has to be told about.
//
// The counts come from the manifest rather than from the directory: a
// collector that was refused wrote nothing, and counting files would report it
// as a collector that was never in the registry.
func SummariseBundle(dir string, m *mustgather.Manifest) Bundle {
	out := Bundle{
		Directory: dir,
		Skipped:   []CollectorOutcome{},
		Errored:   []CollectorOutcome{},
	}
	if m == nil {
		return out
	}

	out.Manifest = filepath.Join(dir, mustgather.ManifestFileName)

	// The registry order is kept, so that two gathers of the same cluster
	// produce reports that can be diffed against each other.
	for _, c := range m.Collectors {
		switch {
		case strings.HasPrefix(c.Status, statusSkipped):
			out.Collectors.Skipped++
			out.Skipped = append(out.Skipped, CollectorOutcome{
				Name:   c.Name,
				Reason: strings.TrimPrefix(c.Status, statusSkipped),
			})
		case strings.HasPrefix(c.Status, statusError):
			out.Collectors.Error++
			out.Errored = append(out.Errored, CollectorOutcome{
				Name:   c.Name,
				Reason: strings.TrimPrefix(c.Status, statusError),
			})
		default:
			out.Collectors.OK++
		}
	}

	out.Files = len(m.Files)
	for _, f := range m.Files {
		out.Bytes += f.Size
	}

	return out
}

// DeepCopyInto copies the receiver into out.
func (r *Report) DeepCopyInto(out *Report) {
	*out = *r
	out.TypeMeta = r.TypeMeta
	r.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	r.Node.DeepCopyInto(&out.Node)
	out.Bundle = r.Bundle.deepCopy()
	out.Status = deepCopyStatus(r.Status)
}

// deepCopy returns a copy of the bundle summary with its own slices.
func (b Bundle) deepCopy() Bundle {
	out := b
	if b.Skipped != nil {
		out.Skipped = append([]CollectorOutcome(nil), b.Skipped...)
	}

	if b.Errored != nil {
		out.Errored = append([]CollectorOutcome(nil), b.Errored...)
	}

	return out
}

// deepCopyStatus returns a copy of the report status with its own slices.
func deepCopyStatus(s output.ReportStatus) output.ReportStatus {
	out := s
	if s.Findings != nil {
		out.Findings = append([]output.Finding(nil), s.Findings...)
	}

	if s.Unreachable != nil {
		out.Unreachable = append([]output.Unreachable(nil), s.Unreachable...)
	}

	return out
}

// DeepCopy returns a copy of the receiver.
func (r *Report) DeepCopy() *Report {
	if r == nil {
		return nil
	}

	out := new(Report)
	r.DeepCopyInto(out)

	return out
}

// DeepCopyObject implements runtime.Object.
func (r *Report) DeepCopyObject() runtime.Object {
	if c := r.DeepCopy(); c != nil {
		return c
	}

	return nil
}
