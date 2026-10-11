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
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
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
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/list"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// fakeFactory hands the command fake clients, so that no test reaches a
// cluster and none of them needs a kubeconfig.
type fakeFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

func (f *fakeFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f *fakeFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f *fakeFactory) RESTConfig() (*rest.Config, error)               { return &rest.Config{}, nil }

// streams is the command's three streams, with the two it writes to captured.
type streams struct {
	out *bytes.Buffer
	err *bytes.Buffer
}

// newCommand builds gpu list over the given cluster objects.
func newCommand(t *testing.T, objects ...runtime.Object) (*cobra.Command, *streams) {
	t.Helper()

	s := &streams{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: s.out, ErrOut: s.err}

	o := &cmd.Options{
		Streams: io,
		Factory: &fakeFactory{
			clientset: fake.NewSimpleClientset(objects...),
			dynamic:   dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		},
		Logger: output.NewLogger(io, output.LoggerOptions{NoColor: true}),
	}

	c := list.NewCommand(o)
	c.SetOut(s.out)
	c.SetErr(s.err)
	c.SilenceUsage = true
	c.SilenceErrors = true

	return c, s
}

// runList executes gpu list with the given arguments.
func runList(t *testing.T, objects []runtime.Object, args ...string) (*streams, error) {
	t.Helper()

	c, s := newCommand(t, objects...)
	c.SetArgs(args)

	return s, c.Execute()
}

// gpuNode is a node the device plugin advertises GPUs on.
func gpuNode(name string, gpus int64, labels map[string]string) *corev1.Node {
	all := map[string]string{}
	for k, v := range labels {
		all[k] = v
	}

	q := *resource.NewQuantity(gpus, resource.DecimalSI)

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: all},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": q},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": q},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

// hintedNode is a node that claims GPUs through a label and advertises none,
// which on most clusters is a control plane node-feature-discovery labelled.
func hintedNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"nvidia.com/gpu.present": "true"},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

// gpuPod is a pod asking for whole GPUs on a node.
func gpuPod(name, namespace, node string, gpus int64) *corev1.Pod {
	q := *resource.NewQuantity(gpus, resource.DecimalSI)

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{"nvidia.com/gpu": q},
					Limits:   corev1.ResourceList{"nvidia.com/gpu": q},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// cells splits a table line on runs of whitespace.
func cells(line string) []string {
	return strings.Fields(line)
}

// lines splits output into non-empty lines.
func lines(s string) []string {
	out := make([]string, 0, 8)

	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}

	return out
}

func TestListTable(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNode("worker-2", 4, map[string]string{
			"nvidia.com/gpu.product":               "NVIDIA-T4",
			"nvidia.com/cuda.driver-version.full":  "550.107.02",
			"nvidia.com/cuda.runtime-version.full": "12.5",
		}),
		gpuNode("worker-1", 4, map[string]string{
			"nvidia.com/gpu.product":               "NVIDIA-T4",
			"nvidia.com/cuda.driver-version.full":  "550.107.02",
			"nvidia.com/cuda.runtime-version.full": "12.5",
		}),
		gpuPod("trainer", "team-ml", "worker-1", 2),
	}

	s, err := runList(t, objects)
	if err != nil {
		t.Fatalf("gpu list: %v\nstderr: %s", err, s.err)
	}

	got := lines(s.out.String())
	if len(got) != 3 {
		t.Fatalf("output has %d lines, want a header and two nodes:\n%s", len(got), s.out)
	}

	wantHeader := []string{"NODE", "PRODUCT", "GPUS", "DRIVER", "CUDA", "MODE", "MIG", "UPGRADE", "STATUS"}
	if h := cells(got[0]); !equalStrings(h, wantHeader) {
		t.Errorf("header = %v, want %v", h, wantHeader)
	}

	// Sorted by name, and the pod on worker-1 is the only requested GPU.
	wantFirst := []string{"worker-1", "NVIDIA-T4", "2/4/4", "550.107.02", "12.5", "device-plugin", "Ready"}
	if row := cells(got[1]); !equalStrings(row, wantFirst) {
		t.Errorf("first row = %v, want %v", row, wantFirst)
	}

	wantSecond := []string{"worker-2", "NVIDIA-T4", "0/4/4", "550.107.02", "12.5", "device-plugin", "Ready"}
	if row := cells(got[2]); !equalStrings(row, wantSecond) {
		t.Errorf("second row = %v, want %v", row, wantSecond)
	}
}

func TestListWide(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNode("worker-1", 8, map[string]string{
			"nvidia.com/gpu.clique":            "7b968a6d-c8aa-45e1-9e07-e1e51be99c31.1",
			"fabric.topograph.run/tier-0":      "switch-a",
			"accelerator.topograph.run/domain": "domain-1",
		}),
	}

	s, err := runList(t, objects, "-o", "wide")
	if err != nil {
		t.Fatalf("gpu list -o wide: %v\nstderr: %s", err, s.err)
	}

	got := lines(s.out.String())
	header := cells(got[0])

	for _, want := range []string{
		"CLIQUE",
		"ACCELERATOR.TOPOGRAPH.RUN/DOMAIN",
		"FABRIC.TOPOGRAPH.RUN/TIER-0",
	} {
		if !contains(header, want) {
			t.Errorf("wide header %v has no %s column", header, want)
		}
	}

	row := cells(got[1])
	for _, want := range []string{"7b968a6d-c8aa-45e1-9e07-e1e51be99c31.1", "switch-a", "domain-1"} {
		if !contains(row, want) {
			t.Errorf("wide row %v does not carry %s", row, want)
		}
	}
}

func TestListName(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{gpuNode("worker-2", 4, nil), gpuNode("worker-1", 4, nil)}

	s, err := runList(t, objects, "-o", "name")
	if err != nil {
		t.Fatalf("gpu list -o name: %v\nstderr: %s", err, s.err)
	}

	want := []string{"gpunode/worker-1", "gpunode/worker-2"}
	if got := lines(s.out.String()); !equalStrings(got, want) {
		t.Errorf("output = %v, want %v", got, want)
	}
}

func TestListJSON(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{gpuNode("worker-1", 4, map[string]string{"nvidia.com/gpu.product": "NVIDIA-T4"})}

	s, err := runList(t, objects, "-o", "json")
	if err != nil {
		t.Fatalf("gpu list -o json: %v\nstderr: %s", err, s.err)
	}

	var envelope struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Items      []struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Product        string `json:"product"`
			Requested      int64  `json:"requested"`
			RequestedKnown bool   `json:"requestedKnown"`
		} `json:"items"`
	}

	if err := json.Unmarshal(s.out.Bytes(), &envelope); err != nil {
		t.Fatalf("decoding the output: %v\n%s", err, s.out)
	}

	if envelope.APIVersion != output.APIVersion {
		t.Errorf("apiVersion = %q, want %q", envelope.APIVersion, output.APIVersion)
	}

	if envelope.Kind != output.KindGPUNodeList {
		t.Errorf("kind = %q, want %q", envelope.Kind, output.KindGPUNodeList)
	}

	if len(envelope.Items) != 1 {
		t.Fatalf("items = %#v, want exactly one", envelope.Items)
	}

	node := envelope.Items[len(envelope.Items)-1]
	if node.Metadata.Name != "worker-1" {
		t.Fatalf("item name = %q, want worker-1", node.Metadata.Name)
	}

	if node.Product != "NVIDIA-T4" {
		t.Errorf("product = %q, want NVIDIA-T4", node.Product)
	}

	if !node.RequestedKnown {
		t.Error("requestedKnown = false, want true when the pods were listed")
	}
}

// TestListHintedNote pins the one line that keeps a quiet node from being
// invisible: a node that should have GPUs and advertises none is the single
// most common thing a user is looking for, and it is out of the list.
func TestListHintedNote(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNode("worker-1", 4, nil),
		hintedNode("control-plane"),
		hintedNode("broken-1"),
	}

	t.Run("counted on stderr by default", func(t *testing.T) {
		t.Parallel()

		s, err := runList(t, objects)
		if err != nil {
			t.Fatalf("gpu list: %v", err)
		}

		if strings.Contains(s.out.String(), "control-plane") {
			t.Errorf("stdout lists a hinted node:\n%s", s.out)
		}

		note := s.err.String()
		if !strings.Contains(note, "2") {
			t.Errorf("stderr does not count the hinted nodes: %q", note)
		}

		if !strings.Contains(note, "--include-hinted") {
			t.Errorf("stderr does not name the flag that shows them: %q", note)
		}
	})

	t.Run("silent when there are none", func(t *testing.T) {
		t.Parallel()

		s, err := runList(t, []runtime.Object{gpuNode("worker-1", 4, nil)})
		if err != nil {
			t.Fatalf("gpu list: %v", err)
		}

		if strings.Contains(s.err.String(), "--include-hinted") {
			t.Errorf("stderr mentions hinted nodes when the cluster has none: %q", s.err)
		}
	})

	t.Run("shown on request", func(t *testing.T) {
		t.Parallel()

		s, err := runList(t, objects, "--include-hinted")
		if err != nil {
			t.Fatalf("gpu list --include-hinted: %v", err)
		}

		got := lines(s.out.String())
		if len(got) != 4 {
			t.Fatalf("output has %d lines, want a header and three nodes:\n%s", len(got), s.out)
		}

		if strings.Contains(s.err.String(), "--include-hinted") {
			t.Errorf("stderr still asks for the flag that was given: %q", s.err)
		}
	})
}

// TestListNoPods pins --no-pods: the requested count is "?" rather than 0,
// because nobody looked.
func TestListNoPods(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNode("worker-1", 4, nil),
		gpuPod("trainer", "team-ml", "worker-1", 2),
	}

	s, err := runList(t, objects, "--no-pods")
	if err != nil {
		t.Fatalf("gpu list --no-pods: %v", err)
	}

	row := cells(lines(s.out.String())[1])
	if !contains(row, "?/4/4") {
		t.Errorf("row = %v, want the requested count reported as unknown", row)
	}
}

func TestListSelectorAndNode(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNode("a100-1", 8, map[string]string{"nvidia.com/gpu.product": "A100-SXM4-40GB"}),
		gpuNode("t4-1", 4, map[string]string{"nvidia.com/gpu.product": "NVIDIA-T4"}),
	}

	t.Run("by selector", func(t *testing.T) {
		t.Parallel()

		s, err := runList(t, objects, "-l", "nvidia.com/gpu.product=NVIDIA-T4", "-o", "name")
		if err != nil {
			t.Fatalf("gpu list -l: %v", err)
		}

		if got := lines(s.out.String()); !equalStrings(got, []string{"gpunode/t4-1"}) {
			t.Errorf("output = %v, want only t4-1", got)
		}
	})

	t.Run("by node", func(t *testing.T) {
		t.Parallel()

		s, err := runList(t, objects, "--node", "a100-1", "-o", "name")
		if err != nil {
			t.Fatalf("gpu list --node: %v", err)
		}

		if got := lines(s.out.String()); !equalStrings(got, []string{"gpunode/a100-1"}) {
			t.Errorf("output = %v, want only a100-1", got)
		}
	})

	t.Run("a selector that does not parse is a usage error", func(t *testing.T) {
		t.Parallel()

		_, err := runList(t, objects, "-l", "==")
		if err == nil {
			t.Fatal("gpu list accepted a selector that does not parse")
		}

		if code := output.ExitCodeFor(err); code != output.ExitUsage {
			t.Errorf("exit code = %d, want %d for a malformed selector", code, output.ExitUsage)
		}
	})
}

// TestListFromBundle is the offline path: the same command over a must-gather
// directory, with no cluster anywhere.
func TestListFromBundle(t *testing.T) {
	t.Parallel()

	s, err := runList(t, nil, "--from-bundle", "testdata/bundle", "-o", "name")
	// The bundle records only the operator's namespace, so the pod list it can
	// answer is a partial one and the run exits 3 saying so.
	if code := output.ExitCodeFor(err); code != output.ExitPartial {
		t.Fatalf("exit code = %d, want %d: a bundle cannot answer for every namespace (err: %v)", code, output.ExitPartial, err)
	}

	want := []string{"gpunode/worker-dra", "gpunode/worker-t4"}
	if got := lines(s.out.String()); !equalStrings(got, want) {
		t.Errorf("output = %v, want %v", got, want)
	}
}

// TestListFromBundleMissing pins that a path that is not a bundle is the
// user's to fix rather than an empty cluster.
func TestListFromBundleMissing(t *testing.T) {
	t.Parallel()

	_, err := runList(t, nil, "--from-bundle", "testdata/there-is-no-bundle-here")
	if err == nil {
		t.Fatal("gpu list accepted a bundle directory that does not exist")
	}
}

// TestListUnknownFormat pins that a typo in -o stops the run rather than
// falling back to the table.
func TestListUnknownFormat(t *testing.T) {
	t.Parallel()

	_, err := runList(t, []runtime.Object{gpuNode("worker-1", 4, nil)}, "-o", "jsonn")
	if err == nil {
		t.Fatal("gpu list accepted an unknown output format")
	}

	if code := output.ExitCodeFor(err); code != output.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, output.ExitUsage)
	}
}

// TestListTakesNoArguments pins that "gpu list worker-1" is a mistake rather
// than a list of every node: the command that takes a node is gpu info.
func TestListTakesNoArguments(t *testing.T) {
	t.Parallel()

	if _, err := runList(t, []runtime.Object{gpuNode("worker-1", 4, nil)}, "worker-1"); err == nil {
		t.Fatal("gpu list accepted a positional argument")
	}
}

// TestListDRAMode pins that a node whose GPUs are published as ResourceSlices
// is listed, and listed as DRA.
func TestListDRAMode(t *testing.T) {
	t.Parallel()

	node := "worker-dra"
	objects := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: node},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			},
		},
		&resourcev1.ResourceSlice{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-dra-pool-0"},
			Spec:       resourcev1.ResourceSliceSpec{Driver: "gpu.nvidia.com", NodeName: &node},
		},
	}

	s, err := runList(t, objects)
	if err != nil {
		t.Fatalf("gpu list: %v\nstderr: %s", err, s.err)
	}

	row := cells(lines(s.out.String())[1])
	if !contains(row, "dra") {
		t.Errorf("row = %v, want the mode reported as dra", row)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}

	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}

	return false
}

// TestListHintedNoteIsExact pins the whole line, not a fragment of it. The
// note is the only thing standing between a user and a node that silently
// vanished from the inventory, so its wording, its count and the flag it names
// are all part of the contract.
func TestListHintedNoteIsExact(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNode("worker-1", 4, nil),
		hintedNode("control-plane"),
		hintedNode("broken-1"),
	}

	s, err := runList(t, objects)
	if err != nil {
		t.Fatalf("gpu list: %v", err)
	}

	want := "2 node(s) carry an NVIDIA label but advertise no GPUs; pass --include-hinted to list them"
	if got := strings.TrimSpace(s.err.String()); got != want {
		t.Errorf("stderr = %q, want %q", got, want)
	}
}

// TestListHintedNoteCountsOnlyWhatTheQueryLeftOut pins that the number answers
// "what did I not see here". A user who asked about one product being told
// about hinted nodes of another goes looking for nodes their query never
// touched.
func TestListHintedNoteCountsOnlyWhatTheQueryLeftOut(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNode("worker-1", 4, map[string]string{"nvidia.com/gpu.product": "NVIDIA-T4"}),
		hintedNodeWithLabels("broken-t4", map[string]string{"nvidia.com/gpu.product": "NVIDIA-T4"}),
		hintedNodeWithLabels("broken-a100", map[string]string{"nvidia.com/gpu.product": "A100-SXM4-40GB"}),
		hintedNode("control-plane"),
	}

	t.Run("the note counts only the matching hinted node", func(t *testing.T) {
		t.Parallel()

		s, err := runList(t, objects, "-l", "nvidia.com/gpu.product=NVIDIA-T4")
		if err != nil {
			t.Fatalf("gpu list -l: %v", err)
		}

		want := "1 node(s) carry an NVIDIA label but advertise no GPUs; pass --include-hinted to list them"
		if got := strings.TrimSpace(s.err.String()); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("the selector keeps the matching hinted node and drops the rest", func(t *testing.T) {
		t.Parallel()

		s, err := runList(t, objects,
			"-l", "nvidia.com/gpu.product=NVIDIA-T4", "--include-hinted", "-o", "name")
		if err != nil {
			t.Fatalf("gpu list -l --include-hinted: %v", err)
		}

		want := []string{"gpunode/broken-t4", "gpunode/worker-1"}
		if got := lines(s.out.String()); !equalStrings(got, want) {
			t.Errorf("output = %v, want %v", got, want)
		}

		if strings.Contains(s.err.String(), "node(s) carry") {
			t.Errorf("stderr still counts hinted nodes that were listed: %q", s.err)
		}
	})
}

// TestListIncludeHintedOrder pins the single sorted order: a hinted node sits
// between its neighbours rather than in an appendix.
func TestListIncludeHintedOrder(t *testing.T) {
	t.Parallel()

	objects := []runtime.Object{
		gpuNode("worker-1", 4, nil),
		hintedNode("control-plane"),
		hintedNode("broken-1"),
	}

	s, err := runList(t, objects, "--include-hinted", "-o", "name")
	if err != nil {
		t.Fatalf("gpu list --include-hinted: %v", err)
	}

	want := []string{"gpunode/broken-1", "gpunode/control-plane", "gpunode/worker-1"}
	if got := lines(s.out.String()); !equalStrings(got, want) {
		t.Errorf("output = %v, want %v", got, want)
	}
}

// hintedNodeWithLabels is a hinted node carrying extra labels, for the
// selector cases.
func hintedNodeWithLabels(name string, labels map[string]string) *corev1.Node {
	node := hintedNode(name)
	for k, v := range labels {
		node.Labels[k] = v
	}

	return node
}
