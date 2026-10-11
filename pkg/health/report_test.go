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
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The cli-runtime printers read TypeMeta off the object before they marshal
// anything, so a HealthReport literal is unprintable at -o json, -o yaml and
// -o name however well MarshalJSON behaves. NewHealthReport is what makes a
// report printable, and this is the test that keeps that true.
func TestNewHealthReportPrintsInEveryMachineFormat(t *testing.T) {
	t.Parallel()

	report := NewHealthReport()

	for _, tc := range []struct {
		format string
		want   []string
	}{
		{"json", []string{`"kind": "HealthReport"`, `"apiVersion": "kubectl-nv.nvidia.com/v1alpha1"`}},
		{"yaml", []string{"kind: HealthReport", "apiVersion: kubectl-nv.nvidia.com/v1alpha1"}},
		{"name", []string{"healthreport/cluster"}},
	} {
		t.Run(tc.format, func(t *testing.T) {
			t.Parallel()

			printer, err := output.NewPrinter(tc.format)
			if err != nil {
				t.Fatalf("NewPrinter(%q): %v", tc.format, err)
			}

			var buf bytes.Buffer
			if err := printer.PrintObj(report, &buf); err != nil {
				t.Fatalf("PrintObj: %v", err)
			}

			for _, want := range tc.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("-o %s output does not contain %q:\n%s", tc.format, want, buf.String())
				}
			}
		})
	}
}

// A literal report, the kind a test or a future caller builds by hand, still
// has to come off the wire stamped and with no nulls in it: a consumer that
// has to check for null before it can range is a consumer that will forget to.
func TestHealthReportMarshalsWithoutNulls(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(HealthReport{})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded["kind"] != output.KindHealthReport {
		t.Errorf("kind is %v, want %q: %s", decoded["kind"], output.KindHealthReport, raw)
	}

	if decoded["apiVersion"] != output.APIVersion {
		t.Errorf("apiVersion is %v, want %q: %s", decoded["apiVersion"], output.APIVersion, raw)
	}

	findings, ok := decoded["findings"].([]any)
	if !ok {
		t.Fatalf("findings is %T, want an array: %s", decoded["findings"], raw)
	}

	if len(findings) != 0 {
		t.Errorf("findings is %v, want it empty", findings)
	}

	if strings.Contains(string(raw), "null") {
		t.Errorf("the zero report marshals a null: %s", raw)
	}
}

// Unreachable is omitempty on ReportStatus, so an empty one is absent rather
// than null. A partial report has to carry it, because "something could not be
// read" with no list of what is the report telling a reader to guess.
func TestHealthReportCarriesWhatItCouldNotSee(t *testing.T) {
	t.Parallel()

	report := NewHealthReport()
	report.Partial = true
	report.Unreachable = []output.Unreachable{{Resource: "resourceslices", Reason: "forbidden"}}

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	for _, want := range []string{`"partial":true`, `"resource":"resourceslices"`, `"reason":"forbidden"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the report does not carry %s: %s", want, raw)
		}
	}
}

// The summary is the one number a reader looks at when the finding list is too
// long to read, so it travels in the machine readable output too.
func TestHealthReportSummaryIsOnTheWire(t *testing.T) {
	t.Parallel()

	report := NewHealthReport()
	report.Summary = Summary{
		Errors:            2,
		Warnings:          1,
		Infos:             3,
		NodesChecked:      4,
		GPUCapacity:       32,
		GPUAllocatable:    30,
		GPURequested:      8,
		GPURequestedKnown: true,
	}

	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded struct {
		Summary map[string]any `json:"summary"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	for key, want := range map[string]any{
		"errors":            float64(2),
		"warnings":          float64(1),
		"infos":             float64(3),
		"nodesChecked":      float64(4),
		"gpuCapacity":       float64(32),
		"gpuAllocatable":    float64(30),
		"gpuRequested":      float64(8),
		"gpuRequestedKnown": true,
	} {
		if decoded.Summary[key] != want {
			t.Errorf("summary.%s is %v, want %v: %s", key, decoded.Summary[key], want, raw)
		}
	}
}

// The exit code is the contract scripts branch on. The report answers for it
// rather than making every caller remember to pass the status half alone.
func TestHealthReportExitCode(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		findings []output.Finding
		partial  bool
		failOn   output.Severity
		want     int
	}{
		{name: "a clean run exits zero", want: output.ExitOK},
		{
			name:     "a warning does not fail the default threshold",
			findings: []output.Finding{{Code: "NV-NODE-003", Severity: output.SeverityWarning}},
			want:     output.ExitOK,
		},
		{
			name:     "a warning fails a warning threshold",
			findings: []output.Finding{{Code: "NV-NODE-003", Severity: output.SeverityWarning}},
			failOn:   output.SeverityWarning,
			want:     output.ExitFindings,
		},
		{
			name:     "an error fails the default threshold",
			findings: []output.Finding{{Code: "NV-NODE-002", Severity: output.SeverityError}},
			want:     output.ExitFindings,
		},
		// An info finding is the one severity that ranks below the default
		// and below a warning threshold, so it is the only one that can tell
		// a threshold that reads the rank from one that compares strings.
		{
			name:     "an info finding does not fail a warning threshold",
			findings: []output.Finding{{Code: "NV-OP-002", Severity: output.SeverityInfo}},
			failOn:   output.SeverityWarning,
			want:     output.ExitOK,
		},
		{
			name:     "an info finding fails an info threshold",
			findings: []output.Finding{{Code: "NV-OP-002", Severity: output.SeverityInfo}},
			failOn:   output.SeverityInfo,
			want:     output.ExitFindings,
		},
		{
			name:     "an info finding does not fail the default threshold",
			findings: []output.Finding{{Code: "NV-OP-002", Severity: output.SeverityInfo}},
			want:     output.ExitOK,
		},
		{name: "partial and clean exits three", partial: true, want: output.ExitPartial},
		{
			name:     "findings win over partial",
			findings: []output.Finding{{Code: "NV-NODE-002", Severity: output.SeverityError}},
			partial:  true,
			want:     output.ExitFindings,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report := NewHealthReport()
			report.Findings = tc.findings
			report.Partial = tc.partial

			if got := report.ExitCode(tc.failOn); got != tc.want {
				t.Errorf("ExitCode(%q) = %d, want %d", tc.failOn, got, tc.want)
			}
		})
	}
}

// The table is what a user actually reads. One row per finding, in the columns
// the handoff named.
func TestHealthReportTable(t *testing.T) {
	t.Parallel()

	report := NewHealthReport()
	report.Findings = []output.Finding{
		{
			Code:      "NV-NODE-002",
			Severity:  output.SeverityError,
			Node:      "worker-1",
			Component: "node",
			Message:   "the node advertises no allocatable GPUs",
		},
		{
			Code:      "NV-OP-001",
			Severity:  output.SeverityWarning,
			Component: "gpu-operator",
			Message:   "the ClusterPolicy is not ready",
		},
	}

	wantColumns := []string{"NODE", "CODE", "SEVERITY", "COMPONENT", "MESSAGE"}
	if got := headers(report.TableColumns(false)); !reflect.DeepEqual(got, wantColumns) {
		t.Errorf("TableColumns = %v, want %v", got, wantColumns)
	}

	wantRows := [][]string{
		{"worker-1", "NV-NODE-002", "error", "node", "the node advertises no allocatable GPUs"},
		// A cluster scoped finding has no node, and an empty cell would make
		// the row ambiguous against a node whose name is genuinely missing.
		{"-", "NV-OP-001", "warning", "gpu-operator", "the ClusterPolicy is not ready"},
	}
	if got := report.TableRows(false); !reflect.DeepEqual(got, wantRows) {
		t.Errorf("TableRows =\n%v\nwant\n%v", got, wantRows)
	}
}

// The wide table adds the next command, because the whole point of a finding
// is what the reader does next and the default table has no room for it.
func TestHealthReportWideTableCarriesTheNextCommand(t *testing.T) {
	t.Parallel()

	report := NewHealthReport()
	report.Findings = []output.Finding{{
		Code:      "NV-NODE-002",
		Severity:  output.SeverityError,
		Node:      "worker-1",
		Component: "node",
		Message:   "the node advertises no allocatable GPUs",
		Next:      "kubectl nv info worker-1",
	}}

	wantColumns := []string{"NODE", "CODE", "SEVERITY", "COMPONENT", "MESSAGE", "NEXT"}
	if got := headers(report.TableColumns(true)); !reflect.DeepEqual(got, wantColumns) {
		t.Errorf("TableColumns(wide) = %v, want %v", got, wantColumns)
	}

	rows := report.TableRows(true)
	if len(rows) != 1 {
		t.Fatalf("TableRows(wide) returned %d rows, want 1", len(rows))
	}

	if got := rows[0][len(rows[0])-1]; got != "kubectl nv info worker-1" {
		t.Errorf("the last wide cell is %q, want the next command", got)
	}
}

// The summary line is the last thing printed, and it has to say what was
// looked at as well as what was found: "0 findings" over 0 nodes is not the
// same answer as "0 findings" over 40.
func TestSummaryLine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		summary Summary
		want    string
	}{
		{
			name: "a clean run over known nodes",
			summary: Summary{
				NodesChecked: 2, GPUCapacity: 8, GPUAllocatable: 8,
				GPURequested: 2, GPURequestedKnown: true,
			},
			want: "2 nodes checked, 8 GPUs capacity, 8 allocatable, 2 requested: 0 errors, 0 warnings, 0 infos",
		},
		{
			name:    "requested is left out when nobody looked",
			summary: Summary{NodesChecked: 1, GPUCapacity: 4, GPUAllocatable: 4},
			want:    "1 node checked, 4 GPUs capacity, 4 allocatable: 0 errors, 0 warnings, 0 infos",
		},
		{
			name:    "findings are counted by severity",
			summary: Summary{Errors: 2, Warnings: 1, Infos: 3, NodesChecked: 1, GPURequestedKnown: true},
			want:    "1 node checked, 0 GPUs capacity, 0 allocatable, 0 requested: 2 errors, 1 warning, 3 infos",
		},
		{
			name:    "no nodes at all is said plainly",
			summary: Summary{},
			want:    "0 nodes checked, 0 GPUs capacity, 0 allocatable: 0 errors, 0 warnings, 0 infos",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := tc.summary.Line(); got != tc.want {
				t.Errorf("Line() =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// A report built by hand and then deep copied must not share its slices or its
// evidence maps with the original: the printers copy objects before rendering.
func TestHealthReportDeepCopy(t *testing.T) {
	t.Parallel()

	report := NewHealthReport()
	report.Findings = []output.Finding{{Code: "NV-NODE-002", Evidence: map[string]string{"capacity": "0"}}}
	report.Unreachable = []output.Unreachable{{Resource: "pods"}}

	copied, ok := report.DeepCopyObject().(*HealthReport)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *HealthReport", report.DeepCopyObject())
	}

	if copied == report {
		t.Fatal("DeepCopyObject returned the receiver itself")
	}

	copied.Findings[0].Code = "changed"
	copied.Findings[0].Evidence["capacity"] = "changed"
	copied.Unreachable[0].Resource = "changed"

	if report.Findings[0].Code != "NV-NODE-002" {
		t.Errorf("the copy shares its findings: code is %q", report.Findings[0].Code)
	}

	if report.Findings[0].Evidence["capacity"] != "0" {
		t.Errorf("the copy shares its evidence map: capacity is %q", report.Findings[0].Evidence["capacity"])
	}

	if report.Unreachable[0].Resource != "pods" {
		t.Errorf("the copy shares its unreachable list: resource is %q", report.Unreachable[0].Resource)
	}

	var nilReport *HealthReport
	if nilReport.DeepCopyObject() != nil {
		t.Error("DeepCopyObject of a nil pointer returned something")
	}
}

// headers pulls the header strings out of the columns, so that the assertions
// above read as the table a user sees rather than as struct literals.
func headers(columns []output.Column) []string {
	t := make([]string, 0, len(columns))
	for _, c := range columns {
		t = append(t, c.Header)
	}

	return t
}
