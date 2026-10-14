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
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// requestedUnknown is what the GPUS column shows where the requested count
// belongs when the pods were not listed. Printing 0 there would be a claim
// nobody made.
const requestedUnknown = "?"

// baseColumns are the columns of the default table, in order, each with the
// dotted JSON path it renders.
//
// The paths are what --fields keeps a column by, so every column names every
// path its cell renders: a column with none is dropped the moment --fields is
// given, with no error anywhere to say it happened, and a column naming fewer
// paths than it prints would show a caller a number it did not ask for.
//
// Two of them render more than one. GPUS shows requested out of allocatable
// out of capacity, and STATUS shows the Ready condition and the cordon. Each
// is kept only when every path it names was asked for, because a table has
// nowhere to put half a cell.
var baseColumns = []output.Column{
	output.OneField("NODE", "metadata.name"),
	output.OneField("PRODUCT", "product"),
	{Header: "GPUS", Fields: []string{"requested", "allocatable", "capacity"}},
	output.OneField("DRIVER", "driverVersion"),
	output.OneField("CUDA", "cudaVersion"),
	output.OneField("MODE", "mode"),
	output.OneField("MIG", "mig.strategy"),
	output.OneField("UPGRADE", "upgradeState"),
	{Header: "STATUS", Fields: []string{"ready", "unschedulable"}},
}

// cliqueColumn and the topology columns are the wide ones. Every topology
// column renders one key of the same map, so they share its path.
var cliqueColumn = output.OneField("CLIQUE", "clique")

// TableColumns implements output.TableRows.
func (n *GPUNode) TableColumns(wide bool) []output.Column {
	return columnsFor(wide, sortedKeys(n.Topology))
}

// TableRows implements output.TableRows.
func (n *GPUNode) TableRows(wide bool) [][]string {
	return [][]string{n.row(wide, sortedKeys(n.Topology))}
}

// TableColumns implements output.TableRows. The wide table grows one column
// per topology key the listed nodes carry, so the columns are a function of
// the items: a cluster with no fabric labels gets no empty fabric columns.
func (l *GPUNodeList) TableColumns(wide bool) []output.Column {
	return columnsFor(wide, l.topologyKeys())
}

// TableRows implements output.TableRows.
func (l *GPUNodeList) TableRows(wide bool) [][]string {
	keys := l.topologyKeys()
	rows := make([][]string, 0, len(l.Items))

	for i := range l.Items {
		rows = append(rows, l.Items[i].row(wide, keys))
	}

	return rows
}

// topologyKeys is every topology label any listed node carries, sorted, so
// that two runs over the same cluster produce the same columns.
func (l *GPUNodeList) topologyKeys() []string {
	seen := make(map[string]struct{})

	for i := range l.Items {
		for key := range l.Items[i].Topology {
			seen[key] = struct{}{}
		}
	}

	out := make([]string, 0, len(seen))
	for key := range seen {
		out = append(out, key)
	}

	sort.Strings(out)

	return out
}

// columnsFor builds the header row.
func columnsFor(wide bool, topologyKeys []string) []output.Column {
	out := append([]output.Column{}, baseColumns...)
	if !wide {
		return out
	}

	out = append(out, cliqueColumn)
	for _, key := range topologyKeys {
		out = append(out, output.OneField(strings.ToUpper(key), "topology"))
	}

	return out
}

// row renders one node. topologyKeys fixes the wide columns, so that every row
// of a list has a cell for every key any node in it carries and a node without
// one gets an empty cell rather than a shifted row.
func (n *GPUNode) row(wide bool, topologyKeys []string) []string {
	row := []string{
		n.Name,
		n.Product,
		n.gpus(),
		n.DriverVersion,
		n.CUDAVersion,
		n.Mode,
		n.MIG.Strategy,
		n.UpgradeState,
		n.status(),
	}

	if !wide {
		return row
	}

	row = append(row, n.Clique)
	for _, key := range topologyKeys {
		row = append(row, n.Topology[key])
	}

	return row
}

// gpus is the requested, allocatable and capacity triple.
func (n *GPUNode) gpus() string {
	requested := requestedUnknown
	if n.RequestedKnown {
		requested = strconv.FormatInt(n.Requested, 10)
	}

	return requested + "/" +
		strconv.FormatInt(n.Allocatable, 10) + "/" +
		strconv.FormatInt(n.Capacity, 10)
}

// status reads like kubectl's own STATUS column, for the same reason: someone
// reading both at once should not have to learn two vocabularies.
func (n *GPUNode) status() string {
	out := "NotReady"
	if n.Ready {
		out = "Ready"
	}

	if n.Unschedulable {
		out += ",SchedulingDisabled"
	}

	return out
}
