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

package adm

import (
	"bytes"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// must-gather prints no object of its own: what it produces is a bundle on
// disk, and the bundle's index is the kind. A caller asking for the schema by
// the command it ran still has to be answered, so the command name maps to
// BundleManifest.
func TestMustGatherMapsToItsKind(t *testing.T) {
	t.Parallel()

	byCommand, err := output.SchemaFor("must-gather")
	if err != nil {
		t.Fatalf("schema for the must-gather command: %v", err)
	}
	byKind, err := output.SchemaFor(output.KindBundleManifest)
	if err != nil {
		t.Fatalf("schema for %s: %v", output.KindBundleManifest, err)
	}
	if !bytes.Equal(byCommand, byKind) {
		t.Errorf("the must-gather command does not map to %s", output.KindBundleManifest)
	}
}
