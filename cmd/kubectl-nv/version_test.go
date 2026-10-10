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
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestVersionComesFromLdflags builds the binary the way the release does and
// checks the stamped version reaches the user. A plain unit test on the
// variable cannot catch a wrong -X path in the Makefile or in goreleaser, which
// is exactly how a release ends up reporting "dev".
func TestVersionComesFromLdflags(t *testing.T) {
	if version != "dev" {
		t.Fatalf("unstamped version = %q, want the %q default", version, "dev")
	}

	// No isolation here on purpose: this test shells out to "go build", whose
	// caches live under HOME, and it never builds a client. "--version" and
	// "version" read no kubeconfig, which the credential-plugin guard checks.
	const stamped = "v9.9.9-ldflags-test"
	binary := filepath.Join(t.TempDir(), "kubectl-nv")

	build := exec.Command("go", "build", "-ldflags", "-X main.version="+stamped, "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("building the binary failed: %v\n%s", err, out)
	}

	for _, args := range [][]string{{"--version"}, {"version"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			out, err := exec.Command(binary, args...).CombinedOutput()
			if err != nil {
				t.Fatalf("running %v failed: %v\n%s", args, err, out)
			}
			if !strings.Contains(string(out), stamped) {
				t.Errorf("output = %q, want it to contain %q", out, stamped)
			}
		})
	}
}
