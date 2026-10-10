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

// Package output carries the machine readable output contract of kubectl-nv:
// the versioned envelope every command emits, the printers behind -o, the exit
// code mapping, a stderr logger, and a deterministic order for findings.
//
// Machine readable output is rendered by the k8s.io/cli-runtime printers, which
// marshal through sigs.k8s.io/yaml. That package reads the JSON struct tags for
// both JSON and YAML output, so the JSON tags below are what the wire format
// actually follows; the yaml tags are there for callers that reach for a plain
// YAML marshaller.
package output

import (
	"fmt"
	"io"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/cli-runtime/pkg/printers"
)

const (
	// GroupName is the API group every kubectl-nv envelope is stamped with.
	GroupName = "kubectl-nv.nvidia.com"
	// Version is the API version of the envelope.
	Version = "v1alpha1"
	// APIVersion is the group and version as they appear on the wire.
	APIVersion = GroupName + "/" + Version
)

// The kinds kubectl-nv emits. Payloads are filled in by the commands; the kinds
// are fixed here so that every consumer can switch on them from day one.
const (
	// KindGPUNodeList is a list of GPU capable nodes.
	KindGPUNodeList = "GPUNodeList"
	// KindGPUNode is a single GPU capable node.
	KindGPUNode = "GPUNode"
	// KindHealthReport is the result of a health check run.
	KindHealthReport = "HealthReport"
	// KindTopologyReport is the device topology of one node or of the cluster.
	KindTopologyReport = "TopologyReport"
	// KindBundleManifest is the index of a must-gather bundle.
	KindBundleManifest = "BundleManifest"
)

// GroupVersion is the group and version of every envelope in this package.
var GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// Object is a single envelope entry. Spec and Status are left opaque so that
// each command can put its own payload in without this package depending on it.
type Object struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	// ObjectMeta carries the name the output is keyed by. It is the apimachinery
	// type so that the cli-runtime printers can read the name through
	// meta.Accessor.
	metav1.ObjectMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// Spec is what was asked for or observed about the subject.
	Spec runtime.RawExtension `json:"spec,omitempty" yaml:"spec,omitempty"`
	// Status is the outcome of looking at the subject.
	Status runtime.RawExtension `json:"status,omitempty" yaml:"status,omitempty"`
}

// List is an envelope holding several items of one kind.
type List struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	metav1.ListMeta `json:"metadata,omitempty" yaml:"metadata,omitempty"`

	// Items keeps the order the command produced. Nothing downstream reorders
	// it, so a command that wants a stable order sorts before handing it over.
	Items []Object `json:"items" yaml:"items"`
}

// NewObject returns an item of the given kind, stamped with the API version.
func NewObject(kind, name string) *Object {
	return &Object{
		TypeMeta:   metav1.TypeMeta{APIVersion: APIVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

// NewList returns an empty list of the given kind, stamped with the API version.
func NewList(kind string) *List {
	return &List{TypeMeta: metav1.TypeMeta{APIVersion: APIVersion, Kind: kind}}
}

// DeepCopyInto copies the receiver into out.
func (o *Object) DeepCopyInto(out *Object) {
	*out = *o
	out.TypeMeta = o.TypeMeta
	o.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
	o.Spec.DeepCopyInto(&out.Spec)
	o.Status.DeepCopyInto(&out.Status)
}

// DeepCopy returns a copy of the receiver.
func (o *Object) DeepCopy() *Object {
	if o == nil {
		return nil
	}

	out := new(Object)
	o.DeepCopyInto(out)

	return out
}

// DeepCopyObject implements runtime.Object.
func (o *Object) DeepCopyObject() runtime.Object {
	if c := o.DeepCopy(); c != nil {
		return c
	}

	return nil
}

// DeepCopyInto copies the receiver into out.
func (l *List) DeepCopyInto(out *List) {
	*out = *l
	out.TypeMeta = l.TypeMeta
	l.ListMeta.DeepCopyInto(&out.ListMeta)
	if l.Items != nil {
		out.Items = make([]Object, len(l.Items))
		for i := range l.Items {
			l.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a copy of the receiver.
func (l *List) DeepCopy() *List {
	if l == nil {
		return nil
	}

	out := new(List)
	l.DeepCopyInto(out)

	return out
}

// DeepCopyObject implements runtime.Object.
func (l *List) DeepCopyObject() runtime.Object {
	if c := l.DeepCopy(); c != nil {
		return c
	}

	return nil
}

// Format is the value of the -o flag.
type Format string

const (
	// FormatTable is the human readable default.
	FormatTable Format = "table"
	// FormatWide is the table with the extra columns.
	FormatWide Format = "wide"
	// FormatJSON is the envelope as JSON.
	FormatJSON Format = "json"
	// FormatYAML is the envelope as YAML.
	FormatYAML Format = "yaml"
	// FormatName prints one "kind.group/name" line per item.
	FormatName Format = "name"
)

// formats lists the accepted -o values in the order they are offered to the user.
var formats = []Format{FormatTable, FormatWide, FormatJSON, FormatYAML, FormatName}

// ParseFormat turns an -o value into a Format. An empty value is the table
// default; anything unrecognised is a usage error, which exits 2.
func ParseFormat(value string) (Format, error) {
	if value == "" {
		return FormatTable, nil
	}

	for _, f := range formats {
		if Format(value) == f {
			return f, nil
		}
	}

	names := make([]string, 0, len(formats))
	for _, f := range formats {
		names = append(names, string(f))
	}

	return "", NewUsageError("unknown output format %q: want one of %s", value, strings.Join(names, ", "))
}

// Printer renders an envelope onto a writer.
type Printer interface {
	PrintObj(obj runtime.Object, w io.Writer) error
}

// NewPrinter returns the printer for an -o value, or a usage error when the
// value is not one this package knows.
func NewPrinter(value string) (Printer, error) {
	format, err := ParseFormat(value)
	if err != nil {
		return nil, err
	}

	switch format {
	case FormatJSON:
		return &printers.JSONPrinter{}, nil
	case FormatYAML:
		return &printers.YAMLPrinter{}, nil
	case FormatName:
		return &namePrinter{delegate: &printers.NamePrinter{}}, nil
	case FormatWide:
		return &TablePrinter{Columns: DefaultColumns(), Wide: true}, nil
	case FormatTable:
		return &TablePrinter{Columns: DefaultColumns()}, nil
	}

	return nil, NewUsageError("unknown output format %q", value)
}

// namePrinter prints one line per item. The cli-runtime NamePrinter refuses any
// list that is not unstructured, so the list is expanded here and each item is
// handed over on its own. The printed line is "kind.group/name", which is what
// that printer renders for a kind that carries an API group.
type namePrinter struct {
	delegate *printers.NamePrinter
}

// PrintObj implements Printer.
func (p *namePrinter) PrintObj(obj runtime.Object, w io.Writer) error {
	list, ok := obj.(*List)
	if !ok {
		return p.delegate.PrintObj(obj, w)
	}

	for i := range list.Items {
		if err := p.delegate.PrintObj(&list.Items[i], w); err != nil {
			return fmt.Errorf("printing item %d: %w", i, err)
		}
	}

	return nil
}

// Column is one column of the table output.
type Column struct {
	// Header is the column title.
	Header string
	// Wide marks a column that only the wide output carries.
	Wide bool
	// Cell renders this column for one item.
	Cell func(Object) string
}

// DefaultColumns is the table of an envelope that carries no payload yet. A
// command with its own payload passes its own columns to TablePrinter.
func DefaultColumns() []Column {
	return []Column{
		{Header: "NAME", Cell: func(o Object) string { return o.Name }},
		{Header: "AGE", Wide: true, Cell: ageCell},
	}
}

// ageCell renders how long ago the item came into being, kubectl style.
func ageCell(o Object) string {
	if o.CreationTimestamp.IsZero() {
		return "<unknown>"
	}

	return duration.HumanDuration(time.Since(o.CreationTimestamp.Time))
}

// TablePrinter renders an envelope as an aligned table. Rows keep the order of
// the items they came from.
type TablePrinter struct {
	// Columns are the columns to render, wide ones included.
	Columns []Column
	// Wide turns on the columns marked wide.
	Wide bool
}

// PrintObj implements Printer.
func (p *TablePrinter) PrintObj(obj runtime.Object, w io.Writer) error {
	var items []Object

	switch typed := obj.(type) {
	case *List:
		items = typed.Items
	case *Object:
		items = []Object{*typed}
	default:
		return fmt.Errorf("table output does not know how to print %T", obj)
	}

	columns := make([]Column, 0, len(p.Columns))
	for _, c := range p.Columns {
		if c.Wide && !p.Wide {
			continue
		}
		columns = append(columns, c)
	}

	tw := printers.GetNewTabWriter(w)

	headers := make([]string, 0, len(columns))
	for _, c := range columns {
		headers = append(headers, c.Header)
	}
	if _, err := fmt.Fprintln(tw, strings.Join(headers, "\t")); err != nil {
		return fmt.Errorf("writing the table header: %w", err)
	}

	for i := range items {
		cells := make([]string, 0, len(columns))
		for _, c := range columns {
			cells = append(cells, c.Cell(items[i]))
		}
		if _, err := fmt.Fprintln(tw, strings.Join(cells, "\t")); err != nil {
			return fmt.Errorf("writing row %d: %w", i, err)
		}
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flushing the table: %w", err)
	}

	return nil
}
