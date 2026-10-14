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

package inventory

import "github.com/NVIDIA/kubectl-nv/pkg/output"

// The kinds this package defines join the published contract here.
//
// Registration happens at import time because that is the only moment that
// works. The schema generator is the plugin itself: "make schema" runs the
// binary, and the drift test links it, so a kind is in the registry exactly
// when the package defining it is linked. Registering from a constructor would
// publish a kind only once something built one, and registering from a
// command's NewCommand would panic the second time a test built that command.
//
// The prototypes are zero values. The generator reflects the type, never the
// value, so what is in them would not reach the schema; schema_test.go is
// where a populated instance is held to it.
func init() {
	output.RegisterSchema(output.KindGPUNode, GPUNode{})
	output.RegisterSchema(output.KindGPUNodeList, GPUNodeList{})
}
