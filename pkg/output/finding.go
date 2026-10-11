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

import (
	"encoding/json"
	"fmt"
)

// Finding is one thing a command noticed. It is what every check in every
// track emits and what every consumer parses, so the field names here are the
// contract and the order they appear in is fixed.
//
// A finding answers four questions in order: what was found (Code, Severity),
// where (Node, Component), what it means (Message, Evidence), and what to do
// about it (Docs, Next). The last two are not decoration. A user who runs a
// health check and is told something is wrong, with no link and no next
// command, has been given a worse version of the problem they already had.
type Finding struct {
	// Code is the stable identifier of the check that produced the finding,
	// in the form NV-<AREA>-<NNN>. It is what a script greps for and what a
	// suppression list names, so it never changes meaning.
	Code string `json:"code,omitempty" yaml:"code,omitempty"`
	// Severity decides whether this finding fails the run, through ExitCode
	// and the --fail-on threshold.
	Severity Severity `json:"severity,omitempty" yaml:"severity,omitempty"`
	// Node is the node the finding is about. It is empty for a finding about
	// the cluster as a whole.
	Node string `json:"node,omitempty" yaml:"node,omitempty"`
	// Component is the part the finding is about. For an operand it is the
	// verbatim value of the pod's "app" label, such as
	// "nvidia-driver-daemonset" or "gpu-feature-discovery", which is what
	// pkg/inventory's Component* constants hold.
	//
	// A finding about the node itself carries the literal "node" rather than
	// an empty string. The column is one a reader scans down and one a script
	// groups by, and an empty cell there is indistinguishable from a component
	// the producing command failed to name.
	Component string `json:"component,omitempty" yaml:"component,omitempty"`
	// Message is the human readable detail, one sentence, no trailing period.
	Message string `json:"message,omitempty" yaml:"message,omitempty"`
	// Evidence is what the finding was concluded from: the label values, the
	// counts, the exit codes. It is keyed by whatever names the value best,
	// usually the label or the field it came from, verbatim, so that a reader
	// can go and look at the same thing. It is never null on the wire.
	//
	// The values are strings because the keys are not a fixed set and the
	// types behind them are not uniform, which rules out a typed struct and
	// makes map[string]any a map whose values a consumer has to type switch
	// on. So the encodings are fixed instead, and every rule in every track
	// follows them:
	//
	//	integers     base 10, no separators, no units: "7", "0", "-1"
	//	booleans     "true" or "false", never "True", "yes" or "1"
	//	lists        sorted, comma joined, no spaces: "a,b,c"
	//	times        RFC3339 in UTC: "2026-01-02T03:04:05Z"
	//	durations    Go's form, as time.Duration.String writes it: "1h30m0s"
	//	quantities   the resource.Quantity string: "16Gi", "250m"
	//	absent       the key is left out, never set to "" or "<none>"
	//
	// Sorted and joined rather than indexed ("node.0", "node.1") because a
	// finding is one line in a report and a reader compares two of them by
	// eye. Deterministic for the same reason every other ordering here is: two
	// runs over the same cluster have to produce the same bytes.
	Evidence map[string]string `json:"evidence" yaml:"evidence"`
	// Docs is where to read more: a URL, or a path to a document in this
	// repository.
	Docs string `json:"docs,omitempty" yaml:"docs,omitempty"`
	// Next is the command to run next, ready to paste.
	Next string `json:"next,omitempty" yaml:"next,omitempty"`
}

// findingJSON breaks the method set, so that MarshalJSON can marshal the value
// without recursing into itself.
type findingJSON Finding

// MarshalJSON renders a nil Evidence as an empty object, for the same reason
// List and ReportStatus render nil slices as empty arrays: a consumer that has
// to check for null before it can index is a consumer that will forget to.
func (f Finding) MarshalJSON() ([]byte, error) {
	out := findingJSON(f)
	if out.Evidence == nil {
		out.Evidence = map[string]string{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling finding %q: %w", f.Code, err)
	}

	return raw, nil
}
