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

package rules

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// OperatorClusterPolicyNotReady is NV-OP-001: a ClusterPolicy whose
// status.state is not ready.
//
// The ClusterPolicy is the operator's own verdict on itself, so this is the
// one check that does not have to infer anything from labels or pods. A policy
// that is not ready explains most of the node findings underneath it, which is
// why the finding is cluster scoped and survives a --node filter: a node
// reported clean while the policy it depends on is broken is a false clean.
//
// One shape is reported at warning instead. The operator reconciles a single
// ClusterPolicy and marks every other one ignored, and it does that in memory:
// the ignored objects never get a status at all. So a statusless policy beside
// a ready one is a duplicate somebody left in the cluster rather than an
// operator that failed, and reporting it at error would fail a run over a
// cluster that is working perfectly.
type OperatorClusterPolicyNotReady struct{ versionRange }

// NewOperatorClusterPolicyNotReady returns the rule. Both version bounds are
// empty: the ClusterPolicy status subresource predates every operator version
// this plugin supports.
func NewOperatorClusterPolicyNotReady() *OperatorClusterPolicyNotReady {
	return &OperatorClusterPolicyNotReady{}
}

// Code implements health.Rule.
func (r *OperatorClusterPolicyNotReady) Code() string { return "NV-OP-001" }

// Severity implements health.Rule. It is the severity of the finding this rule
// is about; the ignored-duplicate case reports at warning, as Rule allows.
func (r *OperatorClusterPolicyNotReady) Severity() output.Severity { return output.SeverityError }

// Evaluate implements health.Rule.
//
// A cluster with no ClusterPolicy at all produces nothing here. The operator
// not being installed is an answer rather than a failure, and pkg/source
// reports it as its own thing.
func (r *OperatorClusterPolicyNotReady) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	reconciling := reconciledPolicyName(in.ClusterPolicies)

	for i := range in.ClusterPolicies {
		policy := in.ClusterPolicies[i]
		name := policy.GetName()

		state := clusterPolicyState(policy)
		// Matched without regard to case: the state is a free-form string the
		// operator writes, and reading "Ready" as not ready would report a
		// healthy cluster as broken.
		if strings.EqualFold(state, clusterPolicyReady) {
			continue
		}

		if state == "" && reconciling != "" && reconciling != name {
			findings = append(findings, output.Finding{
				Code: r.Code(),
				// A working cluster must not fail a run over an object that
				// decides nothing.
				Severity:  output.SeverityWarning,
				Component: inventory.ComponentOperator,
				Message:   "the ClusterPolicy is ignored: the operator is reconciling another one",
				Evidence: map[string]string{
					"name":          name,
					"reconciling":   reconciling,
					"ignoredPolicy": btoa(true),
				},
				Next: describePolicyCommand(name),
			})

			continue
		}

		evidence := map[string]string{"name": name}
		// Absent rather than empty: a policy the operator has not reconciled
		// has no status at all, and the encodings leave an absent value's key
		// out rather than writing "".
		setIf(evidence, "status.state", state)

		findings = append(findings, output.Finding{
			Code:      r.Code(),
			Severity:  r.Severity(),
			Component: inventory.ComponentOperator,
			Message:   "the ClusterPolicy is not ready",
			Evidence:  evidence,
			Next:      describePolicyCommand(name),
		})
	}

	return findings
}

// reconciledPolicyName is the name of the ready ClusterPolicy, or an empty
// string when none of them is ready. It is what tells an abandoned duplicate
// from the only policy on a cluster whose operator never got anywhere: with
// nothing ready, a statusless policy is the latter and is this rule's error.
func reconciledPolicyName(policies []unstructured.Unstructured) string {
	for i := range policies {
		if strings.EqualFold(clusterPolicyState(policies[i]), clusterPolicyReady) {
			return policies[i].GetName()
		}
	}

	return ""
}

// describePolicyCommand is the next command for a ClusterPolicy finding.
// ClusterPolicy is cluster scoped, so the command carries no namespace.
func describePolicyCommand(name string) string {
	return "kubectl describe clusterpolicy " + name
}
