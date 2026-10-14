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
	"bytes"
	"encoding/json"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// A caller asks for a schema by the command it was about to run, so the
// mapping is part of the contract: without it "kubectl nv schema info"
// answers "unknown kind or command".
func TestInfoMapsToItsKind(t *testing.T) {
	t.Parallel()

	byCommand, err := output.SchemaFor("info")
	if err != nil {
		t.Fatalf("schema for the info command: %v", err)
	}
	byKind, err := output.SchemaFor(output.KindGPUNode)
	if err != nil {
		t.Fatalf("schema for %s: %v", output.KindGPUNode, err)
	}
	if !bytes.Equal(byCommand, byKind) {
		t.Errorf("the info command does not map to %s", output.KindGPUNode)
	}
}

// What the command prints has to satisfy the published schema. A field whose
// wire shape differs from its Go shape passes the generator and fails here.
func TestInfoOutputSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	s, err := runInfo(t, cluster(), "worker-1", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv info worker-1 -o json: %v", err)
	}

	var instance any
	if err := json.Unmarshal(s.out.Bytes(), &instance); err != nil {
		t.Fatalf("reading what the command printed: %v\n%s", err, s.out.String())
	}
	if err := output.ValidateAgainstSchema(output.KindGPUNode, instance); err != nil {
		t.Fatal(err)
	}
}
