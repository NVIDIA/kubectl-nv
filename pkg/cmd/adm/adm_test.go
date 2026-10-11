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

package adm

import (
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
)

// TestDefaultArtifactDir pins the name the gpu-operator must-gather script
// uses. Support reads these directories by name, and the layout of the
// timestamp is part of that contract.
func TestDefaultArtifactDir(t *testing.T) {
	when := time.Date(2026, time.October, 10, 14, 59, 30, 0, time.UTC)

	got := defaultArtifactDir(when)

	if want := "/tmp/nvidia-gpu-operator_20261010_1459"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestAdmRejectsAnUnknownSubcommand covers the typo directly on the adm
// command, where the missing Args check lived.
func TestAdmRejectsAnUnknownSubcommand(t *testing.T) {
	isolateKubeconfig(t)

	reached := false
	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	adm := NewCommand(o, func(*mustgather.Options) error { reached = true; return nil })

	// Under a parent, which is where adm actually lives. Cobra rejects an
	// unknown first argument to a ROOT command on its own, so an adm executed
	// as its own root would hide the gap.
	parent := &cobra.Command{Use: "kubectl-nv"}
	parent.AddCommand(adm)
	parent.SetArgs([]string{"adm", "must-gaher"})
	parent.SetOut(io.Discard)
	parent.SetErr(io.Discard)

	err := parent.Execute()
	if err == nil {
		t.Fatal("adm must-gaher returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "must-gaher") {
		t.Errorf("error = %q, want it to name the mistyped subcommand", err.Error())
	}
	if reached {
		t.Error("the must-gather ran for a subcommand that does not exist")
	}
}

// isolateKubeconfig keeps a test away from the kubeconfig of whoever is
// running it. KUBECONFIG has to name a path that does not exist rather than be
// emptied: client-go treats an empty KUBECONFIG as unset and falls back to the
// default location, which is the file this is protecting.
func isolateKubeconfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig-here"))
}

// runMustGather executes "adm must-gather" with the given extra arguments and
// returns the options the gather was handed, so that a flag that is declared
// but never read is caught.
func runMustGather(t *testing.T, args ...string) (*mustgather.Options, error) {
	t.Helper()
	isolateKubeconfig(t)

	var got *mustgather.Options
	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	adm := NewCommand(o, func(opts *mustgather.Options) error { got = opts; return nil })

	parent := &cobra.Command{Use: "kubectl-nv", Version: "v9.9.9"}
	parent.AddCommand(adm)
	parent.SetArgs(append([]string{"adm", "must-gather", "--artifacts-dir", t.TempDir()}, args...))
	parent.SetOut(io.Discard)
	parent.SetErr(io.Discard)
	parent.SilenceUsage = true
	parent.SilenceErrors = true

	return got, parent.Execute()
}

// TestMustGatherFlagsReachTheGatherer covers the three flags the command gained
// for Track C. A flag that is declared and never copied into the options parses
// cleanly, prints in the help, and changes nothing about the bundle.
func TestMustGatherFlagsReachTheGatherer(t *testing.T) {
	got, err := runMustGather(t, "--node", "gpu-node-7", "--since", "45m", "--no-exec")
	if err != nil {
		t.Fatalf("adm must-gather: %v", err)
	}
	if got == nil {
		t.Fatal("the must-gather did not run")
	}

	if got.Node != "gpu-node-7" {
		t.Errorf("Node = %q, want gpu-node-7", got.Node)
	}
	if got.Since != 45*time.Minute {
		t.Errorf("Since = %v, want 45m", got.Since)
	}
	if !got.NoExec {
		t.Error("NoExec is false, but --no-exec was given")
	}
}

// TestMustGatherDefaultsLeaveTheGatherUnnarrowed keeps the flags out of the way
// of the run a support engineer does without them.
func TestMustGatherDefaultsLeaveTheGatherUnnarrowed(t *testing.T) {
	got, err := runMustGather(t)
	if err != nil {
		t.Fatalf("adm must-gather: %v", err)
	}
	if got.Node != "" || got.Since != 0 || got.NoExec {
		t.Errorf("defaults are Node=%q Since=%v NoExec=%v, want the whole cluster and the whole log",
			got.Node, got.Since, got.NoExec)
	}
}

// TestMustGatherRecordsTheToolVersion pins what the bundle manifest says
// produced it. A bundle that names no version, or names "dev" because the
// command read the wrong field, sends support looking at the wrong source.
func TestMustGatherRecordsTheToolVersion(t *testing.T) {
	got, err := runMustGather(t)
	if err != nil {
		t.Fatalf("adm must-gather: %v", err)
	}
	if got.ToolVersion != "v9.9.9" {
		t.Errorf("ToolVersion = %q, want the version the root command carries", got.ToolVersion)
	}
}

// TestMustGatherRejectsAnUnparseableSince keeps a typo from being read as "no
// window at all", which would quietly collect the whole log of a cluster that
// has been unhappy for weeks.
func TestMustGatherRejectsAnUnparseableSince(t *testing.T) {
	_, err := runMustGather(t, "--since", "45minutes")
	if err == nil {
		t.Fatal("adm must-gather --since 45minutes returned nil, want a usage error")
	}
	if !strings.Contains(err.Error(), "since") {
		t.Errorf("error = %q, want it to name the flag", err.Error())
	}
}
