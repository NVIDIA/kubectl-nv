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

// Package schema carries the schema command, which prints the JSON Schema of
// one kubectl-nv kind, and the generator behind "make schema" that keeps the
// checked-in copies of those schemas in step with the code.
package schema

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/mcp"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// schemaExt is the extension of a generated schema file. It is also what
// CheckDrift looks at, so that a README next to the schemas is not drift.
const schemaExt = ".json"

// NewCommand builds the schema command over the registry the other commands
// register their kinds into.
func NewCommand(o *cmd.Options) *cobra.Command {
	return newCommandFor(o, output.DefaultSchemaRegistry())
}

// newCommandFor is NewCommand against a registry the caller chooses, so that a
// test can register kinds without deciding what the next test sees.
func newCommandFor(o *cmd.Options, registry *output.SchemaRegistry) *cobra.Command {
	var writeDir string

	c := &cobra.Command{
		Use:   "schema [KIND|COMMAND]",
		Short: "Print the JSON Schema of a kubectl-nv output kind",
		Long: "Print the JSON Schema of one kubectl-nv output kind, named either by the kind\n" +
			"or by the command that prints it. The schema is generated from the same Go type\n" +
			"the command marshals, so it cannot describe something kubectl-nv does not emit.",
		// A command path is as many words as the command has. "adm must-gather"
		// is two, and asking for it as one would mean knowing which half of
		// the name is the group.
		Args: cobra.MaximumNArgs(2),
		RunE: func(c *cobra.Command, args []string) error {
			if writeDir != "" {
				if len(args) > 0 {
					return output.NewUsageError(
						"--write-dir writes every registered kind: it cannot be given %q as well",
						strings.Join(args, " "))
				}

				return writeAllTo(o, registry, writeDir)
			}

			if len(args) == 0 {
				return output.NewUsageError("schema needs a kind or a command name: %s", known(registry))
			}

			raw, err := registry.Schema(strings.Join(args, " "))
			if err != nil {
				return err
			}
			// The options' output stream, which is the process's stdout.
			// cobra's own stream is not it: main points that at stderr so that
			// usage lands there, and a schema printed through it is a schema no
			// pipeline receives.
			if _, err := o.Streams.Out.Write(raw); err != nil {
				return fmt.Errorf("writing the schema: %w", err)
			}

			return nil
		},
	}

	c.Flags().StringVar(&writeDir, "write-dir", "",
		"Write one <kind>.json per registered kind into this directory instead of printing one")
	// The flag writes files, which is the generator behind "make schema" and
	// not something to hand a model: the MCP server leaves it out of the tool.
	if err := c.Flags().SetAnnotation("write-dir", mcp.SuppressAnnotation, []string{"true"}); err != nil {
		// SetAnnotation only fails on a flag that is not registered, and the
		// line above registered it.
		panic(fmt.Sprintf("marking --write-dir for the MCP server: %v", err))
	}

	return c
}

// writeAllTo writes every schema and reports what it wrote on the error
// stream, which is where progress belongs.
func writeAllTo(o *cmd.Options, registry *output.SchemaRegistry, dir string) error {
	written, err := WriteAll(registry, dir)
	if err != nil {
		return err
	}
	for _, path := range written {
		fmt.Fprintf(o.Streams.ErrOut, "wrote %s\n", path)
	}

	return nil
}

// known renders what the registry has, for an error message.
func known(registry *output.SchemaRegistry) string {
	kinds, commands := registry.Kinds(), registry.Commands()
	if len(kinds) == 0 {
		return "no kinds are registered yet"
	}

	return fmt.Sprintf("kinds are %s; commands are %s",
		strings.Join(kinds, ", "), orNone(commands))
}

// orNone renders a possibly empty list.
func orNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}

	return strings.Join(values, ", ")
}

// WriteAll writes one <kind>.json per registered kind into dir and returns the
// paths it wrote, in the order it wrote them. It is what "make schema" runs.
func WriteAll(registry *output.SchemaRegistry, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}

	written := make([]string, 0, len(registry.Kinds()))
	for _, kind := range registry.Kinds() {
		raw, err := registry.Schema(kind)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(dir, kind+schemaExt)
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			return nil, fmt.Errorf("writing %s: %w", path, err)
		}
		written = append(written, path)
	}

	return written, nil
}

// CheckDrift reports every way the schemas checked in under dir disagree with
// what the registry generates: a file that differs, a kind with no file, and a
// file no kind claims. All three are reported at once, because fixing them one
// error message at a time is three runs of the suite.
//
// The checked-in schemas are the published contract. A consumer validates
// against the file in the repository, not against a binary it does not have, so
// a file that no longer matches the code is a contract that lies.
func CheckDrift(registry *output.SchemaRegistry, dir string) error {
	onDisk, err := schemaFiles(dir)
	if err != nil {
		return err
	}

	var problems []string
	for _, kind := range registry.Kinds() {
		name := kind + schemaExt
		raw, found := onDisk[name]
		delete(onDisk, name)

		generated, err := registry.Schema(kind)
		if err != nil {
			return err
		}
		switch {
		case !found:
			problems = append(problems, fmt.Sprintf("%s is missing", name))
		case !bytes.Equal(raw, generated):
			problems = append(problems, fmt.Sprintf("%s differs from what the generator produces", name))
		}
	}

	for _, name := range sortedKeys(onDisk) {
		problems = append(problems, fmt.Sprintf("%s belongs to no registered kind", name))
	}

	if len(problems) == 0 {
		return nil
	}

	return fmt.Errorf("the schemas in %s have drifted; run \"make schema\" and commit the result:\n  %s",
		dir, strings.Join(problems, "\n  "))
}

// schemaFiles reads the schema files of a directory, keyed by file name.
// Anything that is not a schema is ignored: a README explaining the directory
// is not part of the contract.
func schemaFiles(dir string) (map[string][]byte, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", dir, err)
	}

	out := map[string][]byte{}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != schemaExt {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", entry.Name(), err)
		}
		out[entry.Name()] = raw
	}

	return out, nil
}

// sortedKeys returns the keys of a map, sorted.
func sortedKeys(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)

	return out
}
