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

package mustgather

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// gpuNode is a node as the GPU operator leaves it: an extended resource whose
// quantity is the thing support counts.
func gpuNode(name string) *v1.Node {
	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{"feature.node.kubernetes.io/pci-10de.present": "true"},
		},
		Status: v1.NodeStatus{
			Capacity: v1.ResourceList{
				"nvidia.com/gpu": resource.MustParse("8"),
				v1.ResourceCPU:   resource.MustParse("128"),
			},
			Allocatable: v1.ResourceList{
				"nvidia.com/gpu": resource.MustParse("8"),
			},
		},
	}
}

// offlineConfig answers every request in process with a 404, so a gatherer
// under test never reaches a network.
func offlineConfig() *rest.Config {
	return &rest.Config{
		Host: "https://kubectl-nv.invalid",
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			http.NotFound(w, req)
			resp := w.Result()
			resp.Request = req
			return resp, nil
		}),
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// newTestGatherer builds a gatherer over the given cluster objects, writing to
// a temporary artifact directory.
func newTestGatherer(t *testing.T, objects ...runtime.Object) (*Gatherer, string) {
	t.Helper()
	// A path the gatherer has to create itself: a directory t.TempDir already
	// made would carry its mode, not the one under test.
	dir := filepath.Join(t.TempDir(), "artifacts")
	opts := &Options{ArtifactDir: dir}
	g, err := NewGatherer(opts, stubFactory{
		clientset: fake.NewSimpleClientset(objects...),
		dynamic:   dynamicfake.NewSimpleDynamicClient(runtime.NewScheme()),
		config:    offlineConfig(),
	}, discardProgress{})
	if err != nil {
		t.Fatalf("NewGatherer: %v", err)
	}
	t.Cleanup(g.Close)
	return g, dir
}

// TestNodesYAMLIsKubernetesNative is the golden check on the format. A bundle
// is read with kubectl and with scripts that expect kubectl's output, so
// "nvidia.com/gpu: 8" has to survive the round trip and the document has to
// name its own apiVersion and kind.
func TestNodesYAMLIsKubernetesNative(t *testing.T) {
	g, dir := newTestGatherer(t, gpuNode("gpu-node-1"))

	if err := g.gatherNodeInfo(); err != nil {
		t.Fatalf("gatherNodeInfo: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "nodes.yaml"))
	if err != nil {
		t.Fatalf("reading nodes.yaml: %v", err)
	}

	var list v1.NodeList
	if err := yaml.Unmarshal(data, &list); err != nil {
		t.Fatalf("nodes.yaml does not parse as a NodeList: %v\n%s", err, data)
	}
	if list.APIVersion != "v1" || list.Kind != "NodeList" {
		t.Errorf("apiVersion/kind = %q/%q, want v1/NodeList", list.APIVersion, list.Kind)
	}
	if len(list.Items) != 1 {
		t.Fatalf("got %d nodes, want 1\n%s", len(list.Items), data)
	}

	node := list.Items[0]
	if node.APIVersion != "v1" || node.Kind != "Node" {
		t.Errorf("item apiVersion/kind = %q/%q, want v1/Node", node.APIVersion, node.Kind)
	}
	gpus := node.Status.Capacity["nvidia.com/gpu"]
	if gpus.String() != "8" {
		t.Errorf("nvidia.com/gpu capacity = %q, want %q", gpus.String(), "8")
	}
	if !strings.Contains(string(data), "nvidia.com/gpu: \"8\"") {
		t.Errorf("nodes.yaml does not carry the quantity as kubectl writes it:\n%s", data)
	}
}

// TestArtifactsAreNotWorldReadable keeps a bundle of cluster data out of
// everyone else's reach: it lands in /tmp, where every account on the machine
// can look.
func TestArtifactsAreNotWorldReadable(t *testing.T) {
	g, dir := newTestGatherer(t, gpuNode("gpu-node-1"))

	if err := g.gatherNodeInfo(); err != nil {
		t.Fatalf("gatherNodeInfo: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Errorf("artifact directory mode = %04o, want 0700", got)
	}

	for _, name := range []string{"nodes.yaml", "must-gather.log", "must-gather.stderr.log"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
	}
}

// TestEveryWriterIsKubernetesNative walks the collectors that write YAML and
// reads each file back the way a user would. One writer left on a Go-struct
// marshaller is enough to make a bundle that kubectl cannot read, and the only
// way to catch it is to parse every file.
func TestEveryWriterIsKubernetesNative(t *testing.T) {
	cases := []struct {
		name       string
		file       string
		wantKind   string
		wantInKind string
		collect    func(t *testing.T, g *Gatherer)
		contains   []string
	}{
		{
			name:       "gpu nodes",
			file:       "gpu_nodes.yaml",
			wantKind:   "NodeList",
			wantInKind: "Node",
			collect:    func(t *testing.T, g *Gatherer) { mustCollect(t, g.gatherGPUNodes()) },
			contains:   []string{"nvidia.com/gpu: \"8\"", "apiVersion: v1"},
		},
		{
			name:       "operand and operator pods",
			file:       "gpu_operand_pods.yaml",
			wantKind:   "PodList",
			wantInKind: "Pod",
			collect: func(t *testing.T, g *Gatherer) {
				if _, err := g.gatherPodInfo("gpu-operator"); err != nil {
					t.Fatalf("gatherPodInfo: %v", err)
				}
			},
			contains: []string{"memory: 2Gi", "apiVersion: v1"},
		},
		{
			name:       "daemonsets",
			file:       "daemonsets.yaml",
			wantKind:   "DaemonSetList",
			wantInKind: "DaemonSet",
			collect:    func(t *testing.T, g *Gatherer) { mustCollect(t, g.gatherDaemonSets("gpu-operator")) },
			contains:   []string{"apiVersion: apps/v1", "maxUnavailable: 1"},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, dir := newTestGatherer(t, nativeYAMLCluster()...)
			c.collect(t, g)

			data, err := os.ReadFile(filepath.Join(dir, c.file))
			if err != nil {
				t.Fatalf("reading %s: %v", c.file, err)
			}

			var head struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
				Items      []struct {
					APIVersion string `json:"apiVersion"`
					Kind       string `json:"kind"`
				} `json:"items"`
			}
			if err := yaml.Unmarshal(data, &head); err != nil {
				t.Fatalf("%s does not parse: %v\n%s", c.file, err, data)
			}
			if head.APIVersion == "" || head.Kind != c.wantKind {
				t.Errorf("%s apiVersion/kind = %q/%q, want a version and %s",
					c.file, head.APIVersion, head.Kind, c.wantKind)
			}
			if len(head.Items) == 0 {
				t.Fatalf("%s has no items:\n%s", c.file, data)
			}
			for i, item := range head.Items {
				if item.APIVersion == "" || item.Kind != c.wantInKind {
					t.Errorf("%s item %d apiVersion/kind = %q/%q, want a version and %s",
						c.file, i, item.APIVersion, item.Kind, c.wantInKind)
				}
			}
			for _, want := range c.contains {
				if !strings.Contains(string(data), want) {
					t.Errorf("%s does not contain %q:\n%s", c.file, want, data)
				}
			}
		})
	}
}

func mustCollect(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("collector failed: %v", err)
	}
}

// nativeYAMLCluster is a cluster with something typed in every object the
// writers under test touch: a quantity on the node, a resource request on the
// pods, and an update strategy on the daemonset. A Go-struct marshaller
// mangles all three in a different way.
func nativeYAMLCluster() []runtime.Object {
	operatorPod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gpu-operator-0",
			Namespace: "gpu-operator",
			Labels:    map[string]string{"app": "gpu-operator"},
		},
		Spec: v1.PodSpec{
			NodeName: "gpu-node-1",
			Containers: []v1.Container{{
				Name: "gpu-operator",
				Resources: v1.ResourceRequirements{
					Requests: v1.ResourceList{v1.ResourceMemory: resource.MustParse("2Gi")},
				},
			}},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}

	maxUnavailable := intstr.FromInt32(1)
	driverSet := &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-driver-daemonset", Namespace: "gpu-operator"},
		Spec: appsv1.DaemonSetSpec{
			UpdateStrategy: appsv1.DaemonSetUpdateStrategy{
				Type:          appsv1.RollingUpdateDaemonSetStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDaemonSet{MaxUnavailable: &maxUnavailable},
			},
		},
	}

	return []runtime.Object{gpuNode("gpu-node-1"), operatorPod, driverSet}
}

// TestProgressNeverReachesStdout pins the stream the running commentary goes
// to. Stdout belongs to the command's own output, and a plugin that prints
// progress there breaks the first pipeline someone writes around it. The real
// os.Stdout is captured, not just the injected streams, so a writer that
// reaches past them is caught too.
func TestProgressNeverReachesStdout(t *testing.T) {
	realStdout := os.Stdout
	captured := filepath.Join(t.TempDir(), "stdout")
	file, err := os.Create(captured)
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = file
	t.Cleanup(func() { os.Stdout = realStdout; file.Close() })

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	logger := output.NewLogger(
		genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: stdout, ErrOut: stderr},
		output.LoggerOptions{})

	dir := filepath.Join(t.TempDir(), "artifacts")
	g, err := NewGatherer(&Options{ArtifactDir: dir}, stubFactory{
		clientset: fake.NewSimpleClientset(gpuNode("gpu-node-1")),
		config:    offlineConfig(),
	}, logger)
	if err != nil {
		t.Fatalf("NewGatherer: %v", err)
	}
	defer g.Close()

	g.step("Gathering node information")

	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	onStdout, err := os.ReadFile(captured)
	if err != nil {
		t.Fatal(err)
	}
	if len(onStdout) != 0 || stdout.Len() != 0 {
		t.Errorf("progress reached stdout: os.Stdout=%q streams.Out=%q", onStdout, stdout.String())
	}
	if !strings.Contains(stderr.String(), "Gathering node information") {
		t.Errorf("stderr = %q, want the step line", stderr.String())
	}
}

// TestMarkerFilesAreNotWorldReadable covers the files that record what the
// cluster does not have. They are written whole rather than opened, so they
// take a different path to disk and can keep a looser mode than the rest.
func TestMarkerFilesAreNotWorldReadable(t *testing.T) {
	g, dir := newTestGatherer(t)

	// No ClusterPolicy and no NvidiaDriver in the dynamic client, so both
	// marker files are written.
	if err := g.gatherGPUOperatorCRDs(); err != nil {
		t.Fatalf("gatherGPUOperatorCRDs: %v", err)
	}

	for _, name := range []string{"cluster_policy.missing", "nvidia_driver.missing"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("expected %s to have been written: %v", name, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", name, got)
		}
	}
}

// TestDebugNamesEveryArtifact is what --debug buys on a must-gather: the
// bundle listed as it is built, so that a missing file can be told from an
// empty one.
func TestDebugNamesEveryArtifact(t *testing.T) {
	for _, debug := range []bool{false, true} {
		name := "quiet"
		if debug {
			name = "debug"
		}
		t.Run(name, func(t *testing.T) {
			stderr := &bytes.Buffer{}
			logger := output.NewLogger(
				genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: &bytes.Buffer{}, ErrOut: stderr},
				output.LoggerOptions{})

			g, err := NewGatherer(&Options{ArtifactDir: filepath.Join(t.TempDir(), "artifacts"), Debug: debug},
				stubFactory{
					clientset: fake.NewSimpleClientset(gpuNode("gpu-node-1")),
					config:    offlineConfig(),
				}, logger)
			if err != nil {
				t.Fatalf("NewGatherer: %v", err)
			}
			defer g.Close()

			if err := g.gatherNodeInfo(); err != nil {
				t.Fatalf("gatherNodeInfo: %v", err)
			}

			named := strings.Contains(stderr.String(), "Wrote nodes.yaml")
			if named != debug {
				t.Errorf("artifact named on stderr = %v, want %v\n%s", named, debug, stderr.String())
			}
		})
	}
}

// TestClusterPolicyIsWrittenAsItWasRead covers the custom resources. They
// arrive unstructured, so the file has to carry the apiVersion and kind the
// cluster gave them and the spec has to survive unchanged: support compares
// this file against the ClusterPolicy a customer says they applied.
func TestClusterPolicyIsWrittenAsItWasRead(t *testing.T) {
	policy := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "nvidia.com/v1",
		"kind":       "ClusterPolicy",
		"metadata":   map[string]any{"name": "cluster-policy"},
		"spec": map[string]any{
			"operator": map[string]any{"defaultRuntime": "containerd"},
			"driver": map[string]any{
				"version":  "550.90.07",
				"replicas": int64(3),
				"enabled":  true,
			},
		},
	}}

	dir := filepath.Join(t.TempDir(), "artifacts")
	g, err := NewGatherer(&Options{ArtifactDir: dir}, stubFactory{
		clientset: fake.NewSimpleClientset(),
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(),
			map[schema.GroupVersionResource]string{
				{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}:     "ClusterPolicyList",
				{Group: "nvidia.com", Version: "v1alpha1", Resource: "nvidiadrivers"}: "NvidiaDriverList",
			},
			policy),
		config: offlineConfig(),
	}, discardProgress{})
	if err != nil {
		t.Fatalf("NewGatherer: %v", err)
	}
	defer g.Close()

	if err := g.gatherGPUOperatorCRDs(); err != nil {
		t.Fatalf("gatherGPUOperatorCRDs: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "cluster_policy.yaml"))
	if err != nil {
		t.Fatalf("reading cluster_policy.yaml: %v", err)
	}

	var got unstructured.Unstructured
	if err := yaml.Unmarshal(data, &got.Object); err != nil {
		t.Fatalf("cluster_policy.yaml does not parse: %v\n%s", err, data)
	}
	if got.GetAPIVersion() != "nvidia.com/v1" || got.GetKind() != "ClusterPolicy" {
		t.Errorf("apiVersion/kind = %q/%q, want nvidia.com/v1 ClusterPolicy",
			got.GetAPIVersion(), got.GetKind())
	}
	version, found, err := unstructured.NestedString(got.Object, "spec", "driver", "version")
	if err != nil || !found {
		t.Fatalf("spec.driver.version is missing: found=%v err=%v\n%s", found, err, data)
	}
	if version != "550.90.07" {
		t.Errorf("spec.driver.version = %q, want %q", version, "550.90.07")
	}
	// The number is checked as written rather than as re-parsed: reading YAML
	// back into a map gives float64 for every number, which says nothing about
	// the file.
	for _, want := range []string{"replicas: 3", "version: 550.90.07", "enabled: true"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("cluster_policy.yaml does not contain %q:\n%s", want, data)
		}
	}
}
