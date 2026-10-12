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
	"bytes"
	"encoding/json"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// A caller asks for a schema by the command it was about to run, so the
// mapping is part of the contract rather than a convenience: without it
// "kubectl nv schema list" answers "unknown kind or command".
func TestListMapsToItsKind(t *testing.T) {
	t.Parallel()

	byCommand, err := output.SchemaFor("list")
	if err != nil {
		t.Fatalf("schema for the list command: %v", err)
	}
	byKind, err := output.SchemaFor(output.KindGPUNodeList)
	if err != nil {
		t.Fatalf("schema for %s: %v", output.KindGPUNodeList, err)
	}
	if !bytes.Equal(byCommand, byKind) {
		t.Errorf("the list command does not map to %s", output.KindGPUNodeList)
	}
}

// What the command actually prints has to satisfy the published schema. The
// generator reads the Go type, so a field added to the output without
// regenerating is caught by the drift check; a field whose wire shape differs
// from its Go shape is caught only here.
func TestListOutputSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	s, err := runList(t, []runtime.Object{
		gpuNodeWithAllocatable("node-1", 8, 7, map[string]string{
			"nvidia.com/gpu.product":       "NVIDIA-H100-80GB-HBM3",
			"nvidia.com/gpu.count":         "8",
			"nvidia.com/gpu.clique":        "clique-a",
			"nvidia.com/cuda.driver.major": "550",
		}),
		gpuPod("trainer", "research", "node-1", 2),
	}, "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv list -o json: %v", err)
	}

	var instance any
	if err := json.Unmarshal(s.out.Bytes(), &instance); err != nil {
		t.Fatalf("reading what the command printed: %v\n%s", err, s.out.String())
	}
	if err := output.ValidateAgainstSchema(output.KindGPUNodeList, instance); err != nil {
		t.Fatal(err)
	}
}
