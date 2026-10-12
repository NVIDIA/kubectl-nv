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

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/mcp"
)

// wrappedCommands is what both toolsets have to offer. A toolset that stops
// wrapping one of these, or that wraps something else, has changed what an
// agent can reach and says so here.
var wrappedCommands = []string{"list", "info", "health", "explain"}

// holmesToolset is the part of the HolmesGPT custom toolset format these
// tests read. Upstream: HolmesGPT/holmesgpt, holmes/core/tools.py at commit
// 59e2bd547c8a37082fc99a6dff473bca818fd197 (Toolset, YAMLTool), and
// docs/data-sources/custom-toolsets.md at 2e3742d5dec84ccbc5cb5b39650cd3d563966193
// for the top level `toolsets:` mapping.
type holmesFile struct {
	Toolsets map[string]holmesToolset `json:"toolsets"`
}

type holmesToolset struct {
	Description   string         `json:"description"`
	Tags          []string       `json:"tags"`
	Prerequisites []holmesPrereq `json:"prerequisites"`
	Tools         []holmesTool   `json:"tools"`
}

type holmesPrereq struct {
	Command string `json:"command"`
}

type holmesTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	Command     string                 `json:"command"`
	Parameters  map[string]holmesParam `json:"parameters"`
}

type holmesParam struct {
	Description string `json:"description"`
	Type        string `json:"type"`
	Required    *bool  `json:"required"`
}

// holmesToolsetFields is the field set the upstream Toolset model accepts.
// The model is declared with extra="forbid", so a field outside this set is a
// load failure in HolmesGPT, not a warning.
var holmesToolsetFields = map[string]bool{
	"enabled": true, "name": true, "description": true, "docs_url": true,
	"icon_url": true, "installation_instructions": true, "prerequisites": true,
	"tools": true, "tags": true, "config": true, "llm_instructions": true,
	"transformers": true, "approval_required_tools": true, "expose_remotely": true,
	"status_refresh_interval_seconds": true, "experimental": true, "url": true,
}

// holmesTags is the upstream ToolsetTag enum.
var holmesTags = map[string]bool{"core": true, "cluster": true, "cli": true}

// kubectlAITool is the kubectl-ai custom tool format. Upstream:
// GoogleCloudPlatform/kubectl-ai, pkg/tools/custom_tool.go at commit
// 50662f794da32e3df1ae6d8e1e99a12f16e56455 (CustomToolConfig), and
// docs/tools.md at c18b1240d8ed0d3961a2d14b8e1c667e0b1db556.
type kubectlAITool struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	Command       string `json:"command"`
	CommandDesc   string `json:"command_desc"`
	IsInteractive bool   `json:"is_interactive"`
}

// provenancePattern is the citation each file has to carry: a GitHub
// owner/repo, a path inside it, and the commit the format was read at.
var (
	provenanceRepo   = regexp.MustCompile(`(?m)^#.*\b[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\b`)
	provenancePath   = regexp.MustCompile(`(?m)^#.*\.(py|go|md)\b`)
	provenanceCommit = regexp.MustCompile(`(?m)^#.*\b[0-9a-f]{40}\b`)
)

// assertProvenance fails unless the leading comment block names the upstream
// repository, a path in it and a commit.
func assertProvenance(t *testing.T, name, raw string) {
	t.Helper()

	var header strings.Builder
	for line := range strings.SplitSeq(raw, "\n") {
		if !strings.HasPrefix(line, "#") && strings.TrimSpace(line) != "" {
			break
		}
		header.WriteString(line)
		header.WriteString("\n")
	}
	head := header.String()

	for what, pattern := range map[string]*regexp.Regexp{
		"an upstream repository":    provenanceRepo,
		"a path in it":              provenancePath,
		"the commit it was read at": provenanceCommit,
	} {
		if !pattern.MatchString(head) {
			t.Errorf("%s does not cite %s in its leading comment", name, what)
		}
	}
}

// assertReadOnly fails when a command can run something inside a pod.
func assertReadOnly(t *testing.T, cmd *cobra.Command) {
	t.Helper()

	for c := cmd; c != nil; c = c.Parent() {
		if c.Annotations[mcp.ExecAnnotation] == "true" {
			t.Errorf("%q is annotated as able to exec into a pod, so a toolset must not wrap it", cmd.CommandPath())
		}
	}
}

// assertWrapsEveryCommand fails unless the set of commands the tools resolve
// to is exactly the one both toolsets are required to offer. More than one
// tool may wrap the same command, so the list is reduced to a set first.
func assertWrapsEveryCommand(t *testing.T, name string, got []string) {
	t.Helper()

	seen := map[string]bool{}
	var unique []string
	for _, command := range got {
		if !seen[command] {
			seen[command] = true
			unique = append(unique, command)
		}
	}

	want := append([]string{}, wrappedCommands...)
	sort.Strings(want)
	sort.Strings(unique)

	if strings.Join(unique, ",") != strings.Join(want, ",") {
		t.Errorf("%s wraps %v, want exactly %v", name, unique, want)
	}
}

func TestHolmesGPTToolset(t *testing.T) {
	const path = "agents/holmesgpt-toolset.yaml"
	raw := readRepoFile(t, path)
	assertProvenance(t, path, raw)

	// Round one: the exact keys, so a field the upstream model forbids is
	// caught rather than dropped by a typed unmarshal.
	var loose map[string]map[string]map[string]any
	if err := yaml.Unmarshal([]byte(raw), &loose); err != nil {
		t.Fatalf("%s is not YAML of the shape toolsets.<name>.<field>: %v", path, err)
	}
	toolsets, ok := loose["toolsets"]
	if !ok || len(toolsets) != 1 {
		t.Fatalf("%s must carry exactly one toolset under a top level toolsets key, got %d", path, len(toolsets))
	}
	for name, fields := range toolsets {
		for field := range fields {
			if !holmesToolsetFields[field] {
				t.Errorf("toolset %q carries field %q, which the upstream model forbids", name, field)
			}
		}
	}

	var file holmesFile
	if err := yaml.Unmarshal([]byte(raw), &file); err != nil {
		t.Fatalf("%s does not unmarshal: %v", path, err)
	}

	tree := commandTree(t)
	for name, toolset := range file.Toolsets {
		if strings.TrimSpace(toolset.Description) == "" {
			t.Errorf("toolset %q has no description", name)
		}
		if len(toolset.Tags) == 0 {
			t.Errorf("toolset %q declares no tags", name)
		}
		for _, tag := range toolset.Tags {
			if !holmesTags[tag] {
				t.Errorf("toolset %q declares tag %q, which is not one of core, cluster or cli", name, tag)
			}
		}
		if len(toolset.Prerequisites) == 0 {
			t.Errorf("toolset %q declares no prerequisite, so it would load where the binary is absent", name)
		}
		for _, prereq := range toolset.Prerequisites {
			if strings.TrimSpace(prereq.Command) == "" {
				t.Errorf("toolset %q has a prerequisite with no command", name)
			}
		}

		var wrapped []string
		seen := map[string]bool{}
		for _, tool := range toolset.Tools {
			if strings.TrimSpace(tool.Name) == "" {
				t.Fatalf("toolset %q has a tool with no name", name)
			}
			if seen[tool.Name] {
				t.Errorf("toolset %q declares tool %q twice", name, tool.Name)
			}
			seen[tool.Name] = true

			if strings.TrimSpace(tool.Description) == "" {
				t.Errorf("tool %q has no description", tool.Name)
			}
			if strings.TrimSpace(tool.Command) == "" {
				t.Fatalf("tool %q has no command", tool.Name)
			}

			line := strings.TrimSpace(tool.Command)
			cmd := resolveCommandLine(t, tree, stripTemplates(line))
			assertReadOnly(t, cmd)
			assertJSONWhereSupported(t, tool.Name, cmd, line)
			assertParametersDeclared(t, tool, line)
			wrapped = append(wrapped, cmd.Name())
		}
		assertWrapsEveryCommand(t, path, wrapped)
	}
}

func TestKubectlAITools(t *testing.T) {
	const path = "agents/kubectl-ai-tools.yaml"
	raw := readRepoFile(t, path)
	assertProvenance(t, path, raw)

	var loose []map[string]any
	if err := yaml.Unmarshal([]byte(raw), &loose); err != nil {
		t.Fatalf("%s is not a YAML list of tools: %v", path, err)
	}
	allowed := map[string]bool{
		"name": true, "description": true, "command": true,
		"command_desc": true, "is_interactive": true,
	}
	for i, fields := range loose {
		for field := range fields {
			if !allowed[field] {
				t.Errorf("tool %d carries field %q, which kubectl-ai does not read", i, field)
			}
		}
	}

	var tools []kubectlAITool
	if err := yaml.Unmarshal([]byte(raw), &tools); err != nil {
		t.Fatalf("%s does not unmarshal: %v", path, err)
	}
	if len(tools) == 0 {
		t.Fatalf("%s declares no tools", path)
	}

	tree := commandTree(t)
	var wrapped []string
	seen := map[string]bool{}
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" {
			t.Fatal("a tool has no name, which kubectl-ai rejects")
		}
		if seen[tool.Name] {
			t.Errorf("%s declares tool %q twice", path, tool.Name)
		}
		seen[tool.Name] = true

		if strings.TrimSpace(tool.Description) == "" {
			t.Errorf("tool %q has no description", tool.Name)
		}
		if strings.TrimSpace(tool.Command) == "" {
			t.Fatalf("tool %q has no command, which kubectl-ai rejects", tool.Name)
		}
		if strings.TrimSpace(tool.CommandDesc) == "" {
			t.Errorf("tool %q has no command_desc, so the model is told nothing about its syntax", tool.Name)
		}
		if tool.IsInteractive {
			t.Errorf("tool %q is marked interactive, but no kubectl-nv command prompts", tool.Name)
		}

		cmd := resolveCommandLine(t, tree, tool.Command)
		assertReadOnly(t, cmd)
		// The command field is a prefix; the model writes the rest, so the
		// -o json instruction has to live in the description it reads.
		assertJSONWhereSupported(t, tool.Name, cmd, tool.CommandDesc)
		for _, line := range commandLinesIn(tool.CommandDesc) {
			resolveCommandLine(t, tree, stripTemplates(line))
		}
		wrapped = append(wrapped, cmd.Name())
	}
	assertWrapsEveryCommand(t, path, wrapped)
}

// assertJSONWhereSupported requires -o json wherever the command has an
// output flag, because a toolset that lets a model parse the table has made
// the contract the column widths.
func assertJSONWhereSupported(t *testing.T, tool string, cmd *cobra.Command, text string) {
	t.Helper()

	supportsJSON := cmd.Flags().Lookup("output") != nil
	mentionsJSON := strings.Contains(text, "-o json") || strings.Contains(text, "--output json")

	switch {
	case supportsJSON && !mentionsJSON:
		t.Errorf("tool %q wraps %q, which takes -o json, but never asks for it", tool, cmd.CommandPath())
	case !supportsJSON && mentionsJSON:
		t.Errorf("tool %q asks %q for -o json, which it does not accept", tool, cmd.CommandPath())
	}
}

// templatePattern is a HolmesGPT parameter placeholder.
var templatePattern = regexp.MustCompile(`{{\s*([A-Za-z_][A-Za-z0-9_]*)\s*}}`)

// stripTemplates replaces every placeholder with a value-shaped token, so that
// the command line can be resolved without the braces being read as arguments.
func stripTemplates(line string) string {
	return templatePattern.ReplaceAllString(line, "PLACEHOLDER")
}

// assertParametersDeclared requires every placeholder in a command to be
// declared, and every declared parameter to be used. HolmesGPT infers an
// undeclared one with no description, which is a tool the model cannot call
// correctly.
func assertParametersDeclared(t *testing.T, tool holmesTool, line string) {
	t.Helper()

	used := map[string]bool{}
	for _, match := range templatePattern.FindAllStringSubmatch(line, -1) {
		used[match[1]] = true
	}

	for name := range used {
		param, ok := tool.Parameters[name]
		if !ok {
			t.Errorf("tool %q uses {{ %s }} without declaring it", tool.Name, name)
			continue
		}
		if strings.TrimSpace(param.Description) == "" {
			t.Errorf("tool %q declares parameter %q with no description", tool.Name, name)
		}
	}
	for name := range tool.Parameters {
		if !used[name] {
			t.Errorf("tool %q declares parameter %q and never uses it", tool.Name, name)
		}
	}
}
