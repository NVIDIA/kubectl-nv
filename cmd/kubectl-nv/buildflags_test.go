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
	"strings"
	"testing"
)

// The version is stamped through the linker, so nothing in Go can catch a
// build line that lost the flag: the binary still builds, still runs, and
// quietly reports "dev". These read the build files instead.
//
// The spelling is exact on purpose. The linker matches the symbol name
// literally, so -X main.Version= sets nothing at all and leaves no trace.
const versionStamp = "-X main.version="

func TestMakefileStampsTheVersion(t *testing.T) {
	// The recipe of the build target, whatever it is spelled with.
	recipe := regexp.MustCompile(`(?m)^build:\n(?:\t.*\n)+`).FindString(readRepoFile(t, "Makefile"))
	if recipe == "" {
		t.Fatal("Makefile has no build target")
	}
	line := buildLine(t, recipe, regexp.MustCompile(`(?m)^\t.*-o bin/.*$`))

	if !strings.Contains(line, versionStamp) {
		t.Errorf("the build line does not stamp the version with %q:\n%s", versionStamp, line)
	}
	if !strings.Contains(line, "./cmd/kubectl-nv") {
		t.Errorf("the build line does not build ./cmd/kubectl-nv:\n%s", line)
	}
}

func TestGoreleaserStampsTheVersionAndStrips(t *testing.T) {
	data := readRepoFile(t, ".goreleaser.yaml")

	ldflags := regexp.MustCompile(`(?s)ldflags:\s*\n\s*-\s*(.*?)\n`).FindStringSubmatch(data)
	if ldflags == nil {
		t.Fatalf(".goreleaser.yaml has no ldflags entry:\n%s", data)
	}
	line := ldflags[1]

	if !strings.Contains(line, versionStamp) {
		t.Errorf("goreleaser does not stamp the version with %q: %q", versionStamp, line)
	}
	for _, flag := range []string{"-s", "-w"} {
		if !strings.Contains(line, flag+" ") && !strings.HasSuffix(line, flag) {
			t.Errorf("goreleaser ldflags are missing %q: %q", flag, line)
		}
	}
	if !strings.Contains(data, "main: ./cmd/kubectl-nv") {
		t.Error("goreleaser does not build ./cmd/kubectl-nv")
	}
}

// buildLine returns the first line of text matching pattern.
func buildLine(t *testing.T, text string, pattern *regexp.Regexp) string {
	t.Helper()
	line := pattern.FindString(text)
	if line == "" {
		t.Fatalf("no line matching %s in:\n%s", pattern, text)
	}
	return line
}

// readRepoFile reads a file from the repository root, which is two levels up
// from this package.
func readRepoFile(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(data)
}
