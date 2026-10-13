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

package mcp

import (
	"bytes"
	"reflect"
	"sort"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
)

// runCommand executes the mcp command with args and returns what it printed.
func runCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()

	var out, errOut bytes.Buffer
	o := &cmd.Options{Streams: genericiooptions.IOStreams{Out: &out, ErrOut: &errOut}}
	c := NewCommand(o, testTree)
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(args)
	c.SilenceUsage = true
	c.SilenceErrors = true

	err := c.Execute()

	return out.String(), err
}

// --list-tools is how a reader, a script and the read-only guard see what the
// server offers without speaking the protocol or blocking on stdin.
func TestListToolsPrintsEveryToolTheServerWouldOffer(t *testing.T) {
	t.Parallel()

	out, err := runCommand(t, "--list-tools")
	if err != nil {
		t.Fatalf("listing the tools: %v", err)
	}

	got := make([]string, 0)
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		name, _, found := strings.Cut(line, "\t")
		if !found {
			t.Fatalf("line %q is not a name and a description", line)
		}
		got = append(got, name)
	}
	sort.Strings(got)

	want := expectedToolNames(testTree(discardDeps()))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("--list-tools printed %v,\nwant %v", got, want)
	}
}

// Whoever reads the list has to be able to see which tool is served with the
// exec forced off, or the guarantee is only in the code.
func TestListToolsMarksTheExecGatedTool(t *testing.T) {
	t.Parallel()

	out, err := runCommand(t, "--list-tools")
	if err != nil {
		t.Fatalf("listing the tools: %v", err)
	}

	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		name, _, _ := strings.Cut(line, "\t")
		marked := strings.Contains(line, "--no-exec forced")
		if gated := name == "adm_must-gather"; marked != gated {
			t.Errorf("%q is marked as exec gated: %t, want %t", name, marked, gated)
		}
	}
}

func TestMCPTakesNoPositionalArguments(t *testing.T) {
	t.Parallel()

	if _, err := runCommand(t, "serve"); err == nil {
		t.Error("the command accepted a positional argument")
	}
}
