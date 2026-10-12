// Copyright 2026 NVIDIA CORPORATION
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package release holds the checks for the two files that decide what a
// release page looks like: .github/release.yml, which GitHub reads when it
// generates the notes from the merged pull requests, and the changelog and
// release sections of .goreleaser.yaml.
//
// Nothing here talks to GitHub. The category list and the labels are asserted
// against the YAML, and the release header is rendered locally with
// text/template, which is the engine GoReleaser applies to that field
// (internal/pipe/release/body.go calls tmpl.Apply on Release.Header, and
// internal/tmpl exposes .Major, .Minor, .Patch, .Prerelease, .Version and .Tag
// from the parsed tag). The header template below therefore stays on
// text/template built-ins, so that what this test renders is what GoReleaser
// renders.
package release

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"text/template"

	"gopkg.in/yaml.v2"
)

// repoRoot is this package's directory, hack/release, walked back up.
const repoRoot = "../.."

type releaseNotesConfig struct {
	Changelog struct {
		Exclude struct {
			Labels  []string `yaml:"labels"`
			Authors []string `yaml:"authors"`
		} `yaml:"exclude"`
		Categories []struct {
			Title  string   `yaml:"title"`
			Labels []string `yaml:"labels"`
		} `yaml:"categories"`
	} `yaml:"changelog"`
}

type goreleaserConfig struct {
	Changelog struct {
		Use  string `yaml:"use"`
		Sort string `yaml:"sort"`
	} `yaml:"changelog"`
	Release struct {
		Draft      bool   `yaml:"draft"`
		Prerelease string `yaml:"prerelease"`
		Header     string `yaml:"header"`
	} `yaml:"release"`
}

func readYAML(t *testing.T, rel string, into any) {
	t.Helper()
	path := filepath.Join(repoRoot, rel)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, into); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func TestReleaseNotesCategories(t *testing.T) {
	var cfg releaseNotesConfig
	readYAML(t, ".github/release.yml", &cfg)

	// The order matters: GitHub walks the categories top down and "*" catches
	// whatever did not match a category above it.
	want := []struct {
		title  string
		labels []string
	}{
		{"Breaking changes", []string{"breaking-change"}},
		{"Features", []string{"feature"}},
		{"Fixes", []string{"bug"}},
		{"Documentation", []string{"documentation"}},
		{"Dependencies", []string{"dependencies"}},
		{"Other changes", []string{"*"}},
	}

	got := cfg.Changelog.Categories
	if len(got) != len(want) {
		t.Fatalf("categories: got %d (%v), want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if got[i].Title != w.title {
			t.Errorf("category %d: title is %q, want %q", i, got[i].Title, w.title)
		}
		if strings.Join(got[i].Labels, ",") != strings.Join(w.labels, ",") {
			t.Errorf("category %d (%s): labels are %v, want %v", i, w.title, got[i].Labels, w.labels)
		}
	}
}

func TestReleaseNotesCatchAllIsLast(t *testing.T) {
	var cfg releaseNotesConfig
	readYAML(t, ".github/release.yml", &cfg)

	cats := cfg.Changelog.Categories
	if len(cats) == 0 {
		t.Fatal("no categories configured")
	}
	for i, c := range cats {
		for _, l := range c.Labels {
			if l != "*" {
				continue
			}
			if i != len(cats)-1 {
				t.Errorf("the catch-all category %q is at position %d of %d; anything below it is unreachable",
					c.Title, i, len(cats))
			}
			if len(c.Labels) != 1 {
				t.Errorf("the catch-all category %q also lists %v; \"*\" alone is the catch-all", c.Title, c.Labels)
			}
			return
		}
	}
	t.Error("no category carries the \"*\" catch-all, so an unlabelled pull request would not appear in the notes")
}

func TestReleaseNotesExcludesSkipChangelog(t *testing.T) {
	var cfg releaseNotesConfig
	readYAML(t, ".github/release.yml", &cfg)

	labels := cfg.Changelog.Exclude.Labels
	if len(labels) != 1 || labels[0] != "skip-changelog" {
		t.Errorf("changelog.exclude.labels is %v, want [skip-changelog]", labels)
	}
}

func TestGoreleaserChangelogIsGitHubNative(t *testing.T) {
	var cfg goreleaserConfig
	readYAML(t, ".goreleaser.yaml", &cfg)

	if cfg.Changelog.Use != "github-native" {
		t.Errorf("changelog.use is %q, want \"github-native\"; anything else builds the notes from commits, not pull requests",
			cfg.Changelog.Use)
	}
	// github-native hands the body to GitHub, which drops sorting and grouping
	// done on our side. Leaving sort set would read as if it still applied.
	if cfg.Changelog.Sort != "" {
		t.Errorf("changelog.sort is %q, but github-native ignores it", cfg.Changelog.Sort)
	}
}

func TestGoreleaserReleaseStaysADraft(t *testing.T) {
	var cfg goreleaserConfig
	readYAML(t, ".goreleaser.yaml", &cfg)

	if !cfg.Release.Draft {
		t.Error("release.draft is false; a tag would publish the release without a maintainer reading it")
	}
}

func TestGoreleaserPrereleaseIsAuto(t *testing.T) {
	var cfg goreleaserConfig
	readYAML(t, ".goreleaser.yaml", &cfg)

	if cfg.Release.Prerelease != "auto" {
		t.Errorf("release.prerelease is %q, want \"auto\"; only \"auto\" reads the -rc.N suffix off the tag",
			cfg.Release.Prerelease)
	}
}

// semverFields mirrors the fields GoReleaser puts in front of the header
// template: context.Semver (Major, Minor, Patch, Prerelease) plus the tag and
// the version with the v stripped.
type semverFields struct {
	Major      uint64
	Minor      uint64
	Patch      uint64
	Prerelease string
	Version    string
	Tag        string
}

func fieldsForTag(t *testing.T, tag string) semverFields {
	t.Helper()
	version := strings.TrimPrefix(tag, "v")
	core := version
	pre := ""
	if i := strings.IndexByte(core, '-'); i >= 0 {
		pre = core[i+1:]
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		t.Fatalf("tag %q is not a three part version", tag)
	}
	nums := make([]uint64, 3)
	for i, p := range parts {
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			t.Fatalf("tag %q: %v", tag, err)
		}
		nums[i] = n
	}
	return semverFields{
		Major:      nums[0],
		Minor:      nums[1],
		Patch:      nums[2],
		Prerelease: pre,
		Version:    version,
		Tag:        tag,
	}
}

func renderHeader(t *testing.T, tag string) string {
	t.Helper()
	var cfg goreleaserConfig
	readYAML(t, ".goreleaser.yaml", &cfg)
	if cfg.Release.Header == "" {
		t.Fatal("release.header is empty")
	}
	tmpl, err := template.New("header").Option("missingkey=error").Parse(cfg.Release.Header)
	if err != nil {
		t.Fatalf("parse release.header: %v", err)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, fieldsForTag(t, tag)); err != nil {
		t.Fatalf("render release.header for %s: %v", tag, err)
	}
	return out.String()
}

const minorHeader = `## Highlights

<!-- What this release is for, in two or three sentences. -->

## Breaking changes

<!-- Anything a user has to change, and what to change it to. Write "None." if
there is nothing. -->

## Upgrade notes

<!-- What to do when moving up from the previous release. Write "None." if
there is nothing. -->
`

func TestReleaseHeaderForAMinorRelease(t *testing.T) {
	got := renderHeader(t, "v0.2.0")
	if got != minorHeader {
		t.Errorf("header for v0.2.0:\n--- got ---\n%s\n--- want ---\n%s", got, minorHeader)
	}
}

func TestReleaseHeaderForAPatchRelease(t *testing.T) {
	const want = "This is a patch release of 0.2. It carries fixes only: no new features and\nno breaking changes.\n"
	got := renderHeader(t, "v0.2.1")
	if got != want {
		t.Errorf("header for v0.2.1:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// A release candidate of a minor release has Patch 0, so it takes the same
// header as the minor release it leads to.
func TestReleaseHeaderForAReleaseCandidate(t *testing.T) {
	got := renderHeader(t, "v0.2.0-rc.1")
	if got != minorHeader {
		t.Errorf("header for v0.2.0-rc.1:\n--- got ---\n%s\n--- want ---\n%s", got, minorHeader)
	}
}
