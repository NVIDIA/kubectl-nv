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
	"testing"
)

// twoLevelReport is a report whose groups fall on a level boundary: two in the
// first level and two in the second, so a limit of two ends exactly where one
// level does.
func twoLevelReport() *TopologyReport {
	group := func(value string) Group {
		return Group{Value: value, NodeCount: 1, Allocatable: 8, RequestedKnown: true}
	}

	r := NewTopologyReport()
	r.Levels = []Level{
		{Name: "clique", Label: "nvidia.com/gpu.clique", Source: SourceClique,
			Groups: []Group{group("clique-a"), group("clique-b")}},
		{Name: "accelerator-domain", Label: "accelerator.topograph.run/domain",
			Groups: []Group{group("domain-1"), group("domain-2")}},
	}

	return r
}

// A limit landing exactly on a level boundary must not leave the next level in
// the report with no groups at all. A level that lists none says the cluster
// does not divide that way, which is a different answer from "you asked for
// fewer", and a reader would act on it.
func TestALimitOnALevelBoundaryLeavesNoEmptyLevel(t *testing.T) {
	t.Parallel()

	r := twoLevelReport()
	if total := r.LimitItems(2); total != 4 {
		t.Fatalf("the report holds %d groups, want 4", total)
	}

	if len(r.Levels) != 1 {
		t.Errorf("the report kept %d levels, want the one the limit filled", len(r.Levels))
	}
	for _, level := range r.Levels {
		if len(level.Groups) == 0 {
			t.Errorf("level %q came back with no groups", level.Name)
		}
	}
}

// A limit equal to the number of groups truncates nothing, so there is nothing
// to report: no count in the output and no note on the error stream. A report
// that said it was truncated when it was not would send a reader looking for
// the groups that were left out.
func TestALimitEqualToTheTotalTruncatesNothing(t *testing.T) {
	t.Parallel()

	r := twoLevelReport()
	if total := r.LimitItems(4); total != 4 {
		t.Fatalf("the report holds %d groups, want 4", total)
	}

	if r.Status.RemainingItemCount != nil {
		t.Errorf("remainingItemCount is %d on a report nothing was left out of",
			*r.Status.RemainingItemCount)
	}
	if len(r.Levels) != 2 {
		t.Errorf("the report kept %d levels, want both", len(r.Levels))
	}
}

// A level that had no groups to begin with is dropped too. A report carrying
// one is a report saying the cluster divides a way it has no groups for, and
// the limit is not what made that true.
//
// The empty level goes first, before the limit runs out. Put last it is never
// reached, because the loop stops as soon as the limit is filled, so a report
// whose empty level trails would pass whether the guard is there or not: that
// fixture could not tell the two apart and this one has to.
func TestALimitDropsALevelThatHadNoGroups(t *testing.T) {
	t.Parallel()

	r := twoLevelReport()
	empty := Level{Name: "tier-1", Label: "fabric.topograph.run/tier-1"}
	r.Levels = append([]Level{empty}, r.Levels...)

	if total := r.LimitItems(3); total != 4 {
		t.Fatalf("the report holds %d groups, want 4", total)
	}

	for _, level := range r.Levels {
		if len(level.Groups) == 0 {
			t.Errorf("level %q came back with no groups", level.Name)
		}
	}
	// And the three groups asked for are there: a guard that dropped the
	// empty level by dropping every level would pass the loop above.
	kept := 0
	for _, level := range r.Levels {
		kept += len(level.Groups)
	}
	if kept != 3 {
		t.Errorf("the report kept %d groups, want the 3 that were asked for", kept)
	}
}

// A health report's findings answer the same two questions.
func TestAHealthLimitEqualToTheTotalTruncatesNothing(t *testing.T) {
	t.Parallel()

	r := twoLevelReport()
	before := len(r.Levels)
	if total := r.LimitItems(0); total != 4 {
		t.Fatalf("a limit of zero reported %d groups, want the 4 there are", total)
	}
	if len(r.Levels) != before {
		t.Errorf("a limit of zero changed the report: %d levels, want %d", len(r.Levels), before)
	}
	if r.Status.RemainingItemCount != nil {
		t.Error("a limit of zero recorded a truncation")
	}
}
