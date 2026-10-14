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

package info_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/info"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The three output flags are registered together by pkg/output, so a command
// that prints one object still takes --limit on the command line and refuses
// it when it is used. A program driving kubectl-nv should be able to pass the
// same bounds to every read-only command without first learning which of them
// are list shaped.
func TestInfoRegistersTheOutputFlags(t *testing.T) {
	t.Parallel()

	c := info.NewCommand(&cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()})

	for _, name := range []string{"output", "fields", "limit"} {
		if c.Flags().Lookup(name) == nil {
			t.Errorf("info has no --%s", name)
		}
	}
}

// A GPUNode is one node, so there is nothing for --limit to cut. Answering a
// bounded request with an unbounded one would leave the caller believing it
// asked for less than it got, which is worse than refusing it.
func TestInfoRefusesALimit(t *testing.T) {
	t.Parallel()

	_, err := runInfo(t, cluster(), "worker-1", "--limit", "1", "-o", "json")

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
	if code := output.ExitCodeFor(err); code != output.ExitUsage {
		t.Errorf("exit code is %d, want %d", code, output.ExitUsage)
	}
	if !strings.Contains(err.Error(), output.KindGPUNode) {
		t.Errorf("the error is %q, want it to name the kind", err)
	}
}

// The refusal comes before anything is read.
//
// The cluster given holds no such node, so a check made after the read would
// answer about the node and not about the flag. On a real cluster that
// difference is the caller waiting for a scan they then get no answer from,
// which is the whole reason the flags are validated against a prototype.
func TestInfoRefusesALimitBeforeReadingAnything(t *testing.T) {
	t.Parallel()

	_, err := runInfo(t, nil, "worker-1", "--limit", "1", "-o", "json")

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
	if !strings.Contains(err.Error(), "--limit") {
		t.Errorf("the error is %q: it is about the node, so the flag was checked after the read", err)
	}
}

// --fields has nothing to do on the table this command prints, which is a
// description of one node rather than a row of columns. Saying so is the
// honest answer: applying it to nothing would hand back the whole node to a
// caller that asked for one field of it.
func TestInfoRefusesFieldsOnItsDescription(t *testing.T) {
	t.Parallel()

	_, err := runInfo(t, cluster(), "worker-1", "--fields", "capacity")

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
	for _, want := range []string{"--fields", "-o json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}

	// And it is accepted where there is something to project, or this would
	// pass on a command that refused the flag outright.
	if _, err := runInfo(t, cluster(), "worker-1", "--fields", "capacity", "-o", "json"); err != nil {
		t.Errorf("--fields with -o json: %v", err)
	}
}

// --fields does reach a single object: there is nesting to project away even
// when there is only one of the thing.
func TestInfoProjectsFields(t *testing.T) {
	t.Parallel()

	s, err := runInfo(t, cluster(), "worker-1", "--fields", "capacity", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv info --fields capacity: %v", err)
	}

	var node map[string]any
	if err := json.Unmarshal(s.out.Bytes(), &node); err != nil {
		t.Fatalf("decoding the projected node: %v\n%s", err, s.out.String())
	}

	// The three a projection never drops, plus the one that was asked for, and
	// nothing else: a projection that kept more than it was asked for is a
	// projection that did not happen.
	want := []string{"apiVersion", "capacity", "kind", "metadata"}
	got := make([]string, 0, len(node))
	for key := range node {
		got = append(got, key)
	}
	if !sameSet(got, want) {
		t.Errorf("the projected node carries %v, want %v", sorted(got), want)
	}
	if _, dropped := node["product"]; dropped {
		t.Error("the projection kept product, which was not asked for")
	}
}

// sameSet reports whether two slices hold the same strings.
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]bool, len(got))
	for _, key := range got {
		seen[key] = true
	}
	for _, key := range want {
		if !seen[key] {
			return false
		}
	}

	return true
}

// sorted returns a sorted copy, for a failure message.
func sorted(values []string) []string {
	out := append([]string(nil), values...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}

	return out
}
