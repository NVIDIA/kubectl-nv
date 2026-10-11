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
	"bytes"
	"encoding/json"
	"io"
	"testing"
)

// findingKeys returns the keys of a marshalled Finding in wire order.
func findingKeys(t *testing.T, raw []byte) []string {
	t.Helper()

	dec := json.NewDecoder(bytes.NewReader(raw))

	if _, err := dec.Token(); err != nil {
		t.Fatalf("reading the opening token: %v", err)
	}

	var keys []string

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}

		if err != nil {
			t.Fatalf("reading a token: %v", err)
		}

		if _, ok := tok.(json.Delim); ok {
			break
		}

		key, ok := tok.(string)
		if !ok {
			t.Fatalf("object key is not a string: %v", tok)
		}

		keys = append(keys, key)

		var discard json.RawMessage
		if err := dec.Decode(&discard); err != nil {
			t.Fatalf("skipping the value of %q: %v", key, err)
		}
	}

	return keys
}

// The finding is what every track writes and what every consumer parses. The
// names are the contract, so a rename has to break here and nowhere else.
func TestFindingJSONFieldNames(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(Finding{
		Code:      "NV-DRV-001",
		Severity:  SeverityError,
		Node:      "node-a",
		Component: "nvidia-driver-daemonset",
		Message:   "the driver container is in CrashLoopBackOff",
		Evidence:  map[string]string{"restarts": "7"},
		Docs:      "https://docs.nvidia.com/datacenter/cloud-native/",
		Next:      "kubectl -n gpu-operator logs nvidia-driver-daemonset-xyz",
	})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	want := []string{"code", "severity", "node", "component", "message", "evidence", "docs", "next"}

	got := findingKeys(t, raw)
	if len(got) != len(want) {
		t.Fatalf("fields =\n%v\nwant\n%v", got, want)
	}

	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fields =\n%v\nwant\n%v", got, want)
		}
	}
}

func TestFindingNilEvidenceMarshalsAsEmptyObject(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(Finding{Code: "NV-NODE-001", Severity: SeverityWarning})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	if string(got["evidence"]) != "{}" {
		t.Errorf("evidence = %s, want {}", got["evidence"])
	}

	if bytes.Contains(raw, []byte("null")) {
		t.Errorf("a finding with no evidence marshals a null:\n%s", raw)
	}
}

// A finding inside a report status goes through ReportStatus.MarshalJSON. The
// never-null guarantee has to survive that nesting, or the guarantee is only
// true for findings nobody reports.
func TestFindingInsideReportStatusKeepsEvidence(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(ReportStatus{Findings: []Finding{{Code: "NV-OP-001"}}})
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	if bytes.Contains(raw, []byte("null")) {
		t.Errorf("report status marshals a null:\n%s", raw)
	}

	if !bytes.Contains(raw, []byte(`"evidence":{}`)) {
		t.Errorf("nested finding lost its empty evidence:\n%s", raw)
	}
}

// Round tripping is what a consumer does with -o json piped back in. Every
// field has to survive it, including an evidence map that was never set.
func TestFindingRoundTrips(t *testing.T) {
	t.Parallel()

	want := Finding{
		Code:      "NV-MIG-003",
		Severity:  SeverityInfo,
		Node:      "node-b",
		Component: "mig-manager",
		Message:   "mig.config.state is pending",
		Evidence:  map[string]string{"nvidia.com/mig.config": "all-1g.10gb"},
		Docs:      "docs/mig.md",
		Next:      "kubectl nv info node-b",
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var got Finding
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	if got.Code != want.Code || got.Severity != want.Severity || got.Node != want.Node ||
		got.Component != want.Component || got.Message != want.Message ||
		got.Docs != want.Docs || got.Next != want.Next {
		t.Errorf("round trip =\n%+v\nwant\n%+v", got, want)
	}

	if got.Evidence["nvidia.com/mig.config"] != "all-1g.10gb" {
		t.Errorf("evidence = %v", got.Evidence)
	}
}
