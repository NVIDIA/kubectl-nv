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

package agents

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const (
	evalResultsStart = "<!-- eval-results:start -->"
	evalResultsEnd   = "<!-- eval-results:end -->"
)

// notDocumented are the commands the README does not list. They come from
// cobra and kubectl rather than from this repository.
var notDocumented = map[string]bool{"help": true, "completion": true}

func TestEveryCommandTheReadmeNamesExistsInTheTree(t *testing.T) {
	readme := readRepoFile(t, "README.md")
	tree := commandTree(t)

	lines := commandLinesIn(readme)
	if len(lines) == 0 {
		t.Fatal("README.md names no kubectl-nv command, so there is nothing to check")
	}

	for _, line := range lines {
		t.Run(line, func(t *testing.T) {
			resolveCommandLine(t, tree, line)
		})
	}
}

func TestTheReadmeDocumentsEveryCommand(t *testing.T) {
	readme := readRepoFile(t, "README.md")
	tree := commandTree(t)

	var missing []string
	for _, path := range leafCommandPaths(tree) {
		name := "kubectl nv " + strings.Join(path, " ")
		if !strings.Contains(readme, name) {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("README.md documents no %v; every command gets a line and an example", missing)
	}
}

// leafCommandPaths returns the path of every leaf command of the tree, minus
// the ones the README is not expected to carry.
func leafCommandPaths(tree *cobra.Command) [][]string {
	var out [][]string
	var walk func(cmd *cobra.Command, path []string)
	walk = func(cmd *cobra.Command, path []string) {
		if len(path) > 0 && notDocumented[path[0]] {
			return
		}
		children := cmd.Commands()
		if len(children) == 0 {
			if len(path) > 0 {
				out = append(out, path)
			}
			return
		}
		for _, child := range children {
			walk(child, append(append([]string{}, path...), child.Name()))
		}
	}
	walk(tree, nil)

	return out
}

// markdownLink matches an inline Markdown link.
var markdownLink = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)

func TestEveryReadmeLinkResolves(t *testing.T) {
	readme := readRepoFile(t, "README.md")
	root := repoRoot(t)

	var checked int
	for _, match := range markdownLink.FindAllStringSubmatch(readme, -1) {
		target := match[1]
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
			strings.HasPrefix(target, "#") {
			continue
		}
		target, _, _ = strings.Cut(target, "#")
		if target == "" {
			continue
		}
		checked++
		if _, err := os.Stat(filepath.Join(root, target)); err != nil {
			t.Errorf("README.md links to %s, which is not in the tree: %v", target, err)
		}
	}

	if checked == 0 {
		t.Fatal("README.md has no relative links, so there is nothing to check")
	}
}

func TestTheReadmeLinksTheDocumentationTheAgentContractNames(t *testing.T) {
	readme := readRepoFile(t, "README.md")

	for _, target := range []string{
		"docs/agents.md",
		"docs/must-gather.md",
		"docs/labels.md",
		"docs/findings/",
		"docs/schema/",
		"SKILL.md",
		"agents/holmesgpt-toolset.yaml",
		"agents/kubectl-ai-tools.yaml",
		"eval/README.md",
	} {
		if !strings.Contains(readme, target) {
			t.Errorf("README.md does not point at %s", target)
		}
	}
}

func TestTheReadmeCarriesTheRenderedResultsBlock(t *testing.T) {
	readme := readRepoFile(t, "README.md")

	if strings.Count(readme, evalResultsStart) != 1 || strings.Count(readme, evalResultsEnd) != 1 {
		t.Fatalf("README.md must carry %s and %s exactly once each", evalResultsStart, evalResultsEnd)
	}

	start := strings.Index(readme, evalResultsStart)
	end := strings.Index(readme, evalResultsEnd)
	if end < start {
		t.Fatalf("the %s marker comes before %s", evalResultsEnd, evalResultsStart)
	}

	block := strings.TrimSpace(readme[start+len(evalResultsStart) : end])
	if block == "" {
		t.Fatal("the rendered results block is empty; hack/eval/render-results.sh fills it")
	}

	// The block is rendered from the recorded runs, so what it has to say
	// depends on whether there are any.
	recorded, err := filepath.Glob(filepath.Join(repoRoot(t), "eval", "results", "*.json"))
	if err != nil {
		t.Fatalf("listing the recorded runs: %v", err)
	}

	switch {
	case len(recorded) == 0 && !strings.Contains(block, "No evaluation run has been recorded"):
		t.Errorf("no run is recorded under eval/results, but the README does not say so:\n%s", block)
	case len(recorded) > 0 && !strings.Contains(block, "| Date | Agent |"):
		t.Errorf("%d runs are recorded, but the README carries no results table:\n%s", len(recorded), block)
	}
}
