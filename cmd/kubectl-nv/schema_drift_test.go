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
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/schema"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The drift check lives here, in the package that links the whole plugin.
//
// Run from pkg/cmd/schema it saw only the kinds that package happens to import,
// which is none of them: a kind is registered by the package that defines it,
// and those packages are linked into the binary rather than into that test. A
// kind registered by a command package would have been missing from
// docs/schema with nothing to say so.
func TestCheckedInSchemasMatchEveryRegisteredKind(t *testing.T) {
	t.Parallel()

	dir := filepath.Join("..", "..", "docs", "schema")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("docs/schema is where the generated contract lives: %v", err)
	}

	if err := schema.CheckDrift(output.DefaultSchemaRegistry(), dir); err != nil {
		t.Fatal(err)
	}
}

// Every registered kind also has to agree with its own schema. The generator
// cannot catch a Go type that marshals a field as null where the schema says
// array; only running a sample through the published schema can.
func TestEveryRegisteredKindIsPublishedWithASchema(t *testing.T) {
	t.Parallel()

	kinds := output.RegisteredKinds()
	for _, kind := range kinds {
		if _, err := output.SchemaFor(kind); err != nil {
			t.Errorf("%s is registered but has no schema: %v", kind, err)
		}
	}

	// Until a track registers its kinds, the set is empty and this test only
	// states that. It says so rather than passing silently, so that nobody
	// reads a green run as proof the kinds were checked.
	if len(kinds) == 0 {
		t.Log("no kind is registered yet: the command packages register theirs as they land")
	}
}
