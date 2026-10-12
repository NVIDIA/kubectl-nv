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
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// guardChildEnv tells a re-executed test binary that it is the child half of
// the guard below and should run the invocations rather than set them up.
const guardChildEnv = "KUBECTL_NV_KUBECONFIG_GUARD_CHILD"

// TestTheSuiteNeverRunsAnExecCredentialPlugin is the test that protects the
// other tests. A kubeconfig can name a program to run for credentials, and
// client-go runs it as soon as something builds a client from that file.
// "make test" on a developer's machine, or on a shared runner, must never be
// able to do that: the suite would be executing whatever the person it belongs
// to has configured.
//
// It has to run in a child process. client-go fixes the default kubeconfig
// path from HOME when its package loads, so a HOME set inside a running test
// does not move it; only a process started with a different HOME sees a
// different default.
func TestTheSuiteNeverRunsAnExecCredentialPlugin(t *testing.T) {
	if os.Getenv(guardChildEnv) == "1" {
		t.Skip("the child half runs the guarded tests by name, not this one")
	}

	home := t.TempDir()
	marker := filepath.Join(home, "the-plugin-ran")
	writeExecPluginKubeconfig(t, home, marker)

	child := exec.Command(os.Args[0], "-test.run", guardedTests, "-test.v")
	child.Env = append(environmentWithout("HOME", "KUBECONFIG"),
		guardChildEnv+"=1",
		"HOME="+home,
	)
	out, err := child.CombinedOutput()

	if _, statErr := os.Stat(marker); statErr == nil {
		contents, _ := os.ReadFile(marker)
		t.Fatalf("an exec credential plugin from the default kubeconfig ran during the suite\n"+
			"marker: %s\nchild output:\n%s", contents, out)
	}
	if err != nil {
		t.Fatalf("the guarded tests do not pass without a real kubeconfig: %v\n%s", err, out)
	}
}

// guardedTests are the tests re-run under the fake HOME. They are the ones
// that build a client or execute the command tree, which are the only ones
// that could reach a kubeconfig. Running them by name, rather than a
// hand-written list of command lines, means the guard covers what the suite
// actually does.
const guardedTests = "TestExitCodes|TestFlagErrorsAreUsageErrorsOnTheirOwn|TestPanicBecomesAnExitCode"

// writeExecPluginKubeconfig puts a kubeconfig at the default location under
// home, whose user runs a program for its credentials. The program records
// that it ran, which is the thing the guard looks for.
func writeExecPluginKubeconfig(t *testing.T, home, marker string) {
	t.Helper()

	kubeDir := filepath.Join(home, ".kube")
	if err := os.MkdirAll(kubeDir, 0o700); err != nil {
		t.Fatal(err)
	}

	plugin := filepath.Join(home, "credential-plugin.sh")
	script := "#!/bin/sh\necho ran > " + marker + "\n" +
		`echo '{"apiVersion":"client.authentication.k8s.io/v1","kind":"ExecCredential",` +
		`"status":{"token":"guard"}}'` + "\n"
	if err := os.WriteFile(plugin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: guard
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: guard
  context:
    cluster: guard
    user: guard
current-context: guard
users:
- name: guard
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1
      command: ` + plugin + `
      interactiveMode: Never
`
	if err := os.WriteFile(filepath.Join(kubeDir, "config"), []byte(kubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
}

// environmentWithout returns the environment with the named variables removed,
// so the child cannot inherit a KUBECONFIG that would hide the default path.
func environmentWithout(names ...string) []string {
	drop := make(map[string]bool, len(names))
	for _, name := range names {
		drop[name] = true
	}

	var out []string
	for _, entry := range os.Environ() {
		key, _, _ := bytes.Cut([]byte(entry), []byte("="))
		if !drop[string(key)] {
			out = append(out, entry)
		}
	}
	return out
}
