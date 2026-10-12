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

import "github.com/NVIDIA/kubectl-nv/pkg/output"

// must-gather prints nothing of this contract: what it produces is a
// directory, and the bundle's own index is the kind. The mapping is registered
// all the same, because a caller asks for a schema by the command it ran, and
// the leaf name is what it holds: "schema adm must-gather" is two words where
// the command takes one.
//
// See pkg/cmd/list/schema.go on why this is an init.
func init() {
	output.RegisterCommandKind("must-gather", output.KindBundleManifest)
	// And under the path a caller actually types. "kubectl nv schema adm
	// must-gather" is the command they just ran, and asking them to drop the
	// group is asking them to know which half of the name is the command.
	output.RegisterCommandKind("adm must-gather", output.KindBundleManifest)
}
