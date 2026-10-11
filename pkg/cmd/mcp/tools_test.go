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

package mcp

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// testTree is a command tree shaped like the real one: the four commands that
// never become tools, a list-like leaf, a leaf that takes an argument, and the
// one leaf that can run something inside a pod.
//
// It is built here rather than imported from pkg/cmd/root because this package
// is what root adds: an import of root from here would be a cycle. The test in
// integration_test.go runs the same checks against the real tree.
func testTree(streams genericiooptions.IOStreams) *cobra.Command {
	root := &cobra.Command{Use: "kubectl-nv"}
	root.PersistentFlags().Bool("debug", false, "Enable debug-level logging")
	root.PersistentFlags().String("kubeconfig", "", "Path to the kubeconfig file")

	root.AddCommand(&cobra.Command{Use: "version", Short: "Print the version", RunE: noop})
	root.AddCommand(&cobra.Command{Use: "completion", Short: "Shell completion", RunE: noop})
	root.AddCommand(&cobra.Command{Use: "mcp", Short: "Run the MCP server", RunE: noop})
	root.AddCommand(&cobra.Command{Use: "help", Short: "Help", RunE: noop})

	list := &cobra.Command{
		Use:   "list",
		Short: "List the GPU nodes",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			limit, _ := c.Flags().GetInt("limit")
			format, _ := c.Flags().GetString("output")
			fmt.Fprintf(streams.Out, `{"kind":"GPUNodeList","limit":%d,"format":%q}`+"\n", limit, format)

			return nil
		},
	}
	list.Flags().Int("limit", 0, "Print at most this many items")
	list.Flags().StringSlice("fields", nil, "Keep only these dotted JSON paths")
	list.Flags().StringP("output", "o", "table", "Output format")
	list.Flags().Bool("all-namespaces", false, "Look in every namespace")
	root.AddCommand(list)

	explain := &cobra.Command{
		Use:   "explain CODE",
		Short: "Explain a finding code",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			fmt.Fprintf(streams.Out, `{"kind":"Explanation","code":%q}`+"\n", args[0])

			return nil
		},
	}
	explain.Flags().StringP("output", "o", "table", "Output format")
	explain.Flags().String("write-dir", "", "Write the explanations into this directory")
	if err := explain.Flags().SetAnnotation("write-dir", SuppressAnnotation, []string{"true"}); err != nil {
		panic(err)
	}
	root.AddCommand(explain)

	adm := &cobra.Command{Use: "adm", Short: "Admin level operations"}
	mustGather := &cobra.Command{
		Use:   "must-gather",
		Short: "Collect a support bundle",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			noExec, _ := c.Flags().GetBool("no-exec")
			fmt.Fprintf(streams.Out, `{"kind":"BundleManifest","noExec":%t}`+"\n", noExec)

			return nil
		},
	}
	mustGather.Flags().Bool("no-exec", false, "Never exec into a pod")
	mustGather.Flags().String("artifacts-dir", "", "Where the bundle goes")
	adm.AddCommand(mustGather)
	root.AddCommand(adm)

	return root
}

func noop(*cobra.Command, []string) error { return nil }

// toolNames returns the names of a tool list.
func toolNames(tools []Tool) []string {
	out := make([]string, 0, len(tools))
	for _, tool := range tools {
		out = append(out, tool.Name)
	}

	return out
}

// toolByName returns the one tool with that name.
func toolByName(t *testing.T, tools []Tool, name string) Tool {
	t.Helper()

	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("no tool named %q in %v", name, toolNames(tools))

	return Tool{}
}

// leavesOf walks a tree and returns the path of every command that does work,
// without consulting the code under test. The tool list has to equal this list
// minus the four names that are never tools, so a command added later is
// covered without this test being edited.
func leavesOf(c *cobra.Command) [][]string {
	var out [][]string
	var walk func(*cobra.Command, []string)
	walk = func(cmd *cobra.Command, path []string) {
		children := cmd.Commands()
		if len(children) == 0 {
			if len(path) > 0 {
				out = append(out, path)
			}

			return
		}
		for _, child := range children {
			walk(child, append(append([]string{}, path...), child.Name()))
		}
	}
	walk(c, nil)

	return out
}

// expectedToolNames is the tool list derived from a tree, computed
// independently of Tools.
func expectedToolNames(c *cobra.Command) []string {
	never := map[string]bool{"mcp": true, "help": true, "completion": true, "version": true}

	var out []string
	for _, path := range leavesOf(c) {
		if never[path[len(path)-1]] {
			continue
		}
		out = append(out, strings.Join(path, "_"))
	}
	sort.Strings(out)

	return out
}

func TestToolsAreEveryLeafExceptTheFourThatAreNever(t *testing.T) {
	t.Parallel()

	tree := testTree(genericiooptions.NewTestIOStreamsDiscard())
	want := expectedToolNames(tree)
	if len(want) == 0 {
		t.Fatal("the fixture tree has no leaves to turn into tools")
	}

	if got := toolNames(Tools(tree)); !reflect.DeepEqual(got, want) {
		t.Errorf("tools are %v,\nwant %v", got, want)
	}
}

func TestToolsExcludeEachOfTheFourByName(t *testing.T) {
	t.Parallel()

	names := toolNames(Tools(testTree(genericiooptions.NewTestIOStreamsDiscard())))
	for _, never := range []string{"mcp", "help", "completion", "version"} {
		for _, name := range names {
			if name == never {
				t.Errorf("%q is offered as a tool: a model must not be able to run it", never)
			}
		}
	}
}

// The input schema is what a model fills in, so it has to be the command's own
// flags rather than a fixed set.
func TestToolInputSchemaIsBuiltFromTheFlags(t *testing.T) {
	t.Parallel()

	tool := toolByName(t, Tools(testTree(genericiooptions.NewTestIOStreamsDiscard())), "list")
	properties := schemaProperties(t, tool)

	for name, wantType := range map[string]string{
		"limit":          "integer",
		"fields":         "array",
		"all-namespaces": "boolean",
		"kubeconfig":     "string",
		"debug":          "boolean",
	} {
		property, ok := properties[name].(map[string]any)
		if !ok {
			t.Errorf("the schema has no %q property: %v", name, properties)
			continue
		}
		if property["type"] != wantType {
			t.Errorf("%q is typed %v, want %s", name, property["type"], wantType)
		}
	}

	// The server decides the format, so offering it would let a model ask for a
	// table it cannot parse.
	if _, offered := properties["output"]; offered {
		t.Error("the schema offers --output: the server forces -o json")
	}
	if _, offered := properties["help"]; offered {
		t.Error("the schema offers --help")
	}
}

// A leaf that takes no positional argument must not advertise one, or a model
// will send one and get a usage error back.
func TestOnlyACommandThatTakesArgumentsOffersThem(t *testing.T) {
	t.Parallel()

	tools := Tools(testTree(genericiooptions.NewTestIOStreamsDiscard()))

	explain := schemaProperties(t, toolByName(t, tools, "explain"))
	args, ok := explain["args"].(map[string]any)
	if !ok {
		t.Fatalf("explain offers no args property: %v", explain)
	}
	if args["type"] != "array" {
		t.Errorf("args is typed %v, want array", args["type"])
	}

	list := schemaProperties(t, toolByName(t, tools, "list"))
	if _, offered := list["args"]; offered {
		t.Error("list offers an args property, but it takes none")
	}
}

// schemaProperties decodes the properties of a tool's input schema.
func schemaProperties(t *testing.T, tool Tool) map[string]any {
	t.Helper()

	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatalf("marshalling the input schema of %s: %v", tool.Name, err)
	}
	var decoded struct {
		Type                 string         `json:"type"`
		Properties           map[string]any `json:"properties"`
		AdditionalProperties *bool          `json:"additionalProperties"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding the input schema of %s: %v", tool.Name, err)
	}
	if decoded.Type != "object" {
		t.Errorf("the input schema of %s is typed %q, want object", tool.Name, decoded.Type)
	}
	if decoded.AdditionalProperties == nil || *decoded.AdditionalProperties {
		t.Errorf("the input schema of %s accepts unknown properties", tool.Name)
	}

	return decoded.Properties
}

// must-gather is the one command that can run something inside a pod, so its
// tool must not offer the flag that decides whether it does.
func TestTheExecCapableToolDoesNotOfferTheExecFlag(t *testing.T) {
	t.Parallel()

	tool := toolByName(t, Tools(testTree(genericiooptions.NewTestIOStreamsDiscard())), "adm_must-gather")
	if !tool.ForceNoExec {
		t.Error("the must-gather tool is not marked as exec gated")
	}
	if _, offered := schemaProperties(t, tool)["no-exec"]; offered {
		t.Error("the must-gather tool offers --no-exec: a model could set it to false")
	}
}

func TestOnlyTheExecCapableToolsAreGated(t *testing.T) {
	t.Parallel()

	for _, tool := range Tools(testTree(genericiooptions.NewTestIOStreamsDiscard())) {
		gated := tool.Name == "adm_must-gather" || tool.Name == "debug"
		if tool.ForceNoExec != gated {
			t.Errorf("%s has ForceNoExec=%t, want %t", tool.Name, tool.ForceNoExec, gated)
		}
	}
}

// A flag that writes to the local filesystem is not something a model should
// be handed, even though it touches no cluster. A command marks such a flag
// itself, and the server leaves it out and refuses it if asked for anyway.
func TestASuppressedFlagIsNeitherOfferedNorAccepted(t *testing.T) {
	t.Parallel()

	tool := toolByName(t, Tools(testTree(genericiooptions.NewTestIOStreamsDiscard())), "explain")
	if _, offered := schemaProperties(t, tool)["write-dir"]; offered {
		t.Error("the explain tool offers --write-dir, which the command marked as suppressed")
	}

	if _, err := commandLine(tool, map[string]any{"write-dir": "/tmp/anywhere"}); err == nil {
		t.Error("a suppressed flag was accepted when asked for by name")
	}
}
