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
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/mcp"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// writeVerbs are the API verbs this plugin must never issue. kubectl-nv is a
// diagnostic tool: a support engineer runs it against a cluster that is
// already unhappy, and a write from it would change the very state being
// reported.
var writeVerbs = []string{"create", "update", "patch", "delete", "deletecollection"}

// leafFixture is what a leaf needs to run under the guard, and what it is
// expected to do.
type leafFixture struct {
	// args are the arguments that make this leaf run.
	args func(t *testing.T) []string
	// wantErr is the error Execute must return, matched on substring. Empty
	// means the leaf has to succeed: a leaf that fails early reads as
	// read-only while never having reached the code that would write.
	wantErr string
}

// leafFixtures gives every leaf of the command tree its fixture. A leaf with
// no entry fails the test rather than being skipped, so a command added later
// cannot slip past the guard by needing an argument.
var leafFixtures = map[string]leafFixture{
	"adm must-gather": {
		args: func(t *testing.T) []string { return []string{"--artifacts-dir", t.TempDir()} },
	},
	"version": {
		args: func(*testing.T) []string { return nil },
	},
	"mcp": {
		// --list-tools walks the tree and exits. Serving would block on stdin,
		// and what the guard has to see is the walk, not the transport.
		args: func(*testing.T) []string { return []string{"--list-tools"} },
	},
	"schema": {
		// --write-dir runs the whole generator rather than one kind, which is
		// what "make schema" does and the most this leaf ever touches.
		args: func(t *testing.T) []string { return []string{"--write-dir", t.TempDir()} },
	},
	"list": {
		args: func(*testing.T) []string { return nil },
	},
	"info": {
		// The fixture cluster's one node carries a hint label and no GPU
		// capacity, so info reaches the whole read path and reports it as a
		// hinted node rather than refusing.
		args: func(*testing.T) []string { return []string{"gpu-node-1"} },
	},
	"topology": {
		args: func(*testing.T) []string { return nil },
	},
	"explain": {
		args: func(*testing.T) []string { return []string{"NV-NODE-002"} },
	},
	"health": {
		// The fixture cluster's node is hinted and its driver pod reports no
		// container statuses, so it is not ready: NV-NODE-002 and NV-DRV-001
		// both fire, and a run with a finding at or above --fail-on exits
		// non-zero by design.
		//
		// The error is asserted rather than avoided, because this leaf has to
		// run to the end for the guard to prove anything about what it would
		// have written, and the one thing that must not happen is the command
		// stopping early. The text is the findings error's own, so a run that
		// failed for some other reason still fails here.
		args:    func(*testing.T) []string { return nil },
		wantErr: "--fail-on is error",
	},
	"debug": {
		// The same hinted node, and a directory of its own: debug runs the
		// whole gather after the inventory read, so this leaf walks both the
		// read path and every collector.
		args: func(t *testing.T) []string {
			return []string{"gpu-node-1", "--artifacts-dir", t.TempDir()}
		},
	},
}

// readOptional lists the leaves that have no business talking to a cluster.
// Every other leaf has to issue at least one read, or the run proved nothing
// about what it would have written.
//
// explain is one of them: it reads a catalog compiled into the binary, which
// is the point of it, because a support engineer looking up a finding code has
// no reason to have a working kubeconfig.
var readOptional = map[string]bool{
	"version": true,
	"schema":  true,
	"mcp":     true,
	"explain": true,
}

// TestEveryCommandIsReadOnly executes every leaf of the command tree against
// clients that fail the test on any write. The fakes cover the typed and
// dynamic clients; an HTTP server standing in for the API covers anything
// built straight from the REST config, which the fakes never see.
func TestEveryCommandIsReadOnly(t *testing.T) {
	paths := leafPaths(newTestCommand(t, newRecorder(t, "")))
	if len(paths) == 0 {
		t.Fatal("found no leaf commands to execute")
	}
	t.Logf("leaf commands under test: %v", paths)

	for _, path := range paths {
		name := strings.Join(path, " ")
		t.Run(name, func(t *testing.T) {
			fixture, ok := leafFixtures[name]
			if !ok {
				t.Fatalf("no fixture for leaf %q: add one to leafFixtures so the guard covers it", name)
			}

			isolateKubeconfig(t)

			rec := newRecorder(t, name)
			cmd := newTestCommand(t, rec)

			args := append([]string{}, path...)
			args = append(args, "--kubeconfig", writeKubeconfig(t))
			args = append(args, fixture.args(t)...)
			cmd.SetArgs(args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true

			// The error is checked as well as the calls: a leaf that fails on its
			// first line never reaches the code that would write, and would read
			// as read-only for the wrong reason.
			err := cmd.Execute()
			switch {
			case fixture.wantErr == "" && err != nil:
				t.Errorf("%q returned %v, want it to run to the end", name, err)
			case fixture.wantErr != "" && err == nil:
				t.Errorf("%q returned nil, want an error containing %q", name, fixture.wantErr)
			case fixture.wantErr != "" && !strings.Contains(err.Error(), fixture.wantErr):
				t.Errorf("%q returned %v, want an error containing %q", name, err, fixture.wantErr)
			}

			for _, w := range rec.writes() {
				t.Errorf("kubectl-nv issued %s: this tool must be read-only", w)
			}
			// A leaf that reached pods/exec has to be one the MCP server knows
			// can exec, or it would be served to a model with the exec
			// available and nothing would say so.
			if rec.execs() > 0 && !isExecAnnotated(t, path) {
				t.Errorf("%q issued a pods/exec but carries no %s annotation: "+
					"the MCP server would serve it with the exec available",
					name, mcp.ExecAnnotation)
			}
			if !readOptional[name] && rec.reads() == 0 {
				t.Errorf("%q read nothing from the cluster: the guard proved nothing about it", name)
			}
		})
	}
}

// recorder counts what the clients were asked to do and reports every write.
// The HTTP half runs on the server's goroutines, so it records and uses
// Errorf, which is safe to call from any goroutine, rather than Fatalf, which
// is not.
type recorder struct {
	t    *testing.T
	leaf string

	mu     sync.Mutex
	nReads int
	nExecs int
	bad    []string
}

func newRecorder(t *testing.T, leaf string) *recorder {
	t.Helper()
	return &recorder{t: t, leaf: leaf}
}

// transport stands in for the API server for anything built straight from the
// REST config, which the fake clients never see. It answers in-process, so
// the guard needs no listening socket.
func (r *recorder) transport() http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if req.Method != http.MethodGet {
			what := fmt.Sprintf("an HTTP %s to %s", req.Method, req.URL.Path)
			r.record(what)
			// Errorf, not Fatalf: this runs on whatever goroutine made the
			// request, and only the test's own goroutine may call Fatalf.
			r.t.Errorf("kubectl-nv issued %s: this tool must be read-only", what)
			http.Error(w, "writes are not allowed", http.StatusForbidden)
		} else {
			r.countRead()
			if strings.Contains(req.URL.Path, "/exec") {
				r.countExec()
			}
			http.NotFound(w, req)
		}
		resp := w.Result()
		resp.Request = req
		return resp, nil
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func (r *recorder) record(what string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.bad = append(r.bad, what)
}

// countExec records a request that would run a command inside a pod.
func (r *recorder) countExec() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nExecs++
}

func (r *recorder) execs() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nExecs
}

func (r *recorder) countRead() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nReads++
}

func (r *recorder) reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nReads
}

func (r *recorder) writes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string{}, r.bad...)
}

// install wires the recorder into a fake clientset or dynamic client. Reads
// are counted and passed on to the object tracker; a write fails the test
// where it happens, so the failure names the call site.
func (r *recorder) install(f *k8stesting.Fake) {
	f.PrependReactor("*", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		r.countRead()
		return false, nil, nil
	})
	for _, verb := range writeVerbs {
		f.PrependReactor(verb, "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
			what := fmt.Sprintf("a %s on %s", action.GetVerb(), action.GetResource().Resource)
			r.record(what)
			r.t.Fatalf("kubectl-nv issued %s: this tool must be read-only", what)
			return true, nil, nil
		})
	}
}

// isExecAnnotated reports whether the leaf at path carries the annotation that
// makes the MCP server force --no-exec on it.
func isExecAnnotated(t *testing.T, path []string) bool {
	t.Helper()

	c := root.NewCommand("test", root.Deps{Streams: genericiooptions.NewTestIOStreamsDiscard()})
	for _, name := range path {
		var next *cobra.Command
		for _, child := range c.Commands() {
			if child.Name() == name {
				next = child

				break
			}
		}
		if next == nil {
			t.Fatalf("no %q under %q", name, c.Name())
		}
		c = next
	}

	return c.Annotations[mcp.ExecAnnotation] == "true"
}

// TestEveryExecCapableLeafIsAnnotated states the other half: the leaves the
// tree marks as exec capable are the ones the MCP server gates. The guard above
// catches a leaf that execs without the annotation; this catches an annotation
// put on something that cannot exec, which would quietly refuse a tool that was
// safe all along.
func TestEveryExecCapableLeafIsAnnotated(t *testing.T) {
	annotated := []string{}
	for _, path := range leafPaths(newTestCommand(t, newRecorder(t, ""))) {
		if isExecAnnotated(t, path) {
			annotated = append(annotated, strings.Join(path, " "))
		}
	}

	// debug runs the same gather as adm must-gather, narrowed to one node, so
	// it reaches the same pods/exec and has to be gated the same way.
	want := []string{"adm must-gather", "debug"}
	if !reflect.DeepEqual(annotated, want) {
		t.Errorf("the exec capable leaves are %v, want %v", annotated, want)
	}
}

// newTestCommand builds the real command tree over the recorder's clients.
func newTestCommand(t *testing.T, rec *recorder) *cobra.Command {
	t.Helper()
	return newTestCommandWithExecs(t, rec, &execLog{t: t})
}

// newTestCommandWithExecs builds the tree over the recorder's clients and
// records every command the must-gather would have run inside a pod.
func newTestCommandWithExecs(t *testing.T, rec *recorder, execs *execLog) *cobra.Command {
	t.Helper()

	clientset := fake.NewSimpleClientset(clusterObjects()...)
	// The dynamic fake panics on a List of a resource whose list kind it was
	// not told about, so every custom resource the must-gather reads is
	// registered even though this cluster holds none of them.
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}:           "ClusterPolicyList",
			{Group: "nvidia.com", Version: "v1alpha1", Resource: "nvidiadrivers"}:       "NVIDIADriverList",
			{Group: "nvidia.com", Version: "v1alpha1", Resource: "gpuclusters"}:         "GPUClusterList",
			{Group: "hco.kubevirt.io", Version: "v1beta1", Resource: "hyperconvergeds"}: "HyperConvergedList",
			{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}:                "KubeVirtList",
			{Group: "resource.k8s.io", Version: "v1", Resource: "deviceclasses"}:        "DeviceClassList",
		})
	rec.install(&clientset.Fake)
	rec.install(&dyn.Fake)

	f := &fakeFactory{
		clientset: clientset,
		dynamic:   dyn,
		// Anything built from the REST config rather than from the fakes lands
		// here, where a write is just as visible.
		config: &rest.Config{Host: "https://kubectl-nv.invalid", Transport: rec.transport()},
	}

	return root.NewCommand("test", root.Deps{
		Streams: genericiooptions.NewTestIOStreamsDiscard(),
		Factory: f,
		MustGather: func(o *mustgather.Options) error {
			// The two live exec paths, the bug report copy and the device usage,
			// talk SPDY to a real API server. Stubbing both keeps the rest of
			// the gather, including every List it makes, under the fakes.
			//
			// pods/exec is a POST, which this guard treats as a write, and it
			// is the one call kubectl-nv is allowed to make: it reads a file
			// out of a driver pod and changes nothing. It is behind --no-exec
			// for the users who will not have it.
			o.FetchPodFile = func(_ context.Context, pod *v1.Pod, destPath string) error {
				execs.record("nvidia-bug-report.sh")
				return os.WriteFile(destPath, []byte(pod.Name), 0o600)
			}
			o.RunInPod = func(_ context.Context, pod *v1.Pod, _, command string) (string, error) {
				execs.record(command)
				return pod.Name, nil
			}
			return mustgather.Run(o, f, output.NewLogger(genericiooptions.NewTestIOStreamsDiscard(), output.LoggerOptions{}))
		},
	})
}

// execLog records what the must-gather asked to run inside a pod, and refuses
// anything that is not one of the two commands it is allowed to run.
//
// pods/exec is the single write-shaped call this tool makes, so what it carries
// is part of the read-only guarantee: a collector that grew a command of its
// own would still pass a check that only counted the execs.
type execLog struct {
	t *testing.T

	mu       sync.Mutex
	commands []string
}

// allowedExecCommands are the only things kubectl-nv runs inside a pod: the
// bug report, and the device usage probes and readings. Each is a read.
var allowedExecCommands = []string{
	"nvidia-bug-report.sh",
	"command -v nvidia-smi",
	"nvidia-smi",
	"nsenter --target 1 --mount --pid -- sh -c 'lsof /run/nvidia/driver/dev/nvidia*",
	"nsenter --target 1 --mount --pid -- sh -c 'fuser -v /run/nvidia/driver/dev/nvidia*",
	"nsenter --target 1 --mount --pid -- sh -c 'ls -la /run/nvidia/driver/dev/nvidia*",
	"nsenter --target 1 --mount --pid -- sh -c '\n\t\t\tpids=",
	"nsenter --target 1 --mount --pid -- sh -c 'pids=",
}

func (l *execLog) record(command string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.commands = append(l.commands, command)

	for _, allowed := range allowedExecCommands {
		if strings.HasPrefix(command, allowed) {
			return
		}
	}
	// Errorf, not Fatalf: this can run on another goroutine.
	l.t.Errorf("kubectl-nv ran %q inside a pod, which is not one of the commands it may run:\n  %s",
		command, strings.Join(allowedExecCommands, "\n  "))
}

func (l *execLog) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.commands)
}

// TestMustGatherExecsOnlyWhatItMay covers the one call this tool makes that is
// not a GET. The guard above proves the gather issues no writes; this proves
// that the exec it is allowed to make carries only the commands it is supposed
// to, and that --no-exec means no exec at all rather than fewer of them.
func TestMustGatherExecsOnlyWhatItMay(t *testing.T) {
	for _, noExec := range []bool{false, true} {
		name := "the exec commands are the ones it may run"
		if noExec {
			name = "--no-exec runs nothing at all"
		}
		t.Run(name, func(t *testing.T) {
			isolateKubeconfig(t)

			execs := &execLog{t: t}
			cmd := newTestCommandWithExecs(t, newRecorder(t, "adm must-gather"), execs)

			args := []string{"adm", "must-gather", "--artifacts-dir", t.TempDir(),
				"--kubeconfig", writeKubeconfig(t)}
			if noExec {
				args = append(args, "--no-exec")
			}
			cmd.SetArgs(args)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true

			if err := cmd.Execute(); err != nil {
				t.Fatalf("adm must-gather: %v", err)
			}

			switch got := execs.count(); {
			case noExec && got != 0:
				t.Errorf("--no-exec ran %d commands inside pods, want none: %v", got, execs.commands)
			case !noExec && got == 0:
				t.Error("the gather ran no command inside any pod, so this proved nothing about what it runs")
			}
		})
	}
}

// clusterObjects is the smallest cluster that drives must-gather all the way
// to the bug-report copy: an operator pod to find the namespace from, a GPU
// node, a driver pod to collect from, and a daemonset.
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

// isolateKubeconfig keeps a test away from the kubeconfig of whoever is
// running it. KUBECONFIG has to name a path that does not exist rather than be
// emptied: client-go treats an empty KUBECONFIG as unset and falls back to the
// default location, which is the file this is protecting.
func isolateKubeconfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig-here"))
}
