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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// newKueueFake returns a dynamic client that serves the Kueue resources. The
// fake needs the list kind of every version it will be asked for, so both
// served versions are registered.
func newKueueFake(t *testing.T, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	t.Helper()

	kinds := map[schema.GroupVersionResource]string{
		{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}: "ClusterPolicyList",
	}
	for _, version := range []string{"v1beta2", "v1beta1"} {
		kinds[schema.GroupVersionResource{Group: kueueGroup, Version: version, Resource: "topologies"}] = "TopologyList"
		kinds[schema.GroupVersionResource{Group: kueueGroup, Version: version, Resource: "resourceflavors"}] = "ResourceFlavorList"
	}

	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objects...)
}

// kueueServer returns a clientset whose discovery announces the Kueue
// resources at the given versions, which is how a cluster says which versions
// of the CRD it serves.
func kueueServer(versions ...string) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	for _, version := range versions {
		cs.Resources = append(cs.Resources, &metav1.APIResourceList{
			GroupVersion: kueueGroup + "/" + version,
			APIResources: []metav1.APIResource{
				{Name: "topologies", Kind: "Topology", Namespaced: false},
				{Name: "resourceflavors", Kind: "ResourceFlavor", Namespaced: false},
			},
		})
	}

	return cs
}

// kueueObject builds one Kueue object at the given version.
func kueueObject(version, kind, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kueueGroup + "/" + version,
		"kind":       kind,
		"metadata":   map[string]any{"name": name},
		"spec":       spec,
	}}
}

// TestLiveTopologiesReadsTheStorageVersion covers the common cluster: Kueue
// at v0.18 or newer, where v1beta2 is the storage version.
func TestLiveTopologiesReadsTheStorageVersion(t *testing.T) {
	dyn := newKueueFake(t, kueueObject("v1beta2", "Topology", "default", map[string]any{
		"levels": []any{map[string]any{"nodeLabel": "kubernetes.io/hostname"}},
	}))

	got, err := NewLive(kueueServer("v1beta2", "v1beta1"), dyn).Topologies(context.Background())
	if err != nil {
		t.Fatalf("Topologies() error = %v, want nil", err)
	}
	if len(got) != 1 || got[0].GetName() != "default" {
		t.Fatalf("Topologies() returned %d objects (%v), want the default topology", len(got), got)
	}
}

// TestLiveTopologiesFallsBackToTheOlderServedVersion covers Kueue v0.13 to
// v0.17, which serve v1beta1 and have no v1beta2 at all. Asking only for the
// newer version there reads an endpoint that does not exist and reports a
// cluster with no topology levels.
func TestLiveTopologiesFallsBackToTheOlderServedVersion(t *testing.T) {
	dyn := newKueueFake(t, kueueObject("v1beta1", "Topology", "legacy", map[string]any{
		"levels": []any{map[string]any{"nodeLabel": "kubernetes.io/hostname"}},
	}))

	got, err := NewLive(kueueServer("v1beta1"), dyn).Topologies(context.Background())
	if err != nil {
		t.Fatalf("Topologies() error = %v, want nil", err)
	}
	if len(got) != 1 || got[0].GetName() != "legacy" {
		t.Fatalf("Topologies() returned %d objects (%v), want the legacy topology", len(got), got)
	}
}

// TestLiveTopologiesNotFoundMeansKueueIsNotInstalled: a cluster without Kueue
// serves neither version, which is an answer the command reports rather than
// a failure that ends the run.
func TestLiveTopologiesNotFoundMeansKueueIsNotInstalled(t *testing.T) {
	got, err := NewLive(kueueServer(), newKueueFake(t)).Topologies(context.Background())
	if !errors.Is(err, ErrKueueNotInstalled) {
		t.Fatalf("Topologies() error = %v, want it to match ErrKueueNotInstalled", err)
	}
	if len(got) != 0 {
		t.Errorf("Topologies() returned %d objects, want 0", len(got))
	}

	var pe *PartialError
	if errors.As(err, &pe) {
		t.Error("an absent CRD was reported as a partial read: nothing was denied")
	}
}

// TestLiveTopologiesVersionWithoutTheResourceIsSkipped: a cluster can serve
// the group at a version that does not carry this resource, which is what a
// Kueue old enough to predate topology aware scheduling looks like.
func TestLiveTopologiesVersionWithoutTheResourceIsSkipped(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{{
		GroupVersion: kueueGroup + "/v1beta1",
		APIResources: []metav1.APIResource{{Name: "clusterqueues", Kind: "ClusterQueue"}},
	}}

	_, err := NewLive(cs, newKueueFake(t)).Topologies(context.Background())
	if !errors.Is(err, ErrKueueNotInstalled) {
		t.Fatalf("Topologies() error = %v, want it to match ErrKueueNotInstalled", err)
	}
}

// TestLiveTopologiesForbiddenIsPartial: the run saw the rest of the cluster,
// so the answer is incomplete rather than impossible, and exit 3 says so.
func TestLiveTopologiesForbiddenIsPartial(t *testing.T) {
	dyn := newKueueFake(t)
	dyn.PrependReactor("list", "topologies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: kueueGroup, Resource: "topologies"}, "", errors.New("denied"))
	})

	_, err := NewLive(kueueServer("v1beta2"), dyn).Topologies(context.Background())

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("Topologies() error = %v (%T), want a PartialError", err, err)
	}
	if pe.Resource != "topologies" {
		t.Errorf("the partial read names %q, want %q", pe.Resource, "topologies")
	}
	if pe.ExitCode() != 3 {
		t.Errorf("exit code is %d, want 3", pe.ExitCode())
	}
}

// TestLiveTopologiesDeniedDiscoveryIsPartial: a cluster that will not say
// which APIs it serves has not been read, but the rest of the run has.
func TestLiveTopologiesDeniedDiscoveryIsPartial(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: kueueGroup, Resource: "topologies"}, "", errors.New("denied"))
	})

	_, err := NewLive(cs, newKueueFake(t)).Topologies(context.Background())

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("Topologies() error = %v (%T), want a PartialError", err, err)
	}
}

// TestLiveTopologiesListNotFoundFallsThrough: the CRD can be deleted between
// the discovery read and the list, and a cluster that answers NotFound to the
// list is a cluster without the resource, not a broken run.
func TestLiveTopologiesListNotFoundFallsThrough(t *testing.T) {
	dyn := newKueueFake(t)
	dyn.PrependReactor("list", "topologies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(
			schema.GroupResource{Group: kueueGroup, Resource: "topologies"}, "")
	})

	_, err := NewLive(kueueServer("v1beta2", "v1beta1"), dyn).Topologies(context.Background())
	if !errors.Is(err, ErrKueueNotInstalled) {
		t.Fatalf("Topologies() error = %v, want it to match ErrKueueNotInstalled", err)
	}
}

// TestLiveResourceFlavorsReadsBothServedVersions runs the same version choice
// against the flavors, which is the other half of the TAS read.
func TestLiveResourceFlavorsReadsBothServedVersions(t *testing.T) {
	for _, version := range []string{"v1beta2", "v1beta1"} {
		t.Run(version, func(t *testing.T) {
			dyn := newKueueFake(t, kueueObject(version, "ResourceFlavor", "tas-gpu", map[string]any{
				"topologyName": "default",
			}))

			got, err := NewLive(kueueServer(version), dyn).ResourceFlavors(context.Background())
			if err != nil {
				t.Fatalf("ResourceFlavors() error = %v, want nil", err)
			}
			if len(got) != 1 || got[0].GetName() != "tas-gpu" {
				t.Fatalf("ResourceFlavors() returned %d objects (%v), want the tas-gpu flavor", len(got), got)
			}
		})
	}
}

// TestLiveResourceFlavorsForbiddenIsPartial.
func TestLiveResourceFlavorsForbiddenIsPartial(t *testing.T) {
	dyn := newKueueFake(t)
	dyn.PrependReactor("list", "resourceflavors", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: kueueGroup, Resource: "resourceflavors"}, "", errors.New("denied"))
	})

	_, err := NewLive(kueueServer("v1beta2"), dyn).ResourceFlavors(context.Background())

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("ResourceFlavors() error = %v (%T), want a PartialError", err, err)
	}
	if pe.Resource != "resourceflavors" {
		t.Errorf("the partial read names %q, want %q", pe.Resource, "resourceflavors")
	}
}

// TestBundleTopologiesArePartial: must-gather does not collect the Kueue
// objects, so a bundle cannot answer about them. An empty list with no word
// about it would read as "Kueue is not installed", which is a different fact.
func TestBundleTopologiesArePartial(t *testing.T) {
	b, err := NewBundle(t.TempDir())
	if err != nil {
		t.Fatalf("opening the bundle: %v", err)
	}

	for name, read := range map[string]func(context.Context) ([]unstructured.Unstructured, error){
		"topologies":      b.Topologies,
		"resourceflavors": b.ResourceFlavors,
	} {
		t.Run(name, func(t *testing.T) {
			got, err := read(context.Background())

			var pe *PartialError
			if !errors.As(err, &pe) {
				t.Fatalf("error = %v (%T), want a PartialError", err, err)
			}
			if pe.Resource != name {
				t.Errorf("the partial read names %q, want %q", pe.Resource, name)
			}
			if got == nil {
				t.Error("the list is nil, want an empty one a caller can range over")
			}
			if len(got) != 0 {
				t.Errorf("the list holds %d objects, want 0", len(got))
			}
			if errors.Is(err, ErrKueueNotInstalled) {
				t.Error("a bundle reported Kueue as not installed, which it cannot know")
			}
		})
	}
}
