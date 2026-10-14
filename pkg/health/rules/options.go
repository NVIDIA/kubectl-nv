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

package rules

import "time"

// Options are the settings a caller may hand the rule set.
//
// Almost no rule takes one. A rule is a pure function of its Input and
// everything it concludes comes from there; a setting here is for the one
// thing an Input cannot carry, which is a threshold the user chose. Today
// that is the driver upgrade window and nothing else.
//
// A zero Options is the defaults, which is what All passes.
type Options struct {
	// UpgradeStuckAfter is how long a node may sit in one step of a driver
	// upgrade before NV-UPG-001 calls it stuck. It is --upgrade-stuck-after,
	// and zero or negative means DefaultUpgradeStuckAfter.
	UpgradeStuckAfter time.Duration
}

// upgradeStuckAfter resolves the window, so that the fallback is written once
// here rather than at every caller.
func (o Options) upgradeStuckAfter() time.Duration {
	if o.UpgradeStuckAfter <= 0 {
		return DefaultUpgradeStuckAfter
	}

	return o.UpgradeStuckAfter
}
