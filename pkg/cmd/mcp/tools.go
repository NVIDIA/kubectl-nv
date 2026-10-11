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
// the one inherited flag a tool may carry.
//
// The input schema is an allowlist. Everything that decides which cluster the
// plugin talks to, and as whom, is settled when the server starts and is not
// reachable from a tool call: a model that could set --server, --token or
// --kubeconfig could point kubectl-nv at a cluster, a proxy or an identity
// nobody granted it, and the read-only guarantee says nothing about which
// cluster is being read.
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

// allowedInheritedFlags are the only flags from above a leaf that a tool may
// offer. Which namespace to look in is a question about the request; every
// other persistent flag is a question about the connection, and the connection
// is not a model's to choose.
var allowedInheritedFlags = map[string]bool{"namespace": true}

// execFlags are the flags that decide whether an exec happens. An exec capable
// tool offers none of them, so that the forcing cannot be argued with.
var execFlags = map[string]bool{
	"no-exec": true,
	"exec":    true,
}

// suppressedFlags are the flags no tool offers whatever command owns them. The
// output format belongs to the server, which asks for JSON because that is what
// it turns into structured content; help prints the usage a model already has
// as the tool description.
var suppressedFlags = map[string]bool{
	"output": true,
	"help":   true,
}

const (
	// outputFlag is the flag the server sets to json on a command that has one.
	outputFlag = "output"
	// noExecFlag is the flag the server forces on an exec capable command.
	noExecFlag = "no-exec"
)

const (
	// SuppressAnnotation marks a flag that no tool offers. A command sets it on
	// a flag of its own with pflag's SetAnnotation.
	//
	// It exists for the flags the read-only guard says nothing about because
	// they touch no cluster: --write-dir writes files on the machine kubectl-nv
	// runs on, which is not something to hand a model even though no Kubernetes
	// object changes.
	SuppressAnnotation = "kubectl-nv.nvidia.com/mcp-suppress"

	// HostDirAnnotation marks a flag naming a directory on the machine
	// kubectl-nv runs on that the command writes into. No tool offers one: the
	// server pins it to a fresh private directory per call and reports where
	// that is, so a model can neither choose the path nor read another call's.
	HostDirAnnotation = "kubectl-nv.nvidia.com/host-dir"

	// ExecAnnotation marks a command that can run something inside a pod. The
	// server forces --no-exec on it and refuses it outright when it has no such
	// flag.
	//
	// It is an annotation rather than a list of names here because a list is a
	// thing to forget: a command renamed, or a new one that execs, would be
	// served with the exec available and nothing would say so.
	ExecAnnotation = "kubectl-nv.nvidia.com/exec"
)

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
	// leaf's own flags, the one allowed inherited flag, and, when the command
	// documents any, its positional arguments.
	InputSchema map[string]any
	// ForceNoExec marks a leaf that can run something inside a pod.
	ForceNoExec bool
	// HasNoExecFlag reports whether the command can be told not to exec. An
	// exec capable command that cannot is refused rather than run.
	HasNoExecFlag bool
	// HostDirFlags are the flags naming a directory the command writes into.
	// The server pins each one per call.
	HostDirFlags []string
	// WritesHost reports whether running this tool puts files on the machine
	// kubectl-nv runs on. Such a tool is not read-only, whatever it does to the
	// cluster, and its annotations have to say so.
	WritesHost bool
	// Args is what the Use line says about positional arguments.
	Args ArgSpec
	// HasOutputFlag reports whether -o can be set on this command.
	HasOutputFlag bool
}

// ArgSpec is the arity of a command's positional arguments, read from its Use
// line: <NAME> and a bare NAME are required, [NAME] is optional, and a trailing
// ... means any number.
type ArgSpec struct {
	// Min is how many arguments the command needs.
	Min int
	// Max is how many it accepts, or -1 for any number.
	Max int
	// Usage is the Use line the arity was read from, for the description.
	Usage string
}

// Takes reports whether the command accepts any positional argument.
func (a ArgSpec) Takes() bool { return a.Max != 0 }

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
			// A hidden command is hidden because it is not part of the
			// interface. Publishing it to the one caller that reads every tool
			// would make it part of the interface after all.
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
	forceNoExec := c.Annotations[ExecAnnotation] == "true"
	args := parseArgs(c.Use)
	hostDirs := hostDirFlags(c)

	return Tool{
		Name:          strings.Join(path, "_"),
		Path:          path,
		Description:   describe(c),
		InputSchema:   inputSchema(c, forceNoExec, args),
		ForceNoExec:   forceNoExec,
		HasNoExecFlag: c.Flags().Lookup(noExecFlag) != nil,
		HostDirFlags:  hostDirs,
		WritesHost:    len(hostDirs) > 0,
		Args:          args,
		HasOutputFlag: c.Flags().Lookup(outputFlag) != nil,
	}
}

// hostDirFlags returns the flags of a command that name a directory it writes
// into, sorted.
func hostDirFlags(c *cobra.Command) []string {
	var out []string
	c.Flags().VisitAll(func(f *pflag.Flag) {
		if len(f.Annotations[HostDirAnnotation]) > 0 {
			out = append(out, f.Name)
		}
	})
	sort.Strings(out)

	return out
}

// describe returns what the command says about itself, preferring the one line
// summary and falling back to the first line of the long help.
func describe(c *cobra.Command) string {
	if c.Short != "" {
		return c.Short
	}

	return strings.TrimSpace(strings.SplitN(c.Long, "\n", 2)[0])
}

// parseArgs reads the arity of the positional arguments from a Use line. A
// command that documents none is offered none, and one that needs an argument
// says so in the schema rather than answering a call with a usage error.
func parseArgs(use string) ArgSpec {
	fields := strings.Fields(use)
	if len(fields) < 2 {
		return ArgSpec{Usage: use}
	}

	spec := ArgSpec{Usage: use}
	for _, token := range fields[1:] {
		variadic := strings.Contains(token, "...")
		optional := strings.HasPrefix(token, "[")

		switch {
		case variadic:
			spec.Max = -1
			if !optional {
				spec.Min++
			}
		case optional:
			spec.Max++
		default:
			spec.Min++
			spec.Max++
		}
	}

	if spec.Max >= 0 && spec.Max < spec.Min {
		spec.Max = spec.Min
	}

	return spec
}

// inputSchema builds the JSON Schema of a tool's arguments. It is an allowlist:
// the leaf's own non-persistent flags, plus the inherited flags named in
// allowedInheritedFlags, and nothing else. Unknown properties are refused
// rather than ignored, so that a model that misspells a flag is told so instead
// of getting output that quietly did not have the flag applied.
func inputSchema(c *cobra.Command, forceNoExec bool, args ArgSpec) map[string]any {
	properties := map[string]any{}

	add := func(f *pflag.Flag) {
		if f.Hidden || suppressedFlags[f.Name] {
			return
		}
		if len(f.Annotations[SuppressAnnotation]) > 0 {
			return
		}
		// The server pins these, so offering one would be offering a choice the
		// caller does not get.
		if len(f.Annotations[HostDirAnnotation]) > 0 {
			return
		}
		if forceNoExec && execFlags[f.Name] {
			return
		}
		properties[f.Name] = flagSchema(f)
	}

	// LocalNonPersistentFlags is the leaf's own interface: its persistent flags
	// are inherited by something below it, and a leaf has nothing below it.
	c.LocalNonPersistentFlags().VisitAll(add)
	c.InheritedFlags().VisitAll(func(f *pflag.Flag) {
		if allowedInheritedFlags[f.Name] {
			add(f)
		}
	})

	if args.Takes() {
		schema := map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"minItems":    args.Min,
			"description": "Positional arguments, as in: " + args.Usage + ". None of them may be a flag",
		}
		if args.Max >= 0 {
			schema["maxItems"] = args.Max
		}
		properties["args"] = schema
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
// A model that could pass one could reach every flag the input schema
// deliberately leaves out, the connection settings among them.
func looksLikeAFlag(arg string) bool {
	runes := []rune(arg)
	if len(runes) < 2 || runes[0] != '-' {
		return false
	}

	// "-" alone is stdin and "-1" is a negative number; neither is a flag.
	return runes[1] == '-' || unicode.IsLetter(runes[1])
}
