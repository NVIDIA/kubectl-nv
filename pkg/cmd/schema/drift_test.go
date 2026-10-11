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

package schema

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// writtenRegistry writes the schemas of a registry into a fresh directory and
// returns both.
func writtenRegistry(t *testing.T) (*output.SchemaRegistry, string) {
	t.Helper()

	r := testRegistry(t)
	dir := t.TempDir()
	if _, err := WriteAll(r, dir); err != nil {
		t.Fatalf("writing the schemas: %v", err)
	}

	return r, dir
}

func TestCheckDriftAcceptsWhatWriteAllWrote(t *testing.T) {
	t.Parallel()

	r, dir := writtenRegistry(t)
	if err := CheckDrift(r, dir); err != nil {
		t.Fatalf("CheckDrift rejected what WriteAll had just written: %v", err)
	}
}

// Changing a checked-in schema by hand, or changing the Go type without
// regenerating, is the same thing from here: the file no longer says what the
// code produces.
func TestCheckDriftFailsOnAChangedFile(t *testing.T) {
	t.Parallel()

	r, dir := writtenRegistry(t)
	path := filepath.Join(dir, output.KindGPUNodeList+".json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the schema: %v", err)
	}
	changed := strings.Replace(string(raw), `"gpus"`, `"gpuCount"`, 1)
	if changed == string(raw) {
		t.Fatal("the fixture did not change: the test would pass without the check")
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatalf("writing the changed schema: %v", err)
	}

	err = CheckDrift(r, dir)
	if err == nil {
		t.Fatal("CheckDrift accepted a file that differs from what the generator produces")
	}
	if !strings.Contains(err.Error(), output.KindGPUNodeList+".json") {
		t.Errorf("the error does not name the file that drifted: %v", err)
	}
	if !strings.Contains(err.Error(), "make schema") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

func TestCheckDriftFailsOnAMissingFile(t *testing.T) {
	t.Parallel()

	r, dir := writtenRegistry(t)
	path := filepath.Join(dir, output.KindGPUNode+".json")
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing the schema: %v", err)
	}

	err := CheckDrift(r, dir)
	if err == nil {
		t.Fatal("CheckDrift accepted a registered kind with no checked-in schema")
	}
	if !strings.Contains(err.Error(), output.KindGPUNode+".json") {
		t.Errorf("the error does not name the missing file: %v", err)
	}
}

// A kind that was renamed or removed leaves its file behind, and a stale file
// is a published contract for something that no longer exists.
func TestCheckDriftFailsOnAFileNoKindClaims(t *testing.T) {
	t.Parallel()

	r, dir := writtenRegistry(t)
	path := filepath.Join(dir, "RetiredReport.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("writing the stale schema: %v", err)
	}

	err := CheckDrift(r, dir)
	if err == nil {
		t.Fatal("CheckDrift accepted a schema file no registered kind claims")
	}
	if !strings.Contains(err.Error(), "RetiredReport.json") {
		t.Errorf("the error does not name the stale file: %v", err)
	}
}

// Anything that is not a schema is none of the check's business, or the README
// explaining the directory would read as drift.
func TestCheckDriftIgnoresNonSchemaFiles(t *testing.T) {
	t.Parallel()

	r, dir := writtenRegistry(t)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("generated\n"), 0o600); err != nil {
		t.Fatalf("writing the README: %v", err)
	}
	if err := CheckDrift(r, dir); err != nil {
		t.Fatalf("CheckDrift rejected a directory holding a README: %v", err)
	}
}

// The real one: whatever the commands have registered by the time the suite
// runs has to match what is checked in under docs/schema.
func TestCheckedInSchemasAreUpToDate(t *testing.T) {
	t.Parallel()

	dir := filepath.Join("..", "..", "..", "docs", "schema")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("docs/schema is where the generated contract lives: %v", err)
	}
	if err := CheckDrift(output.DefaultSchemaRegistry(), dir); err != nil {
		t.Fatal(err)
	}
}
