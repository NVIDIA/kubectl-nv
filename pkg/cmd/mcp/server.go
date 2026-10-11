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
	"sort"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// serverName is what the server calls itself over the protocol.
const serverName = "kubectl-nv"

// NewRootFunc builds a fresh kubectl-nv command tree writing to streams.
//
// It is a function rather than a tree because cobra keeps the flags a run set,
// and a tree reused across two tool calls would carry the first call's --limit
// into the second. One tree per call is the only way a call means what it says.
type NewRootFunc func(genericiooptions.IOStreams) *cobra.Command

// NewServer builds the MCP server over a command tree, with one tool per
// read-only leaf.
func NewServer(newRoot NewRootFunc, version string) (*sdk.Server, error) {
	if newRoot == nil {
		return nil, fmt.Errorf("the MCP server needs a way to build the command tree")
	}

	tools := Tools(newRoot(discardStreams()))
	if len(tools) == 0 {
		return nil, fmt.Errorf("the command tree has no read-only leaf to offer as a tool")
	}

	server := sdk.NewServer(&sdk.Implementation{
		Name:        serverName,
		Title:       "kubectl-nv",
		Description: "Read-only diagnostics for NVIDIA powered Kubernetes clusters",
		Version:     version,
	}, nil)

	for _, tool := range tools {
		server.AddTool(&sdk.Tool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.InputSchema,
			Annotations: &sdk.ToolAnnotations{ReadOnlyHint: true},
		}, handlerFor(newRoot, tool))
	}

	return server, nil
}

// Serve runs the server over stdio until the client goes away or ctx is
// cancelled. stdio is the transport an editor or an agent runner starts a
// plugin on, and it is the only one offered: a listening socket would be a way
// into the cluster that outlives the process that asked for it.
func Serve(ctx context.Context, newRoot NewRootFunc, version string) error {
	server, err := NewServer(newRoot, version)
	if err != nil {
		return err
	}

	if err := server.Run(ctx, &sdk.StdioTransport{}); err != nil {
		return fmt.Errorf("serving MCP over stdio: %w", err)
	}

	return nil
}

// discardStreams is where the tree writes when it is built only to be walked.
func discardStreams() genericiooptions.IOStreams {
	return genericiooptions.NewTestIOStreamsDiscard()
}

// handlerFor returns the handler that runs one tool.
func handlerFor(newRoot NewRootFunc, tool Tool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		arguments := map[string]any{}
		if req != nil && req.Params != nil && len(req.Params.Arguments) > 0 {
			if err := json.Unmarshal(req.Params.Arguments, &arguments); err != nil {
				return toolError("could not read the arguments of %s: %v", tool.Name, err), nil
			}
		}

		argv, err := commandLine(tool, arguments)
		if err != nil {
			return toolError("%v", err), nil
		}

		return execute(ctx, newRoot, argv)
	}
}

// commandLine turns a tool call into the argument list the command tree is run
// with. Anything it cannot account for is refused rather than dropped: an
// argument silently ignored is output that does not answer what was asked.
func commandLine(tool Tool, arguments map[string]any) ([]string, error) {
	if tool.ForceNoExec && !tool.HasNoExecFlag {
		return nil, fmt.Errorf(
			"%s can run a command inside a pod and has no --no-exec flag, so it is not served over MCP",
			tool.Name)
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
			return nil, fmt.Errorf("%s does not take %q", tool.Name, name)
		}
		value, err := flagValue(arguments[name])
		if err != nil {
			return nil, fmt.Errorf("%s: %s: %w", tool.Name, name, err)
		}
		argv = append(argv, "--"+name+"="+value)
	}

	positional, err := positionalArgs(tool, arguments)
	if err != nil {
		return nil, err
	}

	// Forced last, so that it wins over anything earlier on the line whatever
	// pflag does with a repeated flag. The refusals above already mean nothing
	// earlier can be --no-exec; this is the second lock on the same door.
	if tool.ForceNoExec {
		argv = append(argv, "--no-exec=true")
	}

	return append(argv, positional...), nil
}

// positionalArgs reads the args property, refusing anything that would be read
// as a flag. Without that check the args array is a way to reach every flag the
// input schema deliberately leaves out, --no-exec among them.
func positionalArgs(tool Tool, arguments map[string]any) ([]string, error) {
	raw, given := arguments["args"]
	if !given {
		return nil, nil
	}
	list, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: args must be an array of strings, got %T", tool.Name, raw)
	}

	// The flag check runs before the "takes no arguments" one, so that an
	// attempt to reach a flag this way is refused by name wherever it is aimed.
	out := make([]string, 0, len(list))
	for _, entry := range list {
		arg, ok := entry.(string)
		if !ok {
			return nil, fmt.Errorf("%s: args must be an array of strings, got a %T in it", tool.Name, entry)
		}
		if looksLikeAFlag(arg) {
			return nil, fmt.Errorf("%s: %q is a flag, and a flag cannot be passed as a positional argument",
				tool.Name, arg)
		}
		out = append(out, arg)
	}

	if !tool.TakesArgs && len(out) > 0 {
		return nil, fmt.Errorf("%s takes no positional arguments", tool.Name)
	}

	return out, nil
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
// tool result. The command's stdout is both the text a model reads and, when it
// is JSON, the structured content a program reads without parsing it again.
func execute(ctx context.Context, newRoot NewRootFunc, argv []string) (*sdk.CallToolResult, error) {
	var out, errOut bytes.Buffer
	streams := genericiooptions.IOStreams{In: strings.NewReader(""), Out: &out, ErrOut: &errOut}

	root := newRoot(streams)
	root.SetArgs(argv)
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetIn(strings.NewReader(""))
	root.SilenceUsage = true
	root.SilenceErrors = true

	runErr := root.ExecuteContext(ctx)
	if runErr != nil {
		return toolError("%s", strings.TrimSpace(runErr.Error()+"\n"+errOut.String())), nil
	}

	result := &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: out.String()}}}

	// Only an object or an array becomes structured content: a bare string or a
	// number would be the text content again under another name.
	var structured any
	if err := json.Unmarshal(out.Bytes(), &structured); err == nil {
		switch structured.(type) {
		case map[string]any, []any:
			result.StructuredContent = structured
		}
	}

	return result, nil
}

// toolError reports a failure to the model rather than to the protocol. A
// protocol error is something the model never sees and cannot correct.
func toolError(format string, a ...any) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		IsError: true,
		Content: []sdk.Content{&sdk.TextContent{Text: fmt.Sprintf(format, a...)}},
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
