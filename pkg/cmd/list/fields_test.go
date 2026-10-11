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
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/info"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/list"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// TestOutputFlagsAreTheSharedOnes pins that both commands take the whole
// output interface, not just -o. The MCP server forces -o json by name, and a
// command that registered its own spelling would be served in whatever format
// it defaults to and its output would not parse.
func TestOutputFlagsAreTheSharedOnes(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name   string
		newCmd func(*cmd.Options) *cobra.Command
	}{
		{name: "list", newCmd: list.NewCommand},
		{name: "info", newCmd: info.NewCommand},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			c := tt.newCmd(&cmd.Options{})

			for _, flag := range []string{"output", "fields", "limit"} {
				if c.Flags().Lookup(flag) == nil {
					t.Errorf("gpu %s has no --%s flag", tt.name, flag)
				}
			}

			short := c.Flags().ShorthandLookup("o")
			if short == nil || short.Name != "output" {
				t.Errorf("gpu %s has no -o shorthand for --output", tt.name)
			}
		})
	}
}

// TestFieldsKeepsTheRightColumns pins that --fields reaches the table. A
// column whose Field is unset is dropped as soon as --fields is given, so this
// is what catches a column that never named its path.
func TestFieldsKeepsTheRightColumns(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNodeWithAllocatable("worker-1", 4, 3, map[string]string{"nvidia.com/gpu.product": "NVIDIA-T4"}),
	}

	s, err := runList(t, objects, "--fields", "product,mode")
	if err != nil {
		t.Fatalf("gpu list --fields: %v\nstderr: %s", err, s.err)
	}

	header := cells(lines(s.out.String())[0])

	// metadata.name is always kept, so NODE survives whatever was asked for.
	want := []string{"NODE", "PRODUCT", "MODE"}
	if !equalStrings(header, want) {
		t.Errorf("header = %v, want %v", header, want)
	}
}

// TestFieldsProjectsJSON pins the same flag on the machine readable path.
func TestFieldsProjectsJSON(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{gpuNode("worker-1", 4, map[string]string{"nvidia.com/gpu.product": "NVIDIA-T4"})}

	s, err := runList(t, objects, "-o", "json", "--fields", "product")
	if err != nil {
		t.Fatalf("gpu list -o json --fields: %v\nstderr: %s", err, s.err)
	}

	var envelope struct {
		Items []map[string]any `json:"items"`
	}

	if err := json.Unmarshal(s.out.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding the output: %v\n%s", err, s.out)
	}

	if len(envelope.Items) != 1 {
		t.Fatalf("items = %#v, want one", envelope.Items)
	}

	item := envelope.Items[0]
	if _, found := item["product"]; !found {
		t.Errorf("the projected item lost the field that was asked for: %v", item)
	}

	if _, found := item["mode"]; found {
		t.Errorf("the projected item kept a field nobody asked for: %v", item)
	}
}

// TestLimitBoundsTheList pins --limit on the command, and that a truncated
// answer says so rather than passing for a complete one.
func TestLimitBoundsTheList(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNode("worker-1", 4, nil),
		gpuNode("worker-2", 4, nil),
		gpuNode("worker-3", 4, nil),
	}

	s, err := runList(t, objects, "--limit", "2", "-o", "name")
	if err != nil {
		t.Fatalf("gpu list --limit: %v\nstderr: %s", err, s.err)
	}

	want := []string{"gpunode/worker-1", "gpunode/worker-2"}
	if got := lines(s.out.String()); !equalStrings(got, want) {
		t.Errorf("output = %v, want %v", got, want)
	}

	if !strings.Contains(s.err.String(), "3") {
		t.Errorf("stderr does not say how many items there were: %q", s.err)
	}
}

// TestUnknownFieldIsAUsageError pins that a misspelled path stops the run. It
// has to be caught before the cluster is read: a scan nobody needed has
// already cost the user the wait by the time the typo is found.
func TestUnknownFieldIsAUsageError(t *testing.T) {
	t.Parallel()

	counter, _, err := runCounted(t, list.NewCommand,
		[]runtime.Object{gpuNode("worker-1", 4, nil)}, "--fields", "prodcut")
	if err == nil {
		t.Fatal("gpu list accepted a field path that does not exist")
	}

	if code := output.ExitCodeFor(err); code != output.ExitUsage {
		t.Errorf("exit code = %d, want %d: %v", code, output.ExitUsage, err)
	}

	if got := counter.count("nodes"); got != 0 {
		t.Errorf("the cluster was read %d times before the typo was found, want 0", got)
	}
}
