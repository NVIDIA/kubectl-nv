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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
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
		config:    offlineConfig(),
	}, os.Stderr)
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
