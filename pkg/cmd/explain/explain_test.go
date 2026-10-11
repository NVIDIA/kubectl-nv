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

package explain_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/explain"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// run executes the command with args and returns what it wrote and what it
// returned.
func run(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()

	var out, errOut bytes.Buffer

	streams := genericiooptions.IOStreams{In: strings.NewReader(""), Out: &out, ErrOut: &errOut}
	command := explain.NewCommand(&cmd.Options{Streams: streams})
	command.SetOut(&out)
	command.SetErr(&errOut)
	command.SetArgs(args)
	command.SilenceErrors = true
	command.SilenceUsage = true

	// Execute first and read the buffers afterwards. Go evaluates a return
	// statement's expressions left to right, so returning them in one line
	// would snapshot both buffers before the command had written anything.
	err = command.Execute()

	return out.String(), errOut.String(), err
}

func TestExplainPrintsTheCatalogEntry(t *testing.T) {
	t.Parallel()

	stdout, _, err := run(t, "NV-NODE-002")
	if err != nil {
		t.Fatalf("explain NV-NODE-002: %v", err)
	}

	for _, want := range []string{
		"# NV-NODE-002:",
		"## Symptom",
		"## Next command",
		"kubectl nv info <node>",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not contain %q:\n%s", want, stdout)
		}
	}
}

// The entry goes to stdout and nothing else does, so that piping the command
// into a pager or a file gets the document and only the document.
func TestExplainWritesNothingToStderrOnSuccess(t *testing.T) {
	t.Parallel()

	_, stderr, err := run(t, "NV-NODE-002")
	if err != nil {
		t.Fatalf("explain NV-NODE-002: %v", err)
	}

	if stderr != "" {
		t.Errorf("stderr is %q, want it empty", stderr)
	}
}

// Nobody types a code the way a report prints it. A lowercase code pasted out
// of a terminal has to work.
func TestExplainAcceptsACodeInAnyCase(t *testing.T) {
	t.Parallel()

	canonical, _, err := run(t, "NV-NODE-002")
	if err != nil {
		t.Fatalf("explain NV-NODE-002: %v", err)
	}

	for _, typed := range []string{"nv-node-002", "Nv-Node-002"} {
		got, _, err := run(t, typed)
		if err != nil {
			t.Errorf("explain %s: %v", typed, err)

			continue
		}

		if got != canonical {
			t.Errorf("explain %s printed something other than the canonical entry", typed)
		}
	}
}

// An unknown code is a usage mistake the user can fix by typing something
// else, which is exit 2, and the message lists what they could have typed.
func TestExplainRejectsAnUnknownCode(t *testing.T) {
	t.Parallel()

	stdout, _, err := run(t, "NV-NODE-999")
	if err == nil {
		t.Fatalf("explain NV-NODE-999 succeeded and printed %q", stdout)
	}

	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("the error exits %d, want %d: %v", got, output.ExitUsage, err)
	}

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Errorf("explain returned %T, want an *output.UsageError", err)
	}

	// Exact, not "contains a code". The message is what a user reads after
	// mistyping, so it has to name what they typed and list every code in the
	// order explain prints them.
	want := `no finding catalogued as "NV-NODE-999": the codes are ` +
		"NV-DCGM-001, NV-DP-001, NV-NODE-001, NV-NODE-002, NV-NODE-003, NV-NODE-004, NV-OP-001, NV-OP-002"
	if err.Error() != want {
		t.Errorf("the message is\n  %q\nwant\n  %q", err.Error(), want)
	}

	for _, rule := range rules.All() {
		if !strings.Contains(err.Error(), rule.Code()) {
			t.Errorf("the error does not list %s: %v", rule.Code(), err)
		}
	}

	if stdout != "" {
		t.Errorf("stdout is %q, want nothing printed for an unknown code", stdout)
	}
}

// A code is a short run of letters, digits and hyphens. Anything else is not a
// code, and a command that turned an argument into a file path would be one
// refactor away from reading whatever the user named.
func TestExplainRejectsSomethingThatIsNotACode(t *testing.T) {
	t.Parallel()

	for _, arg := range []string{"../../etc/passwd", "NV-NODE-002.md", "not a code"} {
		stdout, _, err := run(t, arg)
		if err == nil {
			t.Errorf("explain %q succeeded and printed %q", arg, stdout)

			continue
		}

		if got := output.ExitCodeFor(err); got != output.ExitUsage {
			t.Errorf("explain %q exits %d, want %d", arg, got, output.ExitUsage)
		}
	}
}

// Exactly one code. Zero arguments has nothing to explain and two is
// ambiguous, and both are usage mistakes rather than silent successes.
func TestExplainWantsExactlyOneCode(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{}, {"NV-NODE-001", "NV-NODE-002"}} {
		if _, _, err := run(t, args...); err == nil {
			t.Errorf("explain %v succeeded, want a usage error", args)
		}
	}
}

// The command reads a file compiled into the binary, so it must not want a
// cluster: a support engineer looking up a code has no reason to have a
// kubeconfig that works.
func TestExplainNeedsNoCluster(t *testing.T) {
	t.Parallel()

	command := explain.NewCommand(&cmd.Options{})

	if command.Flags().Lookup("kubeconfig") != nil {
		t.Error("explain registers a kubeconfig flag, want it to need no cluster")
	}
}
