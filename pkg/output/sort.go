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

import "sort"

// SortFindings orders findings by node name and then by code, in place. The
// sort is stable, so findings that share a node and a code keep the order the
// command produced them in. Output has to be byte identical across runs for
// diffs and golden files to be worth anything.
func SortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Node != findings[j].Node {
			return findings[i].Node < findings[j].Node
		}

		return findings[i].Code < findings[j].Code
	})
}
