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

package mcp

import "github.com/NVIDIA/kubectl-nv/pkg/output"

// mcp serves the protocol and prints nothing of this contract, so a caller
// asking for its schema is answered with why rather than with "unknown kind or
// command", which sends it looking for a typo in a name it read off --help.
//
// See pkg/cmd/list/schema.go on why this is an init.
func init() {
	output.RegisterSchemalessCommand("mcp",
		"it serves the Model Context Protocol on stdio rather than printing an object")
}
