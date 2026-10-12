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

package list_test

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/list"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The three output flags are registered together by pkg/output, so every
// command spells them the same way and a program can pass the same bounds to
// all of them.
func TestListRegistersTheOutputFlags(t *testing.T) {
	t.Parallel()

	c := list.NewCommand(&cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()})

	for _, name := range []string{"output", "fields", "limit"} {
		if c.Flags().Lookup(name) == nil {
			t.Errorf("list has no --%s", name)
		}
	}
}

// columnsCluster is one node with enough on it that every base column has
// something to print.
func columnsCluster() []runtime.Object {
	return []runtime.Object{
		gpuNodeWithAllocatable("node-1", 8, 7, map[string]string{
			"nvidia.com/gpu.product":              "NVIDIA-H100-80GB-HBM3",
			"nvidia.com/gpu.count":                "8",
			"nvidia.com/cuda.driver-version.full": "550.107.02",
			"nvidia.com/mig.strategy":             "none",
		}),
		gpuPod("trainer", "research", "node-1", 2),
	}
}

// hasColumn reports whether a header holds a column.
func hasColumn(header []string, name string) bool {
	for _, got := range header {
		if got == name {
			return true
		}
	}

	return false
}

// headerOf runs the command and returns the column headers it printed.
func headerOf(t *testing.T, args ...string) []string {
	t.Helper()

	s, err := runList(t, columnsCluster(), args...)
	if err != nil {
		t.Fatalf("kubectl nv list %v: %v", args, err)
	}

	return strings.Fields(strings.Split(strings.TrimSpace(s.out.String()), "\n")[0])
}

// The GPUS cell renders three numbers: requested, allocatable and capacity. A
// column could name only one path, so --fields capacity kept it and printed
// the other two, which the caller did not ask for.
//
// A column is kept when every path it renders was asked for, so the table
// cannot show a number nobody named and cannot print a cell with a hole in it.
func TestTheGPUSColumnNamesEveryNumberItPrints(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		fields string
		kept   bool
	}{
		{name: "one of the three", fields: "capacity", kept: false},
		{name: "two of the three", fields: "capacity,allocatable", kept: false},
		{name: "all three", fields: "capacity,allocatable,requested", kept: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			header := headerOf(t, "--fields", tc.fields)
			if got := hasColumn(header, "GPUS"); got != tc.kept {
				t.Errorf("--fields %s keeps GPUS: %t, want %t; the header is %v",
					tc.fields, got, tc.kept, header)
			}
			// NODE is kept whatever is asked for: without it a reader cannot
			// tell which node a row is about.
			if !hasColumn(header, "NODE") {
				t.Errorf("--fields %s dropped NODE; the header is %v", tc.fields, header)
			}
		})
	}
}

// The same holds for STATUS, which renders the Ready condition and the cordon.
func TestTheStatusColumnNamesBothOfItsPaths(t *testing.T) {
	t.Parallel()

	if header := headerOf(t, "--fields", "ready"); hasColumn(header, "STATUS") {
		t.Errorf("--fields ready keeps STATUS, which also prints unschedulable; the header is %v", header)
	}
	if header := headerOf(t, "--fields", "ready,unschedulable"); !hasColumn(header, "STATUS") {
		t.Errorf("--fields ready,unschedulable dropped STATUS; the header is %v", header)
	}
}

// A column naming one path is kept by that one path, which is most of them.
func TestASingleFieldColumnIsKeptByItsPath(t *testing.T) {
	t.Parallel()

	header := headerOf(t, "--fields", "product")
	if !hasColumn(header, "PRODUCT") {
		t.Errorf("--fields product dropped PRODUCT; the header is %v", header)
	}
	if hasColumn(header, "DRIVER") {
		t.Errorf("--fields product kept DRIVER; the header is %v", header)
	}
}

// Every column names at least one path. A column with none cannot be asked for
// and is dropped the moment --fields is given, with nothing anywhere to say it
// happened.
func TestEveryColumnNamesAPath(t *testing.T) {
	t.Parallel()

	list := inventory.NewGPUNodeList()
	list.Items = append(list.Items, inventory.GPUNode{
		Topology: map[string]string{"fabric.topograph.run/tier-1": "t1"},
	})

	valid, err := output.FieldPaths(list)
	if err != nil {
		t.Fatalf("reading the paths of a GPU node list: %v", err)
	}
	known := make(map[string]bool, len(valid))
	for _, path := range valid {
		known[path] = true
	}

	for _, wide := range []bool{false, true} {
		for _, column := range list.TableColumns(wide) {
			if len(column.Fields) == 0 {
				t.Errorf("the %s column names no path", column.Header)

				continue
			}
			for _, path := range column.Fields {
				if !known[path] {
					t.Errorf("the %s column names %q, which the kind does not offer", column.Header, path)
				}
			}
		}
	}
}
