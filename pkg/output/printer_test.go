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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// gpuNode stands in for a kind that a later track defines in its own package.
// The point of it is that pkg/output knows nothing about its fields: it is
// printed because it is a runtime.Object that can describe its own table.
type gpuNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Spec   gpuNodeSpec   `json:"spec,omitzero"`
	Status gpuNodeStatus `json:"status,omitzero"`
}

type gpuNodeSpec struct {
	Driver string `json:"driver,omitempty"`
}

type gpuNodeStatus struct {
	GPUs int `json:"gpus,omitempty"`
}

func (n *gpuNode) DeepCopyObject() runtime.Object {
	clone := *n
	n.DeepCopyInto(&clone.ObjectMeta)

	return &clone
}

func (n *gpuNode) TableColumns(wide bool) []Column {
	columns := []Column{
		{Header: "NAME", Field: "metadata.name"},
		{Header: "GPUS", Field: "status.gpus"},
	}
	if wide {
		columns = append(columns, Column{Header: "DRIVER", Field: "spec.driver"})
	}

	return columns
}

func (n *gpuNode) TableRows(wide bool) [][]string {
	row := []string{n.Name, strconv.Itoa(n.Status.GPUs)}
	if wide {
		row = append(row, n.Spec.Driver)
	}

	return [][]string{row}
}

// gpuNodeList is the list form of the same made up kind.
type gpuNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`

	Items []gpuNode `json:"items"`
}

func (l *gpuNodeList) DeepCopyObject() runtime.Object {
	clone := *l
	clone.Items = append([]gpuNode(nil), l.Items...)

	return &clone
}

func (l *gpuNodeList) TableColumns(wide bool) []Column {
	return (&gpuNode{}).TableColumns(wide)
}

func (l *gpuNodeList) TableRows(wide bool) [][]string {
	rows := make([][]string, 0, len(l.Items))
	for i := range l.Items {
		rows = append(rows, l.Items[i].TableRows(wide)...)
	}

	return rows
}

func testGPUNodeList(t *testing.T) *gpuNodeList {
	t.Helper()

	return &gpuNodeList{
		TypeMeta: TypeMetaFor(KindGPUNodeList),
		Items: []gpuNode{
			{
				TypeMeta:   TypeMetaFor(KindGPUNode),
				ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
				Spec:       gpuNodeSpec{Driver: "580.65"},
				Status:     gpuNodeStatus{GPUs: 8},
			},
			{
				TypeMeta:   TypeMetaFor(KindGPUNode),
				ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
				Spec:       gpuNodeSpec{Driver: "575.51"},
				Status:     gpuNodeStatus{GPUs: 4},
			},
		},
	}
}

// splitTable returns the header and the rows of a rendered table.
func splitTable(t *testing.T, out string) (string, []string) {
	t.Helper()

	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatalf("table output is empty")
	}

	return lines[0], lines[1:]
}

// A kind this package has never heard of has to print, because every kind the
// later tracks define lives in their own packages.
func TestTablePrintsAnyKindThatDescribesItsTable(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		format     Format
		wantHeader string
		wantFirst  string
	}{
		{format: FormatTable, wantHeader: "NAME", wantFirst: "node-b"},
		{format: FormatWide, wantHeader: "DRIVER", wantFirst: "node-b"},
	} {
		t.Run(string(tc.format), func(t *testing.T) {
			t.Parallel()

			printer, err := NewPrinter(string(tc.format))
			if err != nil {
				t.Fatalf("NewPrinter(%s): %v", tc.format, err)
			}

			var buf bytes.Buffer
			if err := printer.PrintObj(testGPUNodeList(t), &buf); err != nil {
				t.Fatalf("PrintObj: %v", err)
			}

			header, rows := splitTable(t, buf.String())
			if !strings.Contains(header, tc.wantHeader) {
				t.Errorf("header = %q, want it to carry %q", header, tc.wantHeader)
			}
			if tc.format == FormatTable && strings.Contains(header, "DRIVER") {
				t.Errorf("default header = %q, want no wide column", header)
			}
			if len(rows) != 2 {
				t.Fatalf("table has %d rows, want 2:\n%s", len(rows), buf.String())
			}
			if !strings.HasPrefix(rows[0], tc.wantFirst) {
				t.Errorf("first row = %q, want it to start with %q", rows[0], tc.wantFirst)
			}
			if !strings.Contains(rows[0], "8") {
				t.Errorf("first row = %q, want the GPU count from the payload", rows[0])
			}
			if tc.format == FormatWide && !strings.Contains(rows[0], "580.65") {
				t.Errorf("wide row = %q, want the driver from the payload", rows[0])
			}
		})
	}
}

func TestTableAndJSONAgreeOnRowCount(t *testing.T) {
	t.Parallel()

	for _, format := range []Format{FormatTable, FormatWide} {
		t.Run(string(format), func(t *testing.T) {
			t.Parallel()

			tablePrinter, err := NewPrinter(string(format))
			if err != nil {
				t.Fatalf("NewPrinter(%s): %v", format, err)
			}

			var table bytes.Buffer
			if err := tablePrinter.PrintObj(testList(t), &table); err != nil {
				t.Fatalf("PrintObj: %v", err)
			}
			header, rows := splitTable(t, table.String())

			if !strings.HasPrefix(header, "NAME") {
				t.Errorf("table header = %q, want it to start with NAME", header)
			}
			if format == FormatWide && !strings.Contains(header, "AGE") {
				t.Errorf("wide header = %q, want an AGE column", header)
			}
			if format == FormatTable && strings.Contains(header, "AGE") {
				t.Errorf("default header = %q, want no AGE column", header)
			}

			jsonPrinter, err := NewPrinter(string(FormatJSON))
			if err != nil {
				t.Fatalf("NewPrinter(json): %v", err)
			}
			var asJSON bytes.Buffer
			if err := jsonPrinter.PrintObj(testList(t), &asJSON); err != nil {
				t.Fatalf("PrintObj: %v", err)
			}
			var decoded List
			if err := json.Unmarshal(asJSON.Bytes(), &decoded); err != nil {
				t.Fatalf("json.Unmarshal: %v", err)
			}

			items := decoded.Items
			if len(rows) != len(items) {
				t.Fatalf("table has %d rows (%q), json has %d items", len(rows), rows, len(items))
			}
			// Row order follows item order: the table never reorders what it is given.
			for i, row := range rows {
				if want := items[i].Name; !strings.HasPrefix(row, want) {
					t.Errorf("row %d = %q, want it to start with %q", i, row, want)
				}
			}
		})
	}
}

// A kind that cannot describe its own table is a programming error, not a user
// error, so it must not be mistaken for a usage error and exit 2.
func TestTableRejectsAKindWithoutColumns(t *testing.T) {
	t.Parallel()

	printer, err := NewPrinter(string(FormatTable))
	if err != nil {
		t.Fatalf("NewPrinter(table): %v", err)
	}

	err = printer.PrintObj(&metav1.Status{TypeMeta: metav1.TypeMeta{Kind: "Status"}}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("PrintObj accepted a kind with no table")
	}

	var usage *UsageError
	if errors.As(err, &usage) {
		t.Errorf("error %v is a usage error, want a plain error", err)
	}
	if !strings.Contains(err.Error(), "TableRows") {
		t.Errorf("error = %q, want it to name the interface that is missing", err.Error())
	}
}

func TestNameOutputPrintsLowercaseKindSlashName(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		obj  runtime.Object
		want string
	}{
		{
			name: "a list of the generic envelope",
			obj:  testList(t),
			want: "gpunode/node-b\ngpunode/node-a\n",
		},
		{
			name: "a single object",
			obj:  NewObject(KindHealthReport, "cluster"),
			want: "healthreport/cluster\n",
		},
		{
			name: "a kind defined outside this package",
			obj:  testGPUNodeList(t),
			want: "gpunode/node-b\ngpunode/node-a\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			printer, err := NewPrinter(string(FormatName))
			if err != nil {
				t.Fatalf("NewPrinter(name): %v", err)
			}

			var buf bytes.Buffer
			if err := printer.PrintObj(tc.obj, &buf); err != nil {
				t.Fatalf("PrintObj: %v", err)
			}
			if buf.String() != tc.want {
				t.Errorf("name output =\n%q\nwant\n%q", buf.String(), tc.want)
			}
		})
	}
}

// The group belongs in apiVersion, not on the name line: these kinds are not
// cluster resources and the line must not read as kubectl output for one.
func TestNameOutputCarriesNoAPIGroup(t *testing.T) {
	t.Parallel()

	printer, err := NewPrinter(string(FormatName))
	if err != nil {
		t.Fatalf("NewPrinter(name): %v", err)
	}

	var buf bytes.Buffer
	if err := printer.PrintObj(NewObject(KindGPUNode, "node-a"), &buf); err != nil {
		t.Fatalf("PrintObj: %v", err)
	}
	if strings.Contains(buf.String(), GroupName) {
		t.Errorf("name output %q carries the API group", buf.String())
	}
}

// Items of a typed list do not always carry their own TypeMeta, so the item
// kind falls back to the list kind without its List suffix.
func TestNameOutputDerivesTheItemKindFromTheListKind(t *testing.T) {
	t.Parallel()

	list := NewList(KindGPUNodeList)
	list.Items = []Object{{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}}

	printer, err := NewPrinter(string(FormatName))
	if err != nil {
		t.Fatalf("NewPrinter(name): %v", err)
	}

	var buf bytes.Buffer
	if err := printer.PrintObj(list, &buf); err != nil {
		t.Fatalf("PrintObj: %v", err)
	}
	if buf.String() != "gpunode/node-a\n" {
		t.Errorf("name output = %q, want %q", buf.String(), "gpunode/node-a\n")
	}
}

func TestNameOutputRejectsAnObjectWithNoKind(t *testing.T) {
	t.Parallel()

	printer, err := NewPrinter(string(FormatName))
	if err != nil {
		t.Fatalf("NewPrinter(name): %v", err)
	}

	err = printer.PrintObj(&Object{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("PrintObj accepted an object with no kind")
	}
	if !strings.Contains(err.Error(), "kind") {
		t.Errorf("error = %q, want it to mention the missing kind", err.Error())
	}
}

func TestUnknownOutputFormatIsAUsageError(t *testing.T) {
	t.Parallel()

	printer, err := NewPrinter("toml")
	if err == nil {
		t.Fatalf("NewPrinter(toml) returned printer %v and no error", printer)
	}

	var usage *UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("NewPrinter(toml) error %T (%v), want *UsageError", err, err)
	}
	if usage.ExitCode() != ExitUsage {
		t.Errorf("usage error exit code = %d, want %d", usage.ExitCode(), ExitUsage)
	}
	if ExitCodeFor(err) != ExitUsage {
		t.Errorf("ExitCodeFor = %d, want %d", ExitCodeFor(err), ExitUsage)
	}
	if !strings.Contains(err.Error(), "toml") {
		t.Errorf("error %q does not name the rejected value", err.Error())
	}
	for _, valid := range []string{"json", "yaml", "wide", "name"} {
		if !strings.Contains(err.Error(), valid) {
			t.Errorf("error %q does not list the %q format", err.Error(), valid)
		}
	}
}

func TestKnownFormatsAreAccepted(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]Format{
		"":      FormatTable,
		"table": FormatTable,
		"json":  FormatJSON,
		"yaml":  FormatYAML,
		"wide":  FormatWide,
		"name":  FormatName,
	} {
		got, err := ParseFormat(input)
		if err != nil {
			t.Errorf("ParseFormat(%q): %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseFormat(%q) = %q, want %q", input, got, want)
		}
	}
}

// raggedKind returns a row whose cell count does not match its columns, which
// is a bug in the kind rather than anything the user did.
type raggedKind struct {
	metav1.TypeMeta `json:",inline"`

	cells []string
}

func (r *raggedKind) DeepCopyObject() runtime.Object { clone := *r; return &clone }

func (r *raggedKind) TableColumns(_ bool) []Column {
	return []Column{{Header: "NAME"}, {Header: "GPUS"}}
}

func (r *raggedKind) TableRows(_ bool) [][]string { return [][]string{r.cells} }

// A kind that hands back the wrong number of cells would otherwise print a
// table whose columns do not line up with its headers, silently.
func TestTableRejectsARowWithTheWrongCellCount(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		cells []string
	}{
		{name: "too few cells", cells: []string{"node-a"}},
		{name: "too many cells", cells: []string{"node-a", "8", "extra"}},
		{name: "no cells at all", cells: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			printer, err := NewPrinter(string(FormatTable))
			if err != nil {
				t.Fatalf("NewPrinter(table): %v", err)
			}

			obj := &raggedKind{TypeMeta: TypeMetaFor(KindGPUNode), cells: tc.cells}

			var buf bytes.Buffer
			err = printer.PrintObj(obj, &buf)
			if err == nil {
				t.Fatalf("PrintObj accepted a row of %d cells against 2 columns, output:\n%s", len(tc.cells), buf.String())
			}

			want := fmt.Sprintf("row 0 has %d cells, want 2", len(tc.cells))
			if err.Error() != want {
				t.Errorf("error = %q, want %q", err.Error(), want)
			}

			var usage *UsageError
			if errors.As(err, &usage) {
				t.Errorf("error %v is a usage error, want a plain error", err)
			}
		})
	}
}

// Label values, taints and node names reach the wide columns, and stdout is a
// terminal. A cell that can move the cursor can repaint the table around it.
func TestTableCellsAreSanitized(t *testing.T) {
	t.Parallel()

	obj := &gpuNode{
		TypeMeta:   TypeMetaFor(KindGPUNode),
		ObjectMeta: metav1.ObjectMeta{Name: "node-\x1b[31ma\u009b31mb\x7fc"},
		Spec:       gpuNodeSpec{Driver: "580\t65\n580.66"},
	}

	printer, err := NewPrinter(string(FormatWide))
	if err != nil {
		t.Fatalf("NewPrinter(wide): %v", err)
	}

	var buf bytes.Buffer
	if err := printer.PrintObj(obj, &buf); err != nil {
		t.Fatalf("PrintObj: %v", err)
	}

	out := buf.String()
	for _, bad := range []string{"\x1b", "\u009b", "\x7f", "\x00"} {
		if strings.Contains(out, bad) {
			t.Errorf("table output still carries %q: %q", bad, out)
		}
	}
	if !strings.Contains(out, "node-[31ma31mbc") {
		t.Errorf("table output = %q, want the name with only the control bytes removed", out)
	}

	// A tab or a newline inside a cell would break the columns apart, so in a
	// cell they become a space rather than being kept.
	if !strings.Contains(out, "580 65 580.66") {
		t.Errorf("table output = %q, want the tab and newline inside the cell turned into spaces", out)
	}
	if n := strings.Count(strings.TrimRight(out, "\n"), "\n"); n != 1 {
		t.Errorf("table has %d line breaks, want 1 (header plus one row): %q", n, out)
	}
}
