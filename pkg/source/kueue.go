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
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// kueueGroup is the API group of Kueue's own resources.
const kueueGroup = "kueue.x-k8s.io"

// kueueVersions are the versions of that group this package will read, newest
// first.
//
// Both are in the field. Kueue served v1beta1 alone up to v0.17; v0.18
// introduced v1beta2 and made it the storage version while v1beta1 stayed
// served and deprecated; v0.20 stopped serving v1beta1. Asking only for the
// newer one reads an endpoint that does not exist on a v0.17 cluster, and the
// answer would be "this cluster has no topology levels" rather than "I asked
// the wrong version".
//
// The two versions carry the same two fields this package reads,
// spec.levels[].nodeLabel on a Topology and spec.topologyName on a
// ResourceFlavor, so whichever answers is read the same way. The objects are
// taken as unstructured rather than through Kueue's Go types: the fields are
// two strings, and a module dependency on a scheduler to read two strings is
// a dependency that will have to be upgraded in lockstep with it.
var kueueVersions = []string{"v1beta2", "v1beta1"}

// ErrKueueNotInstalled reports that the cluster serves no Kueue topology API:
// no version of the group answers for the resource, because the CRDs were
// never applied.
//
// Like ErrNotInstalled it carries no exit code. A cluster without Kueue is a
// cluster whose topology levels are the NVIDIA and Topograph labels and
// nothing else, which is an answer the command prints rather than a failure.
var ErrKueueNotInstalled = errors.New("Kueue is not installed: the cluster serves no kueue.x-k8s.io topologies")

// listKueue walks one Kueue resource at the newest version the cluster
// actually serves.
//
// The version is chosen by asking the API server what it serves rather than
// by listing each one and reading the failures. That is the question being
// asked, it is one request where the speculative form is two on every cluster
// running an older Kueue, and a list against a version the cluster does not
// serve is a request nobody meant to make.
//
// No version serving the resource means it is not there, which is
// ErrKueueNotInstalled. Forbidden, on the discovery read or on the list, is a
// partial read naming the resource: the rest of the run is still worth
// printing, and exit 3 says what is missing from it.
func (l *Live) listKueue(ctx context.Context, resource string) ([]unstructured.Unstructured, error) {
	for _, version := range kueueVersions {
		served, err := l.servesKueue(ctx, version, resource)
		switch {
		case apierrors.IsForbidden(err):
			return nil, &PartialError{Resource: resource, Err: err}
		case err != nil:
			return nil, fmt.Errorf("asking which %s/%s resources the cluster serves: %w", kueueGroup, version, err)
		case !served:
			continue
		}

		gvr := schema.GroupVersionResource{Group: kueueGroup, Version: version, Resource: resource}

		items, err := listAll[unstructured.Unstructured](ctx, "", func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
			return l.dynamic.Resource(gvr).List(ctx, opts)
		})
		switch {
		case err == nil:
			return items, nil
		case apierrors.IsNotFound(err):
			// The CRD went away between the two reads, which is what an
			// uninstall part way through a run looks like.
			continue
		case apierrors.IsForbidden(err):
			return nil, &PartialError{Resource: resource, Err: err}
		default:
			return nil, fmt.Errorf("listing %s %s: %w", kueueGroup, resource, err)
		}
	}

	return nil, ErrKueueNotInstalled
}

// servesKueue reports whether the cluster serves resource at this version of
// the Kueue group. A group version the cluster does not have is NotFound,
// which is an answer and not a failure.
func (l *Live) servesKueue(ctx context.Context, version, resource string) (bool, error) {
	list, err := l.clientset.Discovery().ServerResourcesForGroupVersionWithContext(ctx, kueueGroup+"/"+version)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}

	for _, r := range list.APIResources {
		if r.Name == resource {
			return true, nil
		}
	}

	return false, nil
}

// Topologies returns the Kueue Topology objects, which declare the node
// labels topology aware scheduling places against and the order they nest in.
func (l *Live) Topologies(ctx context.Context) ([]unstructured.Unstructured, error) {
	return l.listKueue(ctx, "topologies")
}

// ResourceFlavors returns the Kueue ResourceFlavor objects. A flavor's
// spec.topologyName is what says which Topology is actually in use.
func (l *Live) ResourceFlavors(ctx context.Context) ([]unstructured.Unstructured, error) {
	return l.listKueue(ctx, "resourceflavors")
}

// Topologies reports that a bundle cannot answer: must-gather collects the
// NVIDIA objects and nothing of Kueue, so there is no file to read.
//
// The empty list comes back with the error rather than instead of it. An
// empty success would read as "this cluster has no topology levels", which is
// a claim about the cluster that this bundle never looked at.
func (b *Bundle) Topologies(_ context.Context) ([]unstructured.Unstructured, error) {
	return bundleHasNoKueue("topologies")
}

// ResourceFlavors reports the same absence for the flavors.
func (b *Bundle) ResourceFlavors(_ context.Context) ([]unstructured.Unstructured, error) {
	return bundleHasNoKueue("resourceflavors")
}

func bundleHasNoKueue(resource string) ([]unstructured.Unstructured, error) {
	return []unstructured.Unstructured{}, &PartialError{
		Resource: resource,
		Err:      errors.New("must-gather does not collect the Kueue objects"),
	}
}
