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

package inventory

import (
	"reflect"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// headers reads the column titles off a kind that renders as a table.
func headers(rows output.TableRows, wide bool) []string {
	columns := rows.TableColumns(wide)
	out := make([]string, 0, len(columns))

	for _, c := range columns {
		out = append(out, c.Header)
	}

	return out
}

func TestGPUNodeListColumns(t *testing.T) {
	t.Parallel()

	list := NewGPUNodeList()

	want := []string{"NODE", "PRODUCT", "GPUS", "DRIVER", "CUDA", "MODE", "MIG", "UPGRADE", "STATUS"}
	if got := headers(list, false); !reflect.DeepEqual(got, want) {
		t.Errorf("columns = %v, want %v", got, want)
	}
}

// TestGPUNodeListWideColumns pins the wide table: the clique plus one column
// per topology key the listed nodes actually carry, which is why the columns
// are a function of the items rather than a constant.
func TestGPUNodeListWideColumns(t *testing.T) {
	t.Parallel()

	list := NewGPUNodeList()

	first := NewGPUNode("n1")
	first.Topology = map[string]string{"fabric.topograph.run/tier-1": "spine-a"}

	second := NewGPUNode("n2")
	second.Topology = map[string]string{
		"fabric.topograph.run/tier-0":      "switch-a",
		"accelerator.topograph.run/domain": "domain-1",
	}

	list.Items = append(list.Items, *first, *second)

	want := []string{
		"NODE", "PRODUCT", "GPUS", "DRIVER", "CUDA", "MODE", "MIG", "UPGRADE", "STATUS", "CLIQUE",
		"ACCELERATOR.TOPOGRAPH.RUN/DOMAIN",
		"FABRIC.TOPOGRAPH.RUN/TIER-0",
		"FABRIC.TOPOGRAPH.RUN/TIER-1",
	}
	if got := headers(list, true); !reflect.DeepEqual(got, want) {
		t.Errorf("wide columns = %v, want %v", got, want)
	}

	rows := list.TableRows(true)
	if len(rows) != 2 {
		t.Fatalf("rows = %#v, want one per node", rows)
	}

	for i, row := range rows {
		if len(row) != len(want) {
			t.Errorf("row %d has %d cells, want %d", i, len(row), len(want))
		}
	}

	// n1 carries tier-1 and nothing else, so the two columns it does not have
	// are empty rather than shifted.
	if got := rows[0][len(want)-1]; got != "spine-a" {
		t.Errorf("n1 tier-1 cell = %q, want spine-a", got)
	}

	if got := rows[0][len(want)-2]; got != "" {
		t.Errorf("n1 tier-0 cell = %q, want it empty", got)
	}
}

// TestGPUNodeListRowsWithoutTopologyStayNarrow pins that the wide table does
// not grow columns a cluster has no labels for.
func TestGPUNodeListRowsWithoutTopologyStayNarrow(t *testing.T) {
	t.Parallel()

	list := NewGPUNodeList()
	list.Items = append(list.Items, *NewGPUNode("n1"))

	want := []string{"NODE", "PRODUCT", "GPUS", "DRIVER", "CUDA", "MODE", "MIG", "UPGRADE", "STATUS", "CLIQUE"}
	if got := headers(list, true); !reflect.DeepEqual(got, want) {
		t.Errorf("wide columns = %v, want %v", got, want)
	}
}

func TestGPUNodeRow(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		node func() *GPUNode
		want []string
	}{
		{
			name: "a healthy device plugin node",
			node: func() *GPUNode {
				n := NewGPUNode("worker-1")
				n.Product = "NVIDIA-T4"
				// Three different numbers on purpose: with capacity and
				// allocatable equal, a cell that printed one where the other
				// belongs would read correctly.
				n.Capacity, n.Allocatable = 8, 6
				n.Requested, n.RequestedKnown = 1, true
				n.DriverVersion, n.CUDAVersion = "550.107.02", "12.5"
				n.Mode = ModeDevicePlugin
				n.Ready = true

				return n
			},
			want: []string{"worker-1", "NVIDIA-T4", "1/6/8", "550.107.02", "12.5", "device-plugin", "", "", "Ready"},
		},
		{
			name: "requested is unknown under --no-pods",
			node: func() *GPUNode {
				n := NewGPUNode("worker-1")
				n.Capacity, n.Allocatable = 8, 5
				n.Mode = ModeDRA
				n.Ready = true

				return n
			},
			want: []string{"worker-1", "", "?/5/8", "", "", "dra", "", "", "Ready"},
		},
		{
			name: "a cordoned node reads like kubectl",
			node: func() *GPUNode {
				n := NewGPUNode("worker-1")
				n.Ready = true
				n.Unschedulable = true
				n.RequestedKnown = true

				return n
			},
			want: []string{"worker-1", "", "0/0/0", "", "", "", "", "", "Ready,SchedulingDisabled"},
		},
		{
			name: "a node that is not ready",
			node: func() *GPUNode {
				n := NewGPUNode("worker-1")
				n.RequestedKnown = true

				return n
			},
			want: []string{"worker-1", "", "0/0/0", "", "", "", "", "", "NotReady"},
		},
		{
			name: "MIG and the upgrade state",
			node: func() *GPUNode {
				n := NewGPUNode("worker-1")
				n.Ready = true
				n.RequestedKnown = true
				n.MIG = MIG{Strategy: "single", Capable: true}
				n.UpgradeState = "upgrade-failed"

				return n
			},
			want: []string{"worker-1", "", "0/0/0", "", "", "", "single", "upgrade-failed", "Ready"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rows := tt.node().TableRows(false)
			if len(rows) != 1 {
				t.Fatalf("rows = %#v, want exactly one", rows)
			}

			if !reflect.DeepEqual(rows[0], tt.want) {
				t.Errorf("row = %v, want %v", rows[0], tt.want)
			}
		})
	}
}

// TestColumnsCarryTheirField pins that every column names the JSON path it
// renders. --fields keeps a column by that path, and a column that leaves it
// empty is dropped the moment --fields is given, so an unset Field is an
// invisible way to lose a column.
func TestColumnsCarryTheirField(t *testing.T) {
	t.Parallel()

	list := NewGPUNodeList()

	node := NewGPUNode("n1")
	node.Topology = map[string]string{"fabric.topograph.run/tier-0": "switch-a"}
	list.Items = append(list.Items, *node)

	want := map[string]string{
		"NODE":                        "metadata.name",
		"PRODUCT":                     "product",
		"GPUS":                        "capacity",
		"DRIVER":                      "driverVersion",
		"CUDA":                        "cudaVersion",
		"MODE":                        "mode",
		"MIG":                         "mig",
		"UPGRADE":                     "upgradeState",
		"STATUS":                      "ready",
		"CLIQUE":                      "clique",
		"FABRIC.TOPOGRAPH.RUN/TIER-0": "topology",
	}

	for _, wide := range []bool{false, true} {
		for _, c := range list.TableColumns(wide) {
			expected, known := want[c.Header]
			if !known {
				t.Errorf("column %q is not in the expected set", c.Header)

				continue
			}

			if c.Field != expected {
				t.Errorf("column %q has field %q, want %q", c.Header, c.Field, expected)
			}
		}
	}
}

// TestColumnFieldsArePathsOfTheKind pins that each Field is a path --fields
// actually accepts. A typo here produces a column nothing can keep, and the
// table would quietly lose it under --fields with no error anywhere.
func TestColumnFieldsArePathsOfTheKind(t *testing.T) {
	t.Parallel()

	list := NewGPUNodeList()
	node := NewGPUNode("n1")
	node.Topology = map[string]string{"fabric.topograph.run/tier-0": "switch-a"}
	list.Items = append(list.Items, *node)

	paths, err := output.FieldPaths(list)
	if err != nil {
		t.Fatalf("FieldPaths: %v", err)
	}

	valid := make(map[string]bool, len(paths))
	for _, p := range paths {
		valid[p] = true
	}

	for _, c := range list.TableColumns(true) {
		if c.Field == "" {
			t.Errorf("column %q names no field", c.Header)

			continue
		}

		if !valid[c.Field] {
			t.Errorf("column %q names field %q, which is not a path of this kind: %v", c.Header, c.Field, paths)
		}
	}
}

// TestGPUNodeSingleColumnsCarryTheirField pins the same for one node, which is
// the shape gpu info prints at -o wide.
func TestGPUNodeSingleColumnsCarryTheirField(t *testing.T) {
	t.Parallel()

	for _, c := range NewGPUNode("n1").TableColumns(false) {
		if c.Field == "" {
			t.Errorf("column %q names no field", c.Header)
		}
	}
}
