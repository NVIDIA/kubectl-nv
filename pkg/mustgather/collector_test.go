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
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"
)

// TestRegistryIsWellFormed covers what the rest of the gather assumes about the
// registry: a name to report a collector by, at least one artifact it plans,
// and something to run. A collector added with any of the three left out would
// produce a manifest entry that says nothing.
func TestRegistryIsWellFormed(t *testing.T) {
	seen := make(map[string]bool)
	for _, c := range Collectors() {
		if c.Name == "" {
			t.Error("a collector has no name")
			continue
		}
		if seen[c.Name] {
			t.Errorf("two collectors are named %q: the manifest keys on the name", c.Name)
		}
		seen[c.Name] = true
		if len(c.Artifacts) == 0 {
			t.Errorf("collector %q plans no artifacts", c.Name)
		}
		if c.Run == nil {
			t.Errorf("collector %q has nothing to run", c.Name)
		}
	}
	if len(seen) == 0 {
		t.Fatal("the registry is empty")
	}
}

// parityCluster is a cluster with one of everything the new collectors read:
// an operator pod to find the namespace from, a GPU node, an operand pod on a
// second node, and a daemonset.
func parityCluster() []runtime.Object {
	return []runtime.Object{
		gpuNode("gpu-node-1"),
		gpuNode("gpu-node-2"),
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gpu-operator-0",
				Namespace: "gpu-operator",
				Labels:    map[string]string{"app": "gpu-operator"},
			},
			Spec: v1.PodSpec{
				NodeName:   "gpu-node-1",
				Containers: []v1.Container{{Name: "gpu-operator", Image: "nvcr.io/nvidia/gpu-operator:v25.3.0"}},
			},
			Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.1"},
		},
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nvidia-device-plugin-daemonset-aaaaa",
				Namespace: "gpu-operator",
				Labels:    map[string]string{"app": "nvidia-device-plugin-daemonset"},
			},
			Spec: v1.PodSpec{
				NodeName:   "gpu-node-2",
				Containers: []v1.Container{{Name: "nvidia-device-plugin-ctr", Image: "nvcr.io/nvidia/k8s-device-plugin:v0.17.0"}},
			},
			Status: v1.PodStatus{Phase: v1.PodRunning, PodIP: "10.0.0.2"},
		},
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nvidia-device-plugin-daemonset",
				Namespace: "gpu-operator",
				Labels:    map[string]string{"app": "nvidia-device-plugin-daemonset"},
			},
			Spec: appsv1.DaemonSetSpec{
				Selector: &metav1.LabelSelector{
					MatchLabels: map[string]string{"app": "nvidia-device-plugin-daemonset"},
				},
			},
		},
	}
}

// TestGPUNodeTextArtifacts covers the three text files the script gets from
// "kubectl get -o wide" and "kubectl describe". They are what a support
// engineer reads first, and a bundle whose .status is empty or whose .descr is
// a Go struct dump is a bundle that has to be collected again.
func TestGPUNodeTextArtifacts(t *testing.T) {
	g, dir := newTestGatherer(t, parityCluster()...)

	runCollector(t, g, "gpu-nodes")

	labels := readArtifact(t, dir, "gpu_nodes.labels")
	for _, want := range []string{"gpu-node-1", "feature.node.kubernetes.io/pci-10de.present"} {
		if !strings.Contains(labels, want) {
			t.Errorf("gpu_nodes.labels does not name %q:\n%s", want, labels)
		}
	}

	status := readArtifact(t, dir, "gpu_nodes.status")
	if !strings.HasPrefix(status, "NAME") {
		t.Errorf("gpu_nodes.status does not start with the kubectl header row:\n%s", status)
	}
	for _, want := range []string{"STATUS", "VERSION", "INTERNAL-IP", "gpu-node-1"} {
		if !strings.Contains(status, want) {
			t.Errorf("gpu_nodes.status is missing %q:\n%s", want, status)
		}
	}

	descr := readArtifact(t, dir, "gpu_nodes.descr")
	for _, want := range []string{"Name:", "gpu-node-1", "Labels:", "Capacity:"} {
		if !strings.Contains(descr, want) {
			t.Errorf("gpu_nodes.descr is missing %q:\n%s", want, descr)
		}
	}
}

// TestOperandPodTextArtifacts covers the operand side of the same three forms.
func TestOperandPodTextArtifacts(t *testing.T) {
	g, dir := newTestGatherer(t, parityCluster()...)

	runCollector(t, g, "operator-pod")
	runCollector(t, g, "operand-pods")
	runCollector(t, g, "pod-descriptions")

	status := readArtifact(t, dir, "gpu_operand_pods.status")
	for _, want := range []string{"NAME", "READY", "STATUS", "NODE", "nvidia-device-plugin-daemonset-aaaaa"} {
		if !strings.Contains(status, want) {
			t.Errorf("gpu_operand_pods.status is missing %q:\n%s", want, status)
		}
	}

	operator := readArtifact(t, dir, "gpu_operator_pod.status")
	if !strings.Contains(operator, "gpu-operator-0") {
		t.Errorf("gpu_operator_pod.status does not name the operator pod:\n%s", operator)
	}

	// The image list is what tells a support engineer which release they are
	// looking at, so the tag has to survive into the file.
	images := readArtifact(t, dir, "gpu_operand_pod_images.txt")
	for _, want := range []string{
		"nvidia-device-plugin-daemonset-aaaaa",
		"nvcr.io/nvidia/k8s-device-plugin:v0.17.0",
	} {
		if !strings.Contains(images, want) {
			t.Errorf("gpu_operand_pod_images.txt is missing %q:\n%s", want, images)
		}
	}

	descr := readArtifact(t, dir, "nvidia-device-plugin-daemonset-aaaaa.descr")
	for _, want := range []string{"Name:", "Node:", "Containers:"} {
		if !strings.Contains(descr, want) {
			t.Errorf("the pod description is missing %q:\n%s", want, descr)
		}
	}
}

// TestDaemonSetTextArtifacts covers the daemonset status table and the
// per-daemonset description.
func TestDaemonSetTextArtifacts(t *testing.T) {
	g, dir := newTestGatherer(t, parityCluster()...)

	runCollector(t, g, "daemonsets")

	status := readArtifact(t, dir, "gpu_operand_ds.status")
	for _, want := range []string{"NAME", "DESIRED", "READY", "NODE SELECTOR", "nvidia-device-plugin-daemonset"} {
		if !strings.Contains(status, want) {
			t.Errorf("gpu_operand_ds.status is missing %q:\n%s", want, status)
		}
	}

	descr := readArtifact(t, dir, "gpu_operand_ds_nvidia-device-plugin-daemonset.descr")
	if !strings.Contains(descr, "Name:") || !strings.Contains(descr, "nvidia-device-plugin-daemonset") {
		t.Errorf("the daemonset description is not a describe output:\n%s", descr)
	}
}

// TestClusterScopedCustomResources covers the resources the script reads
// through CRDs that may or may not be installed: the GPUCluster, the
// HyperConverged and KubeVirt pair, and the DRA device classes. Each is
// written when it is there, and the gather carries on when it is not.
func TestClusterScopedCustomResources(t *testing.T) {
	objects := []runtime.Object{
		unstructuredOf("nvidia.com/v1alpha1", "GPUCluster", "", "gpu-cluster"),
		unstructuredOf("hco.kubevirt.io/v1beta1", "HyperConverged", "kubevirt-hyperconverged", "kubevirt-hyperconverged"),
		unstructuredOf("kubevirt.io/v1", "KubeVirt", "kubevirt", "kubevirt"),
		unstructuredOf("resource.k8s.io/v1", "DeviceClass", "", "gpu.nvidia.com"),
	}

	cases := []struct {
		collector string
		file      string
		wantKind  string
	}{
		{"gpu-cluster", "gpu_cluster.yaml", "GPUClusterList"},
		{"hyperconverged", "hyperconverged.yaml", "HyperConvergedList"},
		{"kubevirt", "kubevirt.yaml", "KubeVirtList"},
		{"device-classes", "deviceclasses.yaml", "DeviceClassList"},
	}

	for _, c := range cases {
		t.Run(c.collector, func(t *testing.T) {
			g, dir := newCustomResourceGatherer(t, objects...)
			runCollector(t, g, c.collector)

			data := readArtifact(t, dir, c.file)
			var head struct {
				Kind  string `json:"kind"`
				Items []struct {
					Kind string `json:"kind"`
				} `json:"items"`
			}
			if err := yaml.Unmarshal([]byte(data), &head); err != nil {
				t.Fatalf("%s does not parse: %v\n%s", c.file, err, data)
			}
			if head.Kind != c.wantKind {
				t.Errorf("%s kind = %q, want %q", c.file, head.Kind, c.wantKind)
			}
			if len(head.Items) != 1 {
				t.Fatalf("%s holds %d items, want 1:\n%s", c.file, len(head.Items), data)
			}
		})
	}
}

// TestAbsentCustomResourcesAreNotAFailure keeps a cluster without KubeVirt, a
// GPUCluster or DRA from failing the gather: the script logs and carries on,
// and so must we.
func TestAbsentCustomResourcesAreNotAFailure(t *testing.T) {
	for _, name := range []string{"gpu-cluster", "hyperconverged", "kubevirt", "device-classes"} {
		t.Run(name, func(t *testing.T) {
			g, dir := newCustomResourceGatherer(t)
			runCollector(t, g, name)

			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range entries {
				if strings.HasSuffix(e.Name(), ".yaml") {
					t.Errorf("%s wrote %s for a cluster that has none", name, e.Name())
				}
			}
		})
	}
}

// TestMissingMarkerSaysWhy is the open item the handoff left: a zero-length
// cluster_policy.missing tells a reader that the file is not there and nothing
// else. Not installed and not allowed to look are different problems with
// different owners.
func TestMissingMarkerSaysWhy(t *testing.T) {
	g, dir := newTestGatherer(t)

	runCollector(t, g, "cluster-policy")

	marker := readArtifact(t, dir, "cluster_policy.missing")
	if !strings.Contains(marker, "not installed") {
		t.Errorf("cluster_policy.missing = %q, want it to say the ClusterPolicy is not installed", marker)
	}
}

// runCollector runs one collector of the registry by name, through the same
// state the gather builds, and fails the test when it is not there.
func runCollector(t *testing.T, g *Gatherer, name string) {
	t.Helper()

	s := &gatherState{operatorNamespace: "gpu-operator"}
	for _, c := range Collectors() {
		if c.Name != name {
			continue
		}
		// The collectors that read the operator namespace's pods need the pod
		// lists the earlier collectors put in the state.
		if err := g.prepareState(s); err != nil {
			t.Fatalf("preparing the gather state: %v", err)
		}
		if err := c.Run(g, s); err != nil {
			t.Fatalf("collector %q: %v", name, err)
		}
		return
	}
	t.Fatalf("no collector named %q in the registry: %v", name, collectorNames())
}

// collectorNames lists the registry for a failure message.
func collectorNames() []string {
	names := make([]string, 0)
	for _, c := range Collectors() {
		names = append(names, c.Name)
	}
	return names
}

// readArtifact returns one file of the bundle as text.
func readArtifact(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		entries, _ := os.ReadDir(dir)
		held := make([]string, 0, len(entries))
		for _, e := range entries {
			held = append(held, e.Name())
		}
		t.Fatalf("reading %s: %v\nbundle holds: %v", name, err, held)
	}
	return string(data)
}

// unstructuredOf builds one custom resource for the dynamic fake.
func unstructuredOf(apiVersion, kind, namespace, name string) *unstructured.Unstructured {
	meta := map[string]any{"name": name}
	if namespace != "" {
		meta["namespace"] = namespace
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata":   meta,
		"spec":       map[string]any{},
	}}
}

// newCustomResourceGatherer builds a gatherer whose dynamic client knows the
// list kinds of every custom resource the gather reads, so that a List against
// it behaves the way it does against a cluster that has the CRD.
func newCustomResourceGatherer(t *testing.T, objects ...runtime.Object) (*Gatherer, string) {
	t.Helper()

	listKinds := customResourceListKinds()

	dir := filepath.Join(t.TempDir(), "artifacts")
	g, err := NewGatherer(&Options{ArtifactDir: dir}, stubFactory{
		clientset: fake.NewSimpleClientset(),
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(), listKinds, objects...),
		config: offlineConfig(),
	}, discardProgress{})
	if err != nil {
		t.Fatalf("NewGatherer: %v", err)
	}
	t.Cleanup(g.Close)

	return g, dir
}
