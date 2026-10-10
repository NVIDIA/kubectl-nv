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
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// TestExitCodes is the contract scripts branch on. A wrong exit code is worse
// than a wrong message: nobody reads the message in CI.
func TestExitCodes(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  map[string]string
		want int
	}{
		{name: "a command that works", args: []string{"version"}, want: output.ExitOK},
		{name: "an unknown flag", args: []string{"--bogus"}, want: output.ExitUsage},
		{name: "a typo in a subcommand", args: []string{"adm", "must-gaher"}, want: output.ExitUsage},
		{
			name: "an unreadable DEBUG",
			args: []string{"version"},
			env:  map[string]string{"DEBUG": "notabool"},
			want: output.ExitUsage,
		},
		{
			// Nothing listens there, so the gather cannot start and the bundle
			// says nothing about any cluster.
			name: "a cluster that cannot be reached",
			args: []string{"adm", "must-gather", "--server", "https://127.0.0.1:1", "--artifacts-dir"},
			want: output.ExitError,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, k := range []string{"KUBECONFIG", "ARTIFACT_DIR", "DEBUG"} {
				t.Setenv(k, "")
			}
			for k, v := range c.env {
				t.Setenv(k, v)
			}

			args := append([]string{}, c.args...)
			if len(args) > 0 && args[len(args)-1] == "--artifacts-dir" {
				args = append(args, filepath.Join(t.TempDir(), "artifacts"))
			}

			out := &bytes.Buffer{}
			streams := genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: out, ErrOut: out}

			got := run(args, streams, root.NewCommand("test", root.Deps{Streams: streams}))
			if got != c.want {
				t.Errorf("exit code = %d, want %d\noutput:\n%s", got, c.want, out.String())
			}
		})
	}
}

// TestPanicBecomesAnExitCode keeps a bug in kubectl-nv from reaching the user
// as a page of runtime output. The stack is still there when it is asked for.
func TestPanicBecomesAnExitCode(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantStack bool
	}{
		{name: "without debug", args: []string{"boom"}},
		{name: "with debug", args: []string{"--debug", "boom"}, wantStack: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			streams := genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: out, ErrOut: out}

			tree := root.NewCommand("test", root.Deps{Streams: streams})
			tree.AddCommand(&cobra.Command{
				Use: "boom",
				RunE: func(*cobra.Command, []string) error {
					panic("a nil map somewhere")
				},
			})

			got := run(c.args, streams, tree)
			if got != output.ExitError {
				t.Errorf("exit code = %d, want %d", got, output.ExitError)
			}
			if !strings.Contains(out.String(), "a nil map somewhere") {
				t.Errorf("output does not say what happened:\n%s", out.String())
			}
			hasStack := strings.Contains(out.String(), "goroutine ")
			if hasStack != c.wantStack {
				t.Errorf("stack printed = %v, want %v\n%s", hasStack, c.wantStack, out.String())
			}
		})
	}
}
