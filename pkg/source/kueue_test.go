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
	"sort"
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
	for _, version := range []string{"v1beta2", "v1beta1", "v1alpha1"} {
		kinds[schema.GroupVersionResource{Group: kueueGroup, Version: version, Resource: "topologies"}] = "TopologyList"
		kinds[schema.GroupVersionResource{Group: kueueGroup, Version: version, Resource: "resourceflavors"}] = "ResourceFlavorList"
	}

	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objects...)
}

// kueueServer returns a clientset whose discovery announces both Kueue
// resources at the given versions, which is how a cluster says which versions
// of the CRD it serves.
func kueueServer(versions ...string) *fake.Clientset {
	served := map[string][]string{}
	for _, version := range versions {
		served[version] = []string{"topologies", "resourceflavors"}
	}

	return kueueServing(served)
}

// kueueServing is the same with the resources named per version, for the
// releases where the two resources are not at the same version.
func kueueServing(served map[string][]string) *fake.Clientset {
	cs := fake.NewSimpleClientset()

	versions := make([]string, 0, len(served))
	for version := range served {
		versions = append(versions, version)
	}
	sort.Strings(versions)

	for _, version := range versions {
		list := &metav1.APIResourceList{GroupVersion: kueueGroup + "/" + version}
		for _, name := range served[version] {
			list.APIResources = append(list.APIResources, metav1.APIResource{Name: name})
		}
		cs.Resources = append(cs.Resources, list)
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

// TestLiveTopologiesReadsWhicheverVersionIsServed walks the Topology
// versions that are in the field, each read from the CRD of the release that
// introduced it:
//
//	v0.13.0  v1alpha1 served and storage, no v1beta1 at all
//	v0.14.0  v1beta1 served and storage, v1alpha1 gone
//	v0.15.0  v1beta1 storage, v1beta2 served
//	v0.16.0  v1beta2 storage, v1beta1 still served
//	v0.20.1  v1beta1 no longer served
//
// Asking only for the newest reads an endpoint that does not exist on an
// older cluster, and the answer would be "this cluster has no topology
// levels" rather than "I asked the wrong version".
func TestLiveTopologiesReadsWhicheverVersionIsServed(t *testing.T) {
	for _, tc := range []struct {
		release string
		served  []string
		// want is the version the object is published at, which is the one
		// the read has to land on.
		want string
	}{
		{release: "v0.13.0", served: []string{"v1alpha1"}, want: "v1alpha1"},
		{release: "v0.14.0", served: []string{"v1beta1"}, want: "v1beta1"},
		{release: "v0.16.0", served: []string{"v1beta1", "v1beta2"}, want: "v1beta2"},
		{release: "v0.20.1", served: []string{"v1beta2"}, want: "v1beta2"},
	} {
		t.Run(tc.release, func(t *testing.T) {
			dyn := newKueueFake(t, kueueObject(tc.want, "Topology", "default", map[string]any{
				"levels": []any{map[string]any{"nodeLabel": "kubernetes.io/hostname"}},
			}))

			got, err := NewLive(kueueServer(tc.served...), dyn).Topologies(context.Background())
			if err != nil {
				t.Fatalf("Topologies() error = %v, want nil", err)
			}
			if len(got) != 1 || got[0].GetName() != "default" {
				t.Fatalf("Topologies() returned %d objects (%v), want the default topology", len(got), got)
			}
			if version := got[0].GetAPIVersion(); version != kueueGroup+"/"+tc.want {
				t.Errorf("Topologies() read %s, want %s/%s", version, kueueGroup, tc.want)
			}
		})
	}
}

// TestLiveReadsEachResourceAtItsOwnVersion is Kueue v0.13, where Topology is
// v1alpha1 and ResourceFlavor is already v1beta1. One version chosen for the
// group would read one of the two from an endpoint that is not there.
func TestLiveReadsEachResourceAtItsOwnVersion(t *testing.T) {
	cs := kueueServing(map[string][]string{
		"v1alpha1": {"topologies"},
		"v1beta1":  {"resourceflavors"},
	})
	dyn := newKueueFake(t,
		kueueObject("v1alpha1", "Topology", "default", map[string]any{
			"levels": []any{map[string]any{"nodeLabel": "kubernetes.io/hostname"}},
		}),
		kueueObject("v1beta1", "ResourceFlavor", "tas-gpu", map[string]any{"topologyName": "default"}),
	)
	live := NewLive(cs, dyn)

	topologies, err := live.Topologies(context.Background())
	if err != nil {
		t.Fatalf("Topologies() error = %v, want nil", err)
	}
	if len(topologies) != 1 {
		t.Errorf("Topologies() returned %d objects, want 1", len(topologies))
	}

	flavors, err := live.ResourceFlavors(context.Background())
	if err != nil {
		t.Fatalf("ResourceFlavors() error = %v, want nil", err)
	}
	if len(flavors) != 1 {
		t.Errorf("ResourceFlavors() returned %d objects, want 1", len(flavors))
	}
}

// TestLiveTopologiesTransportFailureIsAPlainError: a cluster that cannot be
// talked to has not been read, and reporting that as a partial result would
// let a run that saw nothing exit 3 and read as "mostly fine".
func TestLiveTopologiesTransportFailureIsAPlainError(t *testing.T) {
	dyn := newKueueFake(t)
	dyn.PrependReactor("list", "topologies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("connection refused")
	})

	_, err := NewLive(kueueServer("v1beta2"), dyn).Topologies(context.Background())
	if err == nil {
		t.Fatal("a transport failure was swallowed")
	}

	var pe *PartialError
	if errors.As(err, &pe) {
		t.Errorf("error = %v, want a plain error, not a partial read", err)
	}
	if errors.Is(err, ErrKueueNotInstalled) {
		t.Errorf("error = %v, want it not to claim Kueue is absent", err)
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
	if errors.Is(err, ErrKueueDiscovery) {
		t.Error("a denied list was reported as a denied discovery: the cluster did say it serves the resource")
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

	// Which read was denied decides what the report can say about Kueue:
	// a denied discovery leaves "is it installed" unanswered, where a denied
	// list has already been told that it is.
	if !errors.Is(err, ErrKueueDiscovery) {
		t.Errorf("error = %v, want it to match ErrKueueDiscovery", err)
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
