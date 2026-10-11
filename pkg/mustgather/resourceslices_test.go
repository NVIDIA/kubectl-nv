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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

func resourceSlice(name, driver, node string) *resourcev1.ResourceSlice {
	return &resourcev1.ResourceSlice{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: resourcev1.ResourceSliceSpec{
			Driver:   driver,
			NodeName: &node,
			Pool: resourcev1.ResourcePool{
				Name:               node,
				ResourceSliceCount: 1,
			},
		},
	}
}

// TestResourceSlicesAreCollectedForTheNVIDIADriver covers what a DRA cluster
// needs in a bug report: the slices the NVIDIA driver published, and nobody
// else's.
func TestResourceSlicesAreCollectedForTheNVIDIADriver(t *testing.T) {
	g, dir := newTestGatherer(t,
		resourceSlice("gpu-node-1-gpus", nvidiaDRADriver, "gpu-node-1"),
		resourceSlice("gpu-node-2-gpus", nvidiaDRADriver, "gpu-node-2"),
		resourceSlice("some-nics", "nic.example.com", "gpu-node-1"),
	)

	if err := g.gatherResourceSlices(); err != nil {
		t.Fatalf("gatherResourceSlices: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "resourceslices.yaml"))
	if err != nil {
		t.Fatalf("reading resourceslices.yaml: %v", err)
	}

	var list resourcev1.ResourceSliceList
	if err := yaml.Unmarshal(data, &list); err != nil {
		t.Fatalf("resourceslices.yaml does not parse: %v\n%s", err, data)
	}
	if list.Kind != "ResourceSliceList" {
		t.Errorf("kind = %q, want ResourceSliceList", list.Kind)
	}

	var names []string
	for _, slice := range list.Items {
		names = append(names, slice.Name)
		if slice.Spec.Driver != nvidiaDRADriver {
			t.Errorf("collected a slice for driver %q", slice.Spec.Driver)
		}
	}
	if len(names) != 2 {
		t.Errorf("collected %v, want the two %s slices", names, nvidiaDRADriver)
	}
}

// TestResourceSlicesToleratesAClusterWithoutThem covers the two ways the
// slices can be out of reach, which a reader has to be able to tell apart.
//
// A cluster that does not serve the API has no slices, which is a fact about
// the cluster: the bundle says so with an empty list, so that it reads the
// same as a live cluster without DRA and nobody has to decide whether a
// missing file means "none" or "not collected".
//
// A user who may not list them is the other case: the slices exist and we did
// not see them, so there is no file to write and the gap goes to the error
// log.
func TestResourceSlicesToleratesAClusterWithoutThem(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "resource.k8s.io", Version: "v1", Resource: "resourceslices"}

	cases := []struct {
		name      string
		err       error
		wantFile  bool
		wantSkip  string
		wantInLog string
	}{
		{
			// No DRA at all is an answer about the cluster, and the empty list
			// says it the same way a live cluster without DRA would.
			name:      "the API is not served",
			err:       apierrors.NewNotFound(gvr.GroupResource(), ""),
			wantFile:  true,
			wantInLog: "resourceslices",
		},
		{
			// Being refused is a gap rather than an answer. Writing nothing
			// and reporting success left the manifest saying ok with no
			// artifacts, which a reader cannot tell from a cluster that has
			// no slices.
			name:      "the user may not list them",
			err:       apierrors.NewForbidden(gvr.GroupResource(), "", nil),
			wantFile:  false,
			wantSkip:  "forbidden",
			wantInLog: "resourceslices",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, dir := newTestGatherer(t)
			g.clientset.(*fake.Clientset).PrependReactor("list", "resourceslices",
				func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, c.err
				})

			err := g.gatherResourceSlices()
			var skipped errSkipped
			switch {
			case c.wantSkip != "":
				if !errors.As(err, &skipped) {
					t.Fatalf("gatherResourceSlices returned %v, want it recorded as skipped", err)
				}
				if skipped.reason != c.wantSkip {
					t.Errorf("skip reason = %q, want %q", skipped.reason, c.wantSkip)
				}
			case err != nil:
				t.Fatalf("gatherResourceSlices returned %v, want the gather to carry on", err)
			}

			path := filepath.Join(dir, "resourceslices.yaml")
			data, err := os.ReadFile(path)
			switch {
			case !c.wantFile:
				if !os.IsNotExist(err) {
					t.Errorf("resourceslices.yaml exists, want no file when the slices could not be listed")
				}
			case err != nil:
				t.Fatalf("expected an empty list to have been written: %v", err)
			default:
				var list resourcev1.ResourceSliceList
				if uerr := yaml.Unmarshal(data, &list); uerr != nil {
					t.Fatalf("resourceslices.yaml does not parse: %v\n%s", uerr, data)
				}
				if list.APIVersion != "resource.k8s.io/v1" || list.Kind != "ResourceSliceList" {
					t.Errorf("apiVersion/kind = %q/%q, want resource.k8s.io/v1 ResourceSliceList",
						list.APIVersion, list.Kind)
				}
				if len(list.Items) != 0 {
					t.Errorf("got %d slices, want an empty list", len(list.Items))
				}
				if !strings.Contains(string(data), "items: []") {
					t.Errorf("the empty list is not written as an empty items array:\n%s", data)
				}
			}

			g.errLogFile.Sync()
			errLog, rerr := os.ReadFile(filepath.Join(dir, "must-gather.stderr.log"))
			if rerr != nil {
				t.Fatal(rerr)
			}
			if !strings.Contains(string(errLog), c.wantInLog) {
				t.Errorf("error log does not mention the slices:\n%s", errLog)
			}
		})
	}
}

// TestResourceSlicesCoverBothNVIDIADrivers covers the second driver the script
// collects (line 153): the NVIDIA DRA driver publishes GPUs under
// gpu.nvidia.com and compute domains, which is how IMEX is allocated, under
// compute-domain.nvidia.com. A bundle holding only the first says a cluster
// using IMEX has no compute domains at all.
func TestResourceSlicesCoverBothNVIDIADrivers(t *testing.T) {
	g, dir := newTestGatherer(t,
		resourceSlice("gpu-node-1-gpus", nvidiaDRADriver, "gpu-node-1"),
		resourceSlice("gpu-node-1-imex", nvidiaComputeDomainDriver, "gpu-node-1"),
		resourceSlice("some-nics", "nic.example.com", "gpu-node-1"),
	)

	if err := g.gatherResourceSlices(); err != nil {
		t.Fatalf("gatherResourceSlices: %v", err)
	}

	var slices resourcev1.ResourceSliceList
	data := readArtifact(t, dir, "resourceslices.yaml")
	if err := yaml.Unmarshal([]byte(data), &slices); err != nil {
		t.Fatalf("resourceslices.yaml does not parse: %v\n%s", err, data)
	}

	got := make(map[string]string, len(slices.Items))
	for _, slice := range slices.Items {
		got[slice.Name] = slice.Spec.Driver
	}
	if got["gpu-node-1-gpus"] != nvidiaDRADriver {
		t.Errorf("the GPU slice is missing or wrong: %v", got)
	}
	if got["gpu-node-1-imex"] != nvidiaComputeDomainDriver {
		t.Errorf("the compute domain slice is missing or wrong: %v", got)
	}
	if _, listed := got["some-nics"]; listed {
		t.Errorf("resourceslices.yaml holds another vendor's slice: %v", got)
	}
}

// TestResourceSlicesNarrowToTheNode covers --node. A slice names the node it
// describes, so a narrowed bundle that still carries every node's slices is
// misleading about where its evidence came from.
func TestResourceSlicesNarrowToTheNode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	g, err := NewGatherer(&Options{ArtifactDir: dir, Node: "gpu-node-1"}, stubFactory{
		clientset: fake.NewSimpleClientset(
			resourceSlice("gpu-node-1-gpus", nvidiaDRADriver, "gpu-node-1"),
			resourceSlice("gpu-node-2-gpus", nvidiaDRADriver, "gpu-node-2"),
		),
		config: offlineConfig(),
	}, discardProgress{})
	if err != nil {
		t.Fatalf("NewGatherer: %v", err)
	}
	t.Cleanup(g.Close)

	if cerr := g.gatherResourceSlices(); cerr != nil {
		t.Fatalf("gatherResourceSlices: %v", cerr)
	}

	var slices resourcev1.ResourceSliceList
	data := readArtifact(t, dir, "resourceslices.yaml")
	if uerr := yaml.Unmarshal([]byte(data), &slices); uerr != nil {
		t.Fatalf("resourceslices.yaml does not parse: %v\n%s", uerr, data)
	}
	for _, slice := range slices.Items {
		if slice.Spec.NodeName == nil || *slice.Spec.NodeName != "gpu-node-1" {
			t.Errorf("resourceslices.yaml holds %s, which is not gpu-node-1's", slice.Name)
		}
	}
	if len(slices.Items) != 1 {
		t.Errorf("resourceslices.yaml holds %d slices, want only gpu-node-1's", len(slices.Items))
	}
}

// TestResourceSlicesForbiddenIsSkippedNotSilent covers a user who may not list
// the slices. The collector wrote nothing and reported success, so the manifest
// said ok with no artifacts: a reader cannot tell that from a cluster without
// DRA, and the two mean different things.
func TestResourceSlicesForbiddenIsSkippedNotSilent(t *testing.T) {
	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("list", "resourceslices",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "resource.k8s.io", Resource: "resourceslices"}, "", errUnreachable)
		})

	dir := filepath.Join(t.TempDir(), "artifacts")
	g, err := NewGatherer(&Options{ArtifactDir: dir}, stubFactory{
		clientset: clientset, config: offlineConfig(),
	}, discardProgress{})
	if err != nil {
		t.Fatalf("NewGatherer: %v", err)
	}
	t.Cleanup(g.Close)

	var skipped errSkipped
	cerr := g.gatherResourceSlices()
	if !errors.As(cerr, &skipped) {
		t.Fatalf("gatherResourceSlices returned %v, want it recorded as skipped", cerr)
	}
	if skipped.reason != "forbidden" {
		t.Errorf("skip reason = %q, want %q", skipped.reason, "forbidden")
	}
	if _, serr := os.Stat(filepath.Join(dir, "resourceslices.yaml")); serr == nil {
		t.Error("resourceslices.yaml was written for a read that was refused")
	}
}

// TestDRAVersionIsDiscovered covers the clusters this plugin is run against.
// resource.k8s.io went v1alpha/v1beta1 in 1.31, v1beta2 in 1.33 and v1 in 1.34,
// and a client pinned to v1 gets a 404 from every cluster below that: the
// bundle then says "no DRA" about a cluster using it.
func TestDRAVersionIsDiscovered(t *testing.T) {
	cases := []struct {
		name    string
		served  []string
		want    string
		wantErr bool
	}{
		{name: "a 1.34 cluster", served: []string{"resource.k8s.io/v1"}, want: "v1"},
		{name: "a 1.33 cluster", served: []string{"resource.k8s.io/v1beta2", "resource.k8s.io/v1beta1"}, want: "v1beta2"},
		{name: "a 1.32 cluster", served: []string{"resource.k8s.io/v1beta1"}, want: "v1beta1"},
		{name: "the newest wins", served: []string{"resource.k8s.io/v1beta1", "resource.k8s.io/v1"}, want: "v1"},
		{name: "a cluster without DRA", served: nil, wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clientset := fake.NewSimpleClientset()
			for _, gv := range c.served {
				clientset.Resources = append(clientset.Resources, &metav1.APIResourceList{
					GroupVersion: gv,
					APIResources: []metav1.APIResource{
						{Name: "resourceslices", Kind: "ResourceSlice"},
						{Name: "deviceclasses", Kind: "DeviceClass"},
					},
				})
			}

			g, _ := newTestGathererWith(t, clientset)
			got, err := g.draVersion("resourceslices")
			if c.wantErr {
				if err == nil {
					t.Fatalf("draVersion returned %q, want the absence reported", got)
				}
				if !apierrors.IsNotFound(err) {
					t.Errorf("error = %v, want a NotFound so callers can tell absence from a failure", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("draVersion: %v", err)
			}
			if got != c.want {
				t.Errorf("draVersion = %q, want %q", got, c.want)
			}
		})
	}
}
