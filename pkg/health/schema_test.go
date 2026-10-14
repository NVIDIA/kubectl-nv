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
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The kind is registered by the package that defines it, at import time:
// nothing else can keep the prototype and the type the command marshals in
// step, and a kind registered by a command package would be missing from any
// binary that does not link that command.
func TestThisPackageRegistersItsKind(t *testing.T) {
	t.Parallel()

	if kinds := output.RegisteredKinds(); !slices.Contains(kinds, output.KindHealthReport) {
		t.Errorf("%s is not registered; registered kinds are %v", output.KindHealthReport, kinds)
	}
}

// populatedReport is a report with every field of the kind set, so that the
// schema is checked against a full instance rather than against the handful of
// fields a zero value happens to marshal.
func populatedReport() *HealthReport {
	r := NewHealthReport()
	r.Findings = []output.Finding{{
		Code:      "NV-NODE-002",
		Severity:  output.SeverityError,
		Node:      "node-1",
		Component: "node",
		Message:   "the node advertises no GPUs",
		Evidence:  map[string]string{"capacity": "0"},
		Docs:      CatalogPath("NV-NODE-002"),
		Next:      "kubectl nv info node-1",
	}}
	r.Partial = true
	r.Unreachable = []output.Unreachable{{Resource: "pods", Node: "node-1", Reason: "forbidden"}}
	r.Summary = Summary{
		Errors:            1,
		Warnings:          2,
		Infos:             3,
		NodesChecked:      4,
		GPUCapacity:       32,
		GPUAllocatable:    32,
		GPURequested:      8,
		GPURequestedKnown: true,
	}
	r.ObjectMeta = metav1.ObjectMeta{Name: ReportName}

	return r
}

// A schema is only a contract if what the command prints satisfies it. The
// generator reads the Go type, so it cannot catch a field that marshals as
// null where the schema says array; running a sample through the published
// schema can.
func TestAPopulatedHealthReportSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	if err := output.ValidateAgainstSchema(output.KindHealthReport, populatedReport()); err != nil {
		t.Fatal(err)
	}
}

// The other end of the range: the never-null guarantees in MarshalJSON turn
// the nil slices into empty arrays, and the schema has to accept that too.
func TestAnEmptyHealthReportSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	if err := output.ValidateAgainstSchema(output.KindHealthReport, NewHealthReport()); err != nil {
		t.Fatal(err)
	}
}
