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

package debug_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	cmddebug "github.com/NVIDIA/kubectl-nv/pkg/cmd/debug"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The three output flags are registered together by pkg/output, so a command
// that prints one object still takes --limit on the command line and refuses
// it when it is used. A program driving kubectl-nv should be able to pass the
// same bounds to every read-only command without first learning which of them
// are list shaped.
func TestDebugRegistersTheOutputFlags(t *testing.T) {
	t.Parallel()

	c := cmddebug.NewCommand(
		&cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()},
		func(*mustgather.Options) error { return nil },
	)

	for _, name := range []string{"output", "fields", "limit"} {
		if c.Flags().Lookup(name) == nil {
			t.Errorf("debug has no --%s", name)
		}
	}
}

// --fields has nothing to do on the summary this command prints, which is a
// description rather than a table of columns. It is the same answer kubectl nv
// info gives, from the same function.
func TestDebugRefusesFieldsOnItsSummary(t *testing.T) {
	_, err := runDebug(t, gatherSeam{}, "worker-1", "--fields", "bundle")

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
	for _, want := range []string{"--fields", "-o json"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
}

// A DebugReport is about one node, so there is nothing for --limit to cut.
// Answering a bounded request with an unbounded one would leave the caller
// believing it asked for less than it got.
func TestDebugRefusesALimit(t *testing.T) {
	_, err := runDebug(t, gatherSeam{}, "worker-1", "--limit", "1", "-o", "json")

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
	if code := output.ExitCodeFor(err); code != output.ExitUsage {
		t.Errorf("exit code is %d, want %d", code, output.ExitUsage)
	}
	if !strings.Contains(err.Error(), output.KindDebugReport) {
		t.Errorf("the error is %q, want it to name the kind", err)
	}
}

// --fields does reach a single object: a debug report nests a whole node and a
// bundle summary, and a caller that wants one of them should not have to take
// both.
func TestDebugProjectsFields(t *testing.T) {
	r, err := runDebug(t, gatherSeam{}, "worker-1", "--fields", "bundle", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv debug --fields bundle: %v", err)
	}

	var report map[string]any
	if err := json.Unmarshal(r.out.Bytes(), &report); err != nil {
		t.Fatalf("decoding the projected report: %v\n%s", err, r.out.String())
	}

	if _, kept := report["bundle"]; !kept {
		t.Errorf("the projection dropped bundle, which was asked for: %v", report)
	}
	if _, kept := report["node"]; kept {
		t.Error("the projection kept node, which was not asked for")
	}
	// The three no projection may drop: without them a consumer cannot tell
	// what it is holding or which node the values belong to.
	for _, want := range []string{"apiVersion", "kind", "metadata"} {
		if _, kept := report[want]; !kept {
			t.Errorf("the projection dropped %s, which it may never drop", want)
		}
	}
}
