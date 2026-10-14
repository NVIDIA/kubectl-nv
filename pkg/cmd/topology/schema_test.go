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
	"bytes"
	"encoding/json"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// A caller asks for a schema by the command it was about to run, so the
// mapping is part of the contract: without it "kubectl nv schema topology"
// answers "unknown kind or command".
func TestTopologyMapsToItsKind(t *testing.T) {
	t.Parallel()

	byCommand, err := output.SchemaFor("topology")
	if err != nil {
		t.Fatalf("schema for the topology command: %v", err)
	}
	byKind, err := output.SchemaFor(output.KindTopologyReport)
	if err != nil {
		t.Fatalf("schema for %s: %v", output.KindTopologyReport, err)
	}
	if !bytes.Equal(byCommand, byKind) {
		t.Errorf("the topology command does not map to %s", output.KindTopologyReport)
	}
}

// What the command prints has to satisfy the published schema. A level and a
// group marshal themselves, so their wire shape is the one thing the generator
// cannot read off the Go type; only this catches it.
func TestTopologyOutputSatisfiesItsSchema(t *testing.T) {
	raw, err := run(t, []string{"-o", "json"},
		gpuNode("node-a", "clique-a", 4),
		gpuNode("node-b", "clique-a", 4),
		gpuPod("busy", "node-a", 3),
	)
	if err != nil {
		t.Fatalf("kubectl nv topology -o json: %v", err)
	}

	var instance any
	if err := json.Unmarshal([]byte(raw), &instance); err != nil {
		t.Fatalf("reading what the command printed: %v\n%s", err, raw)
	}
	if err := output.ValidateAgainstSchema(output.KindTopologyReport, instance); err != nil {
		t.Fatal(err)
	}
}
