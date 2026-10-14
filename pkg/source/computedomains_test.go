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
	"path/filepath"
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

// newComputeDomainFake returns a dynamic client that serves the ComputeDomain
// resource at every version this package will ask for.
func newComputeDomainFake(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	kinds := map[schema.GroupVersionResource]string{
		{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}: "ClusterPolicyList",
	}
	for _, version := range computeDomainVersions {
		kinds[schema.GroupVersionResource{
			Group: computeDomainGroup, Version: version, Resource: computeDomainResource,
		}] = "ComputeDomainList"
	}

	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds, objects...)
}

// computeDomainServer returns a clientset whose discovery announces the
// ComputeDomain resource at the given versions.
func computeDomainServer(versions ...string) *fake.Clientset {
	cs := fake.NewSimpleClientset()
	for _, version := range versions {
		cs.Resources = append(cs.Resources, &metav1.APIResourceList{
			GroupVersion: computeDomainGroup + "/" + version,
			APIResources: []metav1.APIResource{{Name: computeDomainResource, Kind: "ComputeDomain"}},
		})
	}

	return cs
}

func computeDomainObject(version, namespace, name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": computeDomainGroup + "/" + version,
		"kind":       "ComputeDomain",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"status":     map[string]any{"status": "Ready"},
	}}
}

// v1beta1 is the only version the DRA driver has ever served for this
// resource, checked across v25.3.0, v25.8.0 and v25.12.0
// (api/nvidia.com/resource, k8s-dra-driver-gpu fb8b9674).
func TestLiveComputeDomainsReadsTheServedVersion(t *testing.T) {
	dyn := newComputeDomainFake(computeDomainObject("v1beta1", "workload", "cd-a"))

	got, err := NewLive(computeDomainServer("v1beta1"), dyn).ComputeDomains(context.Background())
	if err != nil {
		t.Fatalf("ComputeDomains() error = %v, want nil", err)
	}

	if len(got) != 1 || got[0].GetName() != "cd-a" {
		t.Fatalf("ComputeDomains() returned %d objects (%v), want cd-a", len(got), got)
	}
}

// A cluster without the DRA driver serves no such resource. That is an answer
// and not a failure: an empty list and no error, because nothing was denied
// and nothing is missing.
func TestLiveComputeDomainsNotInstalledIsEmpty(t *testing.T) {
	// The fake holds a ComputeDomain even though discovery serves none, so
	// that a read reaching the collection anyway is visible. Without it this
	// test would pass against a reader that listed unconditionally.
	dyn := newComputeDomainFake(computeDomainObject("v1beta1", "workload", "cd-a"))

	got, err := NewLive(fake.NewSimpleClientset(), dyn).ComputeDomains(context.Background())
	if err != nil {
		t.Fatalf("ComputeDomains() error = %v, want nil on a cluster without the CRD", err)
	}

	if len(got) != 0 {
		t.Fatalf("ComputeDomains() returned %d objects, want none", len(got))
	}
}

// A group version that answers for some other resource is still not this one.
func TestLiveComputeDomainsIgnoresAnotherResourceOfTheGroup(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Resources = []*metav1.APIResourceList{{
		GroupVersion: computeDomainGroup + "/v1beta1",
		APIResources: []metav1.APIResource{{Name: "computedomaincliques", Kind: "ComputeDomainClique"}},
	}}

	// Seeded for the same reason: the group version answers, and only the
	// resource name keeps this read away from the objects.
	dyn := newComputeDomainFake(computeDomainObject("v1beta1", "workload", "cd-a"))

	got, err := NewLive(cs, dyn).ComputeDomains(context.Background())
	if err != nil {
		t.Fatalf("ComputeDomains() error = %v, want nil", err)
	}

	if len(got) != 0 {
		t.Fatalf("ComputeDomains() returned %d objects, want none", len(got))
	}
}

// The CRD can be deleted between the discovery read and the list, which is
// what an uninstall part way through a run looks like. A cluster that answers
// NotFound to the list after saying it serves the resource has none, which is
// the same answer as never having had the CRD.
func TestLiveComputeDomainsListNotFoundIsEmpty(t *testing.T) {
	dyn := newComputeDomainFake()
	dyn.PrependReactor("list", computeDomainResource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(
			schema.GroupResource{Group: computeDomainGroup, Resource: computeDomainResource}, "")
	})

	got, err := NewLive(computeDomainServer("v1beta1"), dyn).ComputeDomains(context.Background())
	if err != nil {
		t.Fatalf("ComputeDomains() error = %v, want nil", err)
	}

	if got != nil {
		t.Fatalf("ComputeDomains() returned %v, want nothing", got)
	}
}

// A denied list is a partial read: the rest of the run is still worth
// printing and exit 3 says what is missing from it.
func TestLiveComputeDomainsForbiddenListIsPartial(t *testing.T) {
	dyn := newComputeDomainFake()
	dyn.PrependReactor("list", computeDomainResource, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: computeDomainGroup, Resource: computeDomainResource}, "", errors.New("denied"))
	})

	_, err := NewLive(computeDomainServer("v1beta1"), dyn).ComputeDomains(context.Background())

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("ComputeDomains() error = %v (%T), want a PartialError", err, err)
	}

	if pe.Resource != computeDomainResource {
		t.Errorf("the partial read names %q, want %q", pe.Resource, computeDomainResource)
	}

	if pe.ExitCode() != 3 {
		t.Errorf("exit code is %d, want 3", pe.ExitCode())
	}

	if errors.Is(err, ErrComputeDomainDiscovery) {
		t.Error("a denied list was reported as a denied discovery: the cluster did say it serves the resource")
	}
}

// A cluster that will not say which APIs it serves leaves "is the DRA driver
// installed" unanswered, which is a different denial from a refused list.
func TestLiveComputeDomainsForbiddenDiscoveryIsPartial(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "resource", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: computeDomainGroup, Resource: computeDomainResource}, "", errors.New("denied"))
	})

	_, err := NewLive(cs, newComputeDomainFake()).ComputeDomains(context.Background())

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("ComputeDomains() error = %v (%T), want a PartialError", err, err)
	}

	if !errors.Is(err, ErrComputeDomainDiscovery) {
		t.Errorf("error = %v, want it to match ErrComputeDomainDiscovery", err)
	}
}

// must-gather does not collect the ComputeDomains, so a bundle answers with
// an empty list beside the partial read rather than instead of it: an empty
// success would read as "this cluster has no ComputeDomains", which is a
// claim about a cluster this bundle never looked at.
func TestBundleComputeDomainsIsPartial(t *testing.T) {
	b, err := NewBundle(t.TempDir())
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}

	got, err := b.ComputeDomains(context.Background())

	var pe *PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("ComputeDomains() error = %v (%T), want a PartialError", err, err)
	}

	if got == nil {
		t.Error("the empty list comes back beside the error, not instead of it")
	}

	if pe.Resource != fileComputeDomains {
		t.Errorf("the partial read names %q, want %q", pe.Resource, fileComputeDomains)
	}
}

// A bundle that does carry the file reads it, so that the partial read above
// is the absence of the file rather than this method never reading one.
func TestBundleComputeDomainsReadsTheFile(t *testing.T) {
	dir := t.TempDir()

	const doc = `apiVersion: v1
kind: List
items:
- apiVersion: resource.nvidia.com/v1beta1
  kind: ComputeDomain
  metadata:
    name: cd-a
    namespace: workload
  status:
    status: NotReady
`

	if err := os.WriteFile(filepath.Join(dir, fileComputeDomains), []byte(doc), 0o600); err != nil {
		t.Fatalf("writing the bundle file: %v", err)
	}

	b, err := NewBundle(dir)
	if err != nil {
		t.Fatalf("NewBundle() error = %v", err)
	}

	got, err := b.ComputeDomains(context.Background())
	if err != nil {
		t.Fatalf("ComputeDomains() error = %v, want nil", err)
	}

	if len(got) != 1 {
		t.Fatalf("ComputeDomains() returned %d objects, want 1: %v", len(got), got)
	}

	if got[0].GetName() != "cd-a" || got[0].GetNamespace() != "workload" {
		t.Errorf("ComputeDomains() returned %s/%s, want workload/cd-a", got[0].GetNamespace(), got[0].GetName())
	}
}

// A source with no ComputeDomain seam is not an error. Every fake in every
// track implements Source, and a command that failed over a method a source
// does not have would make all of them useless.
func TestReadComputeDomainsWithoutTheSeam(t *testing.T) {
	got, err := ReadComputeDomains(context.Background(), noComputeDomainSource{})
	if err != nil {
		t.Fatalf("ReadComputeDomains() error = %v, want nil", err)
	}

	if got != nil {
		t.Errorf("ReadComputeDomains() returned %v, want nothing", got)
	}
}

// TestReadComputeDomainsThroughTheSeam proves the seam is what is read, and
// not some fallback that would make the test above pass for the wrong reason.
func TestReadComputeDomainsThroughTheSeam(t *testing.T) {
	dyn := newComputeDomainFake(computeDomainObject("v1beta1", "workload", "cd-a"))

	got, err := ReadComputeDomains(context.Background(), NewLive(computeDomainServer("v1beta1"), dyn))
	if err != nil {
		t.Fatalf("ReadComputeDomains() error = %v, want nil", err)
	}

	if len(got) != 1 || got[0].GetName() != "cd-a" {
		t.Fatalf("ReadComputeDomains() returned %d objects (%v), want cd-a", len(got), got)
	}
}

// noComputeDomainSource is a Source with no ComputeDomain method, which is
// every Source outside this package.
type noComputeDomainSource struct{ Source }
