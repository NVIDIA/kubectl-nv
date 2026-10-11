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

// Package findings serves the finding catalog out of the binary.
//
// The entries are the Markdown files beside this one, one per rule code,
// embedded at build time. That is what lets `kubectl nv explain NV-NODE-002`
// answer on a support engineer's laptop with no network and no checkout: a
// finding whose documentation lives only on a website is a finding nobody
// reads at three in the morning.
//
// The files are the documentation as well as the data. They are rendered on
// the repository's website and linted by `make mdlint`, and the engine fills
// every finding's Docs with health.CatalogPath, which names the same path.
//
// Three statements hold that together: every rule has an entry, every entry
// has a rule, and CatalogPath names the embedded file. They are tested in
// pkg/health/rules/catalog_test.go rather than here, because `make test` runs
// ./cmd/... and ./pkg/... and nothing under docs/, so a test in this directory
// would be a gate nobody ever saw go red.
package findings

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

// catalog holds the entries. The pattern takes the Markdown files alone, so
// this file and its test are not part of what `explain` can print.
//
//go:embed *.md
var catalog embed.FS

// extension is what an entry's file name ends in, and the only extension the
// catalog holds.
const extension = ".md"

// Entry returns the catalog entry for a finding code.
//
// The code is normalised first: a user types what a report printed, or what
// they pasted out of a terminal, and neither is reliably upper case or free of
// surrounding whitespace. An unknown code returns false rather than an empty
// string, because "there is no such code" and "the entry is empty" are
// different answers and the command exits differently on them.
func Entry(code string) (string, bool) {
	name := normalise(code)
	if name == "" {
		return "", false
	}

	raw, err := catalog.ReadFile(name + extension)
	if err != nil {
		return "", false
	}

	return string(raw), true
}

// Codes returns every code the catalog holds, sorted. It is what `explain`
// prints when a user asks for a code that is not there, so a user who
// mistyped one is shown what they could have asked for.
func Codes() []string {
	entries, err := fs.ReadDir(catalog, ".")
	if err != nil {
		// The filesystem is compiled into the binary, so a failure here is a
		// build that produced something impossible rather than a runtime
		// condition a caller can do anything about.
		return nil
	}

	codes := make([]string, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), extension) {
			continue
		}

		codes = append(codes, strings.TrimSuffix(entry.Name(), extension))
	}

	sort.Strings(codes)

	return codes
}

// normalise turns what a user typed into a file name, and returns an empty
// string for anything that is not one.
//
// The character check is what keeps a code from being a path. ReadFile on an
// embed.FS rejects a path with a "..", but it is one argument away from a user
// controlled file read in any caller that reuses this, and a code is a short
// run of letters, digits and hyphens by definition: anything else is not a
// code whatever it would have opened.
func normalise(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return ""
	}

	for _, r := range code {
		switch {
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return ""
		}
	}

	return code
}
