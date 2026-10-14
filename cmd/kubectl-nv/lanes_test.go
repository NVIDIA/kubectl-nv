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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// buildConstraint is the //go:build line of a file, which is where a lane
// names itself.
var buildConstraint = regexp.MustCompile(`(?m)^//go:build (.+)$`)

// tagWord is one tag inside a constraint. The operators are what is left over.
var tagWord = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.]*`)

// configuredTag is one entry of the build-tags list in .golangci.yml. The file
// is read as text rather than parsed as YAML: the suite depends on no YAML
// library, and the list is four lines of a fixed shape.
var configuredTag = regexp.MustCompile(`(?m)^\s*-\s*([A-Za-z_][A-Za-z0-9_.]*)\s*$`)

// A lane the linter cannot compile is a lane the linter skips, in silence and
// with a green run.
//
// It lives here rather than next to the suite it is about because the suite's
// TestMain exits before any test when there is no cluster to run against, so a
// guard placed there would report ok without having run. This package is in
// "make test", which is where a guard belongs that needs no cluster at all. .golangci.yml decides which it can compile, and it is also
// what CI runs golangci-lint with, so a tag missing from it is a lane nobody
// checks anywhere.
//
// The tags are read off the files rather than listed here, so a lane added by
// another track is covered without this test being edited, and a lane added
// without its tag reaching the configuration fails rather than passes.
func TestEveryLaneTagIsLinted(t *testing.T) {
	used := laneTags(t)
	if len(used) == 0 {
		t.Fatal("no file in this suite carries a build constraint, so this compared nothing")
	}

	configured := configuredTags(t)
	if len(configured) == 0 {
		t.Fatal("the linter configuration lists no build tag, so the whole suite is skipped")
	}

	for _, tag := range used {
		if !configured[tag] {
			t.Errorf("files in this suite are behind the %q build tag, which .golangci.yml does not "+
				"list under run.build-tags: the linter cannot compile them and skips them in silence", tag)
		}
	}
}

// laneTags returns every build tag the files of this directory are behind,
// sorted.
func laneTags(t *testing.T) []string {
	t.Helper()

	dir := filepath.Join("..", "..", "test", "e2e")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the suite directory: %v", err)
	}

	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		match := buildConstraint.FindSubmatch(raw)
		if match == nil {
			continue
		}
		for _, tag := range tagWord.FindAllString(string(match[1]), -1) {
			seen[tag] = true
		}
	}

	out := make([]string, 0, len(seen))
	for tag := range seen {
		out = append(out, tag)
	}
	sort.Strings(out)

	return out
}

// configuredTags returns the build tags .golangci.yml runs the linter with.
func configuredTags(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", ".golangci.yml"))
	if err != nil {
		t.Fatalf("reading .golangci.yml: %v", err)
	}

	// Only the list under run.build-tags. The file holds other lists, and a
	// tag found in the exclusions would be a tag this test accepted for the
	// wrong reason.
	_, after, found := strings.Cut(string(raw), "build-tags:")
	if !found {
		t.Fatal(".golangci.yml has no run.build-tags: the linter skips every file behind a tag")
	}

	out := map[string]bool{}
	for _, line := range strings.Split(after, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		match := configuredTag.FindStringSubmatch(line)
		if match == nil {
			// The first line that is not a list entry ends the list.
			break
		}
		out[match[1]] = true
	}

	return out
}
