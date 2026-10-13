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

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// connectionFlags are the persistent flags that say which cluster to talk to
// and as whom. The real tree gets them from genericclioptions.ConfigFlags; the
// fixture lists them so that a test can assert none of them is ever offered.
var connectionFlags = []string{
	"kubeconfig", "context", "cluster", "user", "server", "token",
	"as", "as-group", "as-uid", "as-user-extra",
	"certificate-authority", "client-certificate", "client-key",
	"insecure-skip-tls-verify", "tls-server-name", "proxy-url",
	"cache-dir", "request-timeout", "disable-compression", "debug",
}

// codedError is an error that decides its own exit code, as the real commands'
// errors do.
type codedError struct {
	code int
	msg  string
}

func (e codedError) Error() string        { return e.msg }
func (e codedError) ExitCode() int        { return e.code }
func (e codedError) String() string       { return e.msg }
func newCoded(code int, msg string) error { return codedError{code: code, msg: msg} }

// testTree is a command tree shaped like the real one: the commands that never
// become tools, a list-like leaf, leaves with required and optional positional
// arguments, a hidden leaf, the leaves that exercise each exit code and a
// panic, and the one leaf that can run something inside a pod.
//
// It is built here rather than imported from pkg/cmd/root because this package
// is what root adds: an import of root from here would be a cycle. The tests in
// integration_test.go run the same checks against the real tree.
func testTree(deps CallDeps) *cobra.Command {
	streams := deps.Streams

	root := &cobra.Command{Use: "kubectl-nv"}
	for _, name := range connectionFlags {
		root.PersistentFlags().String(name, "", "Connection setting "+name)
	}
	root.PersistentFlags().StringP("namespace", "n", "", "The namespace scope for this request")

	root.AddCommand(&cobra.Command{Use: "version", Short: "Print the version", RunE: noop})
	root.AddCommand(&cobra.Command{Use: "completion", Short: "Shell completion", RunE: noop})
	root.AddCommand(&cobra.Command{Use: "mcp", Short: "Run the MCP server", RunE: noop})
	root.AddCommand(&cobra.Command{Use: "help", Short: "Help", RunE: noop})
	root.AddCommand(&cobra.Command{Use: "secret-leaf", Short: "Hidden", Hidden: true, RunE: noop})

	list := &cobra.Command{
		Use:   "list",
		Short: "List the GPU nodes",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			limit, _ := c.Flags().GetInt("limit")
			format, _ := c.Flags().GetString("output")
			namespace, _ := c.Flags().GetString("namespace")
			fmt.Fprintf(streams.Out,
				`{"kind":"GPUNodeList","limit":%d,"format":%q,"namespace":%q,"kubeconfig":%q}`+"\n",
				limit, format, namespace, deps.KubeconfigPath)

			return nil
		},
	}
	list.Flags().Int("limit", 0, "Print at most this many items")
	list.Flags().StringSlice("fields", nil, "Keep only these dotted JSON paths")
	list.Flags().StringP("output", "o", "table", "Output format")
	list.Flags().Bool("all-namespaces", false, "Look in every namespace")
	root.AddCommand(list)

	explain := &cobra.Command{
		Use:   "explain <CODE>",
		Short: "Explain a finding code",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			fmt.Fprintf(streams.Out, `{"kind":"Explanation","code":%q}`+"\n", args[0])

			return nil
		},
	}
	explain.Flags().StringP("output", "o", "table", "Output format")
	explain.Flags().String("write-dir", "", "Write the explanations into this directory")
	mustAnnotate(explain, "write-dir", SuppressAnnotation)
	root.AddCommand(explain)

	describe := &cobra.Command{
		Use:   "describe <KIND> [NAME]",
		Short: "Describe a kind",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(c *cobra.Command, args []string) error {
			fmt.Fprintf(streams.Out, `{"kind":"Description","args":%d}`+"\n", len(args))

			return nil
		},
	}
	root.AddCommand(describe)

	root.AddCommand(&cobra.Command{
		Use:   "findings",
		Short: "Print a report and report findings",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprintln(streams.Out, `{"kind":"HealthReport","status":{"findings":[{"code":"GPU001"}]}}`)

			return newCoded(output.ExitFindings, "found 1 finding at or above error")
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "partial",
		Short: "Print a report over part of the cluster",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			fmt.Fprintln(streams.Out, `{"kind":"HealthReport","status":{"partial":true,"findings":[]}}`)

			return newCoded(output.ExitPartial, "nodes/node-a/proxy was forbidden")
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "broken",
		Short: "Fail without a code",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("the API server could not be reached")
		},
	})

	root.AddCommand(&cobra.Command{
		Use:   "boom",
		Short: "Panic",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			panic("a nil map write somewhere deep")
		},
	})

	adm := &cobra.Command{Use: "adm", Short: "Admin level operations"}
	mustGather := &cobra.Command{
		Use:         "must-gather",
		Short:       "Collect a support bundle",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{ExecAnnotation: "true"},
		RunE: func(c *cobra.Command, _ []string) error {
			noExec, _ := c.Flags().GetBool("no-exec")
			dir, _ := c.Flags().GetString("artifacts-dir")
			fmt.Fprintf(streams.Out, `{"kind":"BundleManifest","noExec":%t,"artifactsDir":%q}`+"\n", noExec, dir)

			return nil
		},
	}
	mustGather.Flags().Bool("no-exec", false, "Never exec into a pod")
	mustGather.Flags().String("artifacts-dir", "", "Where the bundle goes")
	mustAnnotate(mustGather, "artifacts-dir", HostDirAnnotation)
	adm.AddCommand(mustGather)
	root.AddCommand(adm)

	return root
}

// mustAnnotate marks a flag, failing loudly when the flag is not there: a
// fixture whose annotation silently did not apply would test nothing.
func mustAnnotate(c *cobra.Command, flag, annotation string) {
	if err := c.Flags().SetAnnotation(flag, annotation, []string{"true"}); err != nil {
		panic(fmt.Sprintf("annotating --%s of %s: %v", flag, c.Name(), err))
	}
}

func noop(*cobra.Command, []string) error { return nil }

// discardDeps is the fixture tree built only to be walked.
func discardDeps() CallDeps { return CallDeps{Streams: discardStreams()} }

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
// without consulting the code under test.
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
// independently of Tools: every leaf, less the four that are never tools and
// the hidden ones.
func expectedToolNames(c *cobra.Command) []string {
	never := map[string]bool{"mcp": true, "help": true, "completion": true, "version": true}
	hidden := map[string]bool{}
	var mark func(*cobra.Command, []string)
	mark = func(cmd *cobra.Command, path []string) {
		for _, child := range cmd.Commands() {
			childPath := append(append([]string{}, path...), child.Name())
			if child.Hidden {
				hidden[strings.Join(childPath, "_")] = true
			}
			mark(child, childPath)
		}
	}
	mark(c, nil)

	var out []string
	for _, path := range leavesOf(c) {
		if never[path[len(path)-1]] {
			continue
		}
		name := strings.Join(path, "_")
		if hidden[name] {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)

	return out
}

func TestToolsAreEveryLeafExceptTheFourThatAreNever(t *testing.T) {
	t.Parallel()

	tree := testTree(discardDeps())
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

	names := toolNames(Tools(testTree(discardDeps())))
	for _, never := range []string{"mcp", "help", "completion", "version"} {
		for _, name := range names {
			if name == never {
				t.Errorf("%q is offered as a tool: a model must not be able to run it", never)
			}
		}
	}
}

// A hidden command is hidden because it is not part of the interface. Offering
// it over MCP would publish it to the one caller that reads every tool.
func TestAHiddenCommandIsNeverATool(t *testing.T) {
	t.Parallel()

	for _, name := range toolNames(Tools(testTree(discardDeps()))) {
		if name == "secret-leaf" {
			t.Fatal("a hidden leaf is offered as a tool")
		}
	}
}

// The input schema is an allowlist. Everything that says which cluster to talk
// to, and as whom, comes from how the server was started; a model that could
// set any of it could point kubectl-nv at a cluster, a proxy or an identity
// nobody granted it.
func TestNoToolOffersAConnectionFlag(t *testing.T) {
	t.Parallel()

	for _, tool := range Tools(testTree(discardDeps())) {
		properties := schemaProperties(t, tool)
		for _, name := range connectionFlags {
			if _, offered := properties[name]; offered {
				t.Errorf("%s offers --%s: connection settings come from the server's own start", tool.Name, name)
			}
		}
	}
}

// Exactly one inherited flag is allowed through, because which namespace to
// look in is a question about the request rather than about the connection.
func TestNamespaceIsTheOneInheritedFlagOffered(t *testing.T) {
	t.Parallel()

	tool := toolByName(t, Tools(testTree(discardDeps())), "list")
	properties := schemaProperties(t, tool)

	namespace, ok := properties["namespace"].(map[string]any)
	if !ok {
		t.Fatalf("list does not offer --namespace: %v", sortedPropertyNames(properties))
	}
	if namespace["type"] != "string" {
		t.Errorf("namespace is typed %v, want string", namespace["type"])
	}

	// Everything else offered has to be one of the leaf's own flags, or args.
	own := map[string]bool{"limit": true, "fields": true, "all-namespaces": true, "namespace": true}
	for _, name := range sortedPropertyNames(properties) {
		if !own[name] {
			t.Errorf("list offers %q, which is neither its own flag nor namespace", name)
		}
	}
}

// The input schema is what a model fills in, so a leaf's own flags have to be
// there with the right types.
func TestToolInputSchemaIsBuiltFromTheLeafsOwnFlags(t *testing.T) {
	t.Parallel()

	properties := schemaProperties(t, toolByName(t, Tools(testTree(discardDeps())), "list"))

	for name, wantType := range map[string]string{
		"limit":          "integer",
		"fields":         "array",
		"all-namespaces": "boolean",
	} {
		property, ok := properties[name].(map[string]any)
		if !ok {
			t.Errorf("the schema has no %q property: %v", name, sortedPropertyNames(properties))
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

// sortedPropertyNames returns the keys of a properties map, sorted.
func sortedPropertyNames(properties map[string]any) []string {
	out := make([]string, 0, len(properties))
	for name := range properties {
		out = append(out, name)
	}
	sort.Strings(out)

	return out
}

// A leaf that takes no positional argument must not advertise one, and one that
// requires an argument has to say how many it needs.
func TestPositionalArgumentsCarryTheirArity(t *testing.T) {
	t.Parallel()

	tools := Tools(testTree(discardDeps()))

	for _, tc := range []struct {
		tool    string
		offered bool
		min     float64
		max     float64
	}{
		{tool: "explain", offered: true, min: 1, max: 1},
		{tool: "describe", offered: true, min: 1, max: 2},
		{tool: "list", offered: false},
		{tool: "adm_must-gather", offered: false},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			properties := schemaProperties(t, toolByName(t, tools, tc.tool))
			args, ok := properties["args"].(map[string]any)
			if ok != tc.offered {
				t.Fatalf("%s offers args: %t, want %t", tc.tool, ok, tc.offered)
			}
			if !tc.offered {
				return
			}
			if args["type"] != "array" {
				t.Errorf("args is typed %v, want array", args["type"])
			}
			if args["minItems"] != tc.min {
				t.Errorf("minItems is %v, want %v", args["minItems"], tc.min)
			}
			if args["maxItems"] != tc.max {
				t.Errorf("maxItems is %v, want %v", args["maxItems"], tc.max)
			}
		})
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
	if decoded.Properties == nil {
		return map[string]any{}
	}

	return decoded.Properties
}

// The exec gate is driven by an annotation the command carries, not by this
// package recognising a name. A command renamed, or a new one that execs, has
// to stay gated without this package being edited.
func TestTheExecGateIsDrivenByTheCommandsAnnotation(t *testing.T) {
	t.Parallel()

	tools := Tools(testTree(discardDeps()))

	gated := toolByName(t, tools, "adm_must-gather")
	if !gated.ForceNoExec {
		t.Error("the annotated command is not exec gated")
	}
	if _, offered := schemaProperties(t, gated)["no-exec"]; offered {
		t.Error("the exec gated tool offers --no-exec: a model could set it to false")
	}

	for _, tool := range tools {
		if tool.Name == "adm_must-gather" {
			continue
		}
		if tool.ForceNoExec {
			t.Errorf("%s is exec gated but carries no annotation", tool.Name)
		}
	}
}

// Renaming the command must not open the gate, which is the whole point of
// gating on the annotation.
func TestRenamingAnExecCommandKeepsItGated(t *testing.T) {
	t.Parallel()

	tree := testTree(discardDeps())
	for _, group := range tree.Commands() {
		if group.Name() != "adm" {
			continue
		}
		for _, leaf := range group.Commands() {
			if leaf.Name() == "must-gather" {
				leaf.Use = "collect"
			}
		}
	}

	tool := toolByName(t, Tools(tree), "adm_collect")
	if !tool.ForceNoExec {
		t.Error("the renamed command lost its exec gate")
	}
}

// A flag naming a path on the machine kubectl-nv runs on is never filled in by
// a model: the server either pins it or leaves it out.
func TestHostPathFlagsAreNeverOffered(t *testing.T) {
	t.Parallel()

	tools := Tools(testTree(discardDeps()))

	if _, offered := schemaProperties(t, toolByName(t, tools, "explain"))["write-dir"]; offered {
		t.Error("explain offers --write-dir, which it marked as suppressed")
	}

	gather := toolByName(t, tools, "adm_must-gather")
	if _, offered := schemaProperties(t, gather)["artifacts-dir"]; offered {
		t.Error("must-gather offers --artifacts-dir: the server pins it")
	}
	if want := []string{"artifacts-dir"}; !reflect.DeepEqual(gather.HostDirFlags, want) {
		t.Errorf("HostDirFlags is %v, want %v", gather.HostDirFlags, want)
	}
}

// A tool that writes files on the host is not read-only, whatever it does to
// the cluster. Saying otherwise in the annotations would be a lie a model acts
// on.
func TestAToolThatWritesOnTheHostIsNotMarkedReadOnly(t *testing.T) {
	t.Parallel()

	for _, tool := range Tools(testTree(discardDeps())) {
		writes := tool.Name == "adm_must-gather"
		if tool.WritesHost != writes {
			t.Errorf("%s has WritesHost=%t, want %t", tool.Name, tool.WritesHost, writes)
		}
	}
}
