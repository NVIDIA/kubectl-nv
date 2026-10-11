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
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

func TestOperatorClusterPolicyNotReady(t *testing.T) {
	t.Parallel()

	rule := rules.NewOperatorClusterPolicyNotReady()

	if rule.Code() != "NV-OP-001" {
		t.Fatalf("Code is %q, want NV-OP-001", rule.Code())
	}

	if rule.Severity() != output.SeverityError {
		t.Errorf("Severity is %q, want error", rule.Severity())
	}

	t.Run("a policy that never went ready", func(t *testing.T) {
		t.Parallel()

		in := health.Input{
			ClusterPolicies:   []unstructured.Unstructured{clusterPolicy("cluster-policy", withState("notReady"))},
			OperatorInstalled: true,
		}

		f := only(t, evaluate(rule, in))

		checkFinding(t, rule, f, inventory.ComponentOperator,
			"the ClusterPolicy is not ready",
			"kubectl describe clusterpolicy cluster-policy",
		)

		if f.Severity != output.SeverityError {
			t.Errorf("Severity is %q, want error", f.Severity)
		}

		// A ClusterPolicy is cluster scoped, so the finding names no node and
		// survives a --node filter: a node reported clean while the policy it
		// depends on is broken is a false clean.
		if f.Node != "" {
			t.Errorf("Node is %q, want it empty for a cluster scoped finding", f.Node)
		}

		checkEvidence(t, f.Evidence, map[string]string{
			"name":         "cluster-policy",
			"status.state": "notReady",
		})
	})

	t.Run("a ready policy", func(t *testing.T) {
		t.Parallel()

		in := health.Input{
			ClusterPolicies:   []unstructured.Unstructured{clusterPolicy("cluster-policy", withState("ready"))},
			OperatorInstalled: true,
		}

		wantNone(t, evaluate(rule, in))
	})

	// The state is a free-form string the operator writes, and nothing
	// guarantees its case. Reading "Ready" as not ready would report every
	// cluster whose operator capitalises differently as broken.
	t.Run("the ready state is matched without regard to case", func(t *testing.T) {
		t.Parallel()

		for _, state := range []string{"ready", "Ready", "READY"} {
			in := health.Input{
				ClusterPolicies:   []unstructured.Unstructured{clusterPolicy("cluster-policy", withState(state))},
				OperatorInstalled: true,
			}

			if findings := evaluate(rule, in); len(findings) != 0 {
				t.Errorf("state %q reported %+v, want nothing", state, findings)
			}
		}
	})

	// The only ClusterPolicy on the cluster having no status is a policy the
	// operator has not reconciled at all, which is this rule's error. The
	// evidence leaves the key out rather than writing an empty string, as the
	// encodings require.
	t.Run("the only policy has no status at all", func(t *testing.T) {
		t.Parallel()

		in := health.Input{
			ClusterPolicies:   []unstructured.Unstructured{clusterPolicy("cluster-policy")},
			OperatorInstalled: true,
		}

		f := only(t, evaluate(rule, in))

		if f.Severity != output.SeverityError {
			t.Errorf("Severity is %q, want error", f.Severity)
		}

		if _, ok := f.Evidence["status.state"]; ok {
			t.Errorf("evidence carries status.state = %q, want the key left out", f.Evidence["status.state"])
		}

		checkEvidence(t, f.Evidence, map[string]string{"name": "cluster-policy"})
	})

	// The operator reconciles one ClusterPolicy and marks the rest ignored,
	// and it does that in memory: the ignored objects never get a status. So a
	// statusless policy alongside a ready one is a duplicate somebody left
	// behind rather than an operator that failed, and reporting it at error
	// would fail a run over a cluster that is working.
	t.Run("a statusless duplicate alongside a ready policy", func(t *testing.T) {
		t.Parallel()

		in := health.Input{
			ClusterPolicies: []unstructured.Unstructured{
				clusterPolicy("cluster-policy", withState("ready")),
				clusterPolicy("left-over-policy"),
			},
			OperatorInstalled: true,
		}

		f := only(t, evaluate(rule, in))

		if f.Severity != output.SeverityWarning {
			t.Errorf("Severity is %q, want warning: the cluster is working", f.Severity)
		}

		checkFinding(t, rule, f, inventory.ComponentOperator,
			"the ClusterPolicy is ignored: the operator is reconciling another one",
			"kubectl describe clusterpolicy left-over-policy",
		)

		checkEvidence(t, f.Evidence, map[string]string{
			"name":          "left-over-policy",
			"reconciling":   "cluster-policy",
			"ignoredPolicy": "true",
		})
	})

	// A duplicate that does carry a state the operator does not call ready is
	// a real failure, so the duplicate exclusion has to be about the absent
	// status rather than about there being more than one policy.
	t.Run("a duplicate with a failed state is still an error", func(t *testing.T) {
		t.Parallel()

		in := health.Input{
			ClusterPolicies: []unstructured.Unstructured{
				clusterPolicy("cluster-policy", withState("ready")),
				clusterPolicy("second-policy", withState("notReady")),
			},
			OperatorInstalled: true,
		}

		f := only(t, evaluate(rule, in))

		if f.Severity != output.SeverityError {
			t.Errorf("Severity is %q, want error", f.Severity)
		}
	})

	t.Run("one finding per policy", func(t *testing.T) {
		t.Parallel()

		in := health.Input{
			ClusterPolicies: []unstructured.Unstructured{
				clusterPolicy("cluster-policy", withState("notReady")),
				clusterPolicy("second-policy", withState("ready")),
				clusterPolicy("third-policy", withState("notReady")),
			},
			OperatorInstalled: true,
		}

		findings := evaluate(rule, in)
		if len(findings) != 2 {
			t.Fatalf("got %d findings, want 2: %+v", len(findings), findings)
		}

		if findings[0].Evidence["name"] != "cluster-policy" || findings[1].Evidence["name"] != "third-policy" {
			t.Errorf("findings name %q and %q, want cluster-policy and third-policy",
				findings[0].Evidence["name"], findings[1].Evidence["name"])
		}
	})

	// No ClusterPolicy at all is not this rule's finding. A cluster without the
	// operator is a cluster nobody installed it on, which is an answer rather
	// than a failure.
	t.Run("no policies at all", func(t *testing.T) {
		t.Parallel()

		wantNone(t, evaluate(rule, health.Input{OperatorInstalled: false}))
	})

	t.Run("a declared range excludes an out-of-range version", func(t *testing.T) {
		t.Parallel()

		if !rule.Applies("v25.10.0") || !rule.Applies("") {
			t.Error("the rule must apply at a known in-range version and at an unknown one")
		}

		bounded := rules.NewOperatorClusterPolicyNotReady()
		bounded.MinVersion = "v99.0.0"

		if bounded.Applies("v25.10.0") {
			t.Error("a rule bounded at v99.0.0 applies at v25.10.0, want it skipped")
		}
	})
}
