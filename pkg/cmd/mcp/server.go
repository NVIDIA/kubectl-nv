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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

const (
	// serverName is what the server calls itself over the protocol.
	serverName = "kubectl-nv"

	// ExitCodeMetaKey is where a tool result carries the exit code the command
	// would have left the process with. A caller that scripts kubectl-nv on the
	// command line branches on that code, and it has to be able to branch on
	// the same one here.
	ExitCodeMetaKey = "kubectl-nv.nvidia.com/exit-code"

	// artifactDirPrefix names the per-call directory a command that writes on
	// the host puts its files in.
	artifactDirPrefix = "kubectl-nv-mcp-"
)

// CallDeps is what one tool call runs against: where the command writes, and
// which cluster it talks to.
//
// The connection half is settled when the server starts and is the same for
// every call. It is carried here rather than read from the environment inside
// each call, so that a tool call cannot be aimed at a different cluster by
// changing the environment underneath the server.
type CallDeps struct {
	// Streams are the buffers this call's output is collected from.
	Streams genericiooptions.IOStreams
	// ConfigFlags are the kubectl configuration flags the mcp command itself
	// was given, already parsed. A fresh tree is built over them, so every call
	// talks to the cluster the server was started against.
	ConfigFlags *genericclioptions.ConfigFlags
	// Factory is the client factory built from those flags, shared by every
	// call so that the kubeconfig is read once.
	Factory cmd.Factory
	// KubeconfigPath is what ConfigFlags resolved to, for reporting. The
	// clients are built from ConfigFlags and Factory, not from this.
	KubeconfigPath string
}

// NewRootFunc builds a fresh kubectl-nv command tree for one tool call.
//
// It is a function rather than a tree because cobra keeps the flags a run set,
// and a tree reused across two tool calls would carry the first call's --limit
// into the second. One tree per call is the only way a call means what it says.
type NewRootFunc func(CallDeps) *cobra.Command

// Server is the MCP server together with the connection every call runs
// against.
type Server struct {
	server  *sdk.Server
	newRoot NewRootFunc

	// mu guards connection, which the mcp command fills in after its flags are
	// parsed but before the first call is served, and which every call reads.
	mu         sync.RWMutex
	connection CallDeps
}

// servers remembers the connection each built server runs against, so that
// SetConnection can reach it through the SDK server a caller holds.
var servers sync.Map

// NewServer builds the MCP server over a command tree, with one tool per
// read-only leaf.
func NewServer(newRoot NewRootFunc, version string) (*sdk.Server, error) {
	built, err := newServer(newRoot, version)
	if err != nil {
		return nil, err
	}

	return built.server, nil
}

// newServer builds the server and registers it so that its connection can be
// set afterwards.
func newServer(newRoot NewRootFunc, version string) (*Server, error) {
	if newRoot == nil {
		return nil, fmt.Errorf("the MCP server needs a way to build the command tree")
	}

	built := &Server{newRoot: newRoot}

	tools := Tools(newRoot(CallDeps{Streams: discardStreams()}))
	if len(tools) == 0 {
		return nil, fmt.Errorf("the command tree has no read-only leaf to offer as a tool")
	}

	sdkServer := sdk.NewServer(&sdk.Implementation{
		Name:        serverName,
		Title:       "kubectl-nv",
		Description: "Read-only diagnostics for NVIDIA powered Kubernetes clusters",
		Version:     version,
	}, nil)

	for _, tool := range tools {
		sdkServer.AddTool(&sdk.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
			// A tool that leaves files on the machine kubectl-nv runs on is not
			// read-only, whatever it does to the cluster. Claiming otherwise is
			// a lie a model plans around.
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: !tool.WritesHost},
		}, built.handlerFor(tool))
	}

	built.server = sdkServer
	servers.Store(sdkServer, built)

	return built, nil
}

// SetConnection tells a server which cluster its calls run against. It is
// called once, after the mcp command's own flags are parsed and before the
// first call is served.
func SetConnection(server *sdk.Server, deps CallDeps) {
	value, found := servers.Load(server)
	if !found {
		return
	}
	built, ok := value.(*Server)
	if !ok {
		return
	}

	built.mu.Lock()
	defer built.mu.Unlock()
	built.connection = deps
}

// connectionFor returns the connection a call runs against.
func (s *Server) connectionFor() CallDeps {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.connection
}

// Serve runs the server over stdio until the client goes away or ctx is
// cancelled. stdio is the transport an editor or an agent runner starts a
// plugin on, and it is the only one offered: a listening socket would be a way
// into the cluster that outlives the process that asked for it.
func Serve(ctx context.Context, newRoot NewRootFunc, version string, connection CallDeps) error {
	built, err := newServer(newRoot, version)
	if err != nil {
		return err
	}
	SetConnection(built.server, connection)

	if err := built.server.Run(ctx, &sdk.StdioTransport{}); err != nil {
		return fmt.Errorf("serving MCP over stdio: %w", err)
	}

	return nil
}

// discardStreams is where the tree writes when it is built only to be walked.
func discardStreams() genericiooptions.IOStreams {
	return genericiooptions.NewTestIOStreamsDiscard()
}

// handlerFor returns the handler that runs one tool.
func (s *Server) handlerFor(tool Tool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (result *sdk.CallToolResult, err error) {
		// The SDK runs handlers on its own goroutines and does not recover. A
		// panic in one command would take the whole server down, and the caller
		// would see a dropped connection rather than a failure it can act on.
		defer func() {
			if recovered := recover(); recovered != nil {
				result = toolError(output.ExitError, "%s panicked: %v\n\n%s",
					tool.Name, recovered, debug.Stack())
				err = nil
			}
		}()

		arguments := map[string]any{}
		if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
			if decodeErr := json.Unmarshal(req.Params.Arguments, &arguments); decodeErr != nil {
				return toolError(output.ExitUsage, "could not read the arguments of %s: %v",
					tool.Name, decodeErr), nil
			}
		}

		argv, pinned, buildErr := commandLine(tool, arguments)
		if buildErr != nil {
			return toolError(output.ExitUsage, "%v", buildErr), nil
		}

		return s.execute(ctx, argv, pinned), nil
	}
}

// commandLine turns a tool call into the argument list the command tree is run
// with, and the host directories the server pinned for it. Anything it cannot
// account for is refused rather than dropped: an argument silently ignored is
// output that does not answer what was asked.
func commandLine(tool Tool, arguments map[string]any) ([]string, map[string]string, error) {
	if tool.ForceNoExec && !tool.HasNoExecFlag {
		return nil, nil, fmt.Errorf(
			"%s can run a command inside a pod and has no --%s flag, so it is not served over MCP",
			tool.Name, noExecFlag)
	}

	argv := append([]string{}, tool.Path...)

	// The format is the server's decision: structured content is the JSON the
	// command printed, so it is the one format a tool call can be built on.
	if tool.HasOutputFlag {
		argv = append(argv, "--"+outputFlag, "json")
	}

	properties, _ := tool.InputSchema["properties"].(map[string]any)
	for _, name := range sortedKeys(arguments) {
		if name == "args" {
			continue
		}
		if _, offered := properties[name]; !offered {
			return nil, nil, fmt.Errorf("%s does not take %q", tool.Name, name)
		}
		value, err := flagValue(arguments[name])
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %s: %w", tool.Name, name, err)
		}
		argv = append(argv, "--"+name+"="+value)
	}

	positional, err := positionalArgs(tool, arguments)
	if err != nil {
		return nil, nil, err
	}
	if err := checkArity(tool, positional); err != nil {
		return nil, nil, err
	}

	// Where files go is the server's choice, not the model's: a path from a
	// tool call is a way to write under any directory this process can reach.
	pinned := map[string]string{}
	for _, flag := range tool.HostDirFlags {
		dir, err := os.MkdirTemp("", artifactDirPrefix)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: making a directory for --%s: %w", tool.Name, flag, err)
		}
		// MkdirTemp already makes it 0700; setting it again says so out loud,
		// and survives anything that would have widened it.
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, nil, fmt.Errorf("%s: securing %s: %w", tool.Name, dir, err)
		}
		pinned[flag] = dir
		argv = append(argv, "--"+flag+"="+dir)
	}

	// Forced last, so that it wins over anything earlier on the line whatever
	// pflag does with a repeated flag. The refusals above already mean nothing
	// earlier can be --no-exec; this is the second lock on the same door.
	if tool.ForceNoExec {
		argv = append(argv, "--"+noExecFlag+"=true")
	}

	return append(argv, positional...), pinned, nil
}

// positionalArgs reads the args property, refusing anything that would be read
// as a flag. Without that check the args array is a way to reach every flag the
// input schema deliberately leaves out: --no-exec, and every connection
// setting.
func positionalArgs(tool Tool, arguments map[string]any) ([]string, error) {
	raw, given := arguments["args"]
	if !given {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: args must be an array of strings, got %T", tool.Name, raw)
	}

	// The flag check runs before the arity one, so that an attempt to reach a
	// flag this way is refused by name wherever it is aimed.
	out := make([]string, 0, len(list))
	for _, entry := range list {
		arg, ok := entry.(string)
		if !ok {
			return nil, fmt.Errorf("%s: args must be an array of strings, got a %T in it", tool.Name, entry)
		}
		if looksLikeAFlag(arg) {
			// Only the name is quoted back. The value of a rejected --token or
			// --password would otherwise be copied into the model's context and
			// into whatever logs the session, which is the opposite of refusing
			// it.
			name, _, _ := strings.Cut(arg, "=")

			return nil, fmt.Errorf("%s: %q is a flag, and a flag cannot be passed as a positional argument",
				tool.Name, name)
		}
		out = append(out, arg)
	}

	if !tool.Args.Takes() && len(out) > 0 {
		return nil, fmt.Errorf("%s takes no positional arguments", tool.Name)
	}

	return out, nil
}

// checkArity holds a call to the arity the input schema advertised. Cobra would
// catch a wrong count too, but its error carries no exit code, so the caller
// would be told the run could not happen when in fact the call was malformed.
func checkArity(tool Tool, args []string) error {
	if len(args) < tool.Args.Min {
		return fmt.Errorf("%s needs %d positional argument(s), got %d: %s",
			tool.Name, tool.Args.Min, len(args), tool.Args.Usage)
	}
	if tool.Args.Max >= 0 && len(args) > tool.Args.Max {
		return fmt.Errorf("%s takes at most %d positional argument(s), got %d: %s",
			tool.Name, tool.Args.Max, len(args), tool.Args.Usage)
	}

	return nil
}

// flagValue renders one JSON value as the text pflag parses.
func flagValue(value any) (string, error) {
	switch typed := value.(type) {
	case string:
		return typed, nil
	case bool:
		return strconv.FormatBool(typed), nil
	case float64:
		// JSON has one number type, so an integer flag arrives as a float.
		if typed == float64(int64(typed)) {
			return strconv.FormatInt(int64(typed), 10), nil
		}

		return strconv.FormatFloat(typed, 'f', -1, 64), nil
	case []any:
		parts := make([]string, 0, len(typed))
		for _, entry := range typed {
			part, err := flagValue(entry)
			if err != nil {
				return "", err
			}
			parts = append(parts, part)
		}

		return strings.Join(parts, ","), nil
	case nil:
		return "", fmt.Errorf("has no value")
	}

	return "", fmt.Errorf("cannot be a %T", value)
}

// execute runs the command tree in process and turns what it printed into a
// tool result.
//
// Nothing may be written to the process stdout: that belongs to the JSON-RPC
// transport, and a byte of command output landing there would corrupt the
// frame. The tree is given a buffer for both its streams, and it is the buffer
// that becomes the text and the structured content.
//
// This cannot be enforced here. cobra reads os.Stdout on every execution, so a
// library that reassigned it would be racing every command in the process; the
// guarantee is a property of the commands instead, and
// cmd/kubectl-nv asserts it over the whole tree.
func (s *Server) execute(ctx context.Context, argv []string, pinned map[string]string) *sdk.CallToolResult {
	var out, errOut bytes.Buffer

	deps := s.connectionFor()
	deps.Streams = genericiooptions.IOStreams{In: strings.NewReader(""), Out: &out, ErrOut: &errOut}

	root := s.newRoot(deps)
	root.SetArgs(argv)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(""))
	root.SilenceUsage = true
	root.SilenceErrors = true

	runErr := root.ExecuteContext(ctx)
	code := output.ExitCodeFor(runErr)

	// Exit 2 and 4 mean there is nothing to read: the command line was wrong,
	// or the run could not happen. Everything else printed a report, and exit 1
	// and 3 are part of that report rather than a failure to deliver it.
	if code == output.ExitUsage || code == output.ExitError {
		return toolError(code, "%s", strings.TrimSpace(runErr.Error()+"\n"+errOut.String()))
	}

	result := &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: resultText(out.String(), code, pinned)}},
		Meta:    sdk.Meta{ExitCodeMetaKey: code},
	}

	// Only an object or an array becomes structured content: a bare string or a
	// number would be the text content again under another name.
	var decoded any
	if err := json.Unmarshal(out.Bytes(), &decoded); err == nil {
		switch decoded.(type) {
		case map[string]any, []any:
			result.StructuredContent = decoded
		}
	}

	return result
}

// resultText is what the command printed, plus the two things a caller cannot
// read off stdout: the exit code it would have got on the command line, and
// where any files were left.
func resultText(printed string, code int, pinned map[string]string) string {
	parts := []string{strings.TrimRight(printed, "\n")}
	for _, flag := range sortedStringKeys(pinned) {
		parts = append(parts, fmt.Sprintf("--%s: %s", flag, pinned[flag]))
	}
	parts = append(parts, fmt.Sprintf("exit code %d", code))

	return strings.Join(parts, "\n")
}

// toolError reports a failure to the model rather than to the protocol. A
// protocol error is something the model never sees and cannot correct.
func toolError(code int, format string, a ...any) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{
			Text: fmt.Sprintf(format, a...) + fmt.Sprintf("\n\nexit code %d", code),
		}},
		Meta: sdk.Meta{ExitCodeMetaKey: code},
	}
}

// sortedKeys returns the keys of a map, sorted, so that a command line built
// from the same call twice is the same command line.
func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)

	return out
}

// sortedStringKeys is sortedKeys for a map of strings.
func sortedStringKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)

	return out
}
