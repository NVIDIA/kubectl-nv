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

package output

// SanitizeCell makes one string safe to print on a terminal: tabs and newlines
// become spaces, and every other C0 and C1 control character is dropped.
//
// It exists because the strings kubectl-nv prints are not kubectl-nv's. They
// are node labels, taint values, condition reasons and container images, which
// come either from a cluster where anyone with patch on nodes can write them,
// or from a must-gather directory that arrived in a support ticket from a
// cluster the reader does not control. An escape sequence in one of those is
// somebody else's cursor control, window title and scroll back manipulation
// running in the reader's shell.
//
// The rule is the printers' own: this is the cell sanitiser the table output
// has always applied, exported so that a view which does not go through the
// table printer applies exactly the same one. Two sanitisers that drift apart
// would mean two answers to "is this safe to print".
//
// It is deliberately not a quoting function. The text around a control
// character is kept, because a hostile label that erased the field it sits in
// would hide the very node a reader is looking at.
func SanitizeCell(s string) string {
	return sanitizeCell(s)
}
