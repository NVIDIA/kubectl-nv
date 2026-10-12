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
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	cmdtopology "github.com/NVIDIA/kubectl-nv/pkg/cmd/topology"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// A Level and a Group marshal themselves, to guarantee no nulls. The path
// walker used to treat anything that does so as a leaf, so the only path this
// kind offered for the whole of its levels was "levels": a caller could ask
// for every number of every group or for none of them.
func TestTopologyOffersThePathsInsideItsLevels(t *testing.T) {
	t.Parallel()

	paths, err := output.FieldPaths(topology.NewTopologyReport())
	if err != nil {
		t.Fatalf("reading the paths of a topology report: %v", err)
	}

	offered := map[string]bool{}
	for _, path := range paths {
		offered[path] = true
	}

	for _, want := range []string{
		"levels.name",
		"levels.groups.value",
		"levels.groups.allocatable",
		"levels.groups.free",
		"levels.groups.largestFreeNode",
	} {
		if !offered[want] {
			t.Errorf("the kind does not offer %q; it offers %v", want, paths)
		}
	}
}

// Asking for one number inside a group has to return that number, not the
// whole group and not nothing. The projection walks arrays for it: a path into
// levels passes through two of them.
func TestTopologyProjectsInsideItsLevels(t *testing.T) {
	raw, err := run(t, []string{"-o", "json", "--fields", "levels.groups.allocatable"},
		gpuNode("node-a", "clique-a", 4),
		gpuNode("node-b", "clique-a", 4),
	)
	if err != nil {
		t.Fatalf("topology --fields levels.groups.allocatable: %v", err)
	}

	var report struct {
		Levels []map[string]any `json:"levels"`
	}
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("decoding the projected report: %v\n%s", err, raw)
	}
	if len(report.Levels) == 0 {
		t.Fatalf("the projection dropped the levels entirely:\n%s", raw)
	}

	for i, level := range report.Levels {
		if _, kept := level["name"]; kept {
			t.Errorf("level %d kept name, which was not asked for: %v", i, level)
		}
		groups, ok := level["groups"].([]any)
		if !ok {
			t.Fatalf("level %d has no groups array: %v", i, level)
		}
		for j, entry := range groups {
			group, ok := entry.(map[string]any)
			if !ok {
				t.Fatalf("group %d of level %d is %T, want an object", j, i, entry)
			}
			if _, kept := group["allocatable"]; !kept {
				t.Errorf("group %d of level %d dropped allocatable, which was asked for: %v", j, i, group)
			}
			if _, kept := group["value"]; kept {
				t.Errorf("group %d of level %d kept value, which was not asked for: %v", j, i, group)
			}
		}
	}
}

// A column renders what it renders. ALLOCATABLE is one number of a group, so
// asking for that one number keeps that one column; the columns rendering the
// other numbers go, because the caller said which it wanted.
func TestTopologyTableKeepsTheColumnsThatWereAskedFor(t *testing.T) {
	raw, err := run(t, []string{"--fields", "levels.groups.allocatable"},
		gpuNode("node-a", "clique-a", 4))
	if err != nil {
		t.Fatalf("topology --fields levels.groups.allocatable: %v", err)
	}

	header := strings.Fields(strings.Split(strings.TrimSpace(raw), "\n")[0])
	want := []string{"ALLOCATABLE"}
	if len(header) != len(want) || header[0] != want[0] {
		t.Errorf("the header is %v, want %v:\n%s", header, want, raw)
	}
}

// A path the kind does not have is still exit 2, and the message still lists
// what there is. A walker that went deeper without the vocabulary following it
// would accept a path nothing projects.
func TestTopologyStillRefusesAnUnknownNestedField(t *testing.T) {
	t.Parallel()

	c := cmdtopology.NewCommand(&cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()})
	if c.Flags().Lookup("fields") == nil {
		t.Fatal("topology has no --fields")
	}

	_, err := run(t, []string{"--fields", "levels.groups.nonesuch"}, gpuNode("node-a", "clique-a", 4))

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
	if !strings.Contains(err.Error(), "levels.groups.allocatable") {
		t.Errorf("the error does not list the nested paths: %v", err)
	}
}
