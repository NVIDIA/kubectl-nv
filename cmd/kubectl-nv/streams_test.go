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

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// streamsFactory hands the tree fake clients, so that a leaf which reads a
// cluster has one to read and nothing here reaches a real one.
type streamsFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

func (f *streamsFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f *streamsFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f *streamsFactory) RESTConfig() (*rest.Config, error)               { return &rest.Config{}, nil }

// streamsCluster is one GPU node with the operator on it, which is enough for
// every read-only leaf to produce an answer rather than an error.
func streamsCluster() []runtime.Object {
	four := *resource.NewQuantity(4, resource.DecimalSI)

	return []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "gpu-node-1",
				Labels: map[string]string{
					"nvidia.com/gpu.present":                      "true",
					"feature.node.kubernetes.io/pci-10de.present": "true",
					"nvidia.com/gpu.count":                        "4",
					"nvidia.com/gpu.clique":                       "clique-a",
				},
			},
			Status: corev1.NodeStatus{
				Capacity:    corev1.ResourceList{"nvidia.com/gpu": four},
				Allocatable: corev1.ResourceList{"nvidia.com/gpu": four},
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "gpu-operator-0", Namespace: "gpu-operator",
				Labels: map[string]string{"app": "gpu-operator"},
			},
			Spec: corev1.PodSpec{
				NodeName:   "gpu-node-1",
				Containers: []corev1.Container{{Name: "gpu-operator", Image: "nvcr.io/nvidia/gpu-operator:v26.3.3"}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}
}

// streamsRun is one execution of the real main path: the two streams it wrote
// and the code it would have exited with.
type streamsRun struct {
	stdout string
	stderr string
	code   int
}

// runMain executes the command tree exactly as main does, with the two streams
// separated. It is the whole point of these tests: the stream a command writes
// to is decided in main, so a test that builds a command itself and calls
// SetOut proves nothing about what a user sees.
func runMain(t *testing.T, args ...string) streamsRun {
	t.Helper()
	isolateKubeconfig(t)

	var stdout, stderr bytes.Buffer
	streams := genericiooptions.IOStreams{In: strings.NewReader(""), Out: &stdout, ErrOut: &stderr}
	deps := root.Deps{
		Streams: streams,
		Factory: &streamsFactory{
			clientset: fake.NewSimpleClientset(streamsCluster()...),
			dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(), mustgather.CustomResourceListKinds()),
		},
		MustGather: func(*mustgather.Options) error { return nil },
	}
	code := run(args, streams, root.NewCommand("v9.9.9-streams-test", deps))

	return streamsRun{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// bundleForStreams writes the smallest complete bundle the two --from-bundle
// readers accept, so that those leaves answer rather than fail.
func bundleForStreams(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	write("nodes.yaml", `apiVersion: v1
kind: NodeList
items:
- apiVersion: v1
  kind: Node
  metadata:
    name: gpu-node-1
    labels:
      nvidia.com/gpu.present: "true"
      feature.node.kubernetes.io/pci-10de.present: "true"
      nvidia.com/gpu.count: "4"
  status:
    capacity:
      nvidia.com/gpu: "4"
    allocatable:
      nvidia.com/gpu: "4"
    conditions:
    - type: Ready
      status: "True"
`)
	write("gpu_operand_pods.yaml", `apiVersion: v1
kind: PodList
items:
- apiVersion: v1
  kind: Pod
  metadata:
    name: dp-xyz
    namespace: gpu-operator
    labels:
      app: nvidia-device-plugin-daemonset
  spec:
    nodeName: gpu-node-1
    containers:
    - name: plugin
  status:
    phase: Running
    containerStatuses:
    - name: plugin
      ready: true
`)
	write("gpu_pods.yaml", "apiVersion: v1\nkind: PodList\nitems: []\n")
	write("node_events.yaml", "apiVersion: v1\nkind: EventList\nitems: []\n")
	write("resourceslices.yaml", "apiVersion: resource.k8s.io/v1\nkind: ResourceSliceList\nitems: []\n")
	write("cluster_policy.missing", "not installed\n")
	write("compute_domains.missing", "not installed\n")

	return dir
}

// leafPaths returns the argument path of every command of the tree that does
// work, computed by walking the tree rather than written down, so that a
// command another track adds is covered without this test being edited.
func leafPaths(c *cobra.Command) [][]string {
	var out [][]string
	var walk func(*cobra.Command, []string)
	walk = func(current *cobra.Command, path []string) {
		children := current.Commands()
		if len(children) == 0 {
			if len(path) > 0 {
				out = append(out, path)
			}

			return
		}
		for _, child := range children {
			if child.Hidden || child.Name() == "help" || child.Name() == "completion" {
				continue
			}
			walk(child, append(append([]string{}, path...), child.Name()))
		}
	}
	walk(c, nil)

	return out
}

// Help is what the user asked for, so it is the answer and belongs on stdout.
// Every leaf, not a sample: the stream is decided once in main, and a leaf that
// got it wrong would be the one nobody checked.
func TestEveryLeafPrintsItsHelpOnStdout(t *testing.T) {
	tree := root.NewCommand("v9.9.9-streams-test", root.Deps{
		Streams: genericiooptions.NewTestIOStreamsDiscard(),
	})

	leaves := leafPaths(tree)
	if len(leaves) == 0 {
		t.Fatal("the tree has no leaves: the guard would prove nothing")
	}

	for _, path := range leaves {
		name := strings.Join(path, " ")
		t.Run(name, func(t *testing.T) {
			got := runMain(t, append(append([]string{}, path...), "--help")...)
			if got.code != output.ExitOK {
				t.Fatalf("%s --help exited %d\nstderr:\n%s", name, got.code, got.stderr)
			}
			if !strings.Contains(got.stdout, "Usage:") {
				t.Errorf("%s --help put no usage on stdout\nstdout:\n%s\nstderr:\n%s",
					name, got.stdout, got.stderr)
			}
			if got.stderr != "" {
				t.Errorf("%s --help wrote to stderr: %q", name, got.stderr)
			}
		})
	}
}

// leafInvocation is one leaf of the tree, the arguments that make it answer,
// and a string its answer has to carry.
type leafInvocation struct {
	leaf string
	args func(t *testing.T) []string
	want string
	// codes are the exit codes that count as an answer. health is 0 or 1
	// depending on what the fixture has on it, and both are answers: 1 says
	// something was found, not that the command could not run. Empty means
	// exit 0 alone.
	codes []int
}

// The data a command produces goes to stdout, so that a pipeline gets it.
//
// Every leaf of the tree, through the real main path, each given whatever it
// needs to produce an answer rather than an error: the two bundle readers get
// a bundle, the cluster readers get a fake cluster, and the four that need
// neither get nothing. A leaf printing through cobra's stream writes its
// answer to stderr, because main points that at stderr so usage lands there.
//
// kubectl nv version printed its 25 bytes to stderr and nothing to stdout, and
// kubectl nv explain NV-NODE-002 > entry.md wrote an empty file. Both were
// measured on the binary before this test existed.
func TestEveryLeafPutsItsDataOnStdout(t *testing.T) {
	invocations := []leafInvocation{
		{leaf: "version", args: noArgs("version"), want: "kubectl-nv version v9.9.9-streams-test"},
		{leaf: "--version", args: noArgs("--version"), want: "v9.9.9-streams-test"},
		{leaf: "explain", args: noArgs("explain", "NV-NODE-002"), want: "NV-NODE-002"},
		{leaf: "schema", args: noArgs("schema", "list"), want: `"kind"`},
		{leaf: "mcp", args: noArgs("mcp", "--list-tools"), want: "list"},
		{leaf: "list", args: fromBundle("list"), want: `"kind": "GPUNodeList"`},
		{
			leaf: "health", args: fromBundle("health"),
			want: `"kind": "HealthReport"`, codes: []int{output.ExitOK, output.ExitFindings},
		},
		{leaf: "info", args: noArgs("info", "gpu-node-1", "-o", "json"), want: `"kind": "GPUNode"`},
		{leaf: "topology", args: noArgs("topology", "-o", "json"), want: `"kind": "TopologyReport"`},
		{leaf: "debug", args: intoTempDir("debug", "gpu-node-1"), want: `"kind": "DebugReport"`},
		{
			leaf: "adm must-gather", args: intoTempDir("adm", "must-gather"),
			want: "", // It prints no object: its stdout is empty on purpose.
		},
	}

	covered := map[string]bool{}
	for _, tc := range invocations {
		covered[tc.leaf] = true
		t.Run(tc.leaf, func(t *testing.T) {
			got := runMain(t, tc.args(t)...)
			if !acceptable(got.code, tc.codes) {
				t.Fatalf("exited %d, want one of %v\nstdout:\n%s\nstderr:\n%s",
					got.code, wantedCodes(tc.codes), got.stdout, got.stderr)
			}
			if tc.want == "" {
				if got.stdout != "" {
					t.Errorf("stdout is %q, want nothing: this command prints no object", got.stdout)
				}

				return
			}
			if !strings.Contains(got.stdout, tc.want) {
				t.Errorf("stdout does not carry %q\nstdout:\n%s\nstderr:\n%s",
					tc.want, got.stdout, got.stderr)
			}
		})
	}

	// Every leaf of the tree is in the list above. A command another track adds
	// fails here rather than going unchecked.
	tree := root.NewCommand("v9.9.9-streams-test", root.Deps{
		Streams: genericiooptions.NewTestIOStreamsDiscard(),
	})
	var missing []string
	for _, path := range leafPaths(tree) {
		if !covered[strings.Join(path, " ")] {
			missing = append(missing, strings.Join(path, " "))
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these leaves are not covered: %v", missing)
	}
}

// wantedCodes is the exit codes an invocation accepts, defaulting to zero.
func wantedCodes(codes []int) []int {
	if len(codes) == 0 {
		return []int{output.ExitOK}
	}

	return codes
}

// acceptable reports whether an exit code is one the invocation accepts.
func acceptable(code int, codes []int) bool {
	for _, want := range wantedCodes(codes) {
		if code == want {
			return true
		}
	}

	return false
}

// noArgs is a leaf that needs nothing but its own arguments.
func noArgs(args ...string) func(*testing.T) []string {
	return func(*testing.T) []string { return args }
}

// fromBundle is a leaf answering out of a bundle rather than a cluster.
func fromBundle(name string) func(*testing.T) []string {
	return func(t *testing.T) []string {
		t.Helper()

		return []string{name, "--from-bundle", bundleForStreams(t), "-o", "json"}
	}
}

// intoTempDir is a leaf that writes a bundle, pointed at a directory of its
// own so that nothing is written where the developer is standing.
func intoTempDir(args ...string) func(*testing.T) []string {
	return func(t *testing.T) []string {
		t.Helper()

		full := append([]string{}, args...)
		full = append(full, "--artifacts-dir", filepath.Join(t.TempDir(), "artifacts"), "--no-exec")
		if args[0] == "debug" {
			full = append(full, "-o", "json")
		}

		return full
	}
}

// A command the user got wrong is a diagnostic, and a diagnostic on stdout is
// a pipeline reading an error message as data.
func TestUsageErrorsGoToStderr(t *testing.T) {
	got := runMain(t, "explain", "NV-NOT-A-CODE")

	if got.code != output.ExitUsage {
		t.Fatalf("exited %d, want %d\nstderr:\n%s", got.code, output.ExitUsage, got.stderr)
	}
	if got.stdout != "" {
		t.Errorf("stdout is %q, want nothing: the answer is an error", got.stdout)
	}
	if !strings.Contains(got.stderr, "NV-NOT-A-CODE") {
		t.Errorf("stderr does not name the code that was wrong: %q", got.stderr)
	}
}

// cobraStreamCall is how a command reaches cobra's own output stream.
//
// main points that at stderr unless the answer is help or the version, so a
// command printing its data through it writes the data to stderr. The four
// that did are fixed; this is what stops the fifth, because the failure is
// invisible until somebody redirects stdout to a file and finds it empty.
const cobraStreamCall = "OutOrStdout"

// No command package may print through cobra's stream. A test that ran the
// commands would catch it only for the leaves it thought to run; this catches
// it for every line of every command package.
func TestNoCommandPackagePrintsThroughCobrasStream(t *testing.T) {
	root := filepath.Join("..", "..", "pkg", "cmd")

	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		raw, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(raw), cobraStreamCall) {
			offenders = append(offenders, path)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}

	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Errorf("these files print through cobra's stream, which main points at stderr: %v\n"+
			"write to the IOStreams output instead", offenders)
	}
}

// decodeJSON reads what a command printed, so that a failure says what was
// wrong with the document rather than that a substring was missing.
func decodeJSON(t *testing.T, raw string) map[string]any {
	t.Helper()

	var out map[string]any
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("the output does not parse: %v\n%s", err, raw)
	}

	return out
}

// The two bundle readers answer with the kind they are documented to print,
// parsed rather than matched: a substring check passes on a document that
// happens to mention the kind in an error message.
func TestTheBundleReadersPrintTheirOwnKind(t *testing.T) {
	for leaf, kind := range map[string]string{
		"list":   output.KindGPUNodeList,
		"health": output.KindHealthReport,
	} {
		t.Run(leaf, func(t *testing.T) {
			got := runMain(t, leaf, "--from-bundle", bundleForStreams(t), "-o", "json")
			if got.code != output.ExitOK && got.code != output.ExitFindings {
				t.Fatalf("%s --from-bundle exited %d, want 0 or %d\nstderr:\n%s",
					leaf, got.code, output.ExitFindings, got.stderr)
			}
			if decodeJSON(t, got.stdout)["kind"] != kind {
				t.Errorf("%s printed kind %v, want %s", leaf, decodeJSON(t, got.stdout)["kind"], kind)
			}
		})
	}
}

// unusedOptions keeps the cmd import load bearing for the factory above.
var _ cmd.Factory = (*streamsFactory)(nil)
