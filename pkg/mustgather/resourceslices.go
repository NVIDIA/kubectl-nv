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
	"context"
	"fmt"
	"io"
	"slices"

	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"
)

// The drivers the NVIDIA DRA driver publishes under. GPUs go under the first;
// compute domains, which is how IMEX is allocated across a fabric, under the
// second. The gpu-operator script collects both (line 153), and a bundle
// holding only the first says a cluster using IMEX has no compute domains at
// all, which is the thing an IMEX bug report is opened about.
const (
	nvidiaDRADriver           = "gpu.nvidia.com"
	nvidiaComputeDomainDriver = "compute-domain.nvidia.com"
)

// nvidiaDRADrivers is both of them, in the order the script lists them.
var nvidiaDRADrivers = []string{nvidiaDRADriver, nvidiaComputeDomainDriver}

// draGroup is the API group the DRA objects live in.
const draGroup = "resource.k8s.io"

// draVersions are the versions of that group this plugin can read, newest
// first. The group has been through all three in the space of four releases:
// v1beta1 in 1.31, v1beta2 in 1.33 and v1 in 1.34. A client pinned to one of
// them gets a 404 from every other cluster, and the bundle then reports "no
// DRA" about a cluster that is using it.
var draVersions = []string{"v1", "v1beta2", "v1beta1"}

// draVersion asks the cluster which version of the DRA group it serves the
// given resource under, newest first.
//
// A cluster serving none of them has no DRA, which is reported as a NotFound
// so that a caller can tell absence from a discovery that failed: the first is
// a fact about the cluster and the second is a gap in the bundle.
func (g *Gatherer) draVersion(resource string) (string, error) {
	discovery := g.clientset.Discovery()

	for _, version := range draVersions {
		groupVersion := draGroup + "/" + version
		list, err := discovery.ServerResourcesForGroupVersion(groupVersion)
		if err != nil {
			// A version the cluster does not serve is the common case here,
			// not a failure worth reporting.
			continue
		}
		for _, served := range list.APIResources {
			if served.Name == resource {
				return version, nil
			}
		}
	}

	return "", apierrors.NewNotFound(
		schema.GroupResource{Group: draGroup, Resource: resource}, "")
}

// gatherResourceSlices writes the ResourceSlices the NVIDIA DRA driver
// published to "resourceslices.yaml". On a cluster using dynamic resource
// allocation these say what the GPUs are and which node they are on, which is
// the only record of why a pod did or did not get one.
//
// A cluster that does not serve the API has no slices, which is a fact about
// the cluster worth recording: the empty list written below reads the same as a
// live cluster without DRA, where leaving the file out would read as "not
// collected" instead. A read that was refused is the opposite, a gap rather
// than an answer, and is recorded as skipped.
func (g *Gatherer) gatherResourceSlices() error {
	listFunc := func(opts metav1.ListOptions) (resourcev1.ResourceSliceList, string, error) {
		list, err := g.clientset.ResourceV1().ResourceSlices().List(context.Background(), opts)
		if err != nil {
			return resourcev1.ResourceSliceList{}, "", err
		}
		return *list, list.Continue, nil
	}
	appendFunc := func(all *resourcev1.ResourceSliceList, page resourcev1.ResourceSliceList) {
		for _, slice := range page.Items {
			if !slices.Contains(nvidiaDRADrivers, slice.Spec.Driver) {
				continue
			}
			// A slice names the node it describes, so a bundle narrowed to one
			// node that still carried every node's slices would be misleading
			// about where its evidence came from.
			if slice.Spec.NodeName != nil && !g.wantsNode(*slice.Spec.NodeName) {
				continue
			}
			all.Items = append(all.Items, slice)
		}
	}

	list, err := listAllPaginated[resourcev1.ResourceSliceList](500, "", listFunc, appendFunc)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err), apierrors.IsMethodNotSupported(err):
			// The typed client speaks v1 only. A cluster below 1.34 serves
			// this group under a beta version, and answering 404 there would
			// report a cluster that is using DRA as not having it.
			beta, berr := g.resourceSlicesAtServedVersion()
			if berr == nil {
				list = beta
				break
			}
			if _, lerr := fmt.Fprintf(g.errLogFile,
				"No resourceslices: the %s API is not served (%v)\n", draGroup, err); lerr != nil {
				return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			list = resourcev1.ResourceSliceList{}

		case apierrors.IsForbidden(err):
			// The slices are there and we were not allowed to see them. Writing
			// nothing and reporting success would read as a cluster without
			// DRA, which is a different cluster.
			if _, lerr := fmt.Fprintf(g.errLogFile, "Skipping resourceslices: %v\n", err); lerr != nil {
				return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return skip("forbidden")

		default:
			return g.logError("error getting resourceslices: %v", err)
		}
	}

	// An empty list has to marshal as an empty array rather than null, so that
	// a reader can range over .items without checking it first.
	if list.Items == nil {
		list.Items = []resourcev1.ResourceSlice{}
	}

	list.APIVersion, list.Kind = resourcev1.SchemeGroupVersion.String(), "ResourceSliceList"
	for i := range list.Items {
		list.Items[i].APIVersion = resourcev1.SchemeGroupVersion.String()
		list.Items[i].Kind = "ResourceSlice"
	}

	data, err := yaml.Marshal(list)
	if err != nil {
		return g.logError("error marshalling resourceslices: %v", err)
	}

	return g.writeArtifactFile("resourceslices.yaml", func(w io.Writer) error {
		_, werr := w.Write(data)
		return werr
	})
}

// gatherDeviceClasses collects the DRA device classes. They are cluster-scoped
// and collected whole rather than narrowed to NVIDIA's, because which classes
// exist at all is part of why a pod did or did not get a GPU.
//
// The version is discovered rather than pinned, for the same reason the slices
// are: a cluster below 1.34 serves this group under a beta version and a
// pinned client reports its device classes as absent.
func (g *Gatherer) gatherDeviceClasses() error {
	version, err := g.draVersion("deviceclasses")
	if err != nil {
		return g.noteAbsent("DeviceClass resource(s) not found in the cluster")
	}

	gvr := schema.GroupVersionResource{Group: draGroup, Version: version, Resource: "deviceclasses"}
	return g.gatherOptionalCustomResource(gvr, "deviceclasses.yaml", "DeviceClass")
}

// resourceSlicesAtServedVersion reads the slices through the dynamic client at
// whichever version of the group the cluster actually serves, for the clusters
// the typed v1 client cannot address.
func (g *Gatherer) resourceSlicesAtServedVersion() (resourcev1.ResourceSliceList, error) {
	var out resourcev1.ResourceSliceList

	version, err := g.draVersion("resourceslices")
	if err != nil {
		return out, err
	}
	if g.dynClient == nil {
		return out, apierrors.NewNotFound(
			schema.GroupResource{Group: draGroup, Resource: "resourceslices"}, "")
	}

	ctx, cancel := context.WithTimeout(context.Background(), customResourceTimeout)
	defer cancel()

	gvr := schema.GroupVersionResource{Group: draGroup, Version: version, Resource: "resourceslices"}
	list, err := g.dynClient.Resource(gvr).List(ctx, metav1.ListOptions{})
	if err != nil {
		return out, err
	}

	// The beta shapes differ from v1 only in fields this bundle does not read,
	// so the round trip through JSON is enough to get a typed list back; a
	// slice that will not convert is left out rather than failing the rest.
	for i := range list.Items {
		raw, merr := list.Items[i].MarshalJSON()
		if merr != nil {
			continue
		}
		var slice resourcev1.ResourceSlice
		if uerr := yaml.Unmarshal(raw, &slice); uerr != nil {
			continue
		}
		if !slices.Contains(nvidiaDRADrivers, slice.Spec.Driver) {
			continue
		}
		if slice.Spec.NodeName != nil && !g.wantsNode(*slice.Spec.NodeName) {
			continue
		}
		out.Items = append(out.Items, slice)
	}

	return out, nil
}
