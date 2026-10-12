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
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	cmdtopology "github.com/NVIDIA/kubectl-nv/pkg/cmd/topology"
)

// The flag is on the command itself, registered by pkg/output along with -o
// and --fields, so that every command spells it the same way.
func TestTopologyRegistersTheLimitFlag(t *testing.T) {
	t.Parallel()

	c := cmdtopology.NewCommand(&cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()})

	flag := c.Flags().Lookup("limit")
	if flag == nil {
		t.Fatal("topology has no --limit")
	}
	if flag.DefValue != "0" {
		t.Errorf("--limit defaults to %q, want 0, which prints every group", flag.DefValue)
	}
}

// limitedTopology is the shape --limit changes: the groups of each level, and
// the count of the ones left out.
type limitedTopology struct {
	Levels []struct {
		Name   string `json:"name"`
		Groups []struct {
			Value string `json:"value"`
		} `json:"groups"`
	} `json:"levels"`
	Status struct {
		RemainingItemCount *int64 `json:"remainingItemCount"`
	} `json:"status"`
}

// groupCount is how many groups a report carries in all, across its levels.
func groupCount(report limitedTopology) int {
	n := 0
	for _, level := range report.Levels {
		n += len(level.Groups)
	}

	return n
}

// decodeLimited reads the report the command printed.
func decodeLimited(t *testing.T, raw string) limitedTopology {
	t.Helper()

	var report limitedTopology
	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("decoding the report: %v\n%s", err, raw)
	}

	return report
}

// A topology report is a list of groups spread over levels, and the groups are
// what a caller reads it for. --limit bounds them, counted across the levels in
// the order the report carries them: the narrowest level first, which is where
// the answer to "where does this job fit" is read from.
func TestTopologyLimitsTheGroups(t *testing.T) {
	cluster := []runtime.Object{
		gpuNode("node-a", "clique-a", 4),
		gpuNode("node-b", "clique-b", 4),
		gpuNode("node-c", "clique-c", 4),
	}

	whole, err := run(t, []string{"-o", "json"}, cluster...)
	if err != nil {
		t.Fatalf("topology -o json: %v", err)
	}
	full := decodeLimited(t, whole)
	if groupCount(full) < 3 {
		t.Fatalf("the fixture produced %d groups: a limit of 2 would prove nothing", groupCount(full))
	}

	raw, err := run(t, []string{"-o", "json", "--limit", "2"}, cluster...)
	if err != nil {
		t.Fatalf("topology -o json --limit 2: %v", err)
	}
	limited := decodeLimited(t, raw)

	if got := groupCount(limited); got != 2 {
		t.Errorf("--limit 2 printed %d groups", got)
	}
	if limited.Status.RemainingItemCount == nil {
		t.Fatalf("the truncated report carries no remainingItemCount\n%s", raw)
	}
	if want := int64(groupCount(full) - 2); *limited.Status.RemainingItemCount != want {
		t.Errorf("remainingItemCount is %d, want %d", *limited.Status.RemainingItemCount, want)
	}

	// The groups kept are the first ones of the whole report, in its own order.
	kept := flatGroups(limited)
	all := flatGroups(full)
	for i := range kept {
		if kept[i] != all[i] {
			t.Errorf("group %d is %q, the unlimited report has %q", i, kept[i], all[i])
		}
	}
}

// A report nothing was left out of says nothing about truncation.
func TestTopologyReportsNoTruncationWhenThereIsNone(t *testing.T) {
	raw, err := run(t, []string{"-o", "json", "--limit", "500"},
		gpuNode("node-a", "clique-a", 4))
	if err != nil {
		t.Fatalf("topology -o json --limit 500: %v", err)
	}

	if count := decodeLimited(t, raw).Status.RemainingItemCount; count != nil {
		t.Errorf("remainingItemCount is %d on a report nothing was left out of", *count)
	}
}

// flatGroups is every group of a report, level by level, as "level/value".
func flatGroups(report limitedTopology) []string {
	var out []string
	for _, level := range report.Levels {
		for _, group := range level.Groups {
			out = append(out, level.Name+"/"+group.Value)
		}
	}

	return out
}
