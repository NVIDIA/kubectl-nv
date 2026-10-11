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
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// connect runs a server over a tree and returns a client session talking to it
// through an in-memory transport, which is the whole protocol without a process
// boundary.
func connect(t *testing.T, newRoot NewRootFunc) *sdk.ClientSession {
	t.Helper()

	server, err := NewServer(newRoot, "test")
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}

	return sessionFor(t, server)
}

// sessionFor connects a client to an already built server.
func sessionFor(t *testing.T, server *sdk.Server) *sdk.ClientSession {
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

// refusal is the exact message a refused call carries, without the exit code
// line every result ends with. Asserting it whole matters: a substring check
// for "--no-exec" would also match cobra's own "unknown flag: --no-exec",
// which is what a tool that was run anyway would say.
func refusal(t *testing.T, result *sdk.CallToolResult) string {
	t.Helper()

	message, _, found := strings.Cut(text(t, result), "\n\nexit code ")
	if !found {
		t.Fatalf("the result carries no exit code line: %q", text(t, result))
	}

	return message
}

// structured returns the structured content of a result as an object.
func structured(t *testing.T, result *sdk.CallToolResult) map[string]any {
	t.Helper()

	object, ok := result.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structuredContent is %T, want the decoded JSON object (text was %q)",
			result.StructuredContent, text(t, result))
	}

	return object
}

// exitCode reads the exit code a result carries in its metadata.
func exitCode(t *testing.T, result *sdk.CallToolResult) int {
	t.Helper()

	raw, carried := result.Meta[ExitCodeMetaKey]
	if !carried {
		t.Fatalf("the result carries no %s in _meta: %v", ExitCodeMetaKey, result.Meta)
	}
	code, ok := raw.(float64)
	if !ok {
		t.Fatalf("%s is %T, want a number", ExitCodeMetaKey, raw)
	}

	return int(code)
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

	want := expectedToolNames(testTree(discardDeps()))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the server offered %v,\nwant %v", got, want)
	}
}

// The whole published surface has to be free of connection settings, not only
// the ones a unit test thought to look at.
func TestNoToolOverTheProtocolOffersAConnectionFlag(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing the tools: %v", err)
	}

	for _, tool := range listed.Tools {
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling the schema of %s: %v", tool.Name, err)
		}
		var decoded struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decoding the schema of %s: %v", tool.Name, err)
		}
		for _, name := range connectionFlags {
			if _, offered := decoded.Properties[name]; offered {
				t.Errorf("%s offers --%s over the protocol", tool.Name, name)
			}
		}
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

	object := structured(t, result)
	if object["kind"] != "GPUNodeList" {
		t.Errorf("kind is %v, want GPUNodeList", object["kind"])
	}
	if object["limit"] != float64(2) {
		t.Errorf("limit is %v, want 2: the flag did not reach the command", object["limit"])
	}
	if object["format"] != "json" {
		t.Errorf("format is %v, want json", object["format"])
	}
	if !strings.Contains(text(t, result), `"kind":"GPUNodeList"`) {
		t.Errorf("the text content is not what the command printed: %q", text(t, result))
	}
	if got := exitCode(t, result); got != output.ExitOK {
		t.Errorf("exit code is %d, want %d", got, output.ExitOK)
	}
}

func TestTheOneAllowedInheritedFlagReachesTheCommand(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	object := structured(t, call(t, session, "list", map[string]any{"namespace": "gpu-operator"}))
	if object["namespace"] != "gpu-operator" {
		t.Errorf("namespace is %v, want gpu-operator", object["namespace"])
	}
}

// Which cluster the tools talk to is decided when the server is started, not
// per call. A server started against one kubeconfig must not answer from
// another, whatever the environment says.
func TestEveryCallUsesTheConnectionTheServerWasStartedWith(t *testing.T) {
	t.Setenv("KUBECONFIG", "/the/environment/kubeconfig")

	const chosen = "/the/kubeconfig/mcp/was/given"
	newRoot := func(deps CallDeps) *cobra.Command {
		if deps.KubeconfigPath == "" {
			deps.KubeconfigPath = os.Getenv("KUBECONFIG")
		}

		return testTree(deps)
	}

	server, err := NewServer(newRoot, "test")
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	SetConnection(server, CallDeps{KubeconfigPath: chosen})

	object := structured(t, call(t, sessionFor(t, server), "list", map[string]any{}))
	if object["kubeconfig"] != chosen {
		t.Errorf("the call used kubeconfig %v, want %q: the server's own start has to decide",
			object["kubeconfig"], chosen)
	}
}

func TestAPositionalArgumentReachesTheCommand(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	object := structured(t, call(t, session, "explain", map[string]any{"args": []any{"GPU001"}}))
	if object["code"] != "GPU001" {
		t.Errorf("code is %v, want GPU001", object["code"])
	}
}

// must-gather is the one command that can run something inside a pod. Over MCP
// it runs with --no-exec forced on, and there must be no way to turn it off.
func TestMustGatherCannotTurnExecOn(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)

	t.Run("the flag is forced on", func(t *testing.T) {
		result := call(t, session, "adm_must-gather", map[string]any{})
		if result.IsError {
			t.Fatalf("the call reported an error: %s", text(t, result))
		}
		if structured(t, result)["noExec"] != true {
			t.Errorf("noExec is %v, want true: the server did not force --no-exec",
				structured(t, result)["noExec"])
		}
	})

	t.Run("asking for the flag is refused by name", func(t *testing.T) {
		result := call(t, session, "adm_must-gather", map[string]any{"no-exec": false})
		if !result.IsError {
			t.Fatalf("setting no-exec was accepted: %s", text(t, result))
		}
		want := `adm_must-gather does not take "no-exec"`
		if refusal(t, result) != want {
			t.Errorf("refusal is %q, want exactly %q", refusal(t, result), want)
		}
	})

	t.Run("smuggling the flag through args is refused by name", func(t *testing.T) {
		result := call(t, session, "adm_must-gather", map[string]any{"args": []any{"--no-exec=false"}})
		if !result.IsError {
			t.Fatalf("a flag passed as a positional argument was accepted: %s", text(t, result))
		}
		// The value is not echoed back: a rejected --token or --password must
		// not be copied into the model's context or into the session log.
		want := `adm_must-gather: "--no-exec" is a flag, ` +
			`and a flag cannot be passed as a positional argument`
		if refusal(t, result) != want {
			t.Errorf("refusal is %q, want exactly %q", refusal(t, result), want)
		}
	})
}

// Connection settings are not reachable through the args array either.
func TestConnectionFlagsCannotBeSmuggledThroughArgs(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)

	for _, smuggled := range []string{"--server=https://evil.invalid", "--token=hunter2"} {
		result := call(t, session, "explain", map[string]any{"args": []any{"GPU001", smuggled}})
		if !result.IsError {
			t.Fatalf("%s was accepted as a positional argument: %s", smuggled, text(t, result))
		}
		if strings.Contains(text(t, result), "hunter2") || strings.Contains(text(t, result), "evil.invalid") {
			t.Errorf("the refusal echoed the rejected value back: %q", text(t, result))
		}
		name, _, _ := strings.Cut(smuggled, "=")
		want := fmt.Sprintf("explain: %q is a flag, and a flag cannot be passed as a positional argument", name)
		if refusal(t, result) != want {
			t.Errorf("refusal is %q, want exactly %q", refusal(t, result), want)
		}
	}
}

// An exec capable command that has no --no-exec flag cannot be made safe, so it
// is refused rather than run. The message is asserted whole: a substring check
// would also match cobra's own "unknown flag: --no-exec", which is what a tool
// that was run anyway would say.
func TestAnExecCapableCommandWithoutTheFlagIsRefused(t *testing.T) {
	t.Parallel()

	newRoot := func(deps CallDeps) *cobra.Command {
		root := testTree(deps)
		for _, group := range root.Commands() {
			if group.Name() != "adm" {
				continue
			}
			for _, leaf := range group.Commands() {
				if leaf.Name() == "must-gather" {
					leaf.ResetFlags()
					leaf.Flags().String("artifacts-dir", "", "Where the bundle goes")
					mustAnnotate(leaf, "artifacts-dir", HostDirAnnotation)
				}
			}
		}

		return root
	}

	result := call(t, connect(t, newRoot), "adm_must-gather", map[string]any{})
	if !result.IsError {
		t.Fatalf("a command that cannot be told not to exec was run anyway: %s", text(t, result))
	}
	want := "adm_must-gather can run a command inside a pod and has no --no-exec flag, " +
		"so it is not served over MCP"
	if refusal(t, result) != want {
		t.Errorf("refusal is %q,\nwant exactly %q", refusal(t, result), want)
	}
}

// The directory must-gather writes into is the server's choice, private to the
// call, and reported back so the caller can find the bundle.
func TestTheArtifactsDirectoryIsPinnedByTheServer(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	result := call(t, session, "adm_must-gather", map[string]any{})
	if result.IsError {
		t.Fatalf("the call reported an error: %s", text(t, result))
	}

	dir, ok := structured(t, result)["artifactsDir"].(string)
	if !ok || dir == "" {
		t.Fatalf("the command was given no artifacts directory: %v", result.StructuredContent)
	}
	if parent := filepath.Dir(dir); parent != strings.TrimSuffix(os.TempDir(), string(os.PathSeparator)) {
		t.Errorf("the artifacts directory is %q, want one under the OS temp dir %q", dir, os.TempDir())
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("the pinned directory was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("the pinned directory is mode %o, want 700: a bundle holds cluster detail", perm)
	}
	if !strings.Contains(text(t, result), dir) {
		t.Errorf("the result does not tell the caller where the bundle is: %q", text(t, result))
	}

	// A second call gets its own directory, so one call cannot read another's.
	second := call(t, session, "adm_must-gather", map[string]any{})
	if other := structured(t, second)["artifactsDir"]; other == dir {
		t.Error("two calls were given the same artifacts directory")
	}
}

func TestAskingForTheArtifactsDirectoryIsRefused(t *testing.T) {
	t.Parallel()

	result := call(t, connect(t, testTree), "adm_must-gather", map[string]any{"artifacts-dir": "/etc"})
	if !result.IsError {
		t.Fatalf("a model chose where the bundle goes: %s", text(t, result))
	}
	want := `adm_must-gather does not take "artifacts-dir"`
	if refusal(t, result) != want {
		t.Errorf("refusal is %q, want exactly %q", refusal(t, result), want)
	}
}

// Every tool refuses a positional argument that looks like a flag, not only the
// exec gated one.
func TestNoToolAcceptsAFlagAsAPositionalArgument(t *testing.T) {
	t.Parallel()

	result := call(t, connect(t, testTree), "explain", map[string]any{"args": []any{"--kubeconfig=/etc/passwd"}})
	if !result.IsError {
		t.Fatalf("a flag passed as a positional argument was accepted: %s", text(t, result))
	}
}

// A tool call must not leave its flags behind for the next one: cobra does not
// reset a flag between two Execute calls on the same tree.
func TestOneCallDoesNotLeakItsFlagsIntoTheNext(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)

	if limit := structured(t, call(t, session, "list", map[string]any{"limit": 7}))["limit"]; limit != float64(7) {
		t.Fatalf("the first call saw limit %v, want 7", limit)
	}
	if limit := structured(t, call(t, session, "list", map[string]any{}))["limit"]; limit != float64(0) {
		t.Errorf("the second call saw limit %v, want 0: the first call's flag is still set", limit)
	}
}

// A command that found something is not a command that failed. Exit 1 and 3
// carry a report the caller needs, so the report is the result and the code
// rides along; only 2 and 4 mean there is nothing to read.
func TestExitCodesReachTheCaller(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)

	for _, tc := range []struct {
		name     string
		tool     string
		args     map[string]any
		wantCode int
		wantErr  bool
		wantKind string
	}{
		{name: "findings", tool: "findings", wantCode: output.ExitFindings, wantKind: "HealthReport"},
		{name: "partial", tool: "partial", wantCode: output.ExitPartial, wantKind: "HealthReport"},
		{name: "usage", tool: "explain", args: map[string]any{}, wantCode: output.ExitUsage, wantErr: true},
		{name: "could not run", tool: "broken", wantCode: output.ExitError, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := tc.args
			if args == nil {
				args = map[string]any{}
			}
			result := call(t, session, tc.tool, args)

			if result.IsError != tc.wantErr {
				t.Fatalf("isError is %t, want %t (text %q)", result.IsError, tc.wantErr, text(t, result))
			}
			if got := exitCode(t, result); got != tc.wantCode {
				t.Errorf("exit code is %d, want %d", got, tc.wantCode)
			}
			if !strings.Contains(text(t, result), fmt.Sprintf("exit code %d", tc.wantCode)) {
				t.Errorf("the text does not state the exit code: %q", text(t, result))
			}
			if tc.wantKind == "" {
				return
			}
			if kind := structured(t, result)["kind"]; kind != tc.wantKind {
				t.Errorf("structured content is %v, want the report the command printed", kind)
			}
		})
	}
}

// The SDK runs handlers on its own goroutines and does not recover. A panic in
// one command would take the whole server down, and the caller would see a
// dropped connection rather than a failure it could act on.
func TestAPanicComesBackAsAToolErrorRatherThanKillingTheServer(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)

	result := call(t, session, "boom", map[string]any{})
	if !result.IsError {
		t.Fatalf("a panicking command reported success: %s", text(t, result))
	}
	if got := exitCode(t, result); got != output.ExitError {
		t.Errorf("exit code is %d, want %d", got, output.ExitError)
	}
	if !strings.Contains(text(t, result), "a nil map write somewhere deep") {
		t.Errorf("the tool error does not carry what panicked: %q", text(t, result))
	}

	// The session is still usable, which is the point of recovering.
	if kind := structured(t, call(t, session, "list", map[string]any{}))["kind"]; kind != "GPUNodeList" {
		t.Errorf("the server did not survive the panic: %v", kind)
	}
}

// stdout belongs to the transport. A command that writes to the process stdout
// instead of its own stream would inject bytes into the JSON-RPC frame.
func TestNothingAToolPrintsReachesTheProcessStdout(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("making a pipe: %v", err)
	}
	realStdout := os.Stdout
	os.Stdout = write
	t.Cleanup(func() { os.Stdout = realStdout })

	session := connect(t, testTree)
	call(t, session, "loud", map[string]any{})

	if err := write.Close(); err != nil {
		t.Fatalf("closing the pipe: %v", err)
	}
	leaked, err := io.ReadAll(read)
	if err != nil {
		t.Fatalf("reading the pipe: %v", err)
	}
	if len(leaked) != 0 {
		t.Errorf("a tool call wrote %q to the process stdout, which is the transport", leaked)
	}
}

// The SDK serves calls concurrently. Anything the tree sets up globally has to
// be set up once, or two calls racing on it is a data race in the plugin.
func TestConcurrentToolCallsAreRaceFree(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)

	const calls = 40
	var wg sync.WaitGroup
	errs := make(chan string, calls)
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			result, err := session.CallTool(context.Background(),
				&sdk.CallToolParams{Name: "list", Arguments: map[string]any{"limit": i}})
			if err != nil {
				errs <- err.Error()

				return
			}
			object, ok := result.StructuredContent.(map[string]any)
			if !ok {
				errs <- "no structured content"

				return
			}
			if object["limit"] != float64(i) {
				errs <- "a call saw another call's limit"
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for message := range errs {
		t.Error(message)
	}
}

func TestAnUnknownPropertyIsRefused(t *testing.T) {
	t.Parallel()

	result := call(t, connect(t, testTree), "list", map[string]any{"no-such-flag": "x"})
	if !result.IsError {
		t.Fatalf("an unknown property was accepted: %s", text(t, result))
	}
	want := `list does not take "no-such-flag"`
	if refusal(t, result) != want {
		t.Errorf("refusal is %q, want exactly %q", refusal(t, result), want)
	}
}

// The hint a client reads is the one the server published, not the field on
// the Tool struct behind it. A model plans around readOnlyHint, so a host
// writing tool announced as read-only is a lie it acts on.
func TestThePublishedReadOnlyHintMatchesWhatTheToolDoes(t *testing.T) {
	t.Parallel()

	session := connect(t, testTree)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing the tools: %v", err)
	}
	if len(listed.Tools) == 0 {
		t.Fatal("the server offered no tools")
	}

	var sawWriter bool
	for _, tool := range listed.Tools {
		if tool.Annotations == nil {
			t.Errorf("%s carries no annotations", tool.Name)
			continue
		}
		writes := tool.Name == "adm_must-gather"
		if writes {
			sawWriter = true
		}
		if tool.Annotations.ReadOnlyHint == writes {
			t.Errorf("%s publishes readOnlyHint=%t, want %t: it %s files on this machine",
				tool.Name, tool.Annotations.ReadOnlyHint, !writes,
				map[bool]string{true: "writes", false: "writes no"}[writes])
		}
	}
	if !sawWriter {
		t.Fatal("no host writing tool was offered, so the hint was not exercised")
	}
}
