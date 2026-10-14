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

package output_test

import (
	"slices"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// The columns of each kind, written out, header by header and path by path.
//
// A test that compared a column against the kind's own field list only says
// the path exists; it does not say the column renders that path and no other,
// which is the thing --fields depends on. A cell showing a number its column
// does not name is a number printed to a caller who asked for something else,
// and that is how the GPUS column came to show three.
//
// So these are literals. Changing a cell means changing the line below it, and
// the diff is where a reviewer sees that a column started rendering something
// new.
func TestEveryColumnNamesExactlyThePathsItRenders(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		kind  string
		table output.TableRows
		wide  bool
		want  map[string][]string
	}{
		{
			kind:  output.KindGPUNodeList,
			table: inventory.NewGPUNodeList(),
			want: map[string][]string{
				"NODE":    {"metadata.name"},
				"PRODUCT": {"product"},
				"GPUS":    {"requested", "allocatable", "capacity"},
				"DRIVER":  {"driverVersion"},
				"CUDA":    {"cudaVersion"},
				"MODE":    {"mode"},
				"MIG":     {"mig.strategy"},
				"UPGRADE": {"upgradeState"},
				"STATUS":  {"ready", "unschedulable"},
			},
		},
		{
			kind:  output.KindGPUNodeList + " (wide)",
			table: inventory.NewGPUNodeList(),
			wide:  true,
			want: map[string][]string{
				"NODE":    {"metadata.name"},
				"PRODUCT": {"product"},
				"GPUS":    {"requested", "allocatable", "capacity"},
				"DRIVER":  {"driverVersion"},
				"CUDA":    {"cudaVersion"},
				"MODE":    {"mode"},
				"MIG":     {"mig.strategy"},
				"UPGRADE": {"upgradeState"},
				"STATUS":  {"ready", "unschedulable"},
				"CLIQUE":  {"clique"},
			},
		},
		{
			kind:  output.KindHealthReport,
			table: health.NewHealthReport(),
			want: map[string][]string{
				"NODE":      {"findings.node"},
				"CODE":      {"findings.code"},
				"SEVERITY":  {"findings.severity"},
				"COMPONENT": {"findings.component"},
				"MESSAGE":   {"findings.message"},
			},
		},
		{
			kind:  output.KindHealthReport + " (wide)",
			table: health.NewHealthReport(),
			wide:  true,
			want: map[string][]string{
				"NODE":      {"findings.node"},
				"CODE":      {"findings.code"},
				"SEVERITY":  {"findings.severity"},
				"COMPONENT": {"findings.component"},
				"MESSAGE":   {"findings.message"},
				"NEXT":      {"findings.next"},
			},
		},
		{
			kind:  output.KindTopologyReport,
			table: topology.NewTopologyReport(),
			want: map[string][]string{
				"LEVEL":       {"levels.name"},
				"GROUP":       {"levels.groups.value"},
				"NODES":       {"levels.groups.nodeCount"},
				"ALLOCATABLE": {"levels.groups.allocatable"},
				"REQUESTED":   {"levels.groups.requested", "levels.groups.requestedKnown"},
				"FREE":        {"levels.groups.free", "levels.groups.requestedKnown"},
				"LARGEST-FREE": {
					"levels.groups.largestFreeNode", "levels.groups.requestedKnown",
				},
			},
		},
		{
			kind:  output.KindTopologyReport + " (wide)",
			table: topology.NewTopologyReport(),
			wide:  true,
			want: map[string][]string{
				"LEVEL":       {"levels.name"},
				"GROUP":       {"levels.groups.value"},
				"NODES":       {"levels.groups.nodeCount"},
				"ALLOCATABLE": {"levels.groups.allocatable"},
				"REQUESTED":   {"levels.groups.requested", "levels.groups.requestedKnown"},
				"FREE":        {"levels.groups.free", "levels.groups.requestedKnown"},
				"LARGEST-FREE": {
					"levels.groups.largestFreeNode", "levels.groups.requestedKnown",
				},
				"NODE-NAMES": {"levels.groups.nodes"},
			},
		},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			t.Parallel()

			got := map[string][]string{}
			for _, column := range tc.table.TableColumns(tc.wide) {
				got[column.Header] = column.Fields
			}

			for header, want := range tc.want {
				if !slices.Equal(got[header], want) {
					t.Errorf("the %s column names %v, want %v", header, got[header], want)
				}
			}
			for header := range got {
				if _, expected := tc.want[header]; !expected {
					t.Errorf("there is a %s column, which this table does not list", header)
				}
			}
		})
	}
}
