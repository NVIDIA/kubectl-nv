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

package debug

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The kind this package defines joins the published contract here. See
// pkg/inventory/schema.go for why registration happens at import time.
func init() {
	output.RegisterSchema(output.KindDebugReport, schemaShape{})
}

// schemaShape is the Report as it comes off the wire, which is what the
// published schema describes.
//
// Report embeds output.ReportStatus whole and MarshalJSON writes the trimmed
// reportStatus instead, because --limit is refused on this kind and
// remainingItemCount could never be set. The generator reflects a Go type, so
// it has to be given the type the wire shape matches rather than the one the
// command holds.
type schemaShape struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Node   inventory.GPUNode `json:"node"`
	Bundle Bundle            `json:"bundle"`
	Status reportStatus      `json:"status"`
}
