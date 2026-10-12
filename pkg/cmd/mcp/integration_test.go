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
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/mcp"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
)

// connectionFlags are the persistent flags that decide which cluster the plugin
// talks to and as whom. None of them may ever be a tool property.
var connectionFlags = []string{
	"kubeconfig", "context", "cluster", "user", "server", "token",
	"as", "as-group", "as-uid", "as-user-extra",
	"certificate-authority", "client-certificate", "client-key",
	"insecure-skip-tls-verify", "tls-server-name", "proxy-url",
	"cache-dir", "request-timeout", "disable-compression", "debug",
}

// fakeFactory hands the tree fake clients, so that nothing here can reach a
// cluster even if a command tried.
type fakeFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
	config    *rest.Config
}

func (f *fakeFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f *fakeFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f *fakeFactory) RESTConfig() (*rest.Config, error)               { return f.config, nil }

func newFakeFactory() *fakeFactory {
	return &fakeFactory{
		clientset: fake.NewSimpleClientset(),
		dynamic:   dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		config:    &rest.Config{Host: "https://kubectl-nv.invalid"},
	}
}

// gatherRecorder records the options a must-gather was asked for, instead of
// running one. Without it, a tool call would run a real gather against whatever
// kubeconfig the developer happens to have, the moment must-gather grows a
// --no-exec flag and stops being refused.
type gatherRecorder struct {
	mu    sync.Mutex
	calls []mustgather.Options
}

func (g *gatherRecorder) run(o *mustgather.Options) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, *o)

	return nil
}

func (g *gatherRecorder) seen() []mustgather.Options {
	g.mu.Lock()
	defer g.mu.Unlock()

	return append([]mustgather.Options{}, g.calls...)
}

// isolateKubeconfig points the environment at a path that does not exist, so
// that nothing in this file can read the developer's own kubeconfig. It cannot
// be emptied: client-go reads an empty KUBECONFIG as unset and falls back to
// the default location, which is the file this is protecting.
func isolateKubeconfig(t *testing.T) string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, "no-kubeconfig-here")
	t.Setenv("KUBECONFIG", path)

	return path
}

// realRoot builds the kubectl-nv command tree the plugin ships, over fakes.
func realRoot(t *testing.T) (mcp.NewRootFunc, *gatherRecorder) {
	t.Helper()

	return realRootOver(t)
}

// realRootOver is realRoot over a cluster holding the given objects, for the
// tools that do nothing worth watching against an empty one.
func realRootOver(t *testing.T, objects ...runtime.Object) (mcp.NewRootFunc, *gatherRecorder) {
	t.Helper()

	recorder := &gatherRecorder{}
	factory := newFakeFactory()
	factory.clientset = fake.NewSimpleClientset(objects...)

	return func(deps mcp.CallDeps) *cobra.Command {
		return root.NewCommand("test", root.Deps{
			Streams:     deps.Streams,
			ConfigFlags: deps.ConfigFlags,
			Factory:     factory,
			MustGather:  recorder.run,
		})
	}, recorder
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
			if child.Hidden {
				continue
			}
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
	isolateKubeconfig(t)
	newRoot, _ := realRoot(t)
	tree := newRoot(mcp.CallDeps{Streams: genericiooptions.NewTestIOStreamsDiscard()})

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

// The allowlist has to hold on the tree that ships, where the connection flags
// are real genericclioptions flags rather than a fixture's.
func TestTheRealToolListOffersNoConnectionFlag(t *testing.T) {
	isolateKubeconfig(t)
	newRoot, _ := realRoot(t)

	server, err := mcp.NewServer(newRoot, "test")
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	session := connectTo(t, server)

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing the tools: %v", err)
	}
	if len(listed.Tools) == 0 {
		t.Fatal("the real server offered no tools")
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
				t.Errorf("%s offers --%s on the real tree", tool.Name, name)
			}
		}
	}
}

// A probe in the shape of the one QA ran: a kubeconfig holding a secret, named
// only in the environment, and a tool call that tries to reach it. Nothing the
// model can say may make a call read that file.
func TestAToolCallCannotBeAimedAtAnotherKubeconfig(t *testing.T) {
	planted := filepath.Join(t.TempDir(), "planted-kubeconfig")
	contents := `apiVersion: v1
kind: Config
clusters:
- name: planted
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: planted
  context:
    cluster: planted
    user: planted
current-context: planted
users:
- name: planted
  user:
    token: super-secret-token
`
	if err := os.WriteFile(planted, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing the planted kubeconfig: %v", err)
	}

	isolateKubeconfig(t)
	newRoot, _ := realRoot(t)
	server, err := mcp.NewServer(newRoot, "test")
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}
	session := connectTo(t, server)

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing the tools: %v", err)
	}

	for _, tool := range listed.Tools {
		for _, attempt := range []map[string]any{
			{"kubeconfig": planted},
			{"token": "super-secret-token"},
			{"server": "https://127.0.0.1:1"},
			{"args": []any{"--kubeconfig=" + planted}},
			{"args": []any{"--token=super-secret-token"}},
		} {
			result, err := session.CallTool(context.Background(),
				&sdk.CallToolParams{Name: tool.Name, Arguments: attempt})
			if err != nil {
				continue
			}
			if !result.IsError {
				t.Errorf("%s accepted %v: a model steered the connection", tool.Name, attempt)
			}
			if strings.Contains(resultText(result), "super-secret-token") {
				t.Errorf("%s echoed the planted token back: %q", tool.Name, resultText(result))
			}
		}
	}
}

// must-gather is the real exec capable command. Until it grows a --no-exec flag
// it cannot be made safe, so the server refuses it; the refusal is asserted
// whole, because a substring check would also match cobra's own "unknown flag:
// --no-exec", which is what a tool that was run anyway would say.
func TestTheRealMustGatherIsNeverServedWithExecAvailable(t *testing.T) {
	isolateKubeconfig(t)
	newRoot, recorder := realRoot(t)
	tree := newRoot(mcp.CallDeps{Streams: genericiooptions.NewTestIOStreamsDiscard()})

	var found bool
	for _, tool := range mcp.Tools(tree) {
		if tool.Name != "adm_must-gather" {
			continue
		}
		found = true
		if !tool.ForceNoExec {
			t.Fatal("the real must-gather tool is not exec gated")
		}
		if !tool.WritesHost {
			t.Error("the real must-gather tool is marked as writing nothing on the host")
		}

		server, err := mcp.NewServer(newRoot, "test")
		if err != nil {
			t.Fatalf("building the server over the real tree: %v", err)
		}
		result := callTool(t, connectTo(t, server), tool.Name)

		if tool.HasNoExecFlag {
			if result.IsError {
				t.Fatalf("must-gather has --no-exec and the call still failed: %s", resultText(result))
			}
			seen := recorder.seen()
			if len(seen) != 1 {
				t.Fatalf("the gather ran %d times, want once", len(seen))
			}
			if seen[0].ArtifactDir == "" {
				t.Error("the gather was given no artifacts directory")
			}

			return
		}

		want := "adm_must-gather can run a command inside a pod and has no --no-exec flag, " +
			"so it is not served over MCP"
		if !result.IsError {
			t.Fatal("must-gather has no --no-exec flag and was served anyway")
		}
		if refusalText(result) != want {
			t.Errorf("refusal is %q,\nwant exactly %q", refusalText(result), want)
		}
		if len(recorder.seen()) != 0 {
			t.Error("a refused call still ran a gather")
		}
	}
	if !found {
		t.Fatal("the real tree has no adm must-gather leaf")
	}
}

// The connection the tools use is the one the mcp command itself was given.
func TestTheRealTreeHonoursTheConnectionTheServerWasStartedWith(t *testing.T) {
	isolateKubeconfig(t)

	chosen := filepath.Join(t.TempDir(), "chosen-kubeconfig")
	if err := os.WriteFile(chosen, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("writing the chosen kubeconfig: %v", err)
	}

	configFlags := genericclioptions.NewConfigFlags(true)
	*configFlags.KubeConfig = chosen

	newRoot, _ := realRoot(t)
	tree := newRoot(mcp.CallDeps{
		Streams:     genericiooptions.NewTestIOStreamsDiscard(),
		ConfigFlags: configFlags,
	})

	flag := tree.PersistentFlags().Lookup("kubeconfig")
	if flag == nil {
		t.Fatal("the tree has no --kubeconfig flag")
	}
	if flag.Value.String() != chosen {
		t.Errorf("the fresh tree reports kubeconfig %q, want %q: the outer settings did not reach it",
			flag.Value.String(), chosen)
	}
}

// connectTo connects a client to a server through an in-memory transport.
func connectTo(t *testing.T, server *sdk.Server) *sdk.ClientSession {
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

// callTool runs one tool with no arguments.
func callTool(t *testing.T, session *sdk.ClientSession, name string) *sdk.CallToolResult {
	t.Helper()

	result, err := session.CallTool(context.Background(),
		&sdk.CallToolParams{Name: name, Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("calling %s: %v", name, err)
	}

	return result
}

// refusalText is the exact message a refused call carries, without the exit
// code line every result ends with.
func refusalText(result *sdk.CallToolResult) string {
	message, _, _ := strings.Cut(resultText(result), "\n\nexit code ")

	return message
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

// debug is the second exec capable command of the tree, and the first that can
// actually be served: it has a --no-exec flag, so the server forces it rather
// than refusing the tool.
//
// The gate is asserted where it has to hold, which is the gather the call
// reaches, not the Tool value: a server that listed the flag as forced and then
// left it off the argv would pass every assertion made against the tree alone.
func TestTheRealDebugIsServedWithExecForcedOff(t *testing.T) {
	isolateKubeconfig(t)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "worker-1",
			Labels: map[string]string{"nvidia.com/gpu.present": "true"},
		},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(4, resource.DecimalSI)},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(4, resource.DecimalSI)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}

	newRoot, recorder := realRootOver(t, node)
	tree := newRoot(mcp.CallDeps{Streams: genericiooptions.NewTestIOStreamsDiscard()})

	var tool mcp.Tool
	for _, candidate := range mcp.Tools(tree) {
		if candidate.Name == "debug" {
			tool = candidate
		}
	}
	if tool.Name == "" {
		t.Fatal("the real tree offers no debug tool")
	}
	if !tool.ForceNoExec {
		t.Error("the real debug tool is not exec gated")
	}
	if !tool.HasNoExecFlag {
		t.Fatal("the real debug tool has no --no-exec flag, so the server would refuse it")
	}
	if !tool.WritesHost {
		t.Error("the real debug tool is marked as writing nothing on the host: it writes a bundle")
	}

	server, err := mcp.NewServer(newRoot, "test")
	if err != nil {
		t.Fatalf("building the server over the real tree: %v", err)
	}
	session := connectTo(t, server)

	for _, name := range []string{"no-exec", "exec"} {
		if _, offered := toolProperties(t, session, "debug")[name]; offered {
			t.Errorf("the debug tool offers --%s: a model could turn the exec back on", name)
		}
	}

	result, err := session.CallTool(context.Background(), &sdk.CallToolParams{
		Name:      "debug",
		Arguments: map[string]any{"args": []any{"worker-1"}},
	})
	if err != nil {
		t.Fatalf("calling the debug tool: %v", err)
	}
	if result.IsError {
		t.Fatalf("the debug tool was refused: %s", resultText(result))
	}

	seen := recorder.seen()
	if len(seen) != 1 {
		t.Fatalf("the gather ran %d times, want once", len(seen))
	}
	if !seen[0].NoExec {
		t.Error("the gather behind the debug tool was allowed to exec into a pod")
	}
	if seen[0].Node != "worker-1" {
		t.Errorf("the gather was narrowed to %q, want worker-1", seen[0].Node)
	}
	if seen[0].ArtifactDir == "" {
		t.Error("the gather was given no artifacts directory: the server has to pin one")
	}
}

// toolProperties reads the input schema of one tool as the session serves it.
func toolProperties(t *testing.T, session *sdk.ClientSession, name string) map[string]any {
	t.Helper()

	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("listing the tools: %v", err)
	}
	for _, tool := range listed.Tools {
		if tool.Name != name {
			continue
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshalling the schema of %s: %v", name, err)
		}
		var decoded struct {
			Properties map[string]any `json:"properties"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decoding the schema of %s: %v", name, err)
		}
		if decoded.Properties == nil {
			return map[string]any{}
		}

		return decoded.Properties
	}

	t.Fatalf("the server lists no tool named %q", name)

	return nil
}
