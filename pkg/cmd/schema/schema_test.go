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

package schema

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/mcp"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// gpuNode and gpuNodeList stand in for the kinds the later tracks register from
// their own packages. Nothing here knows their fields.
type gpuNode struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Status gpuNodeStatus `json:"status,omitzero"`
}

type gpuNodeStatus struct {
	GPUs int `json:"gpus,omitempty"`
}

func (n *gpuNode) DeepCopyObject() runtime.Object { clone := *n; return &clone }

type gpuNodeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`

	Items []gpuNode `json:"items"`
}

func (l *gpuNodeList) DeepCopyObject() runtime.Object { clone := *l; return &clone }

// testRegistry is the registry the tests below run the command against. The
// package level one belongs to the commands, and a test that wrote to it would
// decide what the next test sees.
func testRegistry(t *testing.T) *output.SchemaRegistry {
	t.Helper()

	r := output.NewSchemaRegistry()
	r.Register(output.KindGPUNodeList, &gpuNodeList{})
	r.Register(output.KindGPUNode, &gpuNode{})
	r.RegisterCommand("list", output.KindGPUNodeList)

	return r
}

// run executes the schema command with args and returns what it printed.
func run(t *testing.T, r *output.SchemaRegistry, args ...string) (string, string, error) {
	t.Helper()

	var out, errOut bytes.Buffer
	o := &cmd.Options{Streams: genericiooptions.IOStreams{Out: &out, ErrOut: &errOut}}
	c := newCommandFor(o, r)
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(args)
	c.SilenceUsage = true
	c.SilenceErrors = true

	// Execute runs first: the return operands are evaluated left to right, so
	// reading the buffers in the return statement would read them empty.
	err := c.Execute()

	return out.String(), errOut.String(), err
}

func TestSchemaPrintsTheSchemaOfAKind(t *testing.T) {
	t.Parallel()

	out, _, err := run(t, testRegistry(t), output.KindGPUNodeList)
	if err != nil {
		t.Fatalf("running the command: %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("the command did not print JSON: %v\n%s", err, out)
	}
	kind, ok := decoded["properties"].(map[string]any)["kind"].(map[string]any)
	if !ok {
		t.Fatalf("the schema has no kind property: %s", out)
	}
	if kind["const"] != output.KindGPUNodeList {
		t.Errorf("kind const is %v, want %q", kind["const"], output.KindGPUNodeList)
	}
}

// A caller knows which command it was about to run, not which kind that command
// prints, so the command name has to be an accepted argument.
func TestSchemaAcceptsACommandName(t *testing.T) {
	t.Parallel()

	r := testRegistry(t)
	byCommand, _, err := run(t, r, "list")
	if err != nil {
		t.Fatalf("by command name: %v", err)
	}
	byKind, _, err := run(t, r, output.KindGPUNodeList)
	if err != nil {
		t.Fatalf("by kind: %v", err)
	}
	if byCommand != byKind {
		t.Error("the command name printed a different schema from the kind it maps to")
	}
}

func TestSchemaForAnUnknownNameExitsTwo(t *testing.T) {
	t.Parallel()

	_, _, err := run(t, testRegistry(t), "GPUNodes")
	if err == nil {
		t.Fatal("an unknown name was accepted")
	}
	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code is %d, want %d", got, output.ExitUsage)
	}
	for _, want := range []string{"GPUNodeList", "GPUNode", "list"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

func TestSchemaWithNoArgumentIsAUsageError(t *testing.T) {
	t.Parallel()

	_, _, err := run(t, testRegistry(t))
	if err == nil {
		t.Fatal("the command ran with no argument and no --write-dir")
	}
	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code is %d, want %d", got, output.ExitUsage)
	}
}

func TestWriteDirWritesOneFilePerKind(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if _, _, err := run(t, testRegistry(t), "--write-dir", dir); err != nil {
		t.Fatalf("writing the schemas: %v", err)
	}

	want := []string{"GPUNode.json", "GPUNodeList.json"}
	if got := jsonFiles(t, dir); !reflect.DeepEqual(got, want) {
		t.Fatalf("wrote %v, want %v", got, want)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "GPUNodeList.json"))
	if err != nil {
		t.Fatalf("reading the written schema: %v", err)
	}
	generated, err := testRegistry(t).Schema(output.KindGPUNodeList)
	if err != nil {
		t.Fatalf("generating the schema: %v", err)
	}
	if !bytes.Equal(raw, generated) {
		t.Error("the written file is not what the generator produces")
	}
}

func TestWriteDirRejectsAnArgumentAsWell(t *testing.T) {
	t.Parallel()

	_, _, err := run(t, testRegistry(t), "--write-dir", t.TempDir(), output.KindGPUNode)
	if err == nil {
		t.Fatal("--write-dir with a kind was accepted: it is ambiguous which one the caller meant")
	}
	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code is %d, want %d", got, output.ExitUsage)
	}
}

// jsonFiles lists the schema files of a directory, sorted.
func jsonFiles(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	out := []string{}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) == ".json" {
			out = append(out, entry.Name())
		}
	}

	return out
}

// NewCommand is what the root tree adds. It has to read the registry the other
// commands register their kinds into, rather than one of its own.
//
// Nothing here registers a kind into that registry: TestCheckedInSchemasAreUpToDate
// compares it against docs/schema, so a kind registered by a test would read as
// drift in a directory no test is allowed to write to.
func TestNewCommandReadsTheDefaultRegistry(t *testing.T) {
	t.Parallel()

	var out, errOut bytes.Buffer
	o := &cmd.Options{Streams: genericiooptions.IOStreams{Out: &out, ErrOut: &errOut}}
	c := NewCommand(o)
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs([]string{"GPUNodeList"})
	c.SilenceUsage = true
	c.SilenceErrors = true

	err := c.Execute()
	kinds := output.DefaultSchemaRegistry().Kinds()
	if len(kinds) == 0 {
		// No track has registered a kind yet, so the command has to say so
		// rather than print the schema of the registry this file builds.
		if err == nil {
			t.Fatalf("the command printed something for GPUNodeList while nothing is registered: %s", out.String())
		}
		if got := output.ExitCodeFor(err); got != output.ExitUsage {
			t.Errorf("exit code is %d, want %d", got, output.ExitUsage)
		}

		return
	}

	// Once a track registers GPUNodeList, the command has to print exactly what
	// the default registry generates for it.
	want, genErr := output.DefaultSchemaRegistry().Schema("GPUNodeList")
	if genErr != nil {
		t.Skipf("GPUNodeList is not registered: %v", genErr)
	}
	if err != nil {
		t.Fatalf("running the command over the default registry: %v", err)
	}
	if out.String() != string(want) {
		t.Error("the command did not print what the default registry generates")
	}
}

// --write-dir writes files. It is the generator behind "make schema", not
// something a model should be handed, so the command marks it for the MCP
// server to leave out.
func TestWriteDirIsMarkedForTheMCPServerToSuppress(t *testing.T) {
	t.Parallel()

	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	flag := NewCommand(o).Flags().Lookup("write-dir")
	if flag == nil {
		t.Fatal("--write-dir is not registered")
	}
	if values := flag.Annotations[mcp.SuppressAnnotation]; len(values) == 0 {
		t.Errorf("--write-dir carries no %q annotation, so the MCP server would offer it",
			mcp.SuppressAnnotation)
	}
}
