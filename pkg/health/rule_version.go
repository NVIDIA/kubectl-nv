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

package health

import (
	"strconv"
	"strings"
)

// VersionInRange reports whether v falls in the half open range [min, max): at
// or above min, strictly below max. It is what a Rule.Applies implementation
// is built on, so that every rule in every track gates on a version the same
// way.
//
// The comparison is numeric per component, not lexical. "v25.10.0" sorts
// before "v25.9.0" as a string, so a rule gated by string comparison stops
// firing at the tenth minor release of a year and nobody notices until a
// cluster is misreported.
//
// What it accepts: MAJOR[.MINOR[.PATCH]], with or without a leading "v". A
// missing component reads as zero, so "v25.3" is "v25.3.0". Anything after the
// patch digits, a "-rc.1" pre-release or a "+build.7" suffix, is ignored, so a
// release candidate is gated the same way the release it precedes is.
//
// Anything it cannot read means no constraint:
//
//   - an empty or unparsable v returns true, whatever the bounds. The version
//     comes off a cluster and may be absent, truncated or shaped in a way this
//     parser has never seen. A rule silently skipped on such a cluster reports
//     clean for the wrong reason, which is worse than a rule that runs and may
//     produce one finding a reader can dismiss.
//   - an empty or unparsable bound is simply not applied. Both bounds empty
//     means the rule always applies.
func VersionInRange(v, min, maxExclusive string) bool {
	version, ok := parseVersion(v)
	if !ok {
		return true
	}

	if lower, ok := parseVersion(min); ok && compareVersions(version, lower) < 0 {
		return false
	}

	if upper, ok := parseVersion(maxExclusive); ok && compareVersions(version, upper) >= 0 {
		return false
	}

	return true
}

// version is a parsed MAJOR.MINOR.PATCH.
type version [3]int

// parseVersion reads MAJOR[.MINOR[.PATCH]] with an optional leading "v" and an
// optional pre-release or build suffix. It reports false for anything else,
// which every caller reads as "no constraint".
func parseVersion(s string) (version, bool) {
	var out version

	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")

	if s == "" {
		return out, false
	}

	// The suffix starts at the first "-" or "+", and everything from there is
	// pre-release or build metadata.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}

	parts := strings.Split(s, ".")
	if len(parts) > len(out) {
		return out, false
	}

	for i, part := range parts {
		// strconv.Atoi accepts a leading sign, which a version component does
		// not have, and it is the difference between rejecting "v1.-2.0" and
		// gating a rule on a negative minor.
		if part == "" || strings.ContainsAny(part, "+-") {
			return out, false
		}

		n, err := strconv.Atoi(part)
		if err != nil {
			return out, false
		}

		out[i] = n
	}

	return out, true
}

// compareVersions returns a negative number when a sorts before b, zero when
// they are equal, and a positive number otherwise.
func compareVersions(a, b version) int {
	for i := range a {
		// Compared rather than subtracted: a component large enough to
		// overflow the difference would flip the answer, and a version string
		// is whatever a cluster hands over.
		switch {
		case a[i] < b[i]:
			return -1
		case a[i] > b[i]:
			return 1
		}
	}

	return 0
}
