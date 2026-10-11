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
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
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

	mustRunCollector(t, g, "gpu-nodes")

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

	mustRunCollector(t, g, "operator-pod")
	mustRunCollector(t, g, "operand-pods")
	mustRunCollector(t, g, "pod-descriptions")

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

	mustRunCollector(t, g, "daemonsets")

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
			mustRunCollector(t, g, c.collector)

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
// and so must we. The collector is recorded as skipped with the reason, which
// is how a reader of the bundle tells "the cluster has no KubeVirt" from
// "nobody looked".
func TestAbsentCustomResourcesAreNotAFailure(t *testing.T) {
	cases := map[string]string{
		"gpu-cluster":    "GPUCluster resource(s) not found in the cluster",
		"hyperconverged": "HyperConverged resource(s) not found in the cluster",
		"kubevirt":       "KubeVirt resource(s) not found in the cluster",
		"device-classes": "DeviceClass resource(s) not found in the cluster",
	}

	for name, wantReason := range cases {
		t.Run(name, func(t *testing.T) {
			g, dir := newCustomResourceGatherer(t)

			var skipped errSkipped
			err := runCollector(t, g, name)
			if !errors.As(err, &skipped) {
				t.Fatalf("collector %s returned %v, want it recorded as skipped", name, err)
			}
			if skipped.reason != wantReason {
				t.Errorf("skip reason = %q, want %q", skipped.reason, wantReason)
			}

			entries, rerr := os.ReadDir(dir)
			if rerr != nil {
				t.Fatal(rerr)
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

	mustRunCollector(t, g, "cluster-policy")

	marker := readArtifact(t, dir, "cluster_policy.missing")
	if !strings.Contains(marker, "not installed") {
		t.Errorf("cluster_policy.missing = %q, want it to say the ClusterPolicy is not installed", marker)
	}
}

// runCollector runs one collector of the registry by name, through the same
// state the gather builds, and returns what it reported. A collector that is
// not in the registry fails the test: a name changed without this being
// updated would otherwise leave the assertions below running over nothing.
func runCollector(t *testing.T, g *Gatherer, name string) error {
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
		return c.Run(g, s)
	}
	t.Fatalf("no collector named %q in the registry: %v", name, collectorNames())
	return nil
}

// mustRunCollector runs a collector that is expected to produce its artifacts.
func mustRunCollector(t *testing.T, g *Gatherer, name string) {
	t.Helper()
	if err := runCollector(t, g, name); err != nil {
		t.Fatalf("collector %q: %v", name, err)
	}
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

// TestCustomResourcesRunBeforeGPUNodes pins the order the base and the
// gpu-operator script both use: the cluster's own configuration first, the
// node survey after. The registry had moved the GPU node collector in front
// of it, so a cluster with no PCI-labelled node lost its ClusterPolicy too,
// which is the first file support asks for.
func TestCustomResourcesRunBeforeGPUNodes(t *testing.T) {
	order := make(map[string]int)
	for i, c := range Collectors() {
		order[c.Name] = i
	}

	gpuNodes, ok := order["gpu-nodes"]
	if !ok {
		t.Fatal("the registry has no gpu-nodes collector")
	}
	for _, name := range []string{
		"cluster-policy", "nvidia-drivers", "gpu-cluster",
		"hyperconverged", "kubevirt", "device-classes", "resource-slices",
	} {
		at, listed := order[name]
		if !listed {
			t.Errorf("the registry has no %s collector", name)
			continue
		}
		if at > gpuNodes {
			t.Errorf("%s runs at %d, after gpu-nodes at %d: the cluster's own resources come first",
				name, at, gpuNodes)
		}
	}
}

// TestNoGPUNodesStillCollectsTheClusterPolicy is the same regression from the
// outside. A cluster whose feature discovery has not labelled anything yet is
// exactly the cluster someone runs a must-gather on, and the bundle has to
// carry the operator's configuration so support can see why.
func TestNoGPUNodesStillCollectsTheClusterPolicy(t *testing.T) {
	plain := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cpu-node-1"}}
	operator := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gpu-operator-0",
			Namespace: "gpu-operator",
			Labels:    map[string]string{"app": "gpu-operator"},
		},
		Spec:   v1.PodSpec{NodeName: "cpu-node-1", Containers: []v1.Container{{Name: "gpu-operator"}}},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}

	dir, names, err := runGatherWithCustomResources(t, &Options{},
		[]runtime.Object{plain, operator},
		unstructuredOf("nvidia.com/v1", "ClusterPolicy", "", "cluster-policy"))
	if err != nil {
		t.Fatalf("Run returned %v, want the gather to carry on past a cluster with no GPU node", err)
	}

	if !contains(names, "cluster_policy.yaml") {
		t.Errorf("the bundle holds no cluster_policy.yaml: %v", names)
	}

	m := readManifest(t, dir)
	if got := m.statusOf(t, "gpu-nodes"); got != "skipped:no GPU nodes" {
		t.Errorf("gpu-nodes status = %q, want %q", got, "skipped:no GPU nodes")
	}
	if got := m.statusOf(t, "cluster-policy"); got != StatusOK {
		t.Errorf("cluster-policy status = %q, want %q", got, StatusOK)
	}
}

// TestOneCollectorFailureDoesNotStopTheRest covers what a bundle is for. The
// gather stopped at the first failing collector, so an API server that refused
// one list cost the user every artifact behind it: a support engineer got a
// bundle that stops mid-cluster with no record of what was left out.
func TestOneCollectorFailureDoesNotStopTheRest(t *testing.T) {
	clientset := fake.NewSimpleClientset(twoNodeCluster()...)
	clientset.PrependReactor("list", "daemonsets", failWith(errors.New("the API server is having a bad day")))

	dir, names, err := runGatherWith(t, &Options{}, clientset)
	if err == nil {
		t.Fatal("Run returned nil, want the daemonset failure reported")
	}
	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitPartial, err)
	}

	// The daemonsets collector runs before the exec collectors, so a gather
	// that stopped at it would ship no bug report at all.
	if !contains(names, "nvidia-bug-report_gpu-node-1.log.gz") {
		t.Errorf("the gather stopped at the failing collector: %v", names)
	}

	m := readManifest(t, dir)
	if status := m.statusOf(t, "daemonsets"); !strings.HasPrefix(status, "error:") {
		t.Errorf("daemonsets status = %q, want it to start with error:", status)
	}
	if status := m.statusOf(t, "bug-report"); status != StatusOK {
		t.Errorf("bug-report status = %q, want it to have run anyway", status)
	}

	// Every collector of the registry is accounted for, including the ones
	// after the failure.
	for _, c := range Collectors() {
		if got := m.statusOf(t, c.Name); got == "" {
			t.Errorf("collector %q has no manifest entry", c.Name)
		}
	}
}

// TestLogErrorWrapsItsCause keeps the error chain intact. logError built its
// message with fmt.Errorf(format+": %w", err) over a format that already
// carried a %v, so the verb count never matched: every error it produced ended
// in "%!w(MISSING)" and errors.Is could not reach the cause.
func TestLogErrorWrapsItsCause(t *testing.T) {
	g, dir := newTestGatherer(t)

	cause := errors.New("the API server is having a bad day")
	got := g.logError("error getting daemonSets: %v", cause)

	if !errors.Is(got, cause) {
		t.Errorf("errors.Is could not reach the cause through %v", got)
	}
	if strings.Contains(got.Error(), "%!") {
		t.Errorf("the error carries a formatting fault: %q", got.Error())
	}
	if !strings.Contains(got.Error(), cause.Error()) {
		t.Errorf("error = %q, want it to carry the cause", got.Error())
	}
	if errLog := readArtifact(t, dir, "must-gather.stderr.log"); !strings.Contains(errLog, cause.Error()) {
		t.Errorf("the error log does not record the cause:\n%s", errLog)
	}
}

// TestDescribeNotFoundIsAPerObjectNote covers a pod that is deleted between
// the listing and its description. That is one line of the bundle missing, not
// a reason to call the whole collector failed.
func TestDescribeNotFoundIsAPerObjectNote(t *testing.T) {
	clientset := fake.NewSimpleClientset(twoNodeCluster()...)
	clientset.PrependReactor("get", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		get, ok := action.(k8stesting.GetAction)
		if !ok || get.GetName() != "plugin-2" {
			return false, nil, nil
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "plugin-2")
	})

	dir, _, err := runGatherWith(t, &Options{}, clientset)
	if err != nil {
		t.Fatalf("Run returned %v, want a pod that went away not to fail its collector", err)
	}

	m := readManifest(t, dir)
	if got := m.statusOf(t, "pod-descriptions"); got != StatusOK {
		t.Errorf("pod-descriptions status = %q, want %q", got, StatusOK)
	}
	if errLog := readArtifact(t, dir, "must-gather.stderr.log"); !strings.Contains(errLog, "plugin-2") {
		t.Errorf("the error log does not note the pod that went away:\n%s", errLog)
	}
}

// TestNVIDIADriversAcrossNamespaces covers the "-A" the script uses. A cluster
// can run an NVIDIADriver per namespace, and a collector that got only one of
// them writes a file that looks complete and is not.
func TestNVIDIADriversAcrossNamespaces(t *testing.T) {
	g, dir := newCustomResourceGatherer(t,
		unstructuredOf("nvidia.com/v1alpha1", "NVIDIADriver", "gpu-operator", "driver-a"),
		unstructuredOf("nvidia.com/v1alpha1", "NVIDIADriver", "nvidia-gpu-operator", "driver-b"))

	mustRunCollector(t, g, "nvidia-drivers")

	data := readArtifact(t, dir, "nvidia_driver.yaml")
	var list struct {
		Items []struct {
			Metadata struct {
				Name      string `json:"name"`
				Namespace string `json:"namespace"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := yaml.Unmarshal([]byte(data), &list); err != nil {
		t.Fatalf("nvidia_driver.yaml does not parse: %v\n%s", err, data)
	}

	got := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		got = append(got, item.Metadata.Namespace+"/"+item.Metadata.Name)
	}
	sort.Strings(got)
	want := "gpu-operator/driver-a,nvidia-gpu-operator/driver-b"
	if strings.Join(got, ",") != want {
		t.Errorf("nvidia_driver.yaml lists %v, want both namespaces (%s)", got, want)
	}
}

// TestForbiddenCustomResourceMarkerSaysForbidden is the difference between
// "the operator is not installed" and "you may not read its configuration".
// They have different owners, and a marker that says the wrong one sends a
// support engineer to the wrong person.
func TestForbiddenCustomResourceMarkerSaysForbidden(t *testing.T) {
	cases := map[string]struct {
		resource string
		marker   string
	}{
		"cluster policy": {resource: "clusterpolicies", marker: "cluster_policy.missing"},
		"nvidia driver":  {resource: "nvidiadrivers", marker: "nvidia_driver.missing"},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			g, dir := newCustomResourceGatherer(t)
			g.dynClient.(*dynamicfake.FakeDynamicClient).PrependReactor("list", c.resource,
				failWith(apierrors.NewForbidden(
					schema.GroupResource{Group: "nvidia.com", Resource: c.resource}, "", errors.New("nope"))))

			collector := "cluster-policy"
			if c.resource == "nvidiadrivers" {
				collector = "nvidia-drivers"
			}
			mustRunCollector(t, g, collector)

			if got := strings.TrimSpace(readArtifact(t, dir, c.marker)); got != "forbidden" {
				t.Errorf("%s = %q, want exactly %q", c.marker, got, "forbidden")
			}
		})
	}
}

// TestAllDriverPodsUnreachableIsSkipped covers a cluster whose driver pods are
// all there and none of them can be asked: every one is pending or not running.
// Reporting that as a clean run with nothing to show reads as "this cluster has
// no driver", which is a different cluster.
func TestAllDriverPodsUnreachableIsSkipped(t *testing.T) {
	pending := driverPodWith("driver-a", "", map[string]string{"app": "nvidia-driver-daemonset"})
	pending.Status.Phase = v1.PodPending
	failed := driverPodWith("driver-b", "node-b", map[string]string{"app": "nvidia-driver-daemonset"})
	failed.Status.Phase = v1.PodFailed

	g, _ := newTestGatherer(t, pending, failed)

	var skipped errSkipped
	err := g.gatherBugReportFromDaemonSet(&gatherState{operatorNamespace: "gpu-operator", prepared: true})
	if !errors.As(err, &skipped) {
		t.Fatalf("the bug report collector returned %v, want it recorded as skipped", err)
	}
	if skipped.reason != "no driver pod could be reached" {
		t.Errorf("skip reason = %q, want %q", skipped.reason, "no driver pod could be reached")
	}
}

// runGatherWithCustomResources runs a whole gather over a cluster that also
// holds custom resources, which the plain helper's dynamic client has none of.
func runGatherWithCustomResources(t *testing.T, opts *Options,
	objects []runtime.Object, customResources ...runtime.Object) (string, []string, error) {
	t.Helper()

	opts.ArtifactDir = filepath.Join(t.TempDir(), "artifacts")
	if opts.FetchPodFile == nil {
		opts.FetchPodFile = func(_ context.Context, pod *v1.Pod, destPath string) error {
			return os.WriteFile(destPath, []byte(pod.Spec.NodeName), 0o600)
		}
	}

	err := Run(opts, stubFactory{
		clientset: fake.NewSimpleClientset(objects...),
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(), customResourceListKinds(), customResources...),
		config: offlineConfig(),
	}, discardProgress{})

	entries, rerr := os.ReadDir(opts.ArtifactDir)
	if rerr != nil {
		t.Fatalf("reading the bundle: %v", rerr)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)

	return opts.ArtifactDir, names, err
}
