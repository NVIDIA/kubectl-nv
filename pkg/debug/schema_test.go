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

package debug

import (
	"slices"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The kind is registered by the package that defines it, at import time. See
// the same test in pkg/health for why it cannot live in the command package.
func TestThisPackageRegistersItsKind(t *testing.T) {
	t.Parallel()

	if kinds := output.RegisteredKinds(); !slices.Contains(kinds, output.KindDebugReport) {
		t.Errorf("%s is not registered; registered kinds are %v", output.KindDebugReport, kinds)
	}
}

// populatedDebugReport sets every field of the kind, so that the schema is
// checked against a full instance rather than a zero value.
func populatedDebugReport() *Report {
	r := NewReport("node-1")
	r.Bundle = Bundle{
		Directory:  "/tmp/must-gather",
		Manifest:   "/tmp/must-gather/manifest.json",
		Collectors: CollectorCounts{OK: 7, Skipped: 1, Error: 1},
		Skipped:    []CollectorOutcome{{Name: "driver-exec", Reason: "--no-exec"}},
		Errored:    []CollectorOutcome{{Name: "logs", Reason: "forbidden"}},
		Files:      12,
		Bytes:      4096,
	}
	r.Status = output.ReportStatus{
		Findings:    []output.Finding{},
		Partial:     true,
		Unreachable: []output.Unreachable{{Resource: "pods", Node: "node-1", Reason: "forbidden"}},
	}

	return r
}

func TestAPopulatedDebugReportSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	if err := output.ValidateAgainstSchema(output.KindDebugReport, populatedDebugReport()); err != nil {
		t.Fatal(err)
	}
}

func TestAnEmptyDebugReportSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	if err := output.ValidateAgainstSchema(output.KindDebugReport, NewReport("node-1")); err != nil {
		t.Fatal(err)
	}
}
