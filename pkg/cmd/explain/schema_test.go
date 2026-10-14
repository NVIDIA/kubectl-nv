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

package explain_test

import (
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// explain prints a catalog entry, which is Markdown: there is no kind behind
// it and no schema to publish. A caller walking the command tree still asks,
// and "unknown kind or command" would send it looking for a typo in the name
// it just read off --help. The answer names the command and says why it has
// none, and it exits 2 because asking for a schema that cannot exist is the
// caller's to fix.
func TestSchemaForExplainSaysItHasNone(t *testing.T) {
	t.Parallel()

	raw, err := output.SchemaFor("explain")
	if err == nil {
		t.Fatalf("explain has no kind, yet a schema came back: %s", raw)
	}
	if code := output.ExitCodeFor(err); code != output.ExitUsage {
		t.Errorf("asking for the explain schema exits %d, want %d: %v", code, output.ExitUsage, err)
	}
	for _, want := range []string{"explain", "Markdown", "no schema"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not say %q: %v", want, err)
		}
	}
}
