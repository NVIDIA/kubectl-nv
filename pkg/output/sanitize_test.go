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

package output_test

import (
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// TestSanitizeCell pins what must never reach a terminal. The strings a
// command prints come from node labels and pod statuses, which anyone who can
// label a node controls, so an escape sequence in one of them is somebody
// else's cursor control running in the reader's shell.
func TestSanitizeCell(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		given string
		want  string
	}{
		{name: "plain text is untouched", given: "NVIDIA-T4", want: "NVIDIA-T4"},
		{name: "a CSI sequence loses its escape", given: "A100\x1b[2J\x1b[H", want: "A100[2J[H"},
		{
			name:  "an OSC title sequence loses both its controls",
			given: "\x1b]0;pwned\x07T4",
			want:  "]0;pwnedT4",
		},
		{name: "a C1 CSI is stripped too", given: "A\u009b31mB", want: "A31mB"},
		{name: "a carriage return cannot rewrite the line", given: "clean\rpwned", want: "cleanpwned"},
		{name: "a newline becomes a space", given: "two\nlines", want: "two lines"},
		{name: "a tab becomes a space", given: "two\tcells", want: "two cells"},
		{name: "a NUL is dropped", given: "a\x00b", want: "ab"},
		{name: "DEL is dropped", given: "a\x7fb", want: "ab"},
		{name: "printable unicode survives", given: "GB200 NVL72", want: "GB200 NVL72"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := output.SanitizeCell(tt.given); got != tt.want {
				t.Errorf("SanitizeCell(%q) = %q, want %q", tt.given, got, tt.want)
			}
		})
	}
}

// TestSanitizeCellLeavesNoControlByte is the property the table and the
// described view both depend on, stated once over the whole C0 and C1 range
// rather than over the handful of sequences anyone thought to list above.
func TestSanitizeCellLeavesNoControlByte(t *testing.T) {
	t.Parallel()

	var hostile strings.Builder
	for r := rune(0); r <= 0x9f; r++ {
		hostile.WriteRune(r)
		hostile.WriteRune('x')
	}

	got := output.SanitizeCell(hostile.String())

	for _, r := range got {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			t.Fatalf("SanitizeCell let U+%04X through", r)
		}
	}

	if !strings.Contains(got, "x") {
		t.Error("SanitizeCell dropped the printable characters as well")
	}
}
