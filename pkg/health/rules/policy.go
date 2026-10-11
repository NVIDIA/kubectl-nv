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
)

// clusterPolicyReady is the status.state value that means the operator has
// everything it was asked for running. Anything else, an empty state
// included, is a policy that has not got there.
const clusterPolicyReady = "ready"

// The ClusterPolicy spec fields that say whether an operand was asked for.
const (
	// PolicyFieldDevicePlugin is spec.devicePlugin.
	PolicyFieldDevicePlugin = "devicePlugin"
	// PolicyFieldDCGMExporter is spec.dcgmExporter.
	PolicyFieldDCGMExporter = "dcgmExporter"
)

// activeClusterPolicy returns the policy the operator is reconciling.
//
// The operator takes one ClusterPolicy and marks the rest ignored, so a
// cluster with several of them has one that decides everything and some
// leftovers that decide nothing. The ready one is the one it settled on; with
// none ready, the first is the best guess available, and guessing is better
// than reading a leftover's spec and concluding an operand was never asked
// for.
func activeClusterPolicy(policies []unstructured.Unstructured) (unstructured.Unstructured, bool) {
	if len(policies) == 0 {
		return unstructured.Unstructured{}, false
	}

	for i := range policies {
		if strings.EqualFold(clusterPolicyState(policies[i]), clusterPolicyReady) {
			return policies[i], true
		}
	}

	return policies[0], true
}

// clusterPolicyState reads status.state, returning an empty string when the
// field is absent or is not a string. The CRD is not vendored and its schema
// moves between operator versions, so a field of an unexpected type is
// something this code has to survive rather than something it can rule out.
func clusterPolicyState(policy unstructured.Unstructured) string {
	state, found, err := unstructured.NestedString(policy.Object, "status", "state")
	if err != nil || !found {
		return ""
	}

	return state
}

// OperandEnabledInPolicy reports whether the active ClusterPolicy asks for an
// operand, reading spec.<field>.enabled.
//
// This is the question the deploy labels cannot answer. The operator writes
// nvidia.com/gpu.deploy.<key>=true on every node carrying the NVIDIA PCI
// label, whatever the ClusterPolicy enables, so a label of true says only that
// the operator saw the node. A rule that read the label as consent warned on
// every node of every cluster that switched an operand off in its chart
// values, which this repository's own lane does four times over.
//
// A missing field means enabled, which is the CRD's own default, and so does a
// cluster with no ClusterPolicy and a field of a type this code cannot read.
// The conservative answer for a rule that reports a missing operand is to
// assume it was wanted: the other way round hides a real fault behind a spec
// nobody could parse.
func OperandEnabledInPolicy(in health.Input, field string) bool {
	policy, ok := activeClusterPolicy(in.ClusterPolicies)
	if !ok {
		return true
	}

	enabled, found, err := unstructured.NestedBool(policy.Object, "spec", field, "enabled")
	if err != nil || !found {
		return true
	}

	return enabled
}
