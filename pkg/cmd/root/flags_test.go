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
	"os"
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
			for _, k := range []string{"KUBECONFIG", "ARTIFACT_DIR", "DEBUG"} {
				t.Setenv(k, "")
			}
			for k, v := range c.env {
				t.Setenv(k, v)
			}

			var got *mustgather.Options
			cmd := root.NewCommand("test", root.Deps{
				Streams: genericiooptions.NewTestIOStreamsDiscard(),
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
			if got.Kubeconfig != c.wantKubeconfig {
				t.Errorf("kubeconfig = %q, want %q", got.Kubeconfig, c.wantKubeconfig)
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

// TestRootRejectsUnparseableDebugEnvironment keeps DEBUG from being read as
// "set means true": the urfave tree failed on a value it could not parse, and
// a silent fallback would hide a typo in a support engineer's shell.
func TestRootRejectsUnparseableDebugEnvironment(t *testing.T) {
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

// TestRuntimeFailureDoesNotPrintUsage keeps a cluster that could not be reached
// from being reported as a usage mistake: the flags were fine, and forty lines
// of flag help bury the one line that says what went wrong.
func TestRuntimeFailureDoesNotPrintUsage(t *testing.T) {
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
