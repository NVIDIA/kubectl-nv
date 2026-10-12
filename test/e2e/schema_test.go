//go:build e2e

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

package e2e

import (
	"encoding/json"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"

	// The kinds register themselves at import time, and the registry is what
	// ValidateAgainstSchema reads. They are imported for that alone: this
	// suite stays a black box over the built binary, and the wire shapes it
	// decodes are still declared here rather than taken from these packages.
	_ "github.com/NVIDIA/kubectl-nv/pkg/debug"
	_ "github.com/NVIDIA/kubectl-nv/pkg/health"
	_ "github.com/NVIDIA/kubectl-nv/pkg/inventory"
	_ "github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	_ "github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// assertMatchesSchema holds what the binary printed to the published schema of
// its kind.
//
// The struct each helper decodes into names the fields that test cares about
// and ignores the rest, so a field that changed shape, or one added without a
// schema to describe it, passes every other assertion in the suite. This is
// the assertion that does not: it checks the whole document against the
// contract a consumer validates against.
func assertMatchesSchema(t *testing.T, kind, raw string) {
	t.Helper()

	var instance any
	if err := json.Unmarshal([]byte(raw), &instance); err != nil {
		t.Fatalf("%s output does not parse: %v\n%s", kind, err, raw)
	}
	if err := output.ValidateAgainstSchema(kind, instance); err != nil {
		t.Errorf("%v\noutput:\n%s", err, raw)
	}
}
