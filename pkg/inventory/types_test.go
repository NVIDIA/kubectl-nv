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

package inventory

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// keysInOrder returns the keys of a JSON object in the order they were written.
// The contract is about the wire bytes, so reading them back into a map would
// test something else.
func keysInOrder(t *testing.T, raw []byte) []string {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(raw))

	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("reading the opening token: %v", err)
	}

	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		t.Fatalf("payload does not start with an object: %v", tok)
	}

	var keys []string

	depth := 0

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatalf("reading a token: %v", err)
		}

		if delim, ok := tok.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				if depth == 0 {
					return keys
				}

				depth--
			}

			continue
		}

		if depth == 0 {
			key, ok := tok.(string)
			if !ok {
				t.Fatalf("object key is not a string: %v", tok)
			}

			keys = append(keys, key)
			// Skip the value, whatever shape it has.
			var discard json.RawMessage
			if err := dec.Decode(&discard); err != nil {
				t.Fatalf("skipping the value of %q: %v", key, err)
			}
		}
	}

	return keys
}

// fields reads a JSON object into its raw members, so that a test can compare
// one member byte for byte without caring about the rest.
func fields(t *testing.T, raw []byte) map[string]json.RawMessage {
	t.Helper()

	out := map[string]json.RawMessage{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshalling: %v\n%s", err, raw)
	}

	return out
}

func TestGPUNodeZeroValueCarriesTheEnvelope(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(GPUNode{})
	if err != nil {
		t.Fatalf("marshalling a zero GPUNode: %v", err)
	}

	got := fields(t, raw)

	if string(got["apiVersion"]) != `"kubectl-nv.nvidia.com/v1alpha1"` {
		t.Errorf("apiVersion = %s", got["apiVersion"])
	}

	if string(got["kind"]) != `"GPUNode"` {
		t.Errorf("kind = %s", got["kind"])
	}
}

// Every slice and every map is an array or an object on the wire, never null. A
// consumer that has to check for null before it can range is a consumer that
// will one day forget to.
func TestGPUNodeZeroValueHasNoNulls(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(GPUNode{})
	if err != nil {
		t.Fatalf("marshalling a zero GPUNode: %v", err)
	}

	got := fields(t, raw)

	for field, want := range map[string]string{
		"deploy":         "{}",
		"topology":       "{}",
		"taints":         "[]",
		"conditions":     "[]",
		"operands":       "[]",
		"resourceSlices": "[]",
	} {
		if string(got[field]) != want {
			t.Errorf("%s = %s, want %s", field, got[field], want)
		}
	}

	if bytes.Contains(raw, []byte("null")) {
		t.Errorf("zero GPUNode marshals a null somewhere:\n%s", raw)
	}
}

// The field names and their order are the contract Tracks D to G all read. A
// renamed or reordered field is a breaking change, so it has to break here.
func TestGPUNodeFieldOrder(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(GPUNode{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	want := []string{
		"kind", "apiVersion", "metadata",
		"product", "count",
		"capacity", "allocatable", "requested", "requestedKnown",
		"driverVersion", "cudaVersion", "mode",
		"mig", "sharing", "upgradeState", "deploy", "clique", "topology",
		"ready", "unschedulable", "taints", "conditions", "operands",
		"resourceSlices", "hinted",
	}

	got := keysInOrder(t, raw)
	if len(got) != len(want) {
		t.Fatalf("fields =\n%v\nwant\n%v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fields =\n%v\nwant\n%v", got, want)
		}
	}
}

func TestOperandPodHasNoNulls(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(GPUNode{Operands: []OperandPod{{Name: "nvidia-driver-daemonset-xyz"}}})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	if bytes.Contains(raw, []byte("null")) {
		t.Errorf("a GPUNode with one operand marshals a null:\n%s", raw)
	}

	var wrapper struct {
		Operands []json.RawMessage `json:"operands"`
	}

	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	if len(wrapper.Operands) != 1 {
		t.Fatalf("operands = %d, want 1", len(wrapper.Operands))
	}

	got := fields(t, wrapper.Operands[0])
	if string(got["images"]) != "[]" {
		t.Errorf("images = %s, want []", got["images"])
	}

	if string(got["initContainers"]) != "[]" {
		t.Errorf("initContainers = %s, want []", got["initContainers"])
	}
}

func TestGPUNodeListZeroValue(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(GPUNodeList{})
	if err != nil {
		t.Fatalf("marshalling a zero GPUNodeList: %v", err)
	}

	got := fields(t, raw)

	if string(got["apiVersion"]) != `"kubectl-nv.nvidia.com/v1alpha1"` {
		t.Errorf("apiVersion = %s", got["apiVersion"])
	}

	if string(got["kind"]) != `"GPUNodeList"` {
		t.Errorf("kind = %s", got["kind"])
	}

	if string(got["items"]) != "[]" {
		t.Errorf("items = %s, want []", got["items"])
	}

	if bytes.Contains(raw, []byte("null")) {
		t.Errorf("zero GPUNodeList marshals a null:\n%s", raw)
	}
}

func TestModeConstants(t *testing.T) {
	t.Parallel()

	for name, pair := range map[string][2]string{
		"ModeDevicePlugin": {ModeDevicePlugin, "device-plugin"},
		"ModeDRA":          {ModeDRA, "dra"},
		"ModeBoth":         {ModeBoth, "both"},
		"ModeNone":         {ModeNone, "none"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
}
