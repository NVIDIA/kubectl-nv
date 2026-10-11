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

// clusterPolicyReady is the status.state value that means the operator has
// everything it was asked for running. Anything else, an empty state
// included, is a policy that has not got there.
const clusterPolicyReady = "ready"

// OperatorClusterPolicyNotReady is NV-OP-001: a ClusterPolicy whose
// status.state is not ready.
//
// The ClusterPolicy is the operator's own verdict on itself, so this is the
// one check that does not have to infer anything from labels or pods. A policy
// that is not ready explains most of the node findings underneath it, which is
// why the finding is cluster scoped and survives a --node filter: a node
// reported clean while the policy it depends on is broken is a false clean.
type OperatorClusterPolicyNotReady struct{ versionRange }

// NewOperatorClusterPolicyNotReady returns the rule. Both version bounds are
// empty: the ClusterPolicy status subresource predates every operator version
// this plugin supports.
func NewOperatorClusterPolicyNotReady() *OperatorClusterPolicyNotReady {
	return &OperatorClusterPolicyNotReady{}
}

// Code implements health.Rule.
func (r *OperatorClusterPolicyNotReady) Code() string { return "NV-OP-001" }

// Severity implements health.Rule.
func (r *OperatorClusterPolicyNotReady) Severity() output.Severity { return output.SeverityError }

// Evaluate implements health.Rule.
//
// A cluster with no ClusterPolicy at all produces nothing here. The operator
// not being installed is an answer rather than a failure, and pkg/source
// reports it as its own thing.
func (r *OperatorClusterPolicyNotReady) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	for i := range in.ClusterPolicies {
		policy := in.ClusterPolicies[i]

		state := clusterPolicyState(policy)
		// Matched without regard to case: the state is a free-form string the
		// operator writes, and reading "Ready" as not ready would report a
		// healthy cluster as broken.
		if strings.EqualFold(state, clusterPolicyReady) {
			continue
		}

		evidence := map[string]string{"name": policy.GetName()}
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
			Next:      "kubectl describe clusterpolicy " + policy.GetName(),
		})
	}

	return findings
}

// clusterPolicyState reads status.state, returning an empty string when the
// field is absent or is not a string. The CRD is not vendored and its schema
// moves between operator versions, so a field of an unexpected type is
// something this rule has to survive rather than something it can rule out.
func clusterPolicyState(policy unstructured.Unstructured) string {
	state, found, err := unstructured.NestedString(policy.Object, "status", "state")
	if err != nil || !found {
		return ""
	}

	return state
}
