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

// This file is the external test package so that it can import the command
// packages. The commands do not import pkg/mustgather, so there is no cycle;
// the internal tests cannot be used because the fixtures they share live in
// the internal one.
package mustgather_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
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

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	cmdhealth "github.com/NVIDIA/kubectl-nv/pkg/cmd/health"
	cmdlist "github.com/NVIDIA/kubectl-nv/pkg/cmd/list"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// roundTripFactory hands the gather fakes, so that nothing here reaches a
// cluster.
type roundTripFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

func (f roundTripFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f roundTripFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f roundTripFactory) RESTConfig() (*rest.Config, error)               { return &rest.Config{}, nil }

// roundTripListKinds names the list kind of every custom resource the gather
// reads. The dynamic fake panics on a List it was not told about, which is a
// crash in the test rather than a cluster this gather could ever meet.
func roundTripListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}:           "ClusterPolicyList",
		{Group: "nvidia.com", Version: "v1alpha1", Resource: "nvidiadrivers"}:       "NVIDIADriverList",
		{Group: "nvidia.com", Version: "v1alpha1", Resource: "gpuclusters"}:         "GPUClusterList",
		{Group: "hco.kubevirt.io", Version: "v1beta1", Resource: "hyperconvergeds"}: "HyperConvergedList",
		{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}:                "KubeVirtList",
		{Group: "resource.k8s.io", Version: "v1", Resource: "deviceclasses"}:        "DeviceClassList",
	}
}

// quietProgress keeps the gather's commentary out of the test output.
type quietProgress struct{}

func (quietProgress) Infof(string, ...any)  {}
func (quietProgress) Warnf(string, ...any)  {}
func (quietProgress) Errorf(string, ...any) {}

// gatherForRoundTrip runs a real gather over a fake cluster and returns the
// bundle directory. The cluster has a GPU node, a device plugin pod on it in
// the operator namespace, a training job in another namespace holding two of
// its GPUs, and an event about the node.
func gatherForRoundTrip(t *testing.T) string {
	t.Helper()

	four := *resource.NewQuantity(4, resource.DecimalSI)
	two := corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(2, resource.DecimalSI)}

	objects := []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "gpu-node-1",
				Labels: map[string]string{
					"nvidia.com/gpu.present":                      "true",
					"feature.node.kubernetes.io/pci-10de.present": "true",
					"nvidia.com/gpu.count":                        "4",
					"nvidia.com/gpu.deploy.device-plugin":         "true",
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
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dp-xyz", Namespace: "gpu-operator",
				Labels: map[string]string{"app": "nvidia-device-plugin-daemonset"},
			},
			Spec: corev1.PodSpec{
				NodeName:   "gpu-node-1",
				Containers: []corev1.Container{{Name: "plugin"}},
			},
			Status: corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "plugin", Ready: true}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "trainer", Namespace: "research"},
			Spec: corev1.PodSpec{
				NodeName: "gpu-node-1",
				Containers: []corev1.Container{{
					Name:      "trainer",
					Resources: corev1.ResourceRequirements{Requests: two, Limits: two},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: "cordoned", Namespace: "default"},
			InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: "gpu-node-1"},
			Reason:         "NodeNotSchedulable",
		},
	}

	dir := filepath.Join(t.TempDir(), "artifacts")
	opts := &mustgather.Options{
		ArtifactDir: dir,
		ToolVersion: "v9.9.9-round-trip",
		NoExec:      true,
		FetchPodFile: func(_ context.Context, pod *corev1.Pod, destPath string) error {
			return os.WriteFile(destPath, []byte(pod.Spec.NodeName), 0o600)
		},
	}
	factory := roundTripFactory{
		clientset: fake.NewSimpleClientset(objects...),
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(), roundTripListKinds()),
	}

	// The gather's own exit code is not what this test is about: a fake
	// cluster serves none of the custom resources, so collectors are skipped.
	// What matters is the bundle it leaves behind.
	_ = mustgather.Run(opts, factory, quietProgress{})

	return dir
}

// runFromBundle executes one command against a bundle directory and returns
// the exit code the process would have left with, plus what it printed.
func runFromBundle(t *testing.T, build func(*cmd.Options) *cobra.Command, dir string, args ...string) (int, string, string) {
	t.Helper()

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	streams := genericiooptions.IOStreams{In: strings.NewReader(""), Out: out, ErrOut: errOut}

	c := build(&cmd.Options{
		Streams: streams,
		Logger:  output.NewLogger(streams, output.LoggerOptions{NoColor: true}),
	})
	c.SetOut(out)
	c.SetErr(errOut)
	c.SetArgs(append([]string{"--from-bundle", dir}, args...))
	c.SilenceUsage = true
	c.SilenceErrors = true

	return output.ExitCodeFor(c.Execute()), out.String(), errOut.String()
}

// The round trip the two flags exist for: gather, then answer from what was
// gathered. Both commands exited 3 on every bundle there had ever been, so a
// caller branching on the exit code learned nothing from it.
//
// No factory is given to either command, so a read that fell through to a
// cluster would fail rather than quietly succeed against the developer's own.
func TestListAndHealthAnswerFromACompleteBundle(t *testing.T) {
	dir := gatherForRoundTrip(t)

	t.Run("list", func(t *testing.T) {
		code, out, errOut := runFromBundle(t, cmdlist.NewCommand, dir, "-o", "json")
		if code != output.ExitOK {
			t.Errorf("list --from-bundle exited %d, want %d\nstderr:\n%s", code, output.ExitOK, errOut)
		}
		if !strings.Contains(out, "\"kind\": \"GPUNodeList\"") {
			t.Errorf("list --from-bundle printed no node list:\n%s", out)
		}
	})

	t.Run("health", func(t *testing.T) {
		// --fail-on error is the default, and the fixture cluster has findings
		// on it, so the code worth asserting is "not partial": the question is
		// whether the bundle could be read, not whether the cluster is well.
		code, _, errOut := runFromBundle(t, cmdhealth.NewCommand, dir, "-o", "json")
		if code == output.ExitPartial {
			t.Errorf("health --from-bundle exited %d, which says the bundle could not be read whole\nstderr:\n%s",
				code, errOut)
		}
	})
}

// An old bundle still exits 3 from both, which is what makes the exit code
// worth reading at all.
func TestListAndHealthStillReportAnOldBundleAsPartial(t *testing.T) {
	dir := gatherForRoundTrip(t)
	for _, name := range []string{"gpu_pods.yaml", "node_events.yaml"} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatalf("removing %s: %v", name, err)
		}
	}

	for name, build := range map[string]func(*cmd.Options) *cobra.Command{
		"list":   cmdlist.NewCommand,
		"health": cmdhealth.NewCommand,
	} {
		t.Run(name, func(t *testing.T) {
			code, _, _ := runFromBundle(t, build, dir, "-o", "json")
			if code != output.ExitPartial {
				t.Errorf("%s --from-bundle on an old bundle exited %d, want %d",
					name, code, output.ExitPartial)
			}
		})
	}
}
