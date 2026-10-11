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

package topology_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// kueueTopology builds a Topology object as the API server serves it. Both
// versions Kueue serves carry the same spec.levels[].nodeLabel, so the
// version is a parameter and the parser is asked for both.
func kueueTopology(version, name string, labels ...string) unstructured.Unstructured {
	levels := make([]any, 0, len(labels))
	for _, label := range labels {
		levels = append(levels, map[string]any{"nodeLabel": label})
	}

	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kueue.x-k8s.io/" + version,
		"kind":       "Topology",
		"metadata":   map[string]any{"name": name},
		"spec":       map[string]any{"levels": levels},
	}}
}

// kueueFlavor builds a ResourceFlavor. An empty topologyName leaves the field
// out, which is what a flavor that is not topology aware looks like.
func kueueFlavor(version, name, topologyName string) unstructured.Unstructured {
	spec := map[string]any{"nodeLabels": map[string]any{"nvidia.com/gpu.present": "true"}}
	if topologyName != "" {
		spec["topologyName"] = topologyName
	}

	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kueue.x-k8s.io/" + version,
		"kind":       "ResourceFlavor",
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}

// TestParseTopologyReadsEveryServedVersion covers the three versions of the
// Topology object that are in the field: v1alpha1 up to Kueue v0.13, v1beta1
// from v0.14 (unserved again at v0.20), and v1beta2 from v0.15.
//
// All three carry the same spec.levels[].nodeLabel, verified against the
// CRDs at v0.13.0, v0.19.0 and v0.20.1, so one reader serves them all.
func TestParseTopologyReadsEveryServedVersion(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"v1alpha1", "v1beta1", "v1beta2"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()

			got, err := topology.ParseTopology(kueueTopology(version, "default",
				"cloud.example.com/topology-block",
				"cloud.example.com/topology-rack",
				"kubernetes.io/hostname",
			))
			if err != nil {
				t.Fatalf("parsing a %s Topology: %v", version, err)
			}

			if got.Name != "default" {
				t.Errorf("name is %q, want %q", got.Name, "default")
			}

			want := []string{
				"cloud.example.com/topology-block",
				"cloud.example.com/topology-rack",
				"kubernetes.io/hostname",
			}
			if !equalStrings(got.Levels, want) {
				t.Errorf("levels are %v, want %v in the declared order", got.Levels, want)
			}
		})
	}
}

// TestParseTopologyRejectsAnObjectWithNoLevels: the CRD makes levels
// required, so an object without them is not a Topology this command can
// group by, and reading it as a level-less one would hide the problem.
func TestParseTopologyRejectsAnObjectWithNoLevels(t *testing.T) {
	t.Parallel()

	empty := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta2",
		"kind":       "Topology",
		"metadata":   map[string]any{"name": "broken"},
		"spec":       map[string]any{},
	}}

	if _, err := topology.ParseTopology(empty); err == nil {
		t.Error("parsing a Topology with no levels returned no error")
	}
}

// TestParseTopologySkipsALevelWithNoNodeLabel keeps a half-written level from
// becoming a group keyed by the empty string, which every node would join.
func TestParseTopologySkipsALevelWithNoNodeLabel(t *testing.T) {
	t.Parallel()

	obj := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta2",
		"kind":       "Topology",
		"metadata":   map[string]any{"name": "default"},
		"spec": map[string]any{"levels": []any{
			map[string]any{"nodeLabel": "cloud.example.com/topology-rack"},
			map[string]any{},
		}},
	}}

	got, err := topology.ParseTopology(obj)
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	if want := []string{"cloud.example.com/topology-rack"}; !equalStrings(got.Levels, want) {
		t.Errorf("levels are %v, want %v", got.Levels, want)
	}
}

// TestReferencedTopologiesFollowTheFlavors: only a Topology some
// ResourceFlavor names is a level the scheduler places against, so an
// unreferenced one is not reported as a level of the cluster.
func TestReferencedTopologiesFollowTheFlavors(t *testing.T) {
	t.Parallel()

	topologies := []unstructured.Unstructured{
		kueueTopology("v1beta2", "unused", "cloud.example.com/topology-zone"),
		kueueTopology("v1beta2", "gpu", "cloud.example.com/topology-block"),
		kueueTopology("v1beta1", "also-gpu", "cloud.example.com/topology-rack"),
	}
	flavors := []unstructured.Unstructured{
		kueueFlavor("v1beta2", "plain", ""),
		kueueFlavor("v1beta2", "tas-gpu", "gpu"),
		kueueFlavor("v1beta1", "tas-gpu-old", "also-gpu"),
	}

	got, err := topology.ReferencedTopologies(topologies, flavors)
	if err != nil {
		t.Fatalf("resolving the referenced topologies: %v", err)
	}

	names := make([]string, 0, len(got))
	for _, tp := range got {
		names = append(names, tp.Name)
	}

	// Sorted by name, so two runs over the same cluster print the same bytes.
	if want := []string{"also-gpu", "gpu"}; !equalStrings(names, want) {
		t.Errorf("referenced topologies are %v, want %v", names, want)
	}
}

// TestReferencedTopologiesReportsAMalformedObjectAndKeepsTheRest: one broken
// Topology must not cost the user the levels of the others.
func TestReferencedTopologiesReportsAMalformedObjectAndKeepsTheRest(t *testing.T) {
	t.Parallel()

	broken := unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kueue.x-k8s.io/v1beta2",
		"kind":       "Topology",
		"metadata":   map[string]any{"name": "broken"},
		"spec":       map[string]any{"levels": "not-a-list"},
	}}
	topologies := []unstructured.Unstructured{
		broken,
		kueueTopology("v1beta2", "gpu", "cloud.example.com/topology-block"),
	}
	flavors := []unstructured.Unstructured{
		kueueFlavor("v1beta2", "tas-broken", "broken"),
		kueueFlavor("v1beta2", "tas-gpu", "gpu"),
	}

	got, err := topology.ReferencedTopologies(topologies, flavors)
	if err == nil {
		t.Error("a malformed Topology was accepted without a word")
	}
	if len(got) != 1 || got[0].Name != "gpu" {
		t.Errorf("resolved %+v, want only the Topology named gpu", got)
	}
}
