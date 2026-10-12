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

package debug_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// A caller asks for a schema by the command it was about to run, so the
// mapping is part of the contract: without it "kubectl nv schema debug"
// answers "unknown kind or command".
func TestDebugMapsToItsKind(t *testing.T) {
	t.Parallel()

	byCommand, err := output.SchemaFor("debug")
	if err != nil {
		t.Fatalf("schema for the debug command: %v", err)
	}
	byKind, err := output.SchemaFor(output.KindDebugReport)
	if err != nil {
		t.Fatalf("schema for %s: %v", output.KindDebugReport, err)
	}
	if !bytes.Equal(byCommand, byKind) {
		t.Errorf("the debug command does not map to %s", output.KindDebugReport)
	}
}

// What the command prints has to satisfy the published schema. The report
// nests a whole GPUNode and a bundle summary that marshals itself, and only a
// real run through the gatherer produces both.
func TestDebugOutputSatisfiesItsSchema(t *testing.T) {
	r, err := runDebug(t, gatherSeam{}, "worker-1", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv debug worker-1 -o json: %v", err)
	}

	var instance any
	if err := json.Unmarshal(r.out.Bytes(), &instance); err != nil {
		t.Fatalf("reading what the command printed: %v\n%s", err, r.out.String())
	}
	if err := output.ValidateAgainstSchema(output.KindDebugReport, instance); err != nil {
		t.Fatal(err)
	}
}
