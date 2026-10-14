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
	"io"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// TestArgumentErrorsExitTwo pins that a command line the user got wrong exits
// 2, through the whole tree rather than through a command built by hand.
//
// A leaf that silences usage before cobra has validated its arguments turns a
// typo into exit 4, "the run could not happen", which tells a script that the
// cluster is unreachable when the truth is that the user typed too many words.
// It does not run in parallel: isolate uses t.Setenv, which testing refuses
// alongside t.Parallel.
func TestArgumentErrorsExitTwo(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "info with no node", args: []string{"info"}},
		{name: "info with two nodes", args: []string{"info", "a", "b"}},
		{name: "list with a positional argument", args: []string{"list", "extra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isolate(t)

			c := root.NewCommand("test", root.Deps{Streams: genericiooptions.NewTestIOStreamsDiscard()})
			c.SetArgs(append(tt.args, "--kubeconfig", unreachableKubeconfig(t)))
			c.SetOut(io.Discard)
			c.SetErr(io.Discard)
			c.SilenceErrors = true

			err := c.Execute()
			if err == nil {
				t.Fatalf("kubectl-nv %v returned no error", tt.args)
			}

			if code := output.ExitCodeFor(err); code != output.ExitUsage {
				t.Errorf("kubectl-nv %v exited %d, want %d: %v", tt.args, code, output.ExitUsage, err)
			}
		})
	}
}

// isolate keeps a test away from the kubeconfig of whoever is running it.
// KUBECONFIG has to name a path that does not exist rather than be emptied:
// client-go reads an empty value as unset and falls back to the default
// location, which is the file this is protecting.
func isolate(t *testing.T) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig-here"))
}

// unreachableKubeconfig writes a kubeconfig pointing at a port nothing серves,
// so that a command which gets past argument validation fails on the cluster
// rather than on the file.
func unreachableKubeconfig(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "config")
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
