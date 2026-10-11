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

const (
	// computeDomainGroup is the API group the NVIDIA DRA driver owns.
	computeDomainGroup = "resource.nvidia.com"

	// computeDomainResource is the collection name.
	computeDomainResource = "computedomains"

	// fileComputeDomains is where a must-gather bundle would carry the
	// ComputeDomains. pkg/mustgather does not write it, so reading one is a
	// partial read; a gather that starts collecting them needs no change
	// here.
	fileComputeDomains = "compute_domains.yaml"
)

// computeDomainVersions are the versions of that group this package will read,
// newest first.
//
// There is one. v1beta1 is the only version the resource has ever been served
// at: api/nvidia.com/resource holds v1beta1 and nothing else at v25.3.0,
// v25.8.0 and v25.12.0 alike, and the CRD the chart installs declares that one
// version served and storage
// (deployments/helm/dra-driver-nvidia-gpu/crds/resource.nvidia.com_computedomains.yaml,
// k8s-dra-driver-gpu fb8b9674).
//
// It is a list rather than a constant because the version is chosen by asking
// the cluster what it serves, exactly as the Kueue reads do: a second version
// slots in here, and until one exists this costs one discovery request.
var computeDomainVersions = []string{"v1beta1"}

// ErrComputeDomainDiscovery reports that the cluster would not say which
// versions of the DRA driver's group it serves.
//
// It is wrapped in the PartialError that carries it, so that a caller can tell
// the two denied reads apart. A denied discovery leaves "is the DRA driver
// installed" unanswered; a denied list has already been told that it is.
var ErrComputeDomainDiscovery = errors.New("the cluster would not say which resource.nvidia.com versions it serves")

// ComputeDomainReader is a source that can read the ComputeDomain objects.
//
// It is deliberately not part of Source, for the same reason NodeEventReader
// is not: every fake in every track implements that interface, so widening it
// would break all of them at once for a seam one command needs.
type ComputeDomainReader interface {
	// ComputeDomains returns the resource.nvidia.com ComputeDomain objects.
	ComputeDomains(ctx context.Context) ([]unstructured.Unstructured, error)
}

// ReadComputeDomains returns the ComputeDomains src can give, or nothing when
// it has no way to read them.
//
// A source without the seam is not an error. The rules that read
// ComputeDomains have to cope with an empty set regardless, because most
// clusters run no multi-node IMEX workload at all.
func ReadComputeDomains(ctx context.Context, src Source) ([]unstructured.Unstructured, error) {
	reader, ok := src.(ComputeDomainReader)
	if !ok {
		return nil, nil
	}

	return reader.ComputeDomains(ctx)
}

// ComputeDomains returns the cluster's ComputeDomain objects.
//
// The version is chosen by asking the API server what it serves rather than by
// listing each one and reading the failures, which is what G1 settled on for
// Kueue: that is the question being asked, and a list against a version the
// cluster does not serve is a request nobody meant to make.
//
// No version serving the resource means the DRA driver is not installed, which
// is an answer and not a failure: an empty list and a nil error. That is the
// difference from ClusterPolicies, which has an ErrNotInstalled of its own
// because a command may want to report on the operator itself; nothing reports
// on the absence of a ComputeDomain CRD, and a sentinel every caller would
// have to ignore is a sentinel that gets mishandled.
//
// Forbidden, on the discovery read or on the list, is a partial read naming
// the resource: the rest of the run is still worth printing, and exit 3 says
// what is missing from it.
func (l *Live) ComputeDomains(ctx context.Context) ([]unstructured.Unstructured, error) {
	for _, version := range computeDomainVersions {
		served, err := l.servesComputeDomains(ctx, version)
		switch {
		case apierrors.IsForbidden(err):
			return nil, &PartialError{
				Resource: computeDomainResource,
				Err:      fmt.Errorf("%w: %w", ErrComputeDomainDiscovery, err),
			}
		case err != nil:
			return nil, fmt.Errorf("asking which %s/%s resources the cluster serves: %w", computeDomainGroup, version, err)
		case !served:
			continue
		}

		gvr := schema.GroupVersionResource{
			Group: computeDomainGroup, Version: version, Resource: computeDomainResource,
		}

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
			return nil, &PartialError{Resource: computeDomainResource, Err: err}
		default:
			return nil, fmt.Errorf("listing %s %s: %w", computeDomainGroup, computeDomainResource, err)
		}
	}

	return nil, nil
}

// servesComputeDomains reports whether the cluster serves the ComputeDomain
// collection at this version of the group. A group version the cluster does
// not have is NotFound, which is an answer and not a failure.
func (l *Live) servesComputeDomains(ctx context.Context, version string) (bool, error) {
	list, err := l.clientset.Discovery().ServerResourcesForGroupVersionWithContext(ctx, computeDomainGroup+"/"+version)
	switch {
	case apierrors.IsNotFound(err):
		return false, nil
	case err != nil:
		return false, err
	}

	for _, r := range list.APIResources {
		if r.Name == computeDomainResource {
			return true, nil
		}
	}

	return false, nil
}

// ComputeDomains returns the ComputeDomains a must-gather bundle recorded.
//
// pkg/mustgather does not collect them, so today this is always a partial read
// naming the file that is not there. The empty list comes back with the error
// rather than instead of it: an empty success would read as "this cluster has
// no ComputeDomains", which is a claim about a cluster this bundle never
// looked at.
func (b *Bundle) ComputeDomains(ctx context.Context) ([]unstructured.Unstructured, error) {
	var list unstructured.UnstructuredList
	if err := b.readYAML(ctx, fileComputeDomains, &list); err != nil {
		return []unstructured.Unstructured{}, err
	}

	return list.Items, nil
}
