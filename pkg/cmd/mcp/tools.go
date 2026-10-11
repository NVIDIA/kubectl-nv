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

// Package mcp serves the kubectl-nv command tree as a Model Context Protocol
// server over stdio, one tool per read-only leaf command.
//
// The tools are derived from the cobra tree rather than listed here, so a
// command another track adds becomes a tool without this package changing. What
// is listed here is the opposite: the four commands that are never tools, and
// the leaves that can run something inside a pod and are therefore served with
// --no-exec forced on.
package mcp

import (
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// neverTools are the commands a model is never offered. mcp would let it start
// another server inside this one, and help, completion and version answer
// questions about the tool rather than about the cluster: a model that asks
// them has spent a call learning nothing it needed.
var neverTools = map[string]bool{
	"mcp":        true,
	"help":       true,
	"completion": true,
	"version":    true,
}

// execCapable are the leaves that can run a command inside a pod. Over MCP they
// run with --no-exec forced on: nothing a model decides should end up executing
// inside a customer's workload, however the request was phrased.
var execCapable = map[string]bool{
	"must-gather": true,
	"debug":       true,
}

// execFlags are the flags that decide whether an exec happens. An exec capable
// tool offers none of them, so that the forcing cannot be argued with.
var execFlags = map[string]bool{
	"no-exec": true,
	"exec":    true,
}

// suppressedFlags are the flags no tool offers. The output format belongs to
// the server, which asks for JSON because that is what it turns into structured
// content; help prints the usage a model already has as the tool description.
var suppressedFlags = map[string]bool{
	"output": true,
	"help":   true,
}

// outputFlag is the flag the server sets to json on a command that has one.
const outputFlag = "output"

// SuppressAnnotation marks a flag that no tool offers. A command sets it on a
// flag of its own with pflag's SetAnnotation.
//
// It exists for the flags the read-only guard says nothing about because they
// touch no cluster: --write-dir writes files on the machine kubectl-nv runs
// on, which is not something to hand a model even though no Kubernetes object
// changes. The command that owns such a flag is the only place that knows it
// is one.
const SuppressAnnotation = "kubectl-nv.nvidia.com/mcp-suppress"

// Tool is one read-only leaf command as it is offered over MCP.
type Tool struct {
	// Name is the command path joined with underscores, for instance
	// "adm_must-gather". It is what a model calls.
	Name string
	// Path is the argument path of the command inside the tree.
	Path []string
	// Description is what the command says about itself.
	Description string
	// InputSchema is the JSON Schema of the tool's arguments, built from the
	// command's flags and, when it takes any, its positional arguments.
	InputSchema map[string]any
	// ForceNoExec marks a leaf that can run something inside a pod. The server
	// appends --no-exec to every call of it and refuses the call when the
	// command has no such flag.
	ForceNoExec bool
	// HasNoExecFlag reports whether the command can be told not to exec. An
	// exec capable command that cannot is refused rather than run.
	HasNoExecFlag bool
	// TakesArgs reports whether the command documents positional arguments.
	TakesArgs bool
	// HasOutputFlag reports whether -o can be set on this command.
	HasOutputFlag bool
}

// Tools returns one tool per read-only leaf of the tree, sorted by name.
func Tools(root *cobra.Command) []Tool {
	var tools []Tool
	var walk func(c *cobra.Command, path []string)
	walk = func(c *cobra.Command, path []string) {
		children := c.Commands()
		if len(children) == 0 {
			if len(path) == 0 || neverTools[path[len(path)-1]] {
				return
			}
			tools = append(tools, newTool(c, path))

			return
		}
		for _, child := range children {
			if child.Hidden {
				continue
			}
			walk(child, append(append([]string{}, path...), child.Name()))
		}
	}
	walk(root, nil)

	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })

	return tools
}

// newTool describes one leaf.
func newTool(c *cobra.Command, path []string) Tool {
	forceNoExec := execCapable[path[len(path)-1]]
	takesArgs := documentsArguments(c)

	return Tool{
		Name:          strings.Join(path, "_"),
		Path:          path,
		Description:   describe(c),
		InputSchema:   inputSchema(c, forceNoExec, takesArgs),
		ForceNoExec:   forceNoExec,
		HasNoExecFlag: c.Flags().Lookup("no-exec") != nil,
		TakesArgs:     takesArgs,
		HasOutputFlag: c.Flags().Lookup(outputFlag) != nil,
	}
}

// describe returns what the command says about itself, preferring the one line
// summary and falling back to the first line of the long help.
func describe(c *cobra.Command) string {
	if c.Short != "" {
		return c.Short
	}

	return strings.TrimSpace(strings.SplitN(c.Long, "\n", 2)[0])
}

// documentsArguments reports whether a command takes positional arguments,
// read from the Use line. A command that documents none is offered none: a
// model that sends one would get a usage error back and have learned nothing.
func documentsArguments(c *cobra.Command) bool {
	fields := strings.Fields(c.Use)

	return len(fields) > 1
}

// inputSchema builds the JSON Schema of a tool's arguments from the command's
// flags. Unknown properties are refused rather than ignored, so that a model
// that misspells a flag is told so instead of getting output that quietly did
// not have the flag applied.
func inputSchema(c *cobra.Command, forceNoExec, takesArgs bool) map[string]any {
	properties := map[string]any{}

	add := func(f *pflag.Flag) {
		if f.Hidden || suppressedFlags[f.Name] {
			return
		}
		if len(f.Annotations[SuppressAnnotation]) > 0 {
			return
		}
		if forceNoExec && execFlags[f.Name] {
			return
		}
		properties[f.Name] = flagSchema(f)
	}
	c.LocalFlags().VisitAll(add)
	c.InheritedFlags().VisitAll(add)

	if takesArgs {
		properties["args"] = map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Positional arguments, as in: " + c.Use + ". None of them may be a flag",
		}
	}

	return map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
}

// flagSchema maps one pflag onto its JSON Schema type. pflag names its types
// itself, so this reads the name rather than reflecting the value.
func flagSchema(f *pflag.Flag) map[string]any {
	schema := map[string]any{"description": f.Usage}

	name := f.Value.Type()
	switch {
	case name == "bool":
		schema["type"] = "boolean"
	case name == "count" || strings.HasPrefix(name, "int") || strings.HasPrefix(name, "uint"):
		schema["type"] = "integer"
	case strings.HasPrefix(name, "float"):
		schema["type"] = "number"
	case strings.HasSuffix(name, "Slice") || strings.HasSuffix(name, "Array"):
		schema["type"] = "array"
		schema["items"] = map[string]any{"type": elementType(name)}
	default:
		schema["type"] = "string"
	}

	return schema
}

// elementType maps a pflag slice type name onto the type of its elements.
func elementType(name string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(name, "Slice"), "Array")
	switch {
	case base == "bool":
		return "boolean"
	case strings.HasPrefix(base, "int") || strings.HasPrefix(base, "uint"):
		return "integer"
	case strings.HasPrefix(base, "float"):
		return "number"
	}

	return "string"
}

// looksLikeAFlag reports whether a positional argument would be read as a flag.
// A model that could pass one could reach any flag of the command, including
// the ones the input schema deliberately leaves out.
func looksLikeAFlag(arg string) bool {
	runes := []rune(arg)
	if len(runes) < 2 || runes[0] != '-' {
		return false
	}

	// "-" alone is stdin and "-1" is a negative number; neither is a flag.
	return runes[1] == '-' || unicode.IsLetter(runes[1])
}
