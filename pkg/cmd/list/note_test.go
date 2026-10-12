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

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/list"
)

// A path the kind has and no column of the table renders is not an error: the
// caller named something real, and -o json would give it. But a table that
// came back without the column they asked for and said nothing reads as a
// table with no such field, and they go looking for the typo.
func TestATableSaysWhichRequestedPathsNoColumnRenders(t *testing.T) {
	t.Parallel()

	// taints is a path of a GPUNode and no column prints it.
	s, err := runList(t, columnsCluster(), "--fields", "taints")
	if err != nil {
		t.Fatalf("kubectl nv list --fields taints: %v", err)
	}

	if !strings.Contains(s.err.String(), "taints") {
		t.Errorf("stderr does not name the path no column renders: %q", s.err.String())
	}
	if !strings.Contains(s.err.String(), "-o json") {
		t.Errorf("stderr does not say how to see it: %q", s.err.String())
	}
}

// A path a column does render is not reported. A note on every run would be a
// note nobody reads.
func TestATableSaysNothingWhenEveryRequestedPathIsRendered(t *testing.T) {
	t.Parallel()

	s, err := runList(t, columnsCluster(), "--fields", "product")
	if err != nil {
		t.Fatalf("kubectl nv list --fields product: %v", err)
	}

	if strings.Contains(s.err.String(), "no column") {
		t.Errorf("stderr reports a path the PRODUCT column renders: %q", s.err.String())
	}
}

// Asking for a whole section keeps the columns inside it, so there is nothing
// to report about the section itself.
func TestASectionIsNotReportedAsUnrendered(t *testing.T) {
	t.Parallel()

	s, err := runList(t, columnsCluster(), "--fields", "mig")
	if err != nil {
		t.Fatalf("kubectl nv list --fields mig: %v", err)
	}

	if strings.Contains(s.err.String(), "no column") {
		t.Errorf("stderr reports mig, whose mig.strategy the MIG column renders: %q", s.err.String())
	}
}

// unusedList keeps the import load bearing for the helpers above.
var _ = list.NewCommand
