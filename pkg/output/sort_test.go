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

package output

import (
	"fmt"
	"strings"
	"testing"
)

// keys renders the findings as "node/code/message" so an ordering mismatch
// prints as one readable line.
func keys(findings []Finding) string {
	parts := make([]string, 0, len(findings))
	for _, f := range findings {
		parts = append(parts, f.Node+"/"+f.Code+"/"+f.Message)
	}

	return strings.Join(parts, " ")
}

func TestSortFindingsOrdersByNodeThenCode(t *testing.T) {
	t.Parallel()

	findings := []Finding{
		{Node: "node-b", Code: "GPU002"},
		{Node: "node-a", Code: "GPU010"},
		{Node: "node-b", Code: "GPU001"},
		{Node: "node-a", Code: "GPU002"},
		{Node: "", Code: "CLUSTER001"},
	}

	SortFindings(findings)

	want := "/CLUSTER001/ node-a/GPU002/ node-a/GPU010/ node-b/GPU001/ node-b/GPU002/"
	if got := keys(findings); got != want {
		t.Errorf("order =\n%s\nwant\n%s", got, want)
	}
}

// Equal keys mixed in among different ones still have to keep their order: the
// stable path has to hold when the sort actually has work to do.
func TestSortFindingsIsStableAmongDifferentKeys(t *testing.T) {
	t.Parallel()

	const perNode = 14

	var findings []Finding
	for _, node := range []string{"node-c", "node-a", "node-b"} {
		for i := range perNode {
			findings = append(findings, Finding{
				Node:    node,
				Code:    "GPU001",
				Message: fmt.Sprintf("%s-%02d", node, i),
			})
		}
	}

	SortFindings(findings)

	var want []string
	for _, node := range []string{"node-a", "node-b", "node-c"} {
		for i := range perNode {
			want = append(want, fmt.Sprintf("%s/GPU001/%s-%02d", node, node, i))
		}
	}

	if got := keys(findings); got != strings.Join(want, " ") {
		t.Errorf("stable order =\n%s\nwant\n%s", got, strings.Join(want, " "))
	}
}

func TestSortFindingsHandlesEmptyAndSingle(t *testing.T) {
	t.Parallel()

	var empty []Finding
	SortFindings(empty)
	if len(empty) != 0 {
		t.Errorf("empty slice grew to %d", len(empty))
	}

	single := []Finding{{Node: "node-a", Code: "GPU001"}}
	SortFindings(single)
	if keys(single) != "node-a/GPU001/" {
		t.Errorf("single = %s", keys(single))
	}
}
