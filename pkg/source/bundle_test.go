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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
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
	// Both membership rules fire offline now that must-gather writes
	// resourceslices.yaml: worker-slice-only has neither a label nor
	// capacity and is a GPU node purely by its slice. control-plane-1
	// carries both hint labels and no GPU capacity, worker-broken-plugin
	// carries a hint and a zero quantity, and plain-worker is named only by
	// a slice for a foreign driver; none of the three is a GPU node.
	want := []string{"worker-capacity", "worker-capacity-no-label", "worker-slice-only"}
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

// TestBundleRejectsLegacyFormat pins that a bundle written by kubectl-nv
// v0.1.x is named for what it is rather than half-decoded. The old writer used
// gopkg.in/yaml.v2, whose lowercased keys match no json tag; a node file gives
// itself away on its quantities, but a pod file has none and decodes silently
// into empty objects, which is the case worth catching.
func TestBundleRejectsLegacyFormat(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-legacy"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}

	cases := []struct {
		name string
		call func() (int, error)
		file string
	}{
		{"nodes", func() (int, error) { n, err := b.Nodes(context.Background()); return len(n), err }, "nodes.yaml"},
		{"pods", func() (int, error) {
			p, err := b.Pods(context.Background(), "gpu-operator", "")
			return len(p), err
		}, "gpu_operand_pods.yaml"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, err := c.call()
			if n != 0 {
				t.Errorf("returned %d items, want 0", n)
			}
			var le *LegacyBundleError
			if !errors.As(err, &le) {
				t.Fatalf("error = %v (%T), want *LegacyBundleError", err, err)
			}
			if le.Name != c.file {
				t.Errorf("LegacyBundleError.Name = %q, want %q", le.Name, c.file)
			}
			const want = "re-run must-gather"
			if !strings.Contains(le.Error(), want) {
				t.Errorf("error = %q, want it to say %q", le.Error(), want)
			}
			var pe *PartialError
			if errors.As(err, &pe) {
				t.Errorf("error = %v, want a plain error: a bundle in the wrong format is not a partial read", err)
			}
		})
	}
}

func TestBundlePods(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-min"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}

	const operator = "gpu-operator"
	const devicePlugin = "nvidia-device-plugin-daemonset-xyz12"
	const operatorPod = "gpu-operator-5d9f8-abcde"

	cases := []struct {
		name        string
		namespace   string
		selector    string
		want        []string
		wantPartial bool
	}{
		{"recorded namespace", operator, "", []string{operatorPod, devicePlugin}, false},
		{"recorded namespace and selector", operator, "app=gpu-operator", []string{operatorPod}, false},
		{"every namespace", "", "", []string{operatorPod, devicePlugin}, true},
		{"a namespace the bundle never recorded", "default", "", nil, true},
		{"selector that matches nothing", operator, "app=nope", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := b.Pods(context.Background(), c.namespace, c.selector)
			if len(got) != len(c.want) {
				gotNames := make([]string, 0, len(got))
				for i := range got {
					gotNames = append(gotNames, got[i].Name)
				}
				t.Fatalf("Pods() = %v, want %v", gotNames, c.want)
			}
			for i := range c.want {
				if got[i].Name != c.want[i] {
					t.Fatalf("Pods()[%d] = %q, want %q", i, got[i].Name, c.want[i])
				}
			}

			var pe *PartialError
			isPartial := errors.As(err, &pe)
			if isPartial != c.wantPartial {
				t.Fatalf("Pods() error = %v, want partial = %v", err, c.wantPartial)
			}
			if c.wantPartial && pe.Resource != "pods" {
				t.Errorf("PartialError.Resource = %q, want pods", pe.Resource)
			}
			if !c.wantPartial && err != nil {
				t.Errorf("Pods() error = %v, want nil", err)
			}
		})
	}
}

// TestBundlePodStatusRoundTrip pins that the container state survives, since
// that is what a health rule reads to say why a plugin is not serving.
func TestBundlePodStatusRoundTrip(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-min"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	pods, err := b.Pods(context.Background(), "gpu-operator", "app=nvidia-device-plugin-daemonset")
	if err != nil {
		t.Fatalf("Pods() error = %v", err)
	}
	if len(pods) != 1 {
		t.Fatalf("Pods() returned %d pods, want 1", len(pods))
	}
	p := pods[0]
	if p.Status.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running", p.Status.Phase)
	}
	if len(p.Status.ContainerStatuses) != 1 {
		t.Fatalf("containerStatuses = %d, want 1", len(p.Status.ContainerStatuses))
	}
	cs := p.Status.ContainerStatuses[0]
	if cs.State.Waiting == nil || cs.State.Waiting.Reason != "CrashLoopBackOff" {
		t.Errorf("container state = %+v, want waiting with reason CrashLoopBackOff", cs.State)
	}
	if cs.Ready {
		t.Error("container ready = true, want false")
	}
	if cs.RestartCount != 147 {
		t.Errorf("restartCount = %d, want 147", cs.RestartCount)
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

func TestBundleResourceSlices(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-min"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	got, err := b.ResourceSlices(context.Background())
	if err != nil {
		t.Fatalf("ResourceSlices() error = %v, want nil", err)
	}
	if len(got) != 1 {
		sliceNames := make([]string, 0, len(got))
		for i := range got {
			sliceNames = append(sliceNames, got[i].Name)
		}
		t.Fatalf("ResourceSlices() = %v, want exactly the gpu.nvidia.com slice", sliceNames)
	}
	if got[0].Spec.Driver != "gpu.nvidia.com" {
		t.Errorf("driver = %q, want gpu.nvidia.com", got[0].Spec.Driver)
	}
	if got[0].Spec.NodeName == nil || *got[0].Spec.NodeName != "worker-slice-only" {
		t.Errorf("nodeName = %v, want worker-slice-only", got[0].Spec.NodeName)
	}
	if got[0].Spec.Pool.ResourceSliceCount != 1 {
		t.Errorf("pool.resourceSliceCount = %d, want 1", got[0].Spec.Pool.ResourceSliceCount)
	}
}

// TestBundleWithoutResourceSlices covers a bundle gathered before B3, or one
// where the slice read was denied. The capacity rule still answers, but one of
// the two membership rules could not be evaluated at all, so every method that
// depends on it says so. worker-slice-only is the node that proves the point:
// without the file it falls out of both sets rather than into the wrong one.
func TestBundleWithoutResourceSlices(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-no-slices"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}

	cases := []struct {
		name string
		call func() ([]string, error)
		want []string
	}{
		{"Nodes", func() ([]string, error) {
			n, err := b.Nodes(context.Background())
			return names(n), err
		}, []string{"worker-capacity", "worker-capacity-no-label"}},
		{"HintedNodes", func() ([]string, error) {
			n, err := b.HintedNodes(context.Background())
			return names(n), err
		}, []string{"control-plane-1", "worker-broken-plugin"}},
		{"ResourceSlices", func() ([]string, error) {
			s, err := b.ResourceSlices(context.Background())
			out := make([]string, 0, len(s))
			for i := range s {
				out = append(out, s[i].Name)
			}
			return out, err
		}, []string{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := c.call()
			if len(got) != len(c.want) {
				t.Fatalf("%s() = %v, want %v", c.name, got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("%s() = %v, want %v", c.name, got, c.want)
				}
			}
			var pe *PartialError
			if !errors.As(err, &pe) {
				t.Fatalf("%s() error = %v (%T), want *PartialError", c.name, err, err)
			}
			if pe.Resource != "resourceslices" {
				t.Fatalf("PartialError.Resource = %q, want resourceslices", pe.Resource)
			}
			for _, n := range got {
				if n == "worker-slice-only" {
					t.Errorf("worker-slice-only was classified without the slice file")
				}
			}
		})
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
		// nodes.yaml is read first, so its absence is what gets reported
		// even though resourceslices.yaml is missing too.
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
			if pe.Resource != c.want {
				t.Fatalf("PartialError.Resource = %q, want %q", pe.Resource, c.want)
			}
		})
	}
}

// TestBundleClassifyNodes pins that the bundle's ClassifyNodes agrees with the
// two convenience methods and decodes the file afresh each call, so nothing is
// carried over between reads of a directory that may have changed.
func TestBundleClassifyNodes(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-min"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	gpus, hinted, err := b.ClassifyNodes(context.Background())
	if err != nil {
		t.Fatalf("ClassifyNodes() error = %v", err)
	}
	assertNames(t, "ClassifyNodes() gpus", gpus,
		[]string{"worker-capacity", "worker-capacity-no-label", "worker-slice-only"})
	assertNames(t, "ClassifyNodes() hinted", hinted,
		[]string{"control-plane-1", "worker-broken-plugin"})

	viaNodes, err := b.Nodes(context.Background())
	if err != nil {
		t.Fatalf("Nodes() error = %v", err)
	}
	viaHinted, err := b.HintedNodes(context.Background())
	if err != nil {
		t.Fatalf("HintedNodes() error = %v", err)
	}
	assertNames(t, "Nodes()", viaNodes, names(gpus))
	assertNames(t, "HintedNodes()", viaHinted, names(hinted))
}

// A must-gather directory is untrusted input: it arrives in a support ticket.
// The next three tests cover what the reader refuses to follow.

func TestBundleRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(t.TempDir(), "real-nodes.yaml")
	if err := os.WriteFile(real, []byte("items: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "nodes.yaml")); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}

	b, err := NewBundle(dir)
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	got, err := b.Nodes(context.Background())
	if len(got) != 0 {
		t.Errorf("Nodes() returned %d nodes, want 0", len(got))
	}
	var ue *UnsafeBundleFileError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %v (%T), want *UnsafeBundleFileError", err, err)
	}
	if ue.Name != "nodes.yaml" {
		t.Errorf("UnsafeBundleFileError.Name = %q, want nodes.yaml", ue.Name)
	}
	var pe *PartialError
	if errors.As(err, &pe) {
		t.Errorf("error = %v, want a plain error, not *PartialError", err)
	}
}

func TestBundleRejectsFIFO(t *testing.T) {
	if _, err := exec.LookPath("mkfifo"); err != nil {
		t.Skip("mkfifo is not available on this platform")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "nodes.yaml")
	if out, err := exec.Command("mkfifo", path).CombinedOutput(); err != nil {
		t.Skipf("mkfifo failed: %v: %s", err, out)
	}

	b, err := NewBundle(dir)
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	// A FIFO with no writer blocks an open forever, so the guard has to fire
	// on the Lstat, before anything is opened. A test that hangs here is the
	// bug this covers.
	done := make(chan error, 1)
	go func() {
		_, err := b.Nodes(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		var ue *UnsafeBundleFileError
		if !errors.As(err, &ue) {
			t.Fatalf("error = %v (%T), want *UnsafeBundleFileError", err, err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Nodes() blocked on a FIFO; the file type must be checked before it is opened")
	}
}

func TestBundleRejectsOversizeFile(t *testing.T) {
	dir := t.TempDir()
	// Well-formed YAML, so that anything other than the size guard would let
	// it through.
	body := "items: []\n# " + strings.Repeat("x", 4096) + "\n"
	if err := os.WriteFile(filepath.Join(dir, "nodes.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// A slice file too, so that the only thing left to fail is the size.
	if err := os.WriteFile(filepath.Join(dir, "resourceslices.yaml"), []byte("items: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	b, err := NewBundle(dir)
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	b.maxFileBytes = 1024

	got, err := b.Nodes(context.Background())
	if len(got) != 0 {
		t.Errorf("Nodes() returned %d nodes, want 0", len(got))
	}
	var te *BundleFileTooLargeError
	if !errors.As(err, &te) {
		t.Fatalf("error = %v (%T), want *BundleFileTooLargeError", err, err)
	}
	if te.Limit != 1024 {
		t.Errorf("BundleFileTooLargeError.Limit = %d, want 1024", te.Limit)
	}
	if te.Name != "nodes.yaml" {
		t.Errorf("BundleFileTooLargeError.Name = %q, want nodes.yaml", te.Name)
	}

	// A file just under the cap still reads.
	b.maxFileBytes = int64(len(body))
	if _, err := b.Nodes(context.Background()); err != nil {
		t.Errorf("Nodes() error = %v at exactly the cap, want nil", err)
	}
}

func TestBundleHonoursContext(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-min"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := b.ClassifyNodes(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ClassifyNodes() error = %v, want context.Canceled", err)
	}
	if _, err := b.Pods(ctx, "gpu-operator", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("Pods() error = %v, want context.Canceled", err)
	}
	if _, err := b.ResourceSlices(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ResourceSlices() error = %v, want context.Canceled", err)
	}
	if _, err := b.ClusterPolicies(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("ClusterPolicies() error = %v, want context.Canceled", err)
	}
}

// TestBundleErrorNamesTheFileOnce guards against the message growing a second
// copy of the file name as the error is wrapped on its way out.
func TestBundleErrorNamesTheFileOnce(t *testing.T) {
	b, err := NewBundle(filepath.Join("testdata", "bundle-empty"))
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}
	_, err = b.Nodes(context.Background())
	if err == nil {
		t.Fatal("Nodes() error = nil, want a failure")
	}
	if n := strings.Count(err.Error(), "nodes.yaml"); n != 1 {
		t.Errorf("error %q names nodes.yaml %d times, want once", err.Error(), n)
	}
}
