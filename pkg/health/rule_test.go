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
	resourcev1 "k8s.io/api/resource/v1"
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

// swapDefaultRegistry points the package level Register and Rules at a fresh
// registry for the duration of one test and restores the real one afterwards.
//
// Without it the test leaves its rule in the process-wide registry, and a
// second run of the same test in the same binary, which is what -count=2 does,
// panics on the duplicate code. A test that only passes once is not a test.
func swapDefaultRegistry(t *testing.T) {
	t.Helper()

	saved := defaultRegistry
	defaultRegistry = NewRegistry()

	t.Cleanup(func() { defaultRegistry = saved })
}

// The package level Register and Rules are the ones rule files use. They have
// to reach the same registry.
func TestPackageLevelRegistry(t *testing.T) {
	swapDefaultRegistry(t)

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

// The swap has to actually isolate: after the cleanup the rule the test
// registered must be gone, or the next test in the binary inherits it.
func TestPackageLevelRegistryIsRestored(t *testing.T) {
	before := len(Rules())

	t.Run("registers into a swapped registry", func(t *testing.T) {
		swapDefaultRegistry(t)
		Register(stubRule{code: "NV-TOPO-901"})

		if len(Rules()) != 1 {
			t.Fatalf("swapped registry holds %d rules, want 1", len(Rules()))
		}
	})

	if got := len(Rules()); got != before {
		t.Errorf("the default registry holds %d rules after the subtest, want %d", got, before)
	}

	for _, rule := range Rules() {
		if rule.Code() == "NV-TOPO-901" {
			t.Error("a rule registered inside the subtest leaked into the default registry")
		}
	}
}

// A Registry used before NewRegistry ran has a nil map. Assigning into one
// panics with a runtime error that says nothing about rules, so the zero value
// has to work rather than fail obscurely.
func TestZeroValueRegistryWorks(t *testing.T) {
	t.Parallel()

	var reg Registry

	if got := len(reg.Rules()); got != 0 {
		t.Errorf("a zero registry holds %d rules, want 0", got)
	}

	if msg := panicMessage(t, func() { reg.Register(stubRule{code: "NV-DP-001"}) }); msg != "" {
		t.Fatalf("registering into a zero registry panicked: %s", msg)
	}

	rules := reg.Rules()
	if len(rules) != 1 || rules[0].Code() != "NV-DP-001" {
		t.Errorf("zero registry = %v, want one NV-DP-001", rules)
	}
}

// A nil rule reaches Register when a rule file's constructor returns nil on a
// path nobody tested. Calling Code on it panics with a nil pointer dereference
// pointing at this package, which sends the reader here rather than to their
// own file.
func TestRegisterRejectsNilRule(t *testing.T) {
	t.Parallel()

	reg := NewRegistry()

	got := panicMessage(t, func() { reg.Register(nil) })

	const want = "health: Register was called with a nil rule"
	if got != want {
		t.Errorf("panic =\n%q\nwant\n%q", got, want)
	}
}

// TestInputIsACompileTimePinOnWhatARuleMayRead does not exercise behaviour:
// it names every field of Input in a composite literal, so that removing or
// renaming one stops this file compiling. Tracks E and G read these fields, and
// a field that quietly disappears between the fork and the merge is a conflict
// in three worktrees at once.
func TestInputIsACompileTimePinOnWhatARuleMayRead(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

	in := Input{
		Nodes:             []inventory.GPUNode{{Product: "NVIDIA-H100-80GB-HBM3"}},
		Hinted:            []inventory.GPUNode{{Hinted: true}},
		ClusterPolicies:   []unstructured.Unstructured{{Object: map[string]any{"kind": "ClusterPolicy"}}},
		OperatorVersion:   "v25.3.0",
		OperatorInstalled: true,
		ComputeDomains:    []unstructured.Unstructured{{Object: map[string]any{"kind": "ComputeDomain"}}},
		ResourceSlices:    []resourcev1.ResourceSlice{{Spec: resourcev1.ResourceSliceSpec{Driver: "gpu.nvidia.com"}}},
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

	if in.ComputeDomains[0].Object["kind"] != "ComputeDomain" {
		t.Errorf("ComputeDomains = %+v", in.ComputeDomains)
	}

	if in.ResourceSlices[0].Spec.Driver != "gpu.nvidia.com" {
		t.Errorf("ResourceSlices = %+v", in.ResourceSlices)
	}
}
