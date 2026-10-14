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
)

// Whether an operand was asked for is a question only the ClusterPolicy
// answers. The deploy labels cannot: the operator writes them true on every
// node with the NVIDIA PCI label whatever the policy enables.
func TestOperandEnabledInPolicy(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		policies []unstructured.Unstructured
		field    string
		want     bool
	}{
		{
			name:  "no policy at all means assume it was wanted",
			field: rules.PolicyFieldDevicePlugin,
			want:  true,
		},
		{
			name: "a policy that does not mention the field",
			policies: []unstructured.Unstructured{
				clusterPolicy("cluster-policy", withState("ready")),
			},
			field: rules.PolicyFieldDevicePlugin,
			want:  true,
		},
		{
			name: "enabled",
			policies: []unstructured.Unstructured{
				clusterPolicy("cluster-policy", withState("ready"),
					withOperandEnabled("devicePlugin", true)),
			},
			field: rules.PolicyFieldDevicePlugin,
			want:  true,
		},
		{
			name: "disabled",
			policies: []unstructured.Unstructured{
				clusterPolicy("cluster-policy", withState("ready"),
					withOperandEnabled("devicePlugin", false)),
			},
			field: rules.PolicyFieldDevicePlugin,
			want:  false,
		},
		{
			name: "the two fields are read separately",
			policies: []unstructured.Unstructured{
				clusterPolicy("cluster-policy", withState("ready"),
					withOperandEnabled("devicePlugin", false),
					withOperandEnabled("dcgmExporter", true)),
			},
			field: rules.PolicyFieldDCGMExporter,
			want:  true,
		},
		// The operator reconciles one policy and ignores the rest, so a
		// leftover's spec decides nothing. Reading the first policy in the
		// list rather than the ready one would let an abandoned object switch
		// a rule off across the whole cluster.
		{
			name: "a leftover policy does not decide",
			policies: []unstructured.Unstructured{
				clusterPolicy("left-over-policy", withOperandEnabled("devicePlugin", false)),
				clusterPolicy("cluster-policy", withState("ready"),
					withOperandEnabled("devicePlugin", true)),
			},
			field: rules.PolicyFieldDevicePlugin,
			want:  true,
		},
		// With nothing ready there is no better guess than the first one, and
		// it still beats ignoring the spec entirely.
		{
			name: "no policy is ready",
			policies: []unstructured.Unstructured{
				clusterPolicy("cluster-policy", withOperandEnabled("devicePlugin", false)),
			},
			field: rules.PolicyFieldDevicePlugin,
			want:  false,
		},
		// The CRD is not vendored and its schema moves, so a field of the
		// wrong type must not be read as "disabled": that would silence a rule
		// on a cluster whose spec this code simply cannot parse.
		{
			name: "a field of the wrong type",
			policies: []unstructured.Unstructured{{Object: map[string]any{
				"metadata": map[string]any{"name": "cluster-policy"},
				"status":   map[string]any{"state": "ready"},
				"spec":     map[string]any{"devicePlugin": map[string]any{"enabled": "yes"}},
			}}},
			field: rules.PolicyFieldDevicePlugin,
			want:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			in := health.Input{ClusterPolicies: tc.policies}

			if got := rules.OperandEnabledInPolicy(in, tc.field); got != tc.want {
				t.Errorf("OperandEnabledInPolicy(%q) = %v, want %v", tc.field, got, tc.want)
			}
		})
	}
}

// The two field names are what the rules pass, so a typo in one of them would
// read a field no ClusterPolicy has and report every operand as enabled.
func TestPolicyFieldNames(t *testing.T) {
	t.Parallel()

	if rules.PolicyFieldDevicePlugin != "devicePlugin" {
		t.Errorf("PolicyFieldDevicePlugin is %q, want devicePlugin", rules.PolicyFieldDevicePlugin)
	}

	if rules.PolicyFieldDCGMExporter != "dcgmExporter" {
		t.Errorf("PolicyFieldDCGMExporter is %q, want dcgmExporter", rules.PolicyFieldDCGMExporter)
	}
}
