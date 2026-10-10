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

package cmd_test

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
)

// writeVerbs are the API verbs this plugin must never issue. kubectl-nv is a
// diagnostic tool: a support engineer runs it against a cluster that is already
// unhappy, and a write from it would change the very state being reported.
var writeVerbs = []string{"create", "update", "patch", "delete", "deletecollection"}

// TestEveryCommandIsReadOnly executes every leaf of the command tree against
// fake clients that fail the test on any write. New commands are covered
// without touching this test, because the tree is walked rather than listed.
func TestEveryCommandIsReadOnly(t *testing.T) {
	paths := leafPaths(newTestCommand(t, &recorder{}))
	if len(paths) == 0 {
		t.Fatal("found no leaf commands to execute")
	}
	t.Logf("leaf commands under test: %v", paths)

	var exercised bool
	for _, path := range paths {
		t.Run(strings.Join(path, " "), func(t *testing.T) {
			rec := &recorder{t: t}
			cmd := newTestCommand(t, rec)

			args := append([]string{}, path...)
			args = append(args, "--kubeconfig", writeKubeconfig(t))
			if leafAt(cmd, path).Flags().Lookup("artifacts-dir") != nil {
				args = append(args, "--artifacts-dir", t.TempDir())
			}
			cmd.SetArgs(args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true

			// A command that reports a cluster problem is still read-only, so the
			// returned error is not the subject here; the recorded verbs are.
			_ = cmd.Execute()

			if rec.reads() > 0 {
				exercised = true
			}
		})
	}

	if !exercised {
		t.Fatal("no leaf command read from the fake cluster: the guard proved nothing")
	}
}

// recorder counts the API calls the fakes see and fails the test on a write.
type recorder struct {
	t  *testing.T
	mu sync.Mutex
	n  int
}

func (r *recorder) reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// install wires the recorder into a fake. Reads are counted and passed on to
// the object tracker; writes stop the test where they happen, so the failure
// names the call site.
func (r *recorder) install(f *k8stesting.Fake) {
	f.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		r.mu.Lock()
		r.n++
		r.mu.Unlock()
		return false, nil, nil
	})
	for _, verb := range writeVerbs {
		f.PrependReactor(verb, "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
			if r.t != nil {
				r.t.Fatalf("kubectl-nv issued a %s on %s: this tool must be read-only",
					action.GetVerb(), action.GetResource().Resource)
			}
			return true, nil, nil
		})
	}
}

// newTestCommand builds the real command tree over fake clients.
func newTestCommand(t *testing.T, rec *recorder) *cobra.Command {
	t.Helper()

	clientset := fake.NewSimpleClientset(clusterObjects()...)
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	rec.install(&clientset.Fake)
	rec.install(&dyn.Fake)

	f := &fakeFactory{
		clientset: clientset,
		dynamic:   dyn,
		// A host nothing listens on: the OpenShift probe has to fail fast rather
		// than reach whatever cluster the developer is pointed at.
		config: &rest.Config{Host: "https://127.0.0.1:1"},
	}

	return root.NewCommand("test", root.Deps{
		Streams: genericiooptions.NewTestIOStreamsDiscard(),
		Factory: f,
		MustGather: func(o *mustgather.Options) error {
			// The live fetcher execs into a pod; stubbing it keeps the rest of the
			// gather, including every List it makes, under the fakes.
			o.FetchPodFile = func(pod *v1.Pod, destPath string) error {
				return os.WriteFile(destPath, []byte(pod.Name), 0o600)
			}
			return mustgather.Run(o, f, io.Discard)
		},
	})
}

// clusterObjects is the smallest cluster that drives must-gather all the way to
// the bug-report copy: an operator pod to find the namespace from, a GPU node,
// a driver pod to collect from, and a daemonset.
func clusterObjects() []runtime.Object {
	return []runtime.Object{
		&v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu-node-1",
			Labels: map[string]string{"feature.node.kubernetes.io/pci-10de.present": "true"},
		}},
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gpu-operator-0",
				Namespace: "gpu-operator",
				Labels:    map[string]string{"app": "gpu-operator"},
			},
			Spec:   v1.PodSpec{NodeName: "gpu-node-1", Containers: []v1.Container{{Name: "gpu-operator"}}},
			Status: v1.PodStatus{Phase: v1.PodRunning},
		},
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nvidia-driver-daemonset-abcde",
				Namespace: "gpu-operator",
				Labels:    map[string]string{"app": "nvidia-driver-daemonset"},
			},
			Spec:   v1.PodSpec{NodeName: "gpu-node-1", Containers: []v1.Container{{Name: "nvidia-driver-ctr"}}},
			Status: v1.PodStatus{Phase: v1.PodRunning},
		},
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{
			Name:      "nvidia-driver-daemonset",
			Namespace: "gpu-operator",
		}},
	}
}

// fakeFactory hands the command tree fake clients instead of real ones.
type fakeFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
	config    *rest.Config
}

func (f *fakeFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f *fakeFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f *fakeFactory) RESTConfig() (*rest.Config, error)               { return f.config, nil }

// leafPaths lists the argument path of every command that does work, so a
// command added later is walked without this test being edited.
func leafPaths(c *cobra.Command) [][]string {
	var out [][]string
	var walk func(cmd *cobra.Command, path []string)
	walk = func(cmd *cobra.Command, path []string) {
		children := cmd.Commands()
		if len(children) == 0 {
			if len(path) > 0 {
				out = append(out, path)
			}
			return
		}
		for _, child := range children {
			walk(child, append(append([]string{}, path...), child.Name()))
		}
	}
	walk(c, nil)
	return out
}

// leafAt returns the command the given argument path names.
func leafAt(root *cobra.Command, path []string) *cobra.Command {
	cmd := root
	for _, name := range path {
		for _, child := range cmd.Commands() {
			if child.Name() == name {
				cmd = child
				break
			}
		}
	}
	return cmd
}

func writeKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	contents := `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}
