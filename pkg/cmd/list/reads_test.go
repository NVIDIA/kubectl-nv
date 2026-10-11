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

package list_test

import (
	"bytes"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/info"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/list"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// readCounter counts the list calls a command makes, per resource.
type readCounter struct {
	mu sync.Mutex
	n  map[string]int
}

func (c *readCounter) install(f *k8stesting.Fake) {
	f.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		c.mu.Lock()
		defer c.mu.Unlock()

		if c.n == nil {
			c.n = map[string]int{}
		}

		c.n[action.GetResource().Resource]++

		return false, nil, nil
	})
}

func (c *readCounter) count(resource string) int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.n[resource]
}

// runCounted executes a command built by newCmd over counted fake clients.
func runCounted(
	t *testing.T,
	newCmd func(*cmd.Options) *cobra.Command,
	objects []runtime.Object,
	args ...string,
) (*readCounter, *bytes.Buffer, error) {
	t.Helper()

	clientset := fake.NewSimpleClientset(objects...)
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())

	counter := &readCounter{}
	counter.install(&clientset.Fake)
	counter.install(&dyn.Fake)

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: out, ErrOut: errOut}

	o := &cmd.Options{
		Streams: io,
		Factory: &fakeFactory{clientset: clientset, dynamic: dyn},
		Logger:  output.NewLogger(io, output.LoggerOptions{NoColor: true}),
	}

	c := newCmd(o)
	c.SetOut(out)
	c.SetErr(errOut)
	c.SetArgs(args)
	c.SilenceUsage = true
	c.SilenceErrors = true

	return counter, errOut, c.Execute()
}

// TestOneReadPerCollection pins the whole read cost of each command. The
// numbers are the contract ROADMAP.md states: one paginated list per resource
// type and no per-node reads, so that a two thousand node cluster is four
// round trips rather than two thousand.
func TestOneReadPerCollection(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNode("worker-1", 4, nil),
		gpuNode("worker-2", 4, nil),
		gpuPod("trainer", "team-ml", "worker-1", 2),
	}

	tests := []struct {
		name   string
		newCmd func(*cmd.Options) *cobra.Command
		args   []string
	}{
		{name: "list", newCmd: list.NewCommand},
		{name: "info", newCmd: info.NewCommand, args: []string{"worker-1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			counter, _, err := runCounted(t, tt.newCmd, objects, tt.args...)
			if err != nil {
				t.Fatalf("gpu %s: %v", tt.name, err)
			}

			for _, resource := range []string{"nodes", "pods", "resourceslices"} {
				if got := counter.count(resource); got != 1 {
					t.Errorf("gpu %s listed %s %d times, want 1", tt.name, resource, got)
				}
			}
		})
	}
}

// TestNoPodsSkipsThePodList pins that --no-pods really does skip the read
// rather than make it and throw the answer away.
func TestNoPodsSkipsThePodList(t *testing.T) {
	t.Parallel()

	counter, _, err := runCounted(t, list.NewCommand, []runtime.Object{gpuNode("worker-1", 4, nil)}, "--no-pods")
	if err != nil {
		t.Fatalf("gpu list --no-pods: %v", err)
	}

	if got := counter.count("pods"); got != 0 {
		t.Errorf("gpu list --no-pods listed pods %d times, want none", got)
	}
}

// TestDeniedSlicesAreReportedOnce pins what the user reads when the slice list
// is refused. Naming the same resource twice reads as two problems and sends
// the reader looking for a second cause that is not there.
func TestDeniedSlicesAreReportedOnce(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		newCmd func(*cmd.Options) *cobra.Command
		args   []string
	}{
		{name: "list", newCmd: list.NewCommand},
		{name: "info", newCmd: info.NewCommand, args: []string{"worker-1"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			clientset := fake.NewSimpleClientset(gpuNode("worker-1", 4, nil))
			clientset.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(
					schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}, "", nil)
			})

			out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
			io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: out, ErrOut: errOut}

			o := &cmd.Options{
				Streams: io,
				Factory: &fakeFactory{
					clientset: clientset,
					dynamic:   dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
				},
				Logger: output.NewLogger(io, output.LoggerOptions{NoColor: true}),
			}

			c := tt.newCmd(o)
			c.SetOut(out)
			c.SetErr(errOut)
			c.SetArgs(tt.args)
			c.SilenceUsage = true
			c.SilenceErrors = true

			err := c.Execute()
			if err == nil {
				t.Fatalf("gpu %s returned no error with the slice list denied", tt.name)
			}

			if code := output.ExitCodeFor(err); code != output.ExitPartial {
				t.Errorf("exit code = %d, want %d", code, output.ExitPartial)
			}

			// The cause is counted by its own prefix, not by the resource
			// name: the API server's message carries
			// "resourceslices.resource.k8s.io" inside it, so counting the bare
			// name would find two occurrences in a correctly deduplicated
			// error.
			if n := strings.Count(err.Error(), "could not read resourceslices"); n != 1 {
				t.Errorf("the slice denial is reported %d times in %q, want once", n, err)
			}
		})
	}
}
