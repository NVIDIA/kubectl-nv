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

// Package agents holds the files that let another program drive kubectl-nv:
// the skill, the HolmesGPT toolset and the kubectl-ai tools. There is no Go
// code here, only the tests that keep those files pointing at commands and
// flags the binary really has.
package agents

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
)

// repoRoot is the checkout this test runs from, resolved from the path of
// this source file rather than from the working directory or from $HOME: a
// test that read a deployed copy would green-light a file this tree does not
// ship.
func repoRoot(t *testing.T) string {
	t.Helper()

	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller could not name this test file")
	}

	root := filepath.Dir(filepath.Dir(self))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("resolved repository root %q has no go.mod: %v", root, err)
	}

	return root
}

// readRepoFile reads a file named relative to the repository root.
func readRepoFile(t *testing.T, rel string) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}

	return string(raw)
}

// commandTree builds the same tree the binary serves. Nothing is run against
// it: the tests only ask which commands and flags exist.
func commandTree(t *testing.T) *cobra.Command {
	t.Helper()

	return root.NewCommand("test", root.Deps{})
}

// resolveCommandLine checks that one `kubectl nv ...` command line names a
// command that exists and uses only flags that command accepts. It returns the
// command the line resolves to.
//
// The whole point is that it fails when a command or a flag is renamed, so it
// refuses rather than skips anything it cannot account for.
func resolveCommandLine(t *testing.T, tree *cobra.Command, line string) *cobra.Command {
	t.Helper()

	tokens := strings.Fields(line)
	switch {
	case len(tokens) >= 2 && tokens[0] == "kubectl" && tokens[1] == "nv":
		tokens = tokens[2:]
	case len(tokens) >= 1 && (tokens[0] == "kubectl-nv" || tokens[0] == "./kubectl-nv" ||
		tokens[0] == "$KUBECTL_NV" || tokens[0] == "${KUBECTL_NV}"):
		tokens = tokens[1:]
	default:
		t.Fatalf("command line %q does not start with kubectl nv", line)
	}

	cmd := tree
	for len(tokens) > 0 && !strings.HasPrefix(tokens[0], "-") {
		child := childNamed(cmd, tokens[0])
		if child == nil {
			if cmd == tree {
				t.Fatalf("command line %q names %q, which is not a kubectl-nv command", line, tokens[0])
			}
			// A non-flag token under a real command is a positional argument.
			break
		}
		cmd = child
		tokens = tokens[1:]
	}

	checkFlags(t, cmd, line, tokens)

	return cmd
}

// childNamed returns the subcommand with this name or alias, or nil.
func childNamed(cmd *cobra.Command, name string) *cobra.Command {
	for _, child := range cmd.Commands() {
		if child.Name() == name {
			return child
		}
		for _, alias := range child.Aliases {
			if alias == name {
				return child
			}
		}
	}

	return nil
}

// checkFlags fails the test for any flag in the remaining tokens that the
// command does not accept, and skips the value of a flag that takes one so
// that the value is never read as a flag of its own.
func checkFlags(t *testing.T, cmd *cobra.Command, line string, tokens []string) {
	t.Helper()

	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		if !strings.HasPrefix(token, "-") || token == "-" || token == "--" {
			continue
		}

		name, _, hasInlineValue := strings.Cut(token, "=")

		flag := lookupFlag(cmd, name)
		if flag == nil {
			t.Fatalf("command line %q uses %s, which %q does not accept", line, name, cmd.CommandPath())
		}

		// A flag that takes a value consumes the next token, unless the value
		// was spelled with an equals sign.
		if !hasInlineValue && flag.Value.Type() != "bool" && i+1 < len(tokens) {
			i++
		}
	}
}

// lookupFlag finds a flag by its long or short spelling, among the command's
// own flags and the ones it inherits.
func lookupFlag(cmd *cobra.Command, name string) *pflag.Flag {
	switch {
	case strings.HasPrefix(name, "--"):
		long := strings.TrimPrefix(name, "--")
		if flag := cmd.Flags().Lookup(long); flag != nil {
			return flag
		}
		return cmd.InheritedFlags().Lookup(long)
	case strings.HasPrefix(name, "-"):
		short := strings.TrimPrefix(name, "-")
		if len(short) != 1 {
			return nil
		}
		if flag := cmd.Flags().ShorthandLookup(short); flag != nil {
			return flag
		}
		return cmd.InheritedFlags().ShorthandLookup(short)
	}

	return nil
}
