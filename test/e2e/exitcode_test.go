//go:build e2e

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

package e2e

import (
	"path/filepath"
	"testing"
)

// TestUnknownFlagIsAUsageError pins the code a script branches on when the
// command line is wrong. Cobra's default is 1, which a caller cannot tell from
// "the run found something", so the tree overrides it; an override that is
// only unit tested does not prove what the built binary exits with.
func TestUnknownFlagIsAUsageError(t *testing.T) {
	res := run(t, "adm", "must-gather", "--bogus")

	if res.code != 2 {
		t.Errorf("exit code = %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}
	// A caller parsing stdout must not be handed a usage message on it.
	if res.stdout != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}
}

// TestUnreachableClusterIsAnError covers the other end: a gather that could not
// happen at all. Exit 4 says nothing was collected, as against exit 3, which
// says part of the bundle is there and worth sending.
func TestUnreachableClusterIsAnError(t *testing.T) {
	// A path inside a fresh temporary directory: it does not exist, and nothing
	// else on the machine can make it exist while the test runs.
	missing := filepath.Join(t.TempDir(), "kubeconfig-that-is-not-there")

	res := runWithKubeconfig(t, missing, "adm", "must-gather",
		"--artifacts-dir", filepath.Join(t.TempDir(), "artifacts"))

	if res.code != 4 {
		t.Errorf("exit code = %d, want 4\nstderr:\n%s", res.code, res.stderr)
	}
	if res.stdout != "" {
		t.Errorf("stdout = %q, want empty", res.stdout)
	}
}
