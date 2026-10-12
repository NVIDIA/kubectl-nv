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
	"regexp"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/kubectl-nv/docs/findings"
)

// The limits below are the Agent Skills specification as the reference
// validator enforces it: agentskills/agentskills, skills-ref at commit
// 547831f3a23724ba64a9b79bbf59c5e0bc8f2d1a (src/skills_ref/validator.py),
// against the specification at https://agentskills.io/specification. There is
// no validator on this machine and none in the repository's toolchain, so the
// rules are restated here rather than shelled out to.
const (
	maxSkillNameLength   = 64
	maxDescriptionLength = 1024
	maxCompatibilityLen  = 500

	// The specification's own recommendation for the body, and the number the
	// task was written against.
	maxSkillBodyLines = 500

	// The directory the skill is installed under, which the specification
	// requires the name to match. It is the repository directory name, because
	// the repository root is the skill root.
	skillName = "kubectl-nv"
)

// allowedFrontmatterFields is the set the reference validator accepts. A field
// outside it is an error there, so it is an error here.
var allowedFrontmatterFields = map[string]bool{
	"name":          true,
	"description":   true,
	"license":       true,
	"allowed-tools": true,
	"metadata":      true,
	"compatibility": true,
}

// skillFrontmatter is the part of SKILL.md the specification constrains.
type skillFrontmatter struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	License       string `json:"license"`
	Compatibility string `json:"compatibility"`
}

// splitSkill returns the raw frontmatter and the body of SKILL.md.
func splitSkill(t *testing.T) (string, string) {
	t.Helper()

	raw := readRepoFile(t, "SKILL.md")
	if !strings.HasPrefix(raw, "---\n") {
		t.Fatal("SKILL.md does not open with a --- frontmatter fence")
	}

	rest := raw[len("---\n"):]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		t.Fatal("SKILL.md has no closing --- frontmatter fence")
	}

	return rest[:end], rest[end+len("\n---\n"):]
}

func TestSkillFrontmatterFollowsTheAgentSkillsSpec(t *testing.T) {
	front, _ := splitSkill(t)

	var fields map[string]any
	if err := yaml.Unmarshal([]byte(front), &fields); err != nil {
		t.Fatalf("SKILL.md frontmatter is not YAML: %v", err)
	}

	var extra []string
	for name := range fields {
		if !allowedFrontmatterFields[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	if len(extra) > 0 {
		t.Errorf("SKILL.md frontmatter carries fields the spec does not define: %v", extra)
	}

	var meta skillFrontmatter
	if err := yaml.Unmarshal([]byte(front), &meta); err != nil {
		t.Fatalf("SKILL.md frontmatter does not unmarshal: %v", err)
	}

	t.Run("name", func(t *testing.T) {
		switch {
		case meta.Name == "":
			t.Fatal("frontmatter has no name")
		case len(meta.Name) > maxSkillNameLength:
			t.Errorf("name %q is %d characters, over the %d the spec allows",
				meta.Name, len(meta.Name), maxSkillNameLength)
		}
		if meta.Name != strings.ToLower(meta.Name) {
			t.Errorf("name %q is not lower case", meta.Name)
		}
		if strings.HasPrefix(meta.Name, "-") || strings.HasSuffix(meta.Name, "-") {
			t.Errorf("name %q starts or ends with a hyphen", meta.Name)
		}
		if strings.Contains(meta.Name, "--") {
			t.Errorf("name %q contains consecutive hyphens", meta.Name)
		}
		for _, r := range meta.Name {
			isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !isAlnum && r != '-' {
				t.Errorf("name %q contains %q, which is neither a lower case alphanumeric nor a hyphen", meta.Name, r)
			}
		}
		// The spec requires the name to equal the directory the skill is
		// installed under. The repository root is the skill root, so that
		// directory is the repository's own name.
		if meta.Name != skillName {
			t.Errorf("name is %q, but the skill directory is %q", meta.Name, skillName)
		}
	})

	t.Run("description", func(t *testing.T) {
		if strings.TrimSpace(meta.Description) == "" {
			t.Fatal("frontmatter has no description")
		}
		if len(meta.Description) > maxDescriptionLength {
			t.Errorf("description is %d characters, over the %d the spec allows",
				len(meta.Description), maxDescriptionLength)
		}
	})

	t.Run("compatibility", func(t *testing.T) {
		if len(meta.Compatibility) > maxCompatibilityLen {
			t.Errorf("compatibility is %d characters, over the %d the spec allows",
				len(meta.Compatibility), maxCompatibilityLen)
		}
	})
}

func TestSkillBodyStaysUnderTheRecommendedLength(t *testing.T) {
	_, body := splitSkill(t)

	lines := strings.Count(strings.TrimRight(body, "\n"), "\n") + 1
	if lines >= maxSkillBodyLines {
		t.Errorf("SKILL.md body is %d lines, which is not under the %d the spec recommends",
			lines, maxSkillBodyLines)
	}
}

// commandLinePattern finds every `kubectl nv ...` invocation in a Markdown
// file. It stops at the end of the line, at a shell operator, or at a closing
// backtick, so that prose around an inline command is not read as arguments.
var commandLinePattern = regexp.MustCompile("kubectl nv [^\n`|>&#$]*")

// commandLinesIn returns every kubectl-nv invocation a Markdown file mentions,
// trimmed of the punctuation that follows one in prose.
func commandLinesIn(text string) []string {
	var out []string
	for _, match := range commandLinePattern.FindAllString(text, -1) {
		line := strings.TrimRight(match, " \t.,:;)]\"'")
		if len(strings.Fields(line)) > 2 {
			out = append(out, line)
		}
	}

	return out
}

func TestEveryCommandTheSkillNamesExistsInTheTree(t *testing.T) {
	_, body := splitSkill(t)
	tree := commandTree(t)

	lines := commandLinesIn(body)
	if len(lines) == 0 {
		t.Fatal("SKILL.md names no kubectl-nv command, so there is nothing to check")
	}

	for _, line := range lines {
		t.Run(line, func(t *testing.T) {
			resolveCommandLine(t, tree, line)
		})
	}
}

// findingCodePattern is the shape of a finding code, NV-<AREA>-<NNN>.
var findingCodePattern = regexp.MustCompile(`NV-[A-Z]+-[0-9]{3}`)

func TestEveryFindingCodeTheSkillNamesIsInTheCatalog(t *testing.T) {
	_, body := splitSkill(t)

	codes := findingCodePattern.FindAllString(body, -1)
	if len(codes) == 0 {
		t.Fatal("SKILL.md names no finding code, so there is nothing to check")
	}

	for _, code := range codes {
		if _, ok := findings.Entry(code); !ok {
			t.Errorf("SKILL.md names %s, which the catalog does not hold; it holds %v",
				code, findings.Codes())
		}
	}
}
