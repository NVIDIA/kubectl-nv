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
	"strings"
	"testing"
)

// testList builds a two item GPUNodeList for the printer tests.
func testList(t *testing.T) *List {
	t.Helper()

	list := NewList(KindGPUNodeList)
	list.Items = []Object{
		*NewObject(KindGPUNode, "node-b"),
		*NewObject(KindGPUNode, "node-a"),
	}

	return list
}

func TestJSONRoundTripKeepsAPIVersionAndKind(t *testing.T) {
	t.Parallel()

	printer, err := NewPrinter(string(FormatJSON))
	if err != nil {
		t.Fatalf("NewPrinter(json): %v", err)
	}

	var buf bytes.Buffer
	if err := printer.PrintObj(testList(t), &buf); err != nil {
		t.Fatalf("PrintObj: %v", err)
	}

	var decoded List
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", buf.String(), err)
	}

	if decoded.APIVersion != "kubectl-nv.nvidia.com/v1alpha1" {
		t.Errorf("list apiVersion = %q, want %q", decoded.APIVersion, "kubectl-nv.nvidia.com/v1alpha1")
	}
	if decoded.Kind != "GPUNodeList" {
		t.Errorf("list kind = %q, want %q", decoded.Kind, "GPUNodeList")
	}
	if len(decoded.Items) != 2 {
		t.Fatalf("decoded %d items, want 2", len(decoded.Items))
	}
	// The envelope preserves the caller's order: deterministic ordering is part
	// of this contract, so the unsorted input order is asserted literally here.
	names := make([]string, 0, len(decoded.Items))
	for i, item := range decoded.Items {
		if item.APIVersion != "kubectl-nv.nvidia.com/v1alpha1" {
			t.Errorf("item %d apiVersion = %q, want %q", i, item.APIVersion, "kubectl-nv.nvidia.com/v1alpha1")
		}
		if item.Kind != "GPUNode" {
			t.Errorf("item %d kind = %q, want %q", i, item.Kind, "GPUNode")
		}
		names = append(names, item.Name)
	}
	if got := strings.Join(names, ","); got != "node-b,node-a" {
		t.Errorf("item names = %q, want %q", got, "node-b,node-a")
	}
}

func TestYAMLOutputCarriesAPIVersionAndKind(t *testing.T) {
	t.Parallel()

	printer, err := NewPrinter(string(FormatYAML))
	if err != nil {
		t.Fatalf("NewPrinter(yaml): %v", err)
	}

	var buf bytes.Buffer
	if err := printer.PrintObj(testList(t), &buf); err != nil {
		t.Fatalf("PrintObj: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "apiVersion: kubectl-nv.nvidia.com/v1alpha1\n") {
		t.Errorf("yaml output missing apiVersion line:\n%s", out)
	}
	if !strings.Contains(out, "kind: GPUNodeList\n") {
		t.Errorf("yaml output missing list kind line:\n%s", out)
	}
	if n := strings.Count(out, "kind: GPUNode\n"); n != 2 {
		t.Errorf("yaml output carries %d item kind lines, want 2:\n%s", n, out)
	}
	for _, name := range []string{"node-a", "node-b"} {
		if !strings.Contains(out, "name: "+name+"\n") {
			t.Errorf("yaml output missing %q:\n%s", name, out)
		}
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

			lines := strings.Split(strings.TrimRight(table.String(), "\n"), "\n")
			if len(lines) < 1 {
				t.Fatalf("table output is empty")
			}
			header, rows := lines[0], lines[1:]

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

func TestNameOutputPrintsKindSlashName(t *testing.T) {
	t.Parallel()

	printer, err := NewPrinter(string(FormatName))
	if err != nil {
		t.Fatalf("NewPrinter(name): %v", err)
	}

	var buf bytes.Buffer
	if err := printer.PrintObj(testList(t), &buf); err != nil {
		t.Fatalf("PrintObj: %v", err)
	}

	want := "gpunode.kubectl-nv.nvidia.com/node-b\ngpunode.kubectl-nv.nvidia.com/node-a\n"
	if buf.String() != want {
		t.Errorf("name output =\n%q\nwant\n%q", buf.String(), want)
	}
}

func TestNameOutputPrintsSingleObject(t *testing.T) {
	t.Parallel()

	printer, err := NewPrinter(string(FormatName))
	if err != nil {
		t.Fatalf("NewPrinter(name): %v", err)
	}

	var buf bytes.Buffer
	if err := printer.PrintObj(NewObject(KindHealthReport, "cluster"), &buf); err != nil {
		t.Fatalf("PrintObj: %v", err)
	}

	want := "healthreport.kubectl-nv.nvidia.com/cluster\n"
	if buf.String() != want {
		t.Errorf("name output = %q, want %q", buf.String(), want)
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

func TestKindConstants(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		constant string
		want     string
	}{
		{KindGPUNodeList, "GPUNodeList"},
		{KindGPUNode, "GPUNode"},
		{KindHealthReport, "HealthReport"},
		{KindTopologyReport, "TopologyReport"},
		{KindBundleManifest, "BundleManifest"},
	} {
		if tc.constant != tc.want {
			t.Errorf("kind constant = %q, want %q", tc.constant, tc.want)
		}
	}

	if APIVersion != "kubectl-nv.nvidia.com/v1alpha1" {
		t.Errorf("APIVersion = %q, want %q", APIVersion, "kubectl-nv.nvidia.com/v1alpha1")
	}
}
