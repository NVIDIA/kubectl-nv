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

// This file is the external test package, so that it can import pkg/cmd/root.
// The internal tests cannot: root adds the mcp command, and an import of root
// from inside this package would be a cycle.
package mcp_test

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/mcp"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
)

// realTree builds the kubectl-nv command tree the plugin ships.
func realTree(streams genericiooptions.IOStreams) *cobra.Command {
	return root.NewCommand("test", root.Deps{Streams: streams})
}

// leafPaths walks a tree and returns the path of every command that does work,
// computed without the code under test.
func leafPaths(c *cobra.Command) [][]string {
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

// The point of deriving the tools from the tree is that a command another track
// adds becomes a tool without this package changing, so the expected list is
// computed from the real tree rather than written down.
func TestTheRealTreeOffersEveryLeafButTheFour(t *testing.T) {
	t.Parallel()

	tree := realTree(genericiooptions.NewTestIOStreamsDiscard())
	never := map[string]bool{"mcp": true, "help": true, "completion": true, "version": true}

	want := []string{}
	for _, path := range leafPaths(tree) {
		if never[path[len(path)-1]] {
			continue
		}
		want = append(want, strings.Join(path, "_"))
	}
	sort.Strings(want)
	if len(want) == 0 {
		t.Fatal("the real tree has no read-only leaf: the guard would prove nothing")
	}

	got := []string{}
	for _, tool := range mcp.Tools(tree) {
		got = append(got, tool.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the real tree offers %v,\nwant %v", got, want)
	}

	// The four are in the real tree, or excluding them proves nothing.
	seen := map[string]bool{}
	for _, path := range leafPaths(tree) {
		seen[path[len(path)-1]] = true
	}
	for _, name := range []string{"mcp", "version"} {
		if !seen[name] {
			t.Errorf("%q is not a leaf of the real tree, so excluding it is untested here", name)
		}
	}
}

// must-gather is the real exec capable command. Until it grows a --no-exec
// flag it cannot be made safe, so the server has to refuse it rather than run
// it with the exec still available.
func TestTheRealMustGatherIsNeverServedWithExecAvailable(t *testing.T) {
	t.Parallel()

	tree := realTree(genericiooptions.NewTestIOStreamsDiscard())

	var found bool
	for _, tool := range mcp.Tools(tree) {
		if tool.Name != "adm_must-gather" {
			continue
		}
		found = true
		if !tool.ForceNoExec {
			t.Fatal("the real must-gather tool is not exec gated")
		}

		server, err := mcp.NewServer(realTree, "test")
		if err != nil {
			t.Fatalf("building the server over the real tree: %v", err)
		}
		result := callOverTheProtocol(t, server, tool.Name)
		switch {
		case tool.HasNoExecFlag:
			if result.IsError {
				t.Fatalf("must-gather has --no-exec and the call still failed: %s", resultText(result))
			}
		default:
			if !result.IsError {
				t.Fatal("must-gather has no --no-exec flag and was served anyway")
			}
			if !strings.Contains(resultText(result), "--no-exec") {
				t.Errorf("the refusal does not say what is missing: %q", resultText(result))
			}
		}
	}
	if !found {
		t.Fatal("the real tree has no adm must-gather leaf")
	}
}

// callOverTheProtocol runs one tool with no arguments through an in-memory
// transport.
func callOverTheProtocol(t *testing.T, server *sdk.Server, name string) *sdk.CallToolResult {
	t.Helper()

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting the server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting the client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	result, err := session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}

	return result
}

// resultText returns the text content of a result.
func resultText(result *sdk.CallToolResult) string {
	var parts []string
	for _, content := range result.Content {
		if textContent, ok := content.(*sdk.TextContent); ok {
			parts = append(parts, textContent.Text)
		}
	}

	return strings.Join(parts, "\n")
}
