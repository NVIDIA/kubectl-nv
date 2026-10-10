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
	"path/filepath"
	"testing"
)

// isolateKubeconfig keeps a test away from the kubeconfig of whoever is
// running it. Every test that builds a client or executes the command tree
// calls it first.
//
// KUBECONFIG has to name a path that does not exist rather than be emptied:
// client-go treats an empty KUBECONFIG as unset and falls back to the default
// location, which is the file this is protecting. HOME is set as well for the
// code that reads it at call time, though on its own it would not be enough,
// because client-go fixes the default path when its package loads.
func isolateKubeconfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig-here"))
}
