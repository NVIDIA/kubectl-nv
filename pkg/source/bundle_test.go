/**
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package source

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Both implementations must satisfy the single read interface; the compiler
// enforces it here rather than at a call site in some other package.
var (
	_ Source = (*Bundle)(nil)
	_ Source = (*Live)(nil)
)

func TestBundleMissingDirectory(t *testing.T) {
	_, err := NewBundle(filepath.Join("testdata", "no-such-bundle"))
	if err == nil {
		t.Fatal("NewBundle() error = nil, want an error for a missing directory")
	}
	var me *MissingBundleError
	if !errors.As(err, &me) {
		t.Fatalf("error = %v (%T), want *MissingBundleError", err, err)
	}
	if me.Dir != filepath.Join("testdata", "no-such-bundle") {
		t.Errorf("MissingBundleError.Dir = %q, want the directory that was asked for", me.Dir)
	}
}

func TestBundleRejectsAFile(t *testing.T) {
	_, err := NewBundle(filepath.Join("testdata", "bundle-min", "nodes.yaml"))
	var me *MissingBundleError
	if !errors.As(err, &me) {
		t.Fatalf("error = %v (%T), want *MissingBundleError for a path that is not a directory", err, err)
	}
}

func TestBundleNodes(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-min"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	got, err := b.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes() error = %v, want nil", err)
	}
	// Membership is capacity alone offline, because must-gather collects no
	// ResourceSlices. control-plane-1 carries both hint labels and no GPU
	// capacity, and worker-broken-plugin carries a hint and a zero quantity;
	// neither is a GPU node.
	want := []string{"worker-capacity", "worker-capacity-no-label"}
	assertNames(t, "Nodes()", got, want)

	// The native format must carry the amount, not just the suffix: this is
	// the whole point of the switch away from the lowercased yaml.v2 dump.
	q := got[0].Status.Capacity["nvidia.com/gpu"]
	if q.Value() != 4 {
		t.Errorf("worker-capacity nvidia.com/gpu = %s, want 4", q.String())
	}
	if lv := got[0].Labels["feature.node.kubernetes.io/pci-10de.present"]; lv != "true" {
		t.Errorf("round trip lost a label: got %q, want \"true\"", lv)
	}
	if rv := got[0].ResourceVersion; rv != "1002" {
		t.Errorf("round trip lost metadata: resourceVersion = %q, want \"1002\"", rv)
	}
}

func TestBundleHintedNodes(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-min"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	got, err := b.HintedNodes(context.Background())
	if err != nil {
		t.Fatalf("HintedNodes() error = %v, want nil", err)
	}
	assertNames(t, "HintedNodes()", got, []string{"control-plane-1", "worker-broken-plugin"})

	// The zero quantity has to survive as a zero, not go missing: a health
	// rule reads it to say "gpu.present but nothing allocatable".
	q, ok := got[1].Status.Capacity["nvidia.com/gpu"]
	if !ok {
		t.Fatal("worker-broken-plugin lost its nvidia.com/gpu capacity entry")
	}
	if q.Value() != 0 {
		t.Errorf("worker-broken-plugin nvidia.com/gpu = %s, want 0", q.String())
	}
}

// TestBundleRejectsLegacyFormat pins that the retired gopkg.in/yaml.v2 dump is
// no longer understood. Its keys are lowercased Go field names and match no
// json tag, so nothing decodes out of it and the bundle reports no nodes
// rather than quietly half-reading one.
func TestBundleRejectsLegacyFormat(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-legacy"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	got, err := b.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes() error = %v, want nil", err)
	}
	if len(got) != 0 {
		t.Errorf("Nodes() = %v, want none: the lowercased yaml.v2 format is not supported", names(got))
	}
}

func TestBundlePods(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-min"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}

	cases := []struct {
		name      string
		namespace string
		selector  string
		want      []string
	}{
		{"all pods", "", "", []string{"gpu-operator-5d9f8-abcde", "nvidia-device-plugin-daemonset-xyz12", "unrelated-pod"}},
		{"by namespace", "gpu-operator", "", []string{"gpu-operator-5d9f8-abcde", "nvidia-device-plugin-daemonset-xyz12"}},
		{"by selector", "", "app=gpu-operator", []string{"gpu-operator-5d9f8-abcde"}},
		{"namespace and selector", "default", "app=gpu-operator", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := b.Pods(context.Background(), c.namespace, c.selector)
			if err != nil {
				t.Fatalf("Pods() error = %v, want nil", err)
			}
			if len(got) != len(c.want) {
				t.Fatalf("Pods() returned %d pods, want %d", len(got), len(c.want))
			}
			for i := range c.want {
				if got[i].Name != c.want[i] {
					t.Fatalf("Pods()[%d] = %q, want %q", i, got[i].Name, c.want[i])
				}
			}
		})
	}
}

func TestBundleClusterPolicies(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-min"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	got, err := b.ClusterPolicies(context.Background())
	if err != nil {
		t.Fatalf("ClusterPolicies() error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("ClusterPolicies() returned %d objects, want 1", len(got))
	}
	if got[0].GetName() != "cluster-policy" {
		t.Errorf("name = %q, want cluster-policy", got[0].GetName())
	}
	if got[0].GetAPIVersion() != "nvidia.com/v1" {
		t.Errorf("apiVersion = %q, want nvidia.com/v1", got[0].GetAPIVersion())
	}
	enabled, found, err := unstructured.NestedBool(got[0].Object, "spec", "driver", "enabled")
	if err != nil || !found || !enabled {
		t.Errorf("spec.driver.enabled = %v (found %v, err %v), want true", enabled, found, err)
	}
}

func TestBundleResourceSlicesArePartial(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-min"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	got, err := b.ResourceSlices(context.Background())
	if len(got) != 0 {
		t.Errorf("ResourceSlices() returned %d items, want 0", len(got))
	}
	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("error = %v (%T), want *PartialError", err, err)
	}
	if len(pe.Resources) != 1 || pe.Resources[0] != "resourceslices" {
		t.Fatalf("PartialError.Resources = %v, want [resourceslices]", pe.Resources)
	}
}

func TestBundleMissingFilesArePartial(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-empty"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}

	cases := []struct {
		name string
		call func() (int, error)
		want string
	}{
		{"nodes", func() (int, error) { n, err := b.Nodes(context.Background()); return len(n), err }, "nodes.yaml"},
		{"hinted nodes", func() (int, error) { n, err := b.HintedNodes(context.Background()); return len(n), err }, "nodes.yaml"},
		{"pods", func() (int, error) { p, err := b.Pods(context.Background(), "", ""); return len(p), err }, "gpu_operand_pods.yaml"},
		{"clusterpolicies", func() (int, error) { c, err := b.ClusterPolicies(context.Background()); return len(c), err }, "cluster_policy.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, err := c.call()
			if n != 0 {
				t.Errorf("returned %d items, want 0", n)
			}
			var pe *PartialError
			if !errors.As(err, &pe) {
				t.Fatalf("error = %v (%T), want *PartialError", err, err)
			}
			if len(pe.Resources) != 1 || pe.Resources[0] != c.want {
				t.Fatalf("PartialError.Resources = %v, want [%s]", pe.Resources, c.want)
			}
		})
	}
}
