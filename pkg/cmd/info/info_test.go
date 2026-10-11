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
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
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

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/info"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

type fakeFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

func (f *fakeFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f *fakeFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f *fakeFactory) RESTConfig() (*rest.Config, error)               { return &rest.Config{}, nil }

type streams struct {
	out *bytes.Buffer
	err *bytes.Buffer
}

// runInfo executes gpu info with the given arguments over a fake cluster.
func runInfo(t *testing.T, objects []runtime.Object, args ...string) (*streams, error) {
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

	c := info.NewCommand(o)
	c.SetOut(s.out)
	c.SetErr(s.err)
	c.SetArgs(args)
	c.SilenceUsage = true
	c.SilenceErrors = true

	return s, c.Execute()
}

func gpuNode(name string, gpus int64, labels map[string]string) *corev1.Node {
	q := *resource.NewQuantity(gpus, resource.DecimalSI)

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": q},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": q},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
				{Type: corev1.NodeMemoryPressure, Status: corev1.ConditionFalse, Reason: "KubeletHasSufficientMemory"},
			},
		},
	}
}

// operandPod is one GPU Operator pod on a node.
func operandPod(name, node, app string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "gpu-operator",
			Labels:    map[string]string{"app": app},
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: app, Image: "nvcr.io/nvidia/" + app + ":v26.3.3"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: app, Ready: true}},
		},
	}
}

// cluster is one GPU node with the operator's operands on it, plus a node the
// labels claim GPUs for and the cluster does not advertise any on.
func cluster() []runtime.Object {
	return []runtime.Object{
		gpuNode("worker-1", 4, map[string]string{
			"nvidia.com/gpu.product":               "NVIDIA-T4",
			"nvidia.com/gpu.count":                 "4",
			"nvidia.com/cuda.driver-version.full":  "550.107.02",
			"nvidia.com/cuda.runtime-version.full": "12.5",
			"nvidia.com/gpu.deploy.driver":         "true",
		}),
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "control-plane",
				Labels: map[string]string{"nvidia.com/gpu.present": "true"},
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			},
		},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "plain-worker"}},
		operandPod("gpu-operator-0", "worker-1", "gpu-operator"),
		operandPod("nvidia-device-plugin-daemonset-xyz", "worker-1", "nvidia-device-plugin-daemonset"),
		operandPod("gpu-feature-discovery-abc", "worker-1", "gpu-feature-discovery"),
	}
}

func TestInfoDescribesTheNode(t *testing.T) {
	t.Parallel()

	s, err := runInfo(t, cluster(), "worker-1")
	if err != nil {
		t.Fatalf("gpu info: %v\nstderr: %s", err, s.err)
	}

	got := s.out.String()
	for _, want := range []string{
		"worker-1",
		"NVIDIA-T4",
		"550.107.02",
		"12.5",
		"device-plugin",
		// The conditions are what separates info from list.
		"KubeletHasSufficientMemory",
		// The operands and what they are running.
		"nvidia-device-plugin-daemonset-xyz",
		"gpu-feature-discovery-abc",
		"nvcr.io/nvidia/gpu-feature-discovery:v26.3.3",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output does not mention %q:\n%s", want, got)
		}
	}
}

// TestInfoUnknownNode pins the message and the exit code a script branches on.
func TestInfoUnknownNode(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"plain-worker", "no-such-node"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := runInfo(t, cluster(), name)
			if err == nil {
				t.Fatalf("gpu info %s returned no error", name)
			}

			want := "node " + name + " has no GPUs and no GPU labels"
			if err.Error() != want {
				t.Errorf("error = %q, want exactly %q", err, want)
			}

			if code := output.ExitCodeFor(err); code != output.ExitUsage {
				t.Errorf("exit code = %d, want %d", code, output.ExitUsage)
			}
		})
	}
}

// TestInfoHintedNode pins that the node a user is most likely asking about,
// one that claims GPUs and advertises none, is shown rather than refused.
func TestInfoHintedNode(t *testing.T) {
	t.Parallel()

	s, err := runInfo(t, cluster(), "control-plane", "-o", "json")
	if err != nil {
		t.Fatalf("gpu info control-plane: %v\nstderr: %s", err, s.err)
	}

	var node struct {
		Kind   string `json:"kind"`
		Hinted bool   `json:"hinted"`
		Mode   string `json:"mode"`
	}

	if err := json.Unmarshal(s.out.Bytes(), &node); err != nil {
		t.Fatalf("decoding the output: %v\n%s", err, s.out)
	}

	if node.Kind != output.KindGPUNode {
		t.Errorf("kind = %q, want %q", node.Kind, output.KindGPUNode)
	}

	if !node.Hinted {
		t.Error("hinted = false, want true for a node that advertises no GPUs")
	}

	if node.Mode != "none" {
		t.Errorf("mode = %q, want none", node.Mode)
	}
}

// TestInfoJSONCarriesTheDetails pins what WithDetails is for: the conditions
// and the slice names are in the machine readable output too, not only in the
// described one.
func TestInfoJSONCarriesTheDetails(t *testing.T) {
	t.Parallel()

	s, err := runInfo(t, cluster(), "worker-1", "-o", "json")
	if err != nil {
		t.Fatalf("gpu info -o json: %v", err)
	}

	var node struct {
		Conditions []struct {
			Type string `json:"type"`
		} `json:"conditions"`
		Operands []struct {
			Name      string `json:"name"`
			Component string `json:"component"`
		} `json:"operands"`
		ResourceSlices []string `json:"resourceSlices"`
	}

	if err := json.Unmarshal(s.out.Bytes(), &node); err != nil {
		t.Fatalf("decoding the output: %v\n%s", err, s.out)
	}

	if len(node.Conditions) != 2 {
		t.Errorf("conditions = %#v, want both of the node's", node.Conditions)
	}

	if len(node.Operands) != 3 {
		t.Errorf("operands = %#v, want the three pods on the node", node.Operands)
	}

	// Never null, so that a consumer can range without a null check first.
	if node.ResourceSlices == nil {
		t.Error("resourceSlices is null, want an empty array")
	}
}

func TestInfoName(t *testing.T) {
	t.Parallel()

	s, err := runInfo(t, cluster(), "worker-1", "-o", "name")
	if err != nil {
		t.Fatalf("gpu info -o name: %v", err)
	}

	if got := strings.TrimSpace(s.out.String()); got != "gpunode/worker-1" {
		t.Errorf("output = %q, want gpunode/worker-1", got)
	}
}

// TestInfoArgumentCount pins that info is about exactly one node: no argument
// is a mistake, and two is a mistake with a plausible reading.
func TestInfoArgumentCount(t *testing.T) {
	t.Parallel()

	for _, args := range [][]string{{}, {"worker-1", "control-plane"}} {
		if _, err := runInfo(t, cluster(), args...); err == nil {
			t.Errorf("gpu info %v returned no error", args)
		}
	}
}

// TestInfoFromBundle is the offline path, over the same fixture gpu list uses.
func TestInfoFromBundle(t *testing.T) {
	t.Parallel()

	s, err := runInfo(t, nil, "worker-t4", "--from-bundle", "../list/testdata/bundle", "-o", "json")
	// A bundle holds only the operator's namespace, so the pod read is partial.
	if code := output.ExitCodeFor(err); code != output.ExitPartial {
		t.Fatalf("exit code = %d, want %d (err: %v)", code, output.ExitPartial, err)
	}

	var node struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Product        string `json:"product"`
		RequestedKnown bool   `json:"requestedKnown"`
		Operands       []struct {
			Name string `json:"name"`
		} `json:"operands"`
	}

	if err := json.Unmarshal(s.out.Bytes(), &node); err != nil {
		t.Fatalf("decoding the output: %v\n%s", err, s.out)
	}

	if node.Metadata.Name != "worker-t4" {
		t.Errorf("name = %q, want worker-t4", node.Metadata.Name)
	}

	if node.Product != "NVIDIA-T4" {
		t.Errorf("product = %q, want NVIDIA-T4", node.Product)
	}

	// The bundle never recorded the pods outside the operator's namespace, so
	// the requested count is not something it can answer.
	if node.RequestedKnown {
		t.Error("requestedKnown = true from a bundle, want false")
	}

	if len(node.Operands) != 2 {
		t.Errorf("operands = %#v, want the two the bundle recorded", node.Operands)
	}
}

// TestInfoPartialReadIsNotAUsageError is the difference between "this node has
// no GPUs" and "I could not see whether it has any".
//
// A DRA node is a GPU node only because a ResourceSlice names it. With the
// slice list denied, the node falls out of the inventory entirely, and
// reporting that as "node X has no GPUs and no GPU labels" tells the user a
// fact about their cluster that nobody established. Exit 3 says the run was
// incomplete, which is what actually happened.
func TestInfoPartialReadIsNotAUsageError(t *testing.T) {
	t.Parallel()

	node := "worker-dra"
	clientset := fake.NewSimpleClientset(&corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: node},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	})
	clientset.PrependReactor("list", "resourceslices", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}, "", nil)
	})

	s := &streams{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: s.out, ErrOut: s.err}

	o := &cmd.Options{
		Streams: io,
		Factory: &fakeFactory{
			clientset: clientset,
			dynamic:   dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		},
		Logger: output.NewLogger(io, output.LoggerOptions{NoColor: true}),
	}

	c := info.NewCommand(o)
	c.SetOut(s.out)
	c.SetErr(s.err)
	c.SetArgs([]string{node})
	c.SilenceUsage = true
	c.SilenceErrors = true

	err := c.Execute()
	if err == nil {
		t.Fatal("gpu info returned no error with the slice list denied")
	}

	if code := output.ExitCodeFor(err); code != output.ExitPartial {
		t.Errorf("exit code = %d, want %d: %v", code, output.ExitPartial, err)
	}

	if strings.Contains(err.Error(), "has no GPUs") {
		t.Errorf("error = %q, want it to report the denied read rather than a fact about the node", err)
	}
}

// TestInfoUnknownNodeStillExitsTwoOnACompleteRead pins the other half: when
// the run saw everything and the node still is not there, the user typed a
// name the cluster does not have, and that is exit 2.
func TestInfoUnknownNodeStillExitsTwoOnACompleteRead(t *testing.T) {
	t.Parallel()

	_, err := runInfo(t, cluster(), "no-such-node")
	if err == nil {
		t.Fatal("gpu info returned no error for a node the cluster does not have")
	}

	if code := output.ExitCodeFor(err); code != output.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, output.ExitUsage)
	}
}

// TestInfoFromBundleTableExitsPartial pins the exit code on the path a person
// actually uses. The json case below asserts it too, but the table path takes
// a different branch through the command and would keep its own bug.
func TestInfoFromBundleTableExitsPartial(t *testing.T) {
	t.Parallel()

	s, err := runInfo(t, nil, "worker-t4", "--from-bundle", "../list/testdata/bundle")
	if code := output.ExitCodeFor(err); code != output.ExitPartial {
		t.Fatalf("exit code = %d, want %d (err: %v)", code, output.ExitPartial, err)
	}

	// The description still has to be printed: a partial read comes with a
	// usable answer, and withholding it would make exit 3 mean exit 4.
	if !strings.Contains(s.out.String(), "worker-t4") {
		t.Errorf("gpu info printed no description alongside the partial read:\n%s", s.out)
	}
}
