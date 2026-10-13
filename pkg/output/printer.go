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

package output

import (
	"fmt"
	"io"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/cli-runtime/pkg/printers"
)

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
	// FormatName prints one "kind/name" line per item.
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

// Printer renders a kind of this contract onto a writer.
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
		return &namePrinter{}, nil
	case FormatWide:
		return &TablePrinter{Wide: true}, nil
	case FormatTable:
		return &TablePrinter{}, nil
	}

	return nil, NewUsageError("unknown output format %q", value)
}

// Column is one column of the table output.
type Column struct {
	// Header is the column title.
	Header string
	// Field is the dotted JSON path this column renders, for instance
	// "status.ready". It is what --fields matches a column against, so a column
	// that leaves it empty cannot be asked for by name and is dropped as soon as
	// --fields is given.
	Field string
}

// TableRows is implemented by a kind that can render itself as a table. It is
// how a payload type in another package gets a default and a wide table without
// this package knowing any of its fields.
type TableRows interface {
	// TableColumns returns the headers, with the wide ones when wide is set.
	TableColumns(wide bool) []Column
	// TableRows returns one row per item, each with one cell per column.
	TableRows(wide bool) [][]string
}

// nameAndAgeColumns is the table of a kind that carries nothing but a name.
func nameAndAgeColumns(wide bool) []Column {
	columns := []Column{{Header: "NAME", Field: "metadata.name"}}
	if wide {
		columns = append(columns, Column{Header: "AGE", Field: "metadata.creationTimestamp"})
	}

	return columns
}

// nameAndAgeRow renders one object into the columns above.
func nameAndAgeRow(o *Object, wide bool) []string {
	row := []string{o.Name}
	if wide {
		row = append(row, age(o.CreationTimestamp))
	}

	return row
}

// age renders how long ago something came into being, kubectl style.
func age(t metav1.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}

	return duration.HumanDuration(time.Since(t.Time))
}

// TablePrinter renders a kind that implements TableRows as an aligned table.
// Rows keep the order the kind produced them in.
type TablePrinter struct {
	// Wide turns on the columns the kind marks as wide.
	Wide bool
	// Fields, when it is not empty, restricts the table to the columns whose
	// Field it names, plus the ones no projection drops. It is how --fields
	// reaches a format that has no nested structure to project.
	Fields []string
}

// PrintObj implements Printer.
func (p *TablePrinter) PrintObj(obj runtime.Object, w io.Writer) error {
	table, ok := obj.(TableRows)
	if !ok {
		// This is a programming error in the command, not something the user
		// typed, so it must not be reported as a usage error.
		return fmt.Errorf("%T cannot be printed as a table: it does not implement output.TableRows", obj)
	}

	columns := table.TableColumns(p.Wide)
	keep := make([]int, 0, len(columns))
	kept := keptColumns(p.Fields)
	headers := make([]string, 0, len(columns))
	for i, c := range columns {
		if len(p.Fields) > 0 && !kept[c.Field] {
			continue
		}
		keep = append(keep, i)
		headers = append(headers, c.Header)
	}

	tw := printers.GetNewTabWriter(w)
	if _, err := fmt.Fprintln(tw, strings.Join(headers, "\t")); err != nil {
		return fmt.Errorf("writing the table header: %w", err)
	}

	for i, row := range table.TableRows(p.Wide) {
		if len(row) != len(columns) {
			return fmt.Errorf("row %d has %d cells, want %d", i, len(row), len(columns))
		}
		cells := make([]string, 0, len(keep))
		for _, column := range keep {
			cells = append(cells, sanitizeCell(row[column]))
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

// namePrinter prints one "kind/name" line per item, with the kind lowercased
// and no API group. The group belongs in apiVersion: these kinds are not
// cluster resources, and a line carrying the group reads as kubectl output for
// a resource that does not exist.
type namePrinter struct{}

// PrintObj implements Printer.
func (p *namePrinter) PrintObj(obj runtime.Object, w io.Writer) error {
	kind := obj.GetObjectKind().GroupVersionKind().Kind
	if kind == "" {
		return fmt.Errorf("%T has no kind: name output needs one", obj)
	}

	if !meta.IsListType(obj) {
		return p.printOne(w, kind, obj)
	}

	items, err := meta.ExtractList(obj)
	if err != nil {
		return fmt.Errorf("reading the items of %s: %w", kind, err)
	}

	// Items of a typed list do not always carry their own TypeMeta, so the item
	// kind falls back to the list kind without its List suffix.
	itemKind := strings.TrimSuffix(kind, "List")
	for i, item := range items {
		k := item.GetObjectKind().GroupVersionKind().Kind
		if k == "" {
			k = itemKind
		}
		if err := p.printOne(w, k, item); err != nil {
			return fmt.Errorf("printing item %d: %w", i, err)
		}
	}

	return nil
}

// printOne writes the "kind/name" line for a single object.
func (p *namePrinter) printOne(w io.Writer, kind string, obj runtime.Object) error {
	name := "<unknown>"
	if accessor, err := meta.Accessor(obj); err == nil && accessor.GetName() != "" {
		name = accessor.GetName()
	}

	if _, err := fmt.Fprintf(w, "%s/%s\n", strings.ToLower(kind), name); err != nil {
		return fmt.Errorf("writing the name line: %w", err)
	}

	return nil
}
