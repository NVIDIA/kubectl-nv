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
	"path/filepath"

	resourcev1 "k8s.io/api/resource/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// nvidiaDRADriver is the name the NVIDIA DRA driver publishes its slices
// under. Slices from other drivers belong to other vendors' bug reports.
const nvidiaDRADriver = "gpu.nvidia.com"

// gatherResourceSlices writes the ResourceSlices the NVIDIA DRA driver
// published to "resourceslices.yaml". On a cluster using dynamic resource
// allocation these say what the GPUs are and which node they are on, which is
// the only record of why a pod did or did not get one.
//
// Clusters that do not serve the API, and users who may not list it, are the
// common case rather than an error: the gap goes to the error log and the
// gather carries on.
func (g *Gatherer) gatherResourceSlices() error {
	listFunc := func(opts metav1.ListOptions) (resourcev1.ResourceSliceList, string, error) {
		// The field selector keeps the transfer down on a large cluster. It is
		// not load bearing: an API server that ignores it hands back everything
		// and the filter below still writes only NVIDIA's slices.
		opts.FieldSelector = resourcev1.ResourceSliceSelectorDriver + "=" + nvidiaDRADriver
		list, err := g.clientset.ResourceV1().ResourceSlices().List(context.Background(), opts)
		if err != nil {
			return resourcev1.ResourceSliceList{}, "", err
		}
		return *list, list.Continue, nil
	}
	appendFunc := func(all *resourcev1.ResourceSliceList, page resourcev1.ResourceSliceList) {
		for _, slice := range page.Items {
			if slice.Spec.Driver == nvidiaDRADriver {
				all.Items = append(all.Items, slice)
			}
		}
	}

	slices, err := listAllPaginated[resourcev1.ResourceSliceList](500, "", listFunc, appendFunc)
	if err != nil {
		switch {
		case apierrors.IsNotFound(err):
			// A cluster that does not serve the API has no slices, which is a
			// fact about the cluster worth recording. The empty list written
			// below reads the same as a live cluster without DRA, where leaving
			// the file out would read as "not collected" instead.
			if _, lerr := fmt.Fprintf(g.errLogFile,
				"No resourceslices: the resource.k8s.io API is not served (%v)\n", err); lerr != nil {
				return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			slices = resourcev1.ResourceSliceList{}

		case apierrors.IsForbidden(err):
			// The slices are there and we were not allowed to see them, which is
			// a gap rather than an answer, so there is no file to write.
			if _, lerr := fmt.Fprintf(g.errLogFile,
				"Skipping resourceslices: %v\n", err); lerr != nil {
				return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return nil

		default:
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting resourceslices: %v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	}

	// An empty list has to marshal as an empty array rather than null, so that
	// a reader can range over .items without checking it first.
	if slices.Items == nil {
		slices.Items = []resourcev1.ResourceSlice{}
	}

	slices.APIVersion, slices.Kind = resourcev1.SchemeGroupVersion.String(), "ResourceSliceList"
	for i := range slices.Items {
		slices.Items[i].APIVersion = resourcev1.SchemeGroupVersion.String()
		slices.Items[i].Kind = "ResourceSlice"
	}

	data, err := yaml.Marshal(slices)
	if err != nil {
		return g.logError("error marshalling resourceslices: %v", err)
	}

	path := filepath.Join(g.opts.ArtifactDir, "resourceslices.yaml")
	file, err := g.createArtifact(path)
	if err != nil {
		return g.logError("error creating resourceslices.yaml: %v", err)
	}
	defer file.Close()

	if _, err := file.Write(data); err != nil {
		return g.logError("error writing resourceslices.yaml: %v", err)
	}
	return nil
}
