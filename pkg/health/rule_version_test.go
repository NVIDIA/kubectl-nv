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
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

func TestVersionInRange(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		v    string
		min  string
		max  string
		want bool
	}{
		// The ordinary cases: half open, min inclusive, max exclusive.
		{"inside the range", "v25.3.0", "v25.0.0", "v26.0.0", true},
		{"exactly at min is inside", "v25.0.0", "v25.0.0", "v26.0.0", true},
		{"exactly at max is outside", "v26.0.0", "v25.0.0", "v26.0.0", false},
		{"below min", "v24.9.9", "v25.0.0", "v26.0.0", false},
		{"above max", "v26.0.1", "v25.0.0", "v26.0.0", false},

		// Comparison is numeric per component, not lexical. "v25.10.0" sorts
		// before "v25.9.0" as a string, and a rule gated that way would stop
		// firing at the tenth minor release.
		{"minor 10 is above minor 9", "v25.10.0", "v25.9.0", "v26.0.0", true},
		{"minor 9 is below minor 10", "v25.9.0", "v25.10.0", "v26.0.0", false},
		{"patch 10 is above patch 9", "v25.3.10", "v25.3.9", "v26.0.0", true},
		{"major 100 is above major 99", "v100.0.0", "v99.0.0", "", true},

		// The leading v is optional on every one of the three.
		{"no v on the version", "25.3.0", "v25.0.0", "v26.0.0", true},
		{"no v on the bounds", "v25.3.0", "25.0.0", "26.0.0", true},
		{"no v anywhere", "25.3.0", "25.0.0", "26.0.0", true},

		// Missing components read as zero, which is what a two component
		// operator version such as "v25.3" means.
		{"two component version", "v25.3", "v25.0.0", "v26.0.0", true},
		{"two component min", "v25.3.0", "v25.3", "v26.0.0", true},
		{"major only", "v25", "v24.0.0", "v26.0.0", true},

		// A build or pre-release suffix is ignored, so a release candidate is
		// gated the same way the release it precedes is.
		{"pre-release suffix", "v25.3.0-rc.1", "v25.0.0", "v26.0.0", true},
		{"build suffix", "v25.3.0+build.7", "v25.0.0", "v26.0.0", true},
		{"suffix on the patch component", "v25.3.0-rc.1", "v25.3.0", "v25.4.0", true},

		// An empty bound is no bound at all.
		{"no min", "v1.0.0", "", "v26.0.0", true},
		{"no max", "v99.0.0", "v25.0.0", "", true},
		{"no bounds at all", "v0.0.1", "", "", true},

		// Anything unreadable means no constraint, and the rule applies. A
		// rule silently skipped on every cluster whose version has an unusual
		// shape is a rule that reports clean for the wrong reason.
		{"empty version applies", "", "v25.0.0", "v26.0.0", true},
		{"unparsable version applies", "unknown", "v25.0.0", "v26.0.0", true},
		{"non numeric version applies", "vX.Y.Z", "v25.0.0", "v26.0.0", true},
		{"unparsable min is no min", "v1.0.0", "garbage", "v26.0.0", true},
		{"unparsable max is no max", "v99.0.0", "v25.0.0", "garbage", true},

		// A version with a huge component must not wrap or overflow into the
		// wrong answer.
		{"very large major", "v99999999.0.0", "v25.0.0", "v26.0.0", false},
		{"component too long to be a version", "v1.0.000000000000000000000", "v1.0.0", "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := VersionInRange(tc.v, tc.min, tc.max); got != tc.want {
				t.Errorf("VersionInRange(%q, %q, %q) = %v, want %v", tc.v, tc.min, tc.max, got, tc.want)
			}
		})
	}
}

// A rule gates on the version through Applies, and the helper is what Applies
// is expected to call. The pairing is what makes "a rule about a field that
// only exists from v25.3.0" expressible, so it is exercised here rather than
// left to the first rule that needs it.
func TestVersionInRangeGatesARule(t *testing.T) {
	t.Parallel()

	rule := versionGatedRule{code: "NV-DRA-001", min: "v25.3.0"}

	for _, tc := range []struct {
		version string
		want    bool
	}{
		{"v25.2.9", false},
		{"v25.3.0", true},
		{"v26.1.0", true},
		{"", true},
	} {
		if got := rule.Applies(tc.version); got != tc.want {
			t.Errorf("Applies(%q) = %v, want %v", tc.version, got, tc.want)
		}
	}
}

// versionGatedRule is the shape a real rule takes when it is version gated.
type versionGatedRule struct {
	code string
	min  string
	max  string
}

func (r versionGatedRule) Code() string              { return r.code }
func (r versionGatedRule) Severity() output.Severity { return output.SeverityWarning }

func (r versionGatedRule) Applies(v string) bool { return VersionInRange(v, r.min, r.max) }

func (r versionGatedRule) Evaluate(Input) []output.Finding { return nil }
