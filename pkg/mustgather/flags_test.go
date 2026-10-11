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
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// twoNodeCluster is a cluster with an operand pod on each of two GPU nodes and
// a driver pod on each, which is what --node has to narrow.
func twoNodeCluster() []runtime.Object {
	return []runtime.Object{
		gpuNode("gpu-node-1"),
		gpuNode("gpu-node-2"),
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "gpu-operator-0",
				Namespace: "gpu-operator",
				Labels:    map[string]string{"app": "gpu-operator"},
			},
			Spec:   v1.PodSpec{NodeName: "gpu-node-1", Containers: []v1.Container{{Name: "gpu-operator"}}},
			Status: v1.PodStatus{Phase: v1.PodRunning},
		},
		driverPodWith("driver-1", "gpu-node-1", map[string]string{"app": "nvidia-driver-daemonset"}),
		driverPodWith("driver-2", "gpu-node-2", map[string]string{"app": "nvidia-driver-daemonset"}),
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "plugin-1", Namespace: "gpu-operator"},
			Spec:       v1.PodSpec{NodeName: "gpu-node-1", Containers: []v1.Container{{Name: "ctr"}}},
			Status:     v1.PodStatus{Phase: v1.PodRunning},
		},
		&v1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "plugin-2", Namespace: "gpu-operator"},
			Spec:       v1.PodSpec{NodeName: "gpu-node-2", Containers: []v1.Container{{Name: "ctr"}}},
			Status:     v1.PodStatus{Phase: v1.PodRunning},
		},
	}
}

// runGather runs a whole gather over the given cluster with the given options,
// returning the bundle directory, the names it holds, and the error.
func runGather(t *testing.T, opts *Options, objects ...runtime.Object) (string, []string, error) {
	t.Helper()
	return runGatherWith(t, opts, fake.NewSimpleClientset(objects...))
}

// runGatherWith runs a gather over a clientset the caller has already set up,
// for the tests that need a reactor on it.
func runGatherWith(t *testing.T, opts *Options, clientset *fake.Clientset) (string, []string, error) {
	t.Helper()

	opts.ArtifactDir = filepath.Join(t.TempDir(), "artifacts")
	if opts.FetchPodFile == nil {
		opts.FetchPodFile = func(_ context.Context, pod *v1.Pod, destPath string) error {
			return os.WriteFile(destPath, []byte(pod.Spec.NodeName), 0o600)
		}
	}

	err := Run(opts, stubFactory{
		clientset: clientset,
		dynamic:   emptyDynamicClient(),
		config:    offlineConfig(),
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

// emptyDynamicClient knows every list kind the gather asks for and holds none
// of them, which is a cluster with the CRDs installed and nothing created.
func emptyDynamicClient() dynamic.Interface {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), customResourceListKinds())
}

// TestNodeScopesEveryCollector covers --node. A support engineer asking about
// one node gets that node's bundle: a bundle that still holds the other node's
// pods and logs is both larger than asked for and misleading about which node
// the evidence came from.
func TestNodeScopesEveryCollector(t *testing.T) {
	dir, names, err := runGather(t, &Options{Node: "gpu-node-1"}, twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	t.Run("the node objects are only that node", func(t *testing.T) {
		for _, file := range []string{"nodes.yaml", "gpu_nodes.yaml"} {
			var list v1.NodeList
			data := readArtifact(t, dir, file)
			if uerr := yaml.Unmarshal([]byte(data), &list); uerr != nil {
				t.Fatalf("%s does not parse: %v", file, uerr)
			}
			if got := strings.Join(nodeNamesOf(list), ","); got != "gpu-node-1" {
				t.Errorf("%s lists %q, want only gpu-node-1", file, got)
			}
		}
	})

	t.Run("the operand pods are only that node's", func(t *testing.T) {
		var list v1.PodList
		data := readArtifact(t, dir, "gpu_operand_pods.yaml")
		if uerr := yaml.Unmarshal([]byte(data), &list); uerr != nil {
			t.Fatalf("gpu_operand_pods.yaml does not parse: %v", uerr)
		}
		for _, pod := range list.Items {
			if pod.Spec.NodeName != "gpu-node-1" {
				t.Errorf("gpu_operand_pods.yaml holds %s, which runs on %s", pod.Name, pod.Spec.NodeName)
			}
		}
	})

	t.Run("only that node's logs and bug report are collected", func(t *testing.T) {
		for _, name := range names {
			if strings.Contains(name, "plugin-2") || strings.Contains(name, "driver-2") {
				t.Errorf("bundle holds %s, which belongs to the node that was not asked for", name)
			}
			if strings.Contains(name, "gpu-node-2") {
				t.Errorf("bundle holds %s, which belongs to the node that was not asked for", name)
			}
		}
		if !contains(names, "nvidia-bug-report_gpu-node-1.log.gz") {
			t.Errorf("bundle has no bug report for gpu-node-1: %v", names)
		}
	})
}

// nodeNamesOf lists a NodeList for a failure message.
func nodeNamesOf(list v1.NodeList) []string {
	names := make([]string, 0, len(list.Items))
	for _, node := range list.Items {
		names = append(names, node.Name)
	}
	return names
}

// contains reports whether names holds name.
func contains(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

// TestSinceBoundsTheLogWindow covers --since. A gather of a cluster that has
// been unhappy for weeks pulls gigabytes of log unless the window is bounded,
// and the request the API server sees is the only place that bound exists.
func TestSinceBoundsTheLogWindow(t *testing.T) {
	cases := []struct {
		name  string
		since time.Duration
		want  *int64
	}{
		{name: "unset leaves the whole log", since: 0, want: nil},
		{name: "two hours", since: 2 * time.Hour, want: ptr(int64(7200))},
		{name: "ninety seconds", since: 90 * time.Second, want: ptr(int64(90))},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "operand-0", Namespace: "gpu-operator"},
				Spec:       v1.PodSpec{NodeName: "node-a", Containers: []v1.Container{{Name: "ctr"}}},
				Status:     v1.PodStatus{Phase: v1.PodRunning},
			}

			dir := filepath.Join(t.TempDir(), "artifacts")
			g, err := NewGatherer(&Options{ArtifactDir: dir, Since: c.since},
				stubFactory{clientset: fake.NewSimpleClientset(pod), config: offlineConfig()},
				discardProgress{})
			if err != nil {
				t.Fatalf("NewGatherer: %v", err)
			}
			t.Cleanup(g.Close)

			var seen []*v1.PodLogOptions
			g.streamPodLogs = func(_ context.Context, _, _ string, opts *v1.PodLogOptions) (io.ReadCloser, error) {
				seen = append(seen, opts.DeepCopy())
				return io.NopCloser(strings.NewReader("")), nil
			}

			if err := g.collectPodLogs("gpu-operator", pod, false); err != nil {
				t.Fatalf("collectPodLogs: %v", err)
			}
			if len(seen) != 1 {
				t.Fatalf("the API server saw %d log requests, want 1", len(seen))
			}

			got := seen[0].SinceSeconds
			switch {
			case c.want == nil && got != nil:
				t.Errorf("sinceSeconds = %d, want it unset", *got)
			case c.want != nil && got == nil:
				t.Errorf("sinceSeconds is unset, want %d", *c.want)
			case c.want != nil && *got != *c.want:
				t.Errorf("sinceSeconds = %d, want %d", *got, *c.want)
			}
		})
	}
}

// TestSinceDoesNotBoundThePreviousLog keeps the window off the previous
// container's log. That log ended when the container died, so a window
// measured from now would empty exactly the file a crash loop is diagnosed
// from.
func TestSinceDoesNotBoundThePreviousLog(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "operand-0", Namespace: "gpu-operator"},
		Spec:       v1.PodSpec{NodeName: "node-a", Containers: []v1.Container{{Name: "ctr"}}},
		Status:     v1.PodStatus{Phase: v1.PodRunning},
	}

	dir := filepath.Join(t.TempDir(), "artifacts")
	g, err := NewGatherer(&Options{ArtifactDir: dir, Since: time.Hour},
		stubFactory{clientset: fake.NewSimpleClientset(pod), config: offlineConfig()},
		discardProgress{})
	if err != nil {
		t.Fatalf("NewGatherer: %v", err)
	}
	t.Cleanup(g.Close)

	var seen []*v1.PodLogOptions
	g.streamPodLogs = func(_ context.Context, _, _ string, opts *v1.PodLogOptions) (io.ReadCloser, error) {
		seen = append(seen, opts.DeepCopy())
		return io.NopCloser(strings.NewReader("")), nil
	}

	if err := g.collectPodLogs("gpu-operator", pod, true); err != nil {
		t.Fatalf("collectPodLogs: %v", err)
	}
	if len(seen) != 1 {
		t.Fatalf("the API server saw %d log requests, want 1", len(seen))
	}
	if !seen[0].Previous {
		t.Error("the request did not ask for the previous log")
	}
	if seen[0].SinceSeconds != nil {
		t.Errorf("the previous log was bounded to %d seconds, want the whole log", *seen[0].SinceSeconds)
	}
}

func ptr[T any](v T) *T { return &v }

// TestNoExecSkipsAndExitsZero covers --no-exec. A user who asked for no exec
// got what they asked for: the bundle holds no bug report, the manifest says
// which collectors were skipped and why, and the run is a success.
func TestNoExecSkipsAndExitsZero(t *testing.T) {
	dir, names, err := runGather(t, &Options{NoExec: true}, twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run returned %v, want a clean run: skipping by flag is not a failure", err)
	}
	if got := output.ExitCodeFor(err); got != output.ExitOK {
		t.Errorf("exit code = %d, want %d", got, output.ExitOK)
	}

	for _, name := range names {
		if strings.HasPrefix(name, "nvidia-bug-report_") || strings.HasPrefix(name, "gpu_device_usage_") {
			t.Errorf("bundle holds %s, but --no-exec was given", name)
		}
	}

	manifest := readManifest(t, dir)
	for _, want := range []string{"bug-report", "device-usage"} {
		status := manifest.statusOf(t, want)
		if status != "skipped:--no-exec" {
			t.Errorf("collector %s status = %q, want %q", want, status, "skipped:--no-exec")
		}
	}
	if !manifest.Flags.NoExec {
		t.Error("manifest.flags.noExec is false, but --no-exec was given")
	}
}

// TestNodeWithoutAGPULabelIsSkippedNotFatal covers --node pointed at a node
// that carries no NVIDIA PCI label. A cluster with no GPU node at all is a
// cluster the gather has nothing to say about, and it reports that. Narrowing
// to one node that happens not to be a GPU node is a different thing: the
// cluster has GPU nodes, this one is not among them, and the rest of the
// bundle, which is what the user asked for, is still worth collecting.
func TestNodeWithoutAGPULabelIsSkippedNotFatal(t *testing.T) {
	plain := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "cpu-node-1"}}
	objects := append(twoNodeCluster(), plain)

	dir, names, err := runGather(t, &Options{Node: "cpu-node-1"}, objects...)
	if err != nil {
		t.Fatalf("Run returned %v, want the gather to carry on past a node with no GPU", err)
	}

	if !contains(names, "nodes.yaml") {
		t.Errorf("the bundle holds no nodes.yaml: %v", names)
	}
	if contains(names, "gpu_nodes.yaml") {
		t.Error("the bundle holds gpu_nodes.yaml for a node that carries no NVIDIA PCI label")
	}

	manifest := readManifest(t, dir)
	if got := manifest.statusOf(t, "gpu-nodes"); got != "skipped:cpu-node-1 carries no NVIDIA PCI label" {
		t.Errorf("gpu-nodes status = %q, want it skipped naming the node", got)
	}
}

// TestNoGPUNodesAtAllIsRecordedNotFatal is the other side of it. A cluster
// where nothing carries an NVIDIA PCI label used to end the gather, which took
// the ClusterPolicy and the operator logs with it. Those are exactly what say
// why the cluster has no GPU node, so the survey being empty is recorded and
// the rest is still collected.
func TestNoGPUNodesAtAllIsRecordedNotFatal(t *testing.T) {
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

	dir, names, err := runGather(t, &Options{}, plain, operator)
	if err != nil {
		t.Fatalf("Run returned %v, want a cluster with no GPU node to still produce a bundle", err)
	}

	for _, name := range []string{"nodes.yaml", "gpu_operand_pods.yaml"} {
		if !contains(names, name) {
			t.Errorf("the bundle holds no %s: %v", name, names)
		}
	}
	if contains(names, "gpu_nodes.yaml") {
		t.Error("the bundle holds gpu_nodes.yaml for a cluster with no GPU node")
	}

	m := readManifest(t, dir)
	if got := m.statusOf(t, "gpu-nodes"); got != "skipped:no GPU nodes" {
		t.Errorf("gpu-nodes status = %q, want %q", got, "skipped:no GPU nodes")
	}
}

// failWith is a reactor that fails every call it is installed for.
func failWith(err error) k8stesting.ReactionFunc {
	return func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, err
	}
}
