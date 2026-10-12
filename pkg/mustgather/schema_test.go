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

package mustgather

import (
	"slices"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The kind is registered by the package that defines it, at import time. See
// the same test in pkg/health for why it cannot live in the command package.
func TestThisPackageRegistersItsKind(t *testing.T) {
	t.Parallel()

	if kinds := output.RegisteredKinds(); !slices.Contains(kinds, output.KindBundleManifest) {
		t.Errorf("%s is not registered; registered kinds are %v", output.KindBundleManifest, kinds)
	}
}

// populatedManifest sets every field of the kind, so that the schema is
// checked against a full instance rather than a zero value.
func populatedManifest() Manifest {
	return Manifest{
		TypeMeta:            output.TypeMetaFor(output.KindBundleManifest),
		BundleSchemaVersion: BundleSchemaVersion,
		ToolVersion:         "0.1.0",
		Context:             "kind-kubectl-nv-mokka",
		Timestamp:           "2026-10-12T00:00:00Z",
		Flags:               ManifestFlags{Node: "node-1", Since: "1h0m0s", NoExec: true},
		Collectors: []CollectorStatus{{
			Name:      "nodes",
			Status:    StatusOK,
			Artifacts: []string{"nodes.yaml"},
		}},
		Files: []FileEntry{{
			Path:   "nodes.yaml",
			Size:   1024,
			SHA256: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		}},
	}
}

// A schema is only a contract if what the bundle carries satisfies it. The
// generator reads the Go type, so only running a sample through the published
// schema catches a field whose wire shape differs from its Go shape.
func TestAPopulatedBundleManifestSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	m := populatedManifest()
	if err := output.ValidateAgainstSchema(output.KindBundleManifest, m); err != nil {
		t.Fatal(err)
	}
}

// A manifest with no collectors and no files still has to validate: a gather
// refused everything is a bundle somebody sends to support.
func TestAnEmptyBundleManifestSatisfiesItsSchema(t *testing.T) {
	t.Parallel()

	m := Manifest{
		TypeMeta:            output.TypeMetaFor(output.KindBundleManifest),
		BundleSchemaVersion: BundleSchemaVersion,
		Collectors:          []CollectorStatus{},
		Files:               []FileEntry{},
	}
	if err := output.ValidateAgainstSchema(output.KindBundleManifest, m); err != nil {
		t.Fatal(err)
	}
}
