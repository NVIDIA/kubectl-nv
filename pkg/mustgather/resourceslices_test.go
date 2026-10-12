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
		wantInLog string
	}{
		{
			name:      "the API is not served",
			err:       apierrors.NewNotFound(gvr.GroupResource(), ""),
			wantFile:  true,
			wantInLog: "resourceslices",
		},
		{
			name:      "the user may not list them",
			err:       apierrors.NewForbidden(gvr.GroupResource(), "", nil),
			wantFile:  false,
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

			if err := g.gatherResourceSlices(); err != nil {
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
