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
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The cli-runtime printers read TypeMeta off the object before they marshal
// anything: the JSON and YAML printers refuse an object with an empty
// GroupVersionKind, and the name printer has no kind to put in "kind/name".
// MarshalJSON stamping the envelope does not help them, because the refusal
// happens before any marshalling.
//
// That is why NewGPUNode and NewGPUNodeList are mandatory rather than a
// convenience, and this is the test that keeps the doc saying so true.
func TestConstructorOutputPrintsInEveryMachineFormat(t *testing.T) {
	t.Parallel()

	node := NewGPUNode("node-a")

	list := NewGPUNodeList()
	list.Items = append(list.Items, *NewGPUNode("node-a"), *NewGPUNode("node-b"))

	for _, tc := range []struct {
		format string
		want   []string
	}{
		{"json", []string{`"kind": "GPUNode"`, `"apiVersion": "kubectl-nv.nvidia.com/v1alpha1"`}},
		{"yaml", []string{"kind: GPUNode", "apiVersion: kubectl-nv.nvidia.com/v1alpha1"}},
		{"name", []string{"gpunode/node-a"}},
	} {
		t.Run("node/"+tc.format, func(t *testing.T) {
			t.Parallel()

			printer, err := output.NewPrinter(tc.format)
			if err != nil {
				t.Fatalf("NewPrinter(%q): %v", tc.format, err)
			}

			var buf bytes.Buffer
			if err := printer.PrintObj(node, &buf); err != nil {
				t.Fatalf("PrintObj: %v", err)
			}

			for _, want := range tc.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("-o %s output does not contain %q:\n%s", tc.format, want, buf.String())
				}
			}
		})
	}

	for _, tc := range []struct {
		format string
		want   []string
	}{
		{"json", []string{`"kind": "GPUNodeList"`, `"kind": "GPUNode"`}},
		{"yaml", []string{"kind: GPUNodeList"}},
		{"name", []string{"gpunode/node-a\ngpunode/node-b\n"}},
	} {
		t.Run("list/"+tc.format, func(t *testing.T) {
			t.Parallel()

			printer, err := output.NewPrinter(tc.format)
			if err != nil {
				t.Fatalf("NewPrinter(%q): %v", tc.format, err)
			}

			var buf bytes.Buffer
			if err := printer.PrintObj(list, &buf); err != nil {
				t.Fatalf("PrintObj: %v", err)
			}

			for _, want := range tc.want {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("-o %s output does not contain %q:\n%s", tc.format, want, buf.String())
				}
			}
		})
	}
}

// The other half of the same contract: a literal built without a constructor
// does not print. This is the behaviour the doc warns about, so it is asserted
// rather than described, and a future change that made the printers tolerant
// has to come here and update the doc with it.
func TestUnstampedLiteralsDoNotPrint(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"json", "yaml", "name"} {
		t.Run("node/"+format, func(t *testing.T) {
			t.Parallel()

			printer, err := output.NewPrinter(format)
			if err != nil {
				t.Fatalf("NewPrinter(%q): %v", format, err)
			}

			var buf bytes.Buffer
			if err := printer.PrintObj(&GPUNode{}, &buf); err == nil {
				t.Errorf("-o %s accepted an unstamped GPUNode and printed:\n%s", format, buf.String())
			}
		})

		t.Run("list/"+format, func(t *testing.T) {
			t.Parallel()

			printer, err := output.NewPrinter(format)
			if err != nil {
				t.Fatalf("NewPrinter(%q): %v", format, err)
			}

			var buf bytes.Buffer
			if err := printer.PrintObj(&GPUNodeList{}, &buf); err == nil {
				t.Errorf("-o %s accepted an unstamped GPUNodeList and printed:\n%s", format, buf.String())
			}
		})
	}
}

// The constructors have to stamp the struct itself, not only make MarshalJSON
// do it: that is the difference the printers see.
func TestConstructorsStampTypeMeta(t *testing.T) {
	t.Parallel()

	node := NewGPUNode("node-a")
	if node.Kind != output.KindGPUNode || node.APIVersion != output.APIVersion {
		t.Errorf("NewGPUNode TypeMeta = %+v", node.TypeMeta)
	}

	if node.Name != "node-a" {
		t.Errorf("NewGPUNode name = %q", node.Name)
	}

	list := NewGPUNodeList()
	if list.Kind != output.KindGPUNodeList || list.APIVersion != output.APIVersion {
		t.Errorf("NewGPUNodeList TypeMeta = %+v", list.TypeMeta)
	}

	if list.Items == nil {
		t.Error("NewGPUNodeList Items is nil")
	}
}
