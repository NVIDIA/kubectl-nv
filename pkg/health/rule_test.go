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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// stubRule is a rule that reports what it was built with. Rules are pure
// functions of an Input, so a stub is enough to exercise the registry.
type stubRule struct {
	code     string
	severity output.Severity
	applies  bool
	findings []output.Finding
}

func (r stubRule) Code() string              { return r.code }
func (r stubRule) Severity() output.Severity { return r.severity }

func (r stubRule) Applies(operatorVersion string) bool { return r.applies }

func (r stubRule) Evaluate(in Input) []output.Finding { return r.findings }

// panicMessage runs fn and returns the message it panicked with. An empty
// string means it did not panic, which the caller reports as the failure.
func panicMessage(t *testing.T, fn func()) string {
	t.Helper()

	var msg string

	func() {
		defer func() {
			if r := recover(); r != nil {
				if err, ok := r.(error); ok {
					msg = err.Error()

					return
				}

				msg, _ = r.(string)
			}
		}()

		fn()
	}()

	return msg
}

// Two rules under one code means one of them silently never runs, and the run
// still reports clean. That has to be loud, and at registration time rather
// than at evaluation time.
func TestRegisterRejectsDuplicateCode(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(stubRule{code: "NV-NODE-001"})

	got := panicMessage(t, func() { reg.Register(stubRule{code: "NV-NODE-001"}) })

	const want = `health: rule code "NV-NODE-001" is registered twice`
	if got != want {
		t.Errorf("panic =\n%q\nwant\n%q", got, want)
	}
}

func TestRegisterRejectsMalformedCode(t *testing.T) {
	t.Parallel()

	for _, code := range []string{
		"",
		"nv-node-001",
		"NV-NODE-01",
		"NV-NODE-0001",
		"NV-FOO-001",
		"NV-NODE-abc",
		"NODE-001",
		"NV-NODE-001 ",
		" NV-NODE-001",
		"XNV-NODE-001",
		"NV-NODE-001X",
	} {
		t.Run(code, func(t *testing.T) {
			t.Parallel()

			reg := NewRegistry()

			got := panicMessage(t, func() { reg.Register(stubRule{code: code}) })

			want := `health: rule code "` + code + `" does not match ` + CodePattern
			if got != want {
				t.Errorf("panic =\n%q\nwant\n%q", got, want)
			}
		})
	}
}

// Every area in the pattern has to be registrable, or a track lands its rules
// and finds the registry rejects its own prefix.
func TestRegisterAcceptsEveryArea(t *testing.T) {
	t.Parallel()

	areas := []string{"NODE", "OP", "DRV", "CTK", "DP", "DRA", "MIG", "DCGM", "CD", "UPG", "TOPO"}

	reg := NewRegistry()

	for _, area := range areas {
		code := "NV-" + area + "-001"
		if msg := panicMessage(t, func() { reg.Register(stubRule{code: code}) }); msg != "" {
			t.Errorf("registering %s panicked: %s", code, msg)
		}
	}

	if got := len(reg.Rules()); got != len(areas) {
		t.Errorf("registered %d rules, want %d", got, len(areas))
	}
}

// The order rules run in is the order their findings are produced in, and that
// has to be the same on every machine.
func TestRulesSortedByCode(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	for _, code := range []string{"NV-UPG-010", "NV-DRV-002", "NV-UPG-002", "NV-DRV-001"} {
		reg.Register(stubRule{code: code})
	}

	var got []string
	for _, rule := range reg.Rules() {
		got = append(got, rule.Code())
	}

	const want = "NV-DRV-001 NV-DRV-002 NV-UPG-002 NV-UPG-010"
	if strings.Join(got, " ") != want {
		t.Errorf("order = %q, want %q", strings.Join(got, " "), want)
	}
}

// Rules hands out a copy. A caller that sorts or truncates what it got must
// not be reordering the registry for everyone else.
func TestRulesReturnsACopy(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()
	reg.Register(stubRule{code: "NV-DRV-001"})
	reg.Register(stubRule{code: "NV-DRV-002"})

	got := reg.Rules()
	got[0] = stubRule{code: "NV-MIG-999"}

	if reg.Rules()[0].Code() != "NV-DRV-001" {
		t.Errorf("mutating the returned slice changed the registry: %s", reg.Rules()[0].Code())
	}
}

// The package level Register and Rules are the ones rule files use. They have
// to reach the same registry.
func TestPackageLevelRegistry(t *testing.T) {
	Register(stubRule{code: "NV-TOPO-900"})

	var found bool

	for _, rule := range Rules() {
		if rule.Code() == "NV-TOPO-900" {
			found = true
		}
	}

	if !found {
		t.Error("a rule registered through Register is not in Rules")
	}
}

// Input is the whole read surface of a rule. If a field it names disappears,
// every rule that reads it stops compiling, so the contract is pinned here.
func TestInputCarriesEverythingARuleReads(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	in := Input{
		Nodes:             []inventory.GPUNode{{Product: "NVIDIA-H100-80GB-HBM3"}},
		Hinted:            []inventory.GPUNode{{Hinted: true}},
		ClusterPolicies:   []unstructured.Unstructured{{Object: map[string]any{"kind": "ClusterPolicy"}}},
		OperatorVersion:   "v25.3.0",
		OperatorInstalled: true,
		Events:            []corev1.Event{{Reason: "Started"}},
		Now:               now,
	}

	rule := stubRule{
		code:     "NV-OP-001",
		severity: output.SeverityError,
		applies:  true,
		findings: []output.Finding{{Code: "NV-OP-001", Node: in.Nodes[0].Product}},
	}

	if !rule.Applies(in.OperatorVersion) {
		t.Error("Applies returned false")
	}

	findings := rule.Evaluate(in)
	if len(findings) != 1 || findings[0].Node != "NVIDIA-H100-80GB-HBM3" {
		t.Errorf("Evaluate = %+v", findings)
	}

	if !in.Now.Equal(now) {
		t.Errorf("Now = %v, want %v", in.Now, now)
	}

	if !in.Hinted[0].Hinted || !in.OperatorInstalled || in.Events[0].Reason != "Started" {
		t.Errorf("Input = %+v", in)
	}
}
