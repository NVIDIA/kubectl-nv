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
	// Fields are the dotted JSON paths this column renders, for instance
	// "status.ready". It is what --fields matches a column against, so a column
	// that leaves it empty cannot be asked for by name and is dropped as soon as
	// --fields is given.
	//
	// A cell often renders more than one path: a GPU count showing requested
	// out of allocatable out of capacity is three of them. All of them are
	// named, and the column survives --fields only when every one was asked
	// for, because a table has nowhere to put half a cell and a column kept by
	// one of its paths would print the other two to a caller that named
	// neither.
	Fields []string
}

// OneField is a column rendering a single path, which is most of them.
func OneField(header, path string) Column {
	return Column{Header: header, Fields: []string{path}}
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
	columns := []Column{OneField("NAME", "metadata.name")}
	if wide {
		columns = append(columns, OneField("AGE", "metadata.creationTimestamp"))
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
	// Fields it names, plus the ones no projection drops. It is how --fields
	// reaches a format that has no nested structure to project.
	Fields []string
	// Unrendered, when it is not nil, is where a note goes naming the
	// requested paths that no column of this table renders.
	//
	// A path a kind has and a table does not show is not an error: the caller
	// asked for something real, and -o json would give it. But a table that
	// came back without the column they asked for and said nothing reads as a
	// table with no such field, and they go looking for the typo.
	Unrendered io.Writer
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
		if len(p.Fields) > 0 && !keepsColumn(kept, c) {
			continue
		}
		keep = append(keep, i)
		headers = append(headers, c.Header)
	}

	p.noteUnrendered(columns, keep)

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

// noteUnrendered reports the requested paths that no kept column renders.
func (p *TablePrinter) noteUnrendered(columns []Column, keep []int) {
	if p.Unrendered == nil || len(p.Fields) == 0 {
		return
	}

	rendered := map[string]bool{}
	for _, i := range keep {
		for _, path := range columns[i].Fields {
			rendered[path] = true
		}
	}

	var missing []string
	for _, path := range p.Fields {
		if !rendered[path] && !coveredByAny(rendered, path) {
			missing = append(missing, path)
		}
	}
	if len(missing) == 0 {
		return
	}

	fmt.Fprintf(p.Unrendered,
		"note: no column of this table renders %s; ask for -o json or -o yaml to see %s\n",
		strings.Join(missing, ", "), pluralPaths(len(missing)))
}

// coveredByAny reports whether a requested section contains a path some kept
// column renders, so that asking for "levels" is not reported as unrendered
// when every level column survived.
func coveredByAny(rendered map[string]bool, requested string) bool {
	for path := range rendered {
		if strings.HasPrefix(path, requested+".") {
			return true
		}
	}

	return false
}

// pluralPaths renders the noun of the note above.
func pluralPaths(n int) string {
	if n == 1 {
		return "it"
	}

	return "them"
}

// keepsColumn reports whether a column survives a projection: every path it
// renders has to have been asked for, or the cell would carry a value the
// caller did not name.
func keepsColumn(kept map[string]bool, c Column) bool {
	if len(c.Fields) == 0 {
		return false
	}
	for _, path := range c.Fields {
		if !coveredBy(kept, path) {
			return false
		}
	}

	return true
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
