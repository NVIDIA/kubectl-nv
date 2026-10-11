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
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// connect runs a server over the fixture tree and returns a client session
// talking to it through an in-memory transport, which is the whole protocol
// without a process boundary.
func connect(t *testing.T, newRoot NewRootFunc) *sdk.ClientSession {
	t.Helper()

	server, err := NewServer(newRoot, "test")
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}

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

	return session
}

// call runs one tool and returns the result.
func call(t *testing.T, session *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}

	return result
}

// text returns the text content of a result.
func text(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()

	var parts []string
	for _, content := range result.Content {
		if textContent, ok := content.(*sdk.TextContent); ok {
			parts = append(parts, textContent.Text)
		}
	}

	return strings.Join(parts, "\n")
}

func TestToolListOverTheProtocolIsTheToolListOfTheTree(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing the tools: %v", err)
	}

	got := make([]string, 0, len(listed.Tools))
	for _, tool := range listed.Tools {
		got = append(got, tool.Name)
	}
	sort.Strings(got)

	want := expectedToolNames(testTree(genericiooptions.NewTestIOStreamsDiscard()))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the server offered %v,\nwant %v", got, want)
	}
}

// A tool call has to come back as both the text the command printed and the
// structured value a program can read without parsing it again.
func TestAToolCallRoundTripsAsTextAndStructuredContent(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	result := call(t, session, "list", map[string]any{"limit": 2})

	if result.IsError {
		t.Fatalf("the call reported an error: %s", text(t, result))
	}

	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is %T, want the decoded JSON object", result.StructuredContent)
	}
	if structured["kind"] != "GPUNodeList" {
		t.Errorf("kind is %v, want GPUNodeList", structured["kind"])
	}
	if structured["limit"] != float64(2) {
		t.Errorf("limit is %v, want 2: the flag did not reach the command", structured["limit"])
	}
	// The server, not the model, decides the format.
	if structured["format"] != "json" {
		t.Errorf("format is %v, want json", structured["format"])
	}

	if !strings.Contains(text(t, result), `"kind":"GPUNodeList"`) {
		t.Errorf("the text content is not what the command printed: %q", text(t, result))
	}
}

func TestAPositionalArgumentReachesTheCommand(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	result := call(t, session, "explain", map[string]any{"args": []any{"GPU001"}})

	if result.IsError {
		t.Fatalf("the call reported an error: %s", text(t, result))
	}
	structured, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is %T, want the decoded JSON object", result.StructuredContent)
	}
	if structured["code"] != "GPU001" {
		t.Errorf("code is %v, want GPU001", structured["code"])
	}
}

// must-gather is the one command that can run something inside a pod. Over MCP
// it runs with --no-exec forced on, and there must be no way to turn it off:
// not through the input schema, and not through a positional argument that
// happens to look like a flag.
func TestMustGatherCannotTurnExecOn(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)

	t.Run("the flag is forced on", func(t *testing.T) {
		result := call(t, session, "adm_must-gather", map[string]any{})
		if result.IsError {
			t.Fatalf("the call reported an error: %s", text(t, result))
		}
		structured, ok := result.StructuredContent.(map[string]any)
		if !ok {
			t.Fatalf("structuredContent is %T, want the decoded JSON object", result.StructuredContent)
		}
		if structured["noExec"] != true {
			t.Errorf("noExec is %v, want true: the server did not force --no-exec", structured["noExec"])
		}
	})

	t.Run("asking for the flag is refused", func(t *testing.T) {
		result := call(t, session, "adm_must-gather", map[string]any{"no-exec": false})
		if !result.IsError {
			t.Fatalf("setting no-exec was accepted: %s", text(t, result))
		}
	})

	t.Run("smuggling the flag through args is refused", func(t *testing.T) {
		result := call(t, session, "adm_must-gather", map[string]any{"args": []any{"--no-exec=false"}})
		if !result.IsError {
			t.Fatalf("a flag passed as a positional argument was accepted: %s", text(t, result))
		}
		if !strings.Contains(text(t, result), "--no-exec=false") {
			t.Errorf("the error does not name the rejected argument: %q", text(t, result))
		}
	})
}

// Every tool refuses a positional argument that looks like a flag, not only the
// exec gated one: otherwise the input schema is a suggestion rather than the
// interface.
func TestNoToolAcceptsAFlagAsAPositionalArgument(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	result := call(t, session, "explain", map[string]any{"args": []any{"--kubeconfig=/etc/passwd"}})
	if !result.IsError {
		t.Fatalf("a flag passed as a positional argument was accepted: %s", text(t, result))
	}
}

// An exec capable command that has no --no-exec flag cannot be made safe, so it
// is refused rather than run.
func TestAnExecCapableCommandWithoutTheFlagIsRefused(t *testing.T) {
	t.Parallel()

	newRoot := func(streams genericiooptions.IOStreams) *cobra.Command {
		root := testTree(streams)
		for _, group := range root.Commands() {
			if group.Name() != "adm" {
				continue
			}
			for _, leaf := range group.Commands() {
				if leaf.Name() == "must-gather" {
					leaf.Flags().MarkHidden("no-exec")
					leaf.ResetFlags()
					leaf.Flags().String("artifacts-dir", "", "Where the bundle goes")
				}
			}
		}

		return root
	}

	session := connect(t, newRoot)
	result := call(t, session, "adm_must-gather", map[string]any{})
	if !result.IsError {
		t.Fatalf("a command that cannot be told not to exec was run anyway: %s", text(t, result))
	}
	if !strings.Contains(text(t, result), "--no-exec") {
		t.Errorf("the refusal does not say what is missing: %q", text(t, result))
	}
}

// A tool call must not leave its flags behind for the next one: cobra does not
// reset a flag between two Execute calls on the same tree.
func TestOneCallDoesNotLeakItsFlagsIntoTheNext(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)

	first := call(t, session, "list", map[string]any{"limit": 7})
	if structured := first.StructuredContent.(map[string]any); structured["limit"] != float64(7) {
		t.Fatalf("the first call saw limit %v, want 7", structured["limit"])
	}

	second := call(t, session, "list", map[string]any{})
	structured, ok := second.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is %T", second.StructuredContent)
	}
	if structured["limit"] != float64(0) {
		t.Errorf("the second call saw limit %v, want 0: the first call's flag is still set", structured["limit"])
	}
}

// A command that fails has to come back as a tool error carrying what it said,
// not as a protocol error the model never sees.
func TestACommandThatFailsComesBackAsAToolError(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	result := call(t, session, "explain", map[string]any{})
	if !result.IsError {
		t.Fatal("explain ran with no argument and reported success")
	}
	if text(t, result) == "" {
		t.Error("the tool error carries no text")
	}
}

func TestAnUnknownPropertyIsRefused(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	result := call(t, session, "list", map[string]any{"no-such-flag": "x"})
	if !result.IsError {
		t.Fatalf("an unknown property was accepted: %s", text(t, result))
	}
}
