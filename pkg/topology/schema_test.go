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
	"slices"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The kind is registered by the package that defines it, at import time. See
// the same test in pkg/health for why it cannot live in the command package.
func TestThisPackageRegistersItsKind(t *testing.T) {
	t.Parallel()

	if kinds := output.RegisteredKinds(); !slices.Contains(kinds, output.KindTopologyReport) {
		t.Errorf("%s is not registered; registered kinds are %v", output.KindTopologyReport, kinds)
	}
}

// populatedTopologyReport sets every field of the kind, the optional fit
// included, so that the schema is checked against a full instance.
func populatedTopologyReport() *TopologyReport {
	r := NewTopologyReport()
	r.Levels = []Level{{
		Name:   "clique",
		Label:  "nvidia.com/gpu.clique",
		Source: SourceClique,
		Groups: []Group{{
			Value:           "clique-a",
			Nodes:           []string{"node-1", "node-2"},
			NodeCount:       2,
			Allocatable:     16,
			Requested:       4,
			Free:            12,
			LargestFreeNode: 8,
			RequestedKnown:  true,
		}},
	}}
	r.PartialNodes = []PartialNode{{
		Name:    "node-3",
		Present: []string{"nvidia.com/gpu.clique"},
		Missing: []string{"accelerator.topograph.run/domain"},
	}}
	r.Kueue = KueueStatus{Installed: true, Reason: "no ResourceFlavor references a Topology"}
	r.Fit = &JobFit{
		Namespace:  "default",
		Name:       "train",
		Pods:       4,
		GPUsPerPod: 2,
		Levels: []LevelFit{{
			Level:  "clique",
			Label:  "nvidia.com/gpu.clique",
			Groups: []string{"clique-a"},
		}},
		Smallest: "clique",
		Excluded: []string{"node-4"},
	}
	r.Status = output.ReportStatus{
		Findings:    []output.Finding{},
		Partial:     true,
		Unreachable: []output.Unreachable{{Resource: "resourceflavors", Reason: "forbidden"}},
	}

	return r
}

func TestAPopulatedTopologyReportSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	if err := output.ValidateAgainstSchema(output.KindTopologyReport, populatedTopologyReport()); err != nil {
		t.Fatal(err)
	}
}

func TestAnEmptyTopologyReportSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	if err := output.ValidateAgainstSchema(output.KindTopologyReport, NewTopologyReport()); err != nil {
		t.Fatal(err)
	}
}
