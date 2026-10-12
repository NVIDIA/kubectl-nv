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
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
)

// TestMustGatherFlagCompatibility pins the invocations the urfave/cli tree
// accepted, so the cobra tree cannot quietly drop one of them. Each case runs
// the real flag parsing and stops at the must-gather seam, where the resolved
// options are captured.
func TestMustGatherFlagCompatibility(t *testing.T) {
	kubeconfig := writeKubeconfig(t)
	otherKubeconfig := writeKubeconfig(t)

	cases := []struct {
		name            string
		args            []string
		env             map[string]string
		wantKubeconfig  string
		wantArtifactDir string
		wantDebug       bool
	}{
		{
			name:           "kubeconfig long flag",
			args:           []string{"adm", "must-gather", "--kubeconfig", kubeconfig},
			wantKubeconfig: kubeconfig,
		},
		{
			name:           "kubeconfig short flag",
			args:           []string{"adm", "must-gather", "-k", kubeconfig},
			wantKubeconfig: kubeconfig,
		},
		{
			name:           "kubeconfig from environment",
			args:           []string{"adm", "must-gather"},
			env:            map[string]string{"KUBECONFIG": kubeconfig},
			wantKubeconfig: kubeconfig,
		},
		{
			name:           "flag wins over environment",
			args:           []string{"adm", "must-gather", "--kubeconfig", kubeconfig},
			env:            map[string]string{"KUBECONFIG": otherKubeconfig},
			wantKubeconfig: kubeconfig,
		},
		{
			// Reporting the KUBECONFIG path here, as a second resolver did, tells
			// the user the gather came from a cluster it never looked at.
			name:           "an explicit path that does not exist is still what we report",
			args:           []string{"adm", "must-gather", "--kubeconfig", "/nonexistent/config"},
			env:            map[string]string{"KUBECONFIG": kubeconfig},
			wantKubeconfig: "/nonexistent/config",
		},
		{
			name:            "artifacts dir flag",
			args:            []string{"adm", "must-gather", "--kubeconfig", kubeconfig, "--artifacts-dir", "/tmp/b3-artifacts"},
			wantKubeconfig:  kubeconfig,
			wantArtifactDir: "/tmp/b3-artifacts",
		},
		{
			name:            "artifacts dir from environment",
			args:            []string{"adm", "must-gather", "--kubeconfig", kubeconfig},
			env:             map[string]string{"ARTIFACT_DIR": "/tmp/b3-env-artifacts"},
			wantKubeconfig:  kubeconfig,
			wantArtifactDir: "/tmp/b3-env-artifacts",
		},
		{
			name:            "artifacts dir flag wins over environment",
			args:            []string{"adm", "must-gather", "--kubeconfig", kubeconfig, "--artifacts-dir", "/tmp/b3-artifacts"},
			env:             map[string]string{"ARTIFACT_DIR": "/tmp/b3-env-artifacts"},
			wantKubeconfig:  kubeconfig,
			wantArtifactDir: "/tmp/b3-artifacts",
		},
		{
			name:           "debug long flag",
			args:           []string{"--debug", "adm", "must-gather", "--kubeconfig", kubeconfig},
			wantKubeconfig: kubeconfig,
			wantDebug:      true,
		},
		{
			name:           "debug short flag",
			args:           []string{"-d", "adm", "must-gather", "--kubeconfig", kubeconfig},
			wantKubeconfig: kubeconfig,
			wantDebug:      true,
		},
		{
			name:           "debug flag after the subcommand",
			args:           []string{"adm", "must-gather", "--kubeconfig", kubeconfig, "--debug"},
			wantKubeconfig: kubeconfig,
			wantDebug:      true,
		},
		{
			name:           "debug from environment",
			args:           []string{"adm", "must-gather", "--kubeconfig", kubeconfig},
			env:            map[string]string{"DEBUG": "1"},
			wantKubeconfig: kubeconfig,
			wantDebug:      true,
		},
		{
			name:           "debug off by default",
			args:           []string{"adm", "must-gather", "--kubeconfig", kubeconfig},
			wantKubeconfig: kubeconfig,
			wantDebug:      false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			isolateKubeconfig(t)
			for _, k := range []string{"ARTIFACT_DIR", "DEBUG"} {
				t.Setenv(k, "")
			}
			for k, v := range c.env {
				t.Setenv(k, v)
			}

			var got *mustgather.Options
			progress := &bytes.Buffer{}
			cmd := root.NewCommand("test", root.Deps{
				Streams: genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: io.Discard, ErrOut: progress},
				MustGather: func(o *mustgather.Options) error {
					got = o
					return nil
				},
			})
			cmd.SetArgs(c.args)
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})

			if err := cmd.Execute(); err != nil {
				t.Fatalf("Execute(%v) returned %v", c.args, err)
			}
			if got == nil {
				t.Fatalf("Execute(%v) never reached the must-gather seam", c.args)
			}
			// The kubeconfig in play is what the run tells the user it is: there
			// is no second copy of it to inspect, because the client
			// configuration is the only thing that resolves one.
			wantLine := "Using kubeconfig: " + c.wantKubeconfig + "\n"
			if !strings.Contains(progress.String(), wantLine) {
				t.Errorf("progress = %q, want it to contain %q", progress.String(), wantLine)
			}
			if c.wantArtifactDir != "" && got.ArtifactDir != c.wantArtifactDir {
				t.Errorf("artifacts dir = %q, want %q", got.ArtifactDir, c.wantArtifactDir)
			}
			if c.wantArtifactDir == "" && !strings.HasPrefix(got.ArtifactDir, "/tmp/nvidia-gpu-operator_") {
				t.Errorf("default artifacts dir = %q, want the /tmp/nvidia-gpu-operator_<timestamp> default", got.ArtifactDir)
			}
			if got.Debug != c.wantDebug {
				t.Errorf("debug = %v, want %v", got.Debug, c.wantDebug)
			}
		})
	}
}

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := t.TempDir() + "/config"
	contents := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
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
