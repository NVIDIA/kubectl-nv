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

package mustgather_test

import (
	"os/exec"
	"strings"
	"testing"
)

// TestMustGatherDoesNotDependOnTheCommandPackages keeps the gatherer a library
// the commands call rather than the other way round. It is the kind of import
// that is added without thinking, for one type, and then cannot be undone
// because everything else followed it.
func TestMustGatherDoesNotDependOnTheCommandPackages(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps: %v", err)
	}

	const forbidden = "github.com/NVIDIA/kubectl-nv/pkg/cmd"
	for _, dep := range strings.Fields(string(out)) {
		if dep == forbidden || strings.HasPrefix(dep, forbidden+"/") {
			t.Errorf("pkg/mustgather depends on %s: the command packages call the gatherer, not the other way round", dep)
		}
	}
}
