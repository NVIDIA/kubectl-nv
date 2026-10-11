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

package rules_test

import (
	"reflect"
	"regexp"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// wantCodes is this milestone's eight checks. Written out rather than derived
// from All, because a test that asked All what All returns would pass on an
// empty list, and an empty rule set reports every cluster as clean.
var wantCodes = []string{
	"NV-DCGM-001",
	"NV-DP-001",
	"NV-NODE-001",
	"NV-NODE-002",
	"NV-NODE-003",
	"NV-NODE-004",
	"NV-OP-001",
	"NV-OP-002",
}

func TestAllReturnsEveryRuleInCodeOrder(t *testing.T) {
	t.Parallel()

	all := rules.All()

	got := make([]string, 0, len(all))
	for _, rule := range all {
		got = append(got, rule.Code())
	}

	if !reflect.DeepEqual(got, wantCodes) {
		t.Errorf("All returns\n%v\nwant\n%v", got, wantCodes)
	}
}

// Two callers must never share a rule. Rules hold no state today, and the
// moment one does, a shared instance turns a per-run detail into a process
// wide one.
func TestAllReturnsFreshRules(t *testing.T) {
	t.Parallel()

	first, second := rules.All(), rules.All()

	for i := range first {
		if first[i] == second[i] {
			t.Errorf("All returns the same %s instance twice", first[i].Code())
		}
	}
}

// Every code has to match the registry's pattern, because a code is what a
// user suppresses and what a script greps for. The registry panics on a
// malformed one, so this says so before a user meets the panic.
func TestEveryCodeMatchesThePattern(t *testing.T) {
	t.Parallel()

	pattern := regexp.MustCompile(health.CodePattern)

	for _, rule := range rules.All() {
		if !pattern.MatchString(rule.Code()) {
			t.Errorf("code %q does not match %s", rule.Code(), health.CodePattern)
		}
	}
}

// A severity a rule never declared would rank below every --fail-on threshold,
// so the rule could never fail a run however bad its finding.
func TestEveryRuleDeclaresAKnownSeverity(t *testing.T) {
	t.Parallel()

	known := map[output.Severity]bool{
		output.SeverityError:   true,
		output.SeverityWarning: true,
		output.SeverityInfo:    true,
	}

	for _, rule := range rules.All() {
		if !known[rule.Severity()] {
			t.Errorf("%s declares severity %q, which is none of error, warning, info", rule.Code(), rule.Severity())
		}
	}
}

func TestRegisterFillsARegistry(t *testing.T) {
	t.Parallel()

	registry := health.NewRegistry()
	rules.Register(registry)

	registered := registry.Rules()

	got := make([]string, 0, len(registered))
	for _, rule := range registered {
		got = append(got, rule.Code())
	}

	if !reflect.DeepEqual(got, wantCodes) {
		t.Errorf("the registry holds\n%v\nwant\n%v", got, wantCodes)
	}
}

// The empty input is the shape every rule meets on a cluster with no GPU nodes
// and no operator. A rule that panicked or reported something there would turn
// "nothing to check" into a crash or a false finding.
func TestEveryRuleIsQuietAndSafeOnAnEmptyInput(t *testing.T) {
	t.Parallel()

	for _, rule := range rules.All() {
		t.Run(rule.Code(), func(t *testing.T) {
			t.Parallel()

			if findings := rule.Evaluate(health.Input{}); len(findings) != 0 {
				t.Errorf("found %+v on an empty input, want nothing", findings)
			}
		})
	}
}

// Every finding carries its own rule's code, and the catalog path is derived
// from the code, so a rule that emitted a finding under another one would link
// the reader to the wrong entry.
func TestEveryRuleStampsItsOwnCodeAndSeverity(t *testing.T) {
	t.Parallel()

	in := brokenInput()

	for _, rule := range rules.All() {
		t.Run(rule.Code(), func(t *testing.T) {
			t.Parallel()

			findings := rule.Evaluate(in)
			if len(findings) == 0 {
				t.Fatal("found nothing on a cluster broken in every way the eight rules look at")
			}

			for _, f := range findings {
				if f.Code != rule.Code() {
					t.Errorf("finding carries code %q, want %q", f.Code, rule.Code())
				}

				if f.Severity != rule.Severity() {
					t.Errorf("finding carries severity %q, want the rule's %q", f.Severity, rule.Severity())
				}

				if f.Next == "" {
					t.Error("finding has no next command")
				}

				if len(f.Evidence) == 0 {
					t.Error("finding has no evidence")
				}
			}
		})
	}
}

// brokenInput is a cluster that trips all eight checks at once:
//
//	worker-1  a GPU node short of allocatable GPUs, whose count label and
//	          capacity disagree, and whose device plugin and dcgm-exporter
//	          pods are missing
//	worker-2  an NVIDIA PCI device with no GPU labels, and one operand
//	          switched off by label
//	worker-3  a node that should have GPUs and advertises none
//	          plus a ClusterPolicy that never went ready
func brokenInput() health.Input {
	worker1 := buildNode("worker-1",
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withCount(8),
		withGPUs(4, 2),
		withDeploy(map[string]string{"device-plugin": "true", "dcgm-exporter": "true"}),
		withOperand(inventory.ComponentGFD, true),
	)

	worker2 := buildNode("worker-2",
		withLabels(map[string]string{rules.LabelPCIVendor: "true"}),
		// One operand off and one on: all of them off would make this a node
		// the operator does not target, and NV-NODE-001 would go quiet.
		withDeploy(map[string]string{"driver": "true", "device-plugin": "false"}),
		hinted(),
	)

	worker3 := buildNode("worker-3",
		withLabels(map[string]string{rules.LabelGPUPresent: "true"}),
		hinted(),
	)

	return health.Input{
		Nodes:             []inventory.GPUNode{worker1},
		Hinted:            []inventory.GPUNode{worker2, worker3},
		ClusterPolicies:   []unstructured.Unstructured{clusterPolicy("cluster-policy", "notReady")},
		OperatorInstalled: true,
	}
}
