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

package findings_test

import (
	"path"
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/docs/findings"
	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
)

// The catalog and the rule set have to be the same set, in both directions.
//
// A rule with no entry hands the user a code, a link to a file that is not
// there, and nothing to read. An entry with no rule is a page describing a
// check that cannot fire, which is worse than no page: a reader who finds it
// concludes the check exists.
func TestEveryRuleHasACatalogEntry(t *testing.T) {
	t.Parallel()

	for _, rule := range rules.All() {
		if _, ok := findings.Entry(rule.Code()); !ok {
			t.Errorf("rule %s has no catalog entry: add %s", rule.Code(), health.CatalogPath(rule.Code()))
		}
	}
}

func TestEveryCatalogEntryHasARule(t *testing.T) {
	t.Parallel()

	registered := map[string]bool{}
	for _, rule := range rules.All() {
		registered[rule.Code()] = true
	}

	for _, code := range findings.Codes() {
		if !registered[code] {
			t.Errorf("catalog entry %s describes no registered rule", code)
		}
	}
}

// The engine fills a finding's Docs from health.CatalogPath and the binary
// serves the entry from this package. If those two disagree about where an
// entry lives, every report links somewhere the file is not.
func TestCatalogPathNamesTheEmbeddedFile(t *testing.T) {
	t.Parallel()

	for _, code := range findings.Codes() {
		docs := health.CatalogPath(code)

		if want := code + ".md"; path.Base(docs) != want {
			t.Errorf("CatalogPath(%q) is %q, whose file name is not %q", code, docs, want)
		}

		if dir := path.Dir(docs); dir != "docs/findings" {
			t.Errorf("CatalogPath(%q) is %q, want it under docs/findings", code, docs)
		}
	}
}

// A code is what a user types, and nobody types it the way a machine writes
// it. Looking one up has to work from a copy-pasted lowercase code and from a
// code with stray whitespace around it.
func TestEntryNormalisesTheCode(t *testing.T) {
	t.Parallel()

	canonical, ok := findings.Entry("NV-NODE-002")
	if !ok {
		t.Fatal("NV-NODE-002 has no entry")
	}

	for _, typed := range []string{"nv-node-002", "  NV-NODE-002  ", "Nv-Node-002"} {
		got, ok := findings.Entry(typed)
		if !ok {
			t.Errorf("Entry(%q) found nothing", typed)

			continue
		}

		if got != canonical {
			t.Errorf("Entry(%q) returned a different entry from the canonical one", typed)
		}
	}
}

func TestEntryRejectsAnUnknownCode(t *testing.T) {
	t.Parallel()

	for _, code := range []string{"NV-NODE-999", "", "not a code", "../../etc/passwd", "NV-NODE-002.md"} {
		if entry, ok := findings.Entry(code); ok {
			t.Errorf("Entry(%q) returned %d bytes, want nothing", code, len(entry))
		}
	}
}

// Codes is what the command prints when a user asks for a code that is not
// there, so it has to be sorted and complete.
func TestCodesAreSortedAndComplete(t *testing.T) {
	t.Parallel()

	codes := findings.Codes()
	if len(codes) != len(rules.All()) {
		t.Errorf("Codes returns %d entries, want %d", len(codes), len(rules.All()))
	}

	for i := 1; i < len(codes); i++ {
		if codes[i-1] >= codes[i] {
			t.Errorf("Codes is not sorted: %q comes before %q", codes[i-1], codes[i])
		}
	}
}

// Every entry is what a user reads when a finding points at it, so each one
// has to answer the four questions a finding raises: what it means, what
// proves it, what to run next, and how to see it happen.
func TestEveryEntryCarriesTheSectionsAFindingNeeds(t *testing.T) {
	t.Parallel()

	for _, code := range findings.Codes() {
		t.Run(code, func(t *testing.T) {
			t.Parallel()

			entry, ok := findings.Entry(code)
			if !ok {
				t.Fatal("no entry")
			}

			if !strings.HasPrefix(entry, "# "+code+":") {
				t.Errorf("the entry does not start with a heading naming %s:\n%.80s", code, entry)
			}

			for _, section := range []string{
				"## Symptom",
				"## Evidence",
				"## Versions",
				"## Documentation",
				"## Next command",
				"## Reproduction",
			} {
				if !strings.Contains(entry, section) {
					t.Errorf("the entry has no %q section", section)
				}
			}
		})
	}
}
