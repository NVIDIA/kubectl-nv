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

package debug_test

import (
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/info"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// debug and info answer the same question about a node that is not a GPU node,
// and a support engineer reading one answer has to recognise the other. Two
// copies of the sentence drift the moment either is improved, so there is one,
// and this is what says debug uses it rather than a copy that happens to match.
func TestTheUnknownNodeMessageIsTheOneInfoPrints(t *testing.T) {
	t.Parallel()

	_, err := runDebug(t, gatherSeam{}, "plain-worker")
	if err == nil {
		t.Fatal("debug accepted a node with no GPUs and no GPU labels")
	}

	want := info.NotAGPUNodeError("plain-worker")
	if err.Error() != want.Error() {
		t.Errorf("debug says\n  %q\ninfo says\n  %q", err.Error(), want.Error())
	}
	if code := output.ExitCodeFor(err); code != output.ExitUsage {
		t.Errorf("debug exits %d on it, want %d", code, output.ExitUsage)
	}
}
