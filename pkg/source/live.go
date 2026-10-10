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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

// clusterPolicyGVR is the ClusterPolicy custom resource the GPU operator owns.
var clusterPolicyGVR = schema.GroupVersionResource{
	Group:    "nvidia.com",
	Version:  "v1",
	Resource: "clusterpolicies",
}

// Live reads a running cluster. Both clients are injected so that tests can
// hand it a fake.
type Live struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

// NewLive returns a Source backed by the given clients.
func NewLive(clientset kubernetes.Interface, dyn dynamic.Interface) *Live {
	return &Live{clientset: clientset, dynamic: dyn}
}

// paginate walks a list endpoint at pageLimit, following Continue until the
// server stops handing one back. list reports the page's items and the next
// continue token.
func paginate[T any](
	ctx context.Context,
	selector string,
	list func(context.Context, metav1.ListOptions) ([]T, string, error),
) ([]T, error) {
	var all []T
	var cont string
	for {
		page, next, err := list(ctx, metav1.ListOptions{
			Limit:         pageLimit,
			Continue:      cont,
			LabelSelector: selector,
		})
		if err != nil {
			return all, err
		}
		all = append(all, page...)
		if next == "" {
			return all, nil
		}
		cont = next
	}
}

// Nodes lists every node once and filters client-side. A label match and a
// capacity match cannot be expressed as one server-side selector (selectors
// are an AND of terms, and capacity is not selectable at all), so the union of
// the discovery rules has to be computed here. There is no per-node Get.
//
// A node list that fails is fatal. A ResourceSlice list that fails is not: the
// three remaining rules still produce an answer, so the nodes are returned
// with a PartialError naming resourceslices.
func (l *Live) Nodes(ctx context.Context) ([]corev1.Node, error) {
	nodes, err := paginate(ctx, "", func(ctx context.Context, opts metav1.ListOptions) ([]corev1.Node, string, error) {
		nl, err := l.clientset.CoreV1().Nodes().List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return nl.Items, nl.Continue, nil
	})
	if err != nil {
		return nil, &PartialError{Resources: []string{"nodes"}, Err: fmt.Errorf("list nodes: %w", err)}
	}

	// The slices are taken unfiltered: sliceNodeNames is the one place that
	// decides which driver counts.
	slices, sliceErr := l.listResourceSlices(ctx)
	selected := selectGPUNodes(nodes, sliceNodeNames(slices))
	if sliceErr != nil {
		return selected, &PartialError{Resources: []string{"resourceslices"}, Err: sliceErr}
	}
	return selected, nil
}

// Pods lists the pods in namespace matching selector. An empty namespace means
// every namespace.
func (l *Live) Pods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error) {
	pods, err := paginate(ctx, selector, func(ctx context.Context, opts metav1.ListOptions) ([]corev1.Pod, string, error) {
		pl, err := l.clientset.CoreV1().Pods(namespace).List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return pl.Items, pl.Continue, nil
	})
	if err != nil {
		return nil, &PartialError{Resources: []string{"pods"}, Err: fmt.Errorf("list pods in namespace %q: %w", namespace, err)}
	}
	return pods, nil
}

// listResourceSlices walks every ResourceSlice in the cluster, whatever driver
// published it.
func (l *Live) listResourceSlices(ctx context.Context) ([]resourcev1.ResourceSlice, error) {
	all, err := paginate(ctx, "", func(ctx context.Context, opts metav1.ListOptions) ([]resourcev1.ResourceSlice, string, error) {
		sl, err := l.clientset.ResourceV1().ResourceSlices().List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return sl.Items, sl.Continue, nil
	})
	if err != nil {
		return nil, fmt.Errorf("list resourceslices: %w", err)
	}
	return all, nil
}

// ResourceSlices returns the slices published by DRADriverName. The driver is
// filtered client-side: a field selector on spec.driver would cost a second
// pass over the same data the node union already needs.
func (l *Live) ResourceSlices(ctx context.Context) ([]resourcev1.ResourceSlice, error) {
	all, err := l.listResourceSlices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]resourcev1.ResourceSlice, 0, len(all))
	for i := range all {
		if all[i].Spec.Driver == DRADriverName {
			out = append(out, all[i])
		}
	}
	return out, nil
}

// ClusterPolicies returns the nvidia.com/v1 ClusterPolicy objects. A cluster
// without the GPU operator installed has no such resource at all, which the
// API server reports as an error rather than an empty list.
func (l *Live) ClusterPolicies(ctx context.Context) ([]unstructured.Unstructured, error) {
	items, err := paginate(ctx, "", func(ctx context.Context, opts metav1.ListOptions) ([]unstructured.Unstructured, string, error) {
		ul, err := l.dynamic.Resource(clusterPolicyGVR).List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return ul.Items, ul.GetContinue(), nil
	})
	if err != nil {
		return nil, &PartialError{Resources: []string{"clusterpolicies"}, Err: fmt.Errorf("list clusterpolicies: %w", err)}
	}
	return items, nil
}
