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

package root_test

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	nvcmd "github.com/NVIDIA/kubectl-nv/pkg/cmd"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
)

// TestRootRejectsUnparseableDebugEnvironment keeps DEBUG from being read as
// "set means true": the urfave tree failed on a value it could not parse, and
// a silent fallback would hide a typo in a support engineer's shell.
func TestRootRejectsUnparseableDebugEnvironment(t *testing.T) {
	isolateKubeconfig(t)

	t.Setenv("DEBUG", "yesplease")
	kubeconfig := writeKubeconfig(t)

	cmd := root.NewCommand("test", root.Deps{
		Streams:    genericiooptions.NewTestIOStreamsDiscard(),
		MustGather: func(*mustgather.Options) error { return nil },
	})
	cmd.SetArgs([]string{"adm", "must-gather", "--kubeconfig", kubeconfig})
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SilenceUsage = true

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected an error for DEBUG=yesplease, got nil")
	}
	if !strings.Contains(err.Error(), "DEBUG") {
		t.Errorf("error = %q, want it to name DEBUG", err.Error())
	}
}

// TestVersionIsReported covers both ways a user asks for the version.
func TestVersionIsReported(t *testing.T) {
	isolateKubeconfig(t)

	for _, args := range [][]string{{"--version"}, {"version"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out := &bytes.Buffer{}
			cmd := root.NewCommand("v1.2.3-test", root.Deps{
				Streams:    genericiooptions.NewTestIOStreamsDiscard(),
				MustGather: func(*mustgather.Options) error { return nil },
			})
			cmd.SetArgs(args)
			cmd.SetOut(out)
			cmd.SetErr(out)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute(%v) returned %v", args, err)
			}
			if !strings.Contains(out.String(), "v1.2.3-test") {
				t.Errorf("output = %q, want it to contain the version", out.String())
			}
		})
	}
}

// writeKubeconfig drops a syntactically valid kubeconfig on disk and returns
// its path. The path has to exist: resolution checks the filesystem.
// TestRuntimeFailureDoesNotPrintUsage keeps a cluster that could not be reached
// from being reported as a usage mistake: the flags were fine, and forty lines
// of flag help bury the one line that says what went wrong.
func TestRuntimeFailureDoesNotPrintUsage(t *testing.T) {
	isolateKubeconfig(t)

	out := &bytes.Buffer{}
	cmd := root.NewCommand("test", root.Deps{
		Streams: genericiooptions.NewTestIOStreamsDiscard(),
		MustGather: func(*mustgather.Options) error {
			return errors.New("dial tcp 127.0.0.1:1: connect: connection refused")
		},
	})
	cmd.SetArgs([]string{"adm", "must-gather", "--kubeconfig", writeKubeconfig(t)})
	cmd.SetOut(out)
	cmd.SetErr(out)

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected the must-gather failure to be returned")
	}
	if strings.Contains(out.String(), "Usage:") {
		t.Errorf("output = %q, want no usage block for a runtime failure", out.String())
	}
}

// TestFlagMistakeStillPrintsUsage is the other half: an unknown flag is a usage
// mistake, and the user should see the flags on offer.
func TestFlagMistakeStillPrintsUsage(t *testing.T) {
	isolateKubeconfig(t)

	out := &bytes.Buffer{}
	cmd := root.NewCommand("test", root.Deps{
		Streams:    genericiooptions.NewTestIOStreamsDiscard(),
		MustGather: func(*mustgather.Options) error { return nil },
	})
	cmd.SetArgs([]string{"adm", "must-gather", "--not-a-flag"})
	cmd.SetOut(out)
	cmd.SetErr(out)

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for an unknown flag")
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("output = %q, want the usage block for a flag mistake", out.String())
	}
}

// TestUnknownSubcommandIsAnError keeps a typo from looking like success. The
// adm command used to print its help and exit 0 for "adm must-gaher", so a
// script that collected nothing reported that it had.
func TestUnknownSubcommandIsAnError(t *testing.T) {
	isolateKubeconfig(t)

	cases := []struct {
		name    string
		args    []string
		wantHas string
	}{
		{name: "typo in a subcommand", args: []string{"adm", "must-gaher"}, wantHas: "must-gaher"},
		{name: "unknown top-level command", args: []string{"bogus"}, wantHas: "bogus"},
		{name: "stray argument to a leaf", args: []string{"adm", "must-gather", "extra"}, wantHas: "extra"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reached := false
			cmd := root.NewCommand("test", root.Deps{
				Streams:    genericiooptions.NewTestIOStreamsDiscard(),
				MustGather: func(*mustgather.Options) error { reached = true; return nil },
			})
			cmd.SetArgs(c.args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)

			err := cmd.Execute()
			if err == nil {
				t.Fatalf("Execute(%v) returned nil, want an error", c.args)
			}
			if !strings.Contains(err.Error(), c.wantHas) {
				t.Errorf("error = %q, want it to name %q", err.Error(), c.wantHas)
			}
			if reached {
				t.Error("the must-gather ran for an invocation that should not have parsed")
			}
		})
	}
}

// TestParentHooksRunForASubcommandWithItsOwnHook covers the shape every
// command added after this one will have. Cobra runs only the closest
// PersistentPreRunE in the chain unless told otherwise, so a subcommand that
// defines one of its own would otherwise skip the root's and run with no
// client factory and no debug setting at all.
func TestParentHooksRunForASubcommandWithItsOwnHook(t *testing.T) {
	isolateKubeconfig(t)

	t.Setenv("DEBUG", "1")

	options := &nvcmd.Options{}
	cmd := root.NewCommand("test", root.Deps{
		Streams:    genericiooptions.NewTestIOStreamsDiscard(),
		Options:    options,
		MustGather: func(*mustgather.Options) error { return nil },
	})

	ran := false
	probe := &cobra.Command{
		Use: "probe",
		// A hook of its own, which is what hides the root's.
		PersistentPreRunE: func(*cobra.Command, []string) error { return nil },
		RunE:              func(*cobra.Command, []string) error { ran = true; return nil },
	}
	cmd.AddCommand(probe)

	cmd.SetArgs([]string{"probe"})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute returned %v", err)
	}
	if !ran {
		t.Fatal("the subcommand did not run")
	}
	if options.Factory == nil {
		t.Error("factory is nil: the subcommand would panic on its first client")
	}
	if !options.Debug {
		t.Error("debug is false although DEBUG=1: the root hook did not run")
	}
}
