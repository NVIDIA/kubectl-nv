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

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// streamsRun is one execution of the real main path: the two streams it wrote
// and the code it would have exited with.
type streamsRun struct {
	stdout string
	stderr string
	code   int
}

// runMain executes the command tree exactly as main does, with the two streams
// separated. It is the whole point of these tests: the stream a command writes
// to is decided in main, so a test that builds a command itself and calls
// SetOut proves nothing about what a user sees.
func runMain(t *testing.T, args ...string) streamsRun {
	t.Helper()
	isolateKubeconfig(t)

	var stdout, stderr bytes.Buffer
	streams := genericiooptions.IOStreams{In: strings.NewReader(""), Out: &stdout, ErrOut: &stderr}
	code := run(args, streams, root.NewCommand("v9.9.9-streams-test", root.Deps{Streams: streams}))

	return streamsRun{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// leafPaths returns the argument path of every command of the tree that does
// work, computed by walking the tree rather than written down, so that a
// command another track adds is covered without this test being edited.
func leafPaths(c *cobra.Command) [][]string {
	var out [][]string
	var walk func(*cobra.Command, []string)
	walk = func(current *cobra.Command, path []string) {
		children := current.Commands()
		if len(children) == 0 {
			if len(path) > 0 {
				out = append(out, path)
			}

			return
		}
		for _, child := range children {
			if child.Hidden || child.Name() == "help" || child.Name() == "completion" {
				continue
			}
			walk(child, append(append([]string{}, path...), child.Name()))
		}
	}
	walk(c, nil)

	return out
}

// Help is what the user asked for, so it is the answer and belongs on stdout.
// Every leaf, not a sample: the stream is decided once in main, and a leaf that
// got it wrong would be the one nobody checked.
func TestEveryLeafPrintsItsHelpOnStdout(t *testing.T) {
	tree := root.NewCommand("v9.9.9-streams-test", root.Deps{
		Streams: genericiooptions.NewTestIOStreamsDiscard(),
	})

	leaves := leafPaths(tree)
	if len(leaves) == 0 {
		t.Fatal("the tree has no leaves: the guard would prove nothing")
	}

	for _, path := range leaves {
		name := strings.Join(path, " ")
		t.Run(name, func(t *testing.T) {
			got := runMain(t, append(append([]string{}, path...), "--help")...)
			if got.code != output.ExitOK {
				t.Fatalf("%s --help exited %d\nstderr:\n%s", name, got.code, got.stderr)
			}
			if !strings.Contains(got.stdout, "Usage:") {
				t.Errorf("%s --help put no usage on stdout\nstdout:\n%s\nstderr:\n%s",
					name, got.stdout, got.stderr)
			}
			if got.stderr != "" {
				t.Errorf("%s --help wrote to stderr: %q", name, got.stderr)
			}
		})
	}
}

// The data a command produces goes to stdout, so that a pipeline gets it. These
// are the leaves that answer without a cluster, which is what lets the
// assertion be made against the real main path rather than against a fake.
//
// kubectl nv version printed its 25 bytes to stderr and nothing to stdout, and
// kubectl nv explain NV-NODE-002 > entry.md wrote an empty file. Both were
// measured on the binary before this test existed.
func TestDataGoesToStdout(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "version", args: []string{"version"}, want: "kubectl-nv version v9.9.9-streams-test"},
		{name: "explain", args: []string{"explain", "NV-NODE-002"}, want: "NV-NODE-002"},
		{name: "schema", args: []string{"schema", "list"}, want: "\"kind\""},
		{name: "mcp --list-tools", args: []string{"mcp", "--list-tools"}, want: "list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := runMain(t, tc.args...)
			if got.code != output.ExitOK {
				t.Fatalf("exited %d\nstdout:\n%s\nstderr:\n%s", got.code, got.stdout, got.stderr)
			}
			if !strings.Contains(got.stdout, tc.want) {
				t.Errorf("stdout does not carry %q\nstdout:\n%s\nstderr:\n%s",
					tc.want, got.stdout, got.stderr)
			}
			if got.stderr != "" {
				t.Errorf("stderr is %q, want nothing: these commands have no progress to report", got.stderr)
			}
		})
	}
}

// A command the user got wrong is a diagnostic, and a diagnostic on stdout is
// a pipeline reading an error message as data.
func TestUsageErrorsGoToStderr(t *testing.T) {
	got := runMain(t, "explain", "NV-NOT-A-CODE")

	if got.code != output.ExitUsage {
		t.Fatalf("exited %d, want %d\nstderr:\n%s", got.code, output.ExitUsage, got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout is %q, want nothing: the answer is an error", got.stdout)
	}
	if !strings.Contains(got.stderr, "NV-NOT-A-CODE") {
		t.Errorf("stderr does not name the code that was wrong: %q", got.stderr)
	}
}
