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
