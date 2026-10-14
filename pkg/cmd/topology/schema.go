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

package topology

import "github.com/NVIDIA/kubectl-nv/pkg/output"

// The command name a caller can ask for a schema by, registered next to the
// command rather than in a table somewhere central: a command that is renamed
// or moved takes its mapping with it.
//
// Registration is at import time for the reason the kinds are, which
// pkg/inventory/schema.go sets out. The kind has to be registered first, and
// it is: this package imports the one that defines it, so that package's init
// has run by the time this one does.
func init() {
	output.RegisterCommandKind("topology", output.KindTopologyReport)
}
