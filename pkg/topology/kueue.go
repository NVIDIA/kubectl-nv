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

package topology

import (
	"errors"
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// ParseTopology reduces a Kueue Topology object to its name and the node
// labels of its levels, in the order the object declares them.
//
// The object is read as unstructured, through the two fields both served
// versions share: metadata.name and spec.levels[].nodeLabel. v1beta1 and
// v1beta2 differ in validation and in what else they carry, not in these, so
// one reader serves both and this package needs no Kueue dependency.
//
// A Topology with no levels is refused. The CRD makes the field required, so
// an object without it is not something the cluster is scheduling against,
// and reading it as a topology with nothing in it would bury that.
func ParseTopology(obj unstructured.Unstructured) (KueueTopology, error) {
	name := obj.GetName()

	raw, found, err := unstructured.NestedSlice(obj.Object, "spec", "levels")
	if err != nil {
		return KueueTopology{}, fmt.Errorf("reading spec.levels of Topology %q: %w", name, err)
	}
	if !found || len(raw) == 0 {
		return KueueTopology{}, fmt.Errorf("the Topology %q declares no spec.levels", name)
	}

	levels := make([]string, 0, len(raw))
	for _, item := range raw {
		level, ok := item.(map[string]any)
		if !ok {
			return KueueTopology{}, fmt.Errorf("the Topology %q has a level that is not an object", name)
		}
		// A level with no nodeLabel is skipped rather than refused: it groups
		// nothing, and a group keyed by the empty string would hold every
		// node in the cluster.
		label, _, err := unstructured.NestedString(level, "nodeLabel")
		if err != nil {
			return KueueTopology{}, fmt.Errorf("reading a nodeLabel of Topology %q: %w", name, err)
		}
		if label == "" {
			continue
		}
		levels = append(levels, label)
	}

	if len(levels) == 0 {
		return KueueTopology{}, fmt.Errorf("the Topology %q declares no level with a nodeLabel", name)
	}

	return KueueTopology{Name: name, Levels: levels}, nil
}

// ResourceFlavorTopology returns the Topology a ResourceFlavor references,
// and the empty string for a flavor that is not topology aware.
func ResourceFlavorTopology(obj unstructured.Unstructured) string {
	name, _, err := unstructured.NestedString(obj.Object, "spec", "topologyName")
	if err != nil {
		return ""
	}

	return name
}

// ReferencedTopologies returns the topologies at least one ResourceFlavor
// names, sorted by name.
//
// The flavors are what decides. A Topology object nobody references declares
// a division of the cluster that nothing schedules against, so reporting its
// levels would offer a reader a placement the scheduler will not make.
//
// A malformed object costs its own levels and nothing else: the ones that
// parsed come back alongside an error naming the ones that did not, so a
// single broken Topology does not take the rest of the report with it.
func ReferencedTopologies(topologies, flavors []unstructured.Unstructured) ([]KueueTopology, error) {
	wanted := make(map[string]struct{}, len(flavors))
	for i := range flavors {
		if name := ResourceFlavorTopology(flavors[i]); name != "" {
			wanted[name] = struct{}{}
		}
	}

	var problems []error
	out := make([]KueueTopology, 0, len(wanted))
	for i := range topologies {
		if _, referenced := wanted[topologies[i].GetName()]; !referenced {
			continue
		}

		parsed, err := ParseTopology(topologies[i])
		if err != nil {
			problems = append(problems, err)
			continue
		}
		out = append(out, parsed)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	return out, errors.Join(problems...)
}
