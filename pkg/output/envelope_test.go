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
	"strings"
	"testing"
)

// testList builds a two item GPUNodeList for the envelope and printer tests.
func testList(t *testing.T) *List {
	t.Helper()

	list := NewList(KindGPUNodeList)
	list.Items = []Object{
		*NewObject(KindGPUNode, "node-b"),
		*NewObject(KindGPUNode, "node-a"),
	}

	return list
}

func TestJSONRoundTripKeepsAPIVersionAndKind(t *testing.T) {
	t.Parallel()

	printer, err := NewPrinter(string(FormatJSON))
	if err != nil {
		t.Fatalf("NewPrinter(json): %v", err)
	}

	var buf bytes.Buffer
	if err := printer.PrintObj(testList(t), &buf); err != nil {
		t.Fatalf("PrintObj: %v", err)
	}

	var decoded List
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal(%q): %v", buf.String(), err)
	}

	if decoded.APIVersion != "kubectl-nv.nvidia.com/v1alpha1" {
		t.Errorf("list apiVersion = %q, want %q", decoded.APIVersion, "kubectl-nv.nvidia.com/v1alpha1")
	}
	if decoded.Kind != "GPUNodeList" {
		t.Errorf("list kind = %q, want %q", decoded.Kind, "GPUNodeList")
	}

	// The envelope preserves the caller's order: deterministic ordering is part
	// of this contract, so the unsorted input order is asserted literally here.
	names := make([]string, 0, len(decoded.Items))
	for i, item := range decoded.Items {
		if item.APIVersion != "kubectl-nv.nvidia.com/v1alpha1" {
			t.Errorf("item %d apiVersion = %q, want %q", i, item.APIVersion, "kubectl-nv.nvidia.com/v1alpha1")
		}
		if item.Kind != "GPUNode" {
			t.Errorf("item %d kind = %q, want %q", i, item.Kind, "GPUNode")
		}
		names = append(names, item.Name)
	}
	if got := strings.Join(names, ","); got != "node-b,node-a" {
		t.Errorf("item names = %q, want %q", got, "node-b,node-a")
	}
}

func TestYAMLOutputCarriesAPIVersionAndKind(t *testing.T) {
	t.Parallel()

	printer, err := NewPrinter(string(FormatYAML))
	if err != nil {
		t.Fatalf("NewPrinter(yaml): %v", err)
	}

	var buf bytes.Buffer
	if err := printer.PrintObj(testList(t), &buf); err != nil {
		t.Fatalf("PrintObj: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "apiVersion: kubectl-nv.nvidia.com/v1alpha1\n") {
		t.Errorf("yaml output missing apiVersion line:\n%s", out)
	}
	if !strings.Contains(out, "kind: GPUNodeList\n") {
		t.Errorf("yaml output missing list kind line:\n%s", out)
	}
	if n := strings.Count(out, "kind: GPUNode\n"); n != 2 {
		t.Errorf("yaml output carries %d item kind lines, want 2:\n%s", n, out)
	}
	for _, name := range []string{"node-a", "node-b"} {
		if !strings.Contains(out, "name: "+name+"\n") {
			t.Errorf("yaml output missing %q:\n%s", name, out)
		}
	}
}

// An empty list is the common case for a cluster with no GPU nodes, and a
// consumer that has to special case null before it can range over items is a
// consumer that will get it wrong.
func TestEmptyListMarshalsItemsAsAnEmptyArray(t *testing.T) {
	t.Parallel()

	t.Run("json from NewList", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		printer, err := NewPrinter(string(FormatJSON))
		if err != nil {
			t.Fatalf("NewPrinter(json): %v", err)
		}
		if err := printer.PrintObj(NewList(KindGPUNodeList), &buf); err != nil {
			t.Fatalf("PrintObj: %v", err)
		}

		if strings.Contains(buf.String(), "null") {
			t.Errorf("json output carries a null:\n%s", buf.String())
		}
		if !strings.Contains(buf.String(), `"items": []`) {
			t.Errorf("json output has no empty items array:\n%s", buf.String())
		}
	})

	t.Run("json from a list built by hand", func(t *testing.T) {
		t.Parallel()

		// A list with a nil Items has to marshal the same way: the guarantee is
		// on the type, not on the constructor.
		raw, err := json.Marshal(&List{TypeMeta: TypeMetaFor(KindGPUNodeList)})
		if err != nil {
			t.Fatalf("json.Marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"items":[]`) {
			t.Errorf("json = %s, want an empty items array", raw)
		}
	})

	t.Run("yaml", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		printer, err := NewPrinter(string(FormatYAML))
		if err != nil {
			t.Fatalf("NewPrinter(yaml): %v", err)
		}
		if err := printer.PrintObj(NewList(KindGPUNodeList), &buf); err != nil {
			t.Fatalf("PrintObj: %v", err)
		}

		if !strings.Contains(buf.String(), "items: []\n") {
			t.Errorf("yaml output has no empty items array:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), "items: null") {
			t.Errorf("yaml output carries a null items:\n%s", buf.String())
		}
	})
}

// An envelope with nothing in its metadata should not print an empty metadata
// block, and it should never print metadata as null.
func TestZeroMetadataIsOmitted(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(NewList(KindGPUNodeList))
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if strings.Contains(string(raw), "metadata") {
		t.Errorf("json = %s, want no metadata key", raw)
	}

	named, err := json.Marshal(NewObject(KindGPUNode, "node-a"))
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if !strings.Contains(string(named), `"name":"node-a"`) {
		t.Errorf("json = %s, want the name under metadata", named)
	}
}

func TestTypeMetaFor(t *testing.T) {
	t.Parallel()

	tm := TypeMetaFor(KindHealthReport)
	if tm.Kind != "HealthReport" {
		t.Errorf("Kind = %q, want %q", tm.Kind, "HealthReport")
	}
	if tm.APIVersion != "kubectl-nv.nvidia.com/v1alpha1" {
		t.Errorf("APIVersion = %q, want %q", tm.APIVersion, "kubectl-nv.nvidia.com/v1alpha1")
	}
}

func TestKindConstants(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		constant string
		want     string
	}{
		{KindGPUNodeList, "GPUNodeList"},
		{KindGPUNode, "GPUNode"},
		{KindHealthReport, "HealthReport"},
		{KindTopologyReport, "TopologyReport"},
		{KindBundleManifest, "BundleManifest"},
	} {
		if tc.constant != tc.want {
			t.Errorf("kind constant = %q, want %q", tc.constant, tc.want)
		}
	}

	if APIVersion != "kubectl-nv.nvidia.com/v1alpha1" {
		t.Errorf("APIVersion = %q, want %q", APIVersion, "kubectl-nv.nvidia.com/v1alpha1")
	}
	if GroupName != "kubectl-nv.nvidia.com" {
		t.Errorf("GroupName = %q, want %q", GroupName, "kubectl-nv.nvidia.com")
	}
}

func TestDeepCopyDoesNotShareItems(t *testing.T) {
	t.Parallel()

	list := testList(t)
	clone, ok := list.DeepCopyObject().(*List)
	if !ok {
		t.Fatalf("DeepCopyObject did not return a *List")
	}

	clone.Items[0].Name = "changed"
	if list.Items[0].Name != "node-b" {
		t.Errorf("the original item was renamed to %q through the copy", list.Items[0].Name)
	}
}
