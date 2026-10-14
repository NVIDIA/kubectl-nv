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

package info_test

import (
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// hostileBundle is a must-gather directory whose node labels, taints,
// condition reasons and operand images carry terminal escape sequences.
//
// This is not a far fetched input. A must-gather arrives in a support ticket
// from a cluster this tool's user does not control, and on any cluster a node
// label is writable by anyone with patch on nodes. Printing one unfiltered
// hands that person the reader's cursor, their window title and their scroll
// back.
const hostileBundle = "testdata/hostile-bundle"

// TestHostileBundleFixtureIsHostile fails if the fixture stops carrying
// control characters. The sanitiser tests below would pass against a clean
// fixture while proving nothing, which is the failure mode a guard like this
// exists to catch.
func TestHostileBundleFixtureIsHostile(t *testing.T) {
	t.Parallel()

	bundle, err := source.NewBundle(hostileBundle)
	if err != nil {
		t.Fatalf("opening the hostile bundle: %v", err)
	}

	nodes, err := bundle.Nodes(t.Context())
	if err != nil {
		t.Fatalf("reading the hostile bundle's nodes: %v", err)
	}

	if len(nodes) != 1 {
		t.Fatalf("the hostile bundle holds %d nodes, want 1", len(nodes))
	}

	product := nodes[0].Labels["nvidia.com/gpu.product"]
	if !strings.ContainsRune(product, 0x1b) {
		t.Fatalf("gpu.product = %q, want it to carry an ESC: the fixture is no longer hostile", product)
	}
}

// TestDescribeSanitisesEveryControlByte is the assertion that matters: no C0
// or C1 byte reaches stdout, whatever the bundle said.
func TestDescribeSanitisesEveryControlByte(t *testing.T) {
	t.Parallel()

	s, err := runInfo(t, nil, "hostile-1", "--from-bundle", hostileBundle)
	if err != nil && !strings.Contains(err.Error(), "could not read pods") {
		t.Fatalf("kubectl nv info over the hostile bundle: %v\nstderr: %s", err, s.err)
	}

	out := s.out.String()
	if out == "" {
		t.Fatal("kubectl nv info printed nothing")
	}

	for i, r := range out {
		// Newline is the only control character a line oriented view may emit.
		if r == '\n' {
			continue
		}

		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			t.Fatalf("U+%04X reached stdout at offset %d:\n%q", r, i, out)
		}
	}

	// The sanitiser must strip the control bytes, not the text around them, or
	// a hostile label would simply erase the field it is in.
	for _, want := range []string{"NVIDIA-T4", "PWNED", "KubeletReady", "nvidia.com/gpu", "gpu-operator-0"} {
		if !strings.Contains(out, want) {
			t.Errorf("the description lost %q along with the escapes:\n%s", want, out)
		}
	}
}
