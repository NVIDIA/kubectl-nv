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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/pager"
)

// clusterPolicyGVR is the ClusterPolicy custom resource the GPU operator owns.
// Every field is load bearing: the dynamic client sends whatever is here, so a
// typo in any of the three reads an endpoint that does not exist.
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

// listAll walks a list endpoint in pages of pageLimit and returns the items.
//
// The walk is client-go's own pager, which follows the continue token and,
// when the server drops that token with a 410 part way through, starts again
// without one. A collection that changes under a long read is ordinary on a
// large cluster, and half a node list silently returned as the whole thing
// would be a wrong answer rather than a slow one.
func listAll[T any](
	ctx context.Context,
	selector string,
	page func(context.Context, metav1.ListOptions) (runtime.Object, error),
) ([]T, error) {
	// The context is the pager's to check: it tests ctx.Done() before every
	// page and returns ctx.Err(), so a dead context never reaches the
	// transport. A second check here would be a guard nothing can trip.
	p := pager.New(page)
	p.PageSize = pageLimit
	p.FullListIfExpired = true

	obj, _, err := p.List(ctx, metav1.ListOptions{Limit: pageLimit, LabelSelector: selector})
	if err != nil {
		return nil, err
	}

	raw, err := meta.ExtractList(obj)
	if err != nil {
		return nil, fmt.Errorf("reading the list items: %w", err)
	}

	out := make([]T, 0, len(raw))
	for _, item := range raw {
		// The assertion goes through any because *T is a pointer to a type
		// parameter, which the compiler will not let be asserted straight out
		// of runtime.Object.
		typed, ok := any(item).(*T)
		if !ok {
			return nil, fmt.Errorf("unexpected item type %T in the list", item)
		}
		out = append(out, *typed)
	}

	return out, nil
}

// Nodes returns the GPU nodes. See source.go for what makes a node one.
func (l *Live) Nodes(ctx context.Context) ([]corev1.Node, error) {
	gpus, _, err := l.ClassifyNodes(ctx)
	return gpus, err
}

// HintedNodes returns the nodes that carry an NVIDIA discovery label without
// backing it with capacity or a ResourceSlice.
func (l *Live) HintedNodes(ctx context.Context) ([]corev1.Node, error) {
	_, hinted, err := l.ClassifyNodes(ctx)
	return hinted, err
}

// ClassifyNodes lists every node once and sorts them into the two sets
// client-side. One paginated list serves the whole classification: neither
// rule is expressible as a server-side selector (selectors are an AND of
// terms, and capacity is not selectable at all), and there is no per-node Get,
// which on a 1200-node cluster would be 1200 round trips for data the list
// already carried.
//
// A node list that fails ends the call, whatever the reason: without the nodes
// there is nothing to classify, so there is no partial answer to give.
//
// A denied ResourceSlice list does leave an answer. The capacity rule still
// holds for most of the cluster, so both sets are returned with a PartialError
// naming resourceslices, and the answer is then wrong in a known direction: a
// node that is a GPU node only by its slice looks hinted, or looks like
// nothing at all. Any other ResourceSlice failure is a plain error, because a
// cluster that cannot be talked to has not been read at all.
func (l *Live) ClassifyNodes(ctx context.Context) (gpu, hinted []corev1.Node, err error) {
	nodes, err := listAll[corev1.Node](ctx, "", func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return l.clientset.CoreV1().Nodes().List(ctx, opts)
	})
	if err != nil {
		return nil, nil, fmt.Errorf("listing nodes: %w", err)
	}

	// The slices are taken unfiltered: sliceNodeNames is the one place that
	// decides which driver counts.
	slices, sliceErr := l.listResourceSlices(ctx)
	var partial *PartialError
	if sliceErr != nil && !asPartial(sliceErr, &partial) {
		return nil, nil, sliceErr
	}

	gpu, hinted = classifyNodes(nodes, sliceNodeNames(slices))
	return gpu, hinted, sliceErr
}

// asPartial reports whether err is a PartialError, assigning it to target.
func asPartial(err error, target **PartialError) bool {
	pe, ok := err.(*PartialError)
	if ok {
		*target = pe
	}
	return ok
}

// listResourceSlices walks every ResourceSlice in the cluster, whatever driver
// published it.
//
// A cluster that does not serve resource.k8s.io/v1 returns NotFound for the
// collection. That is not a denial and not a failure: there are no DRA devices
// to miss, so it is an empty list. Clusters that serve only the beta versions
// of the API (v1beta1 and v1beta2, which is every cluster before 1.34 with DRA
// turned on) are not covered: this package asks for v1 and will read them as
// having no slices at all.
func (l *Live) listResourceSlices(ctx context.Context) ([]resourcev1.ResourceSlice, error) {
	all, err := listAll[resourcev1.ResourceSlice](ctx, "", func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return l.clientset.ResourceV1().ResourceSlices().List(ctx, opts)
	})
	switch {
	case err == nil:
		return all, nil
	case apierrors.IsNotFound(err):
		return nil, nil
	case apierrors.IsForbidden(err):
		return nil, &PartialError{Resource: "resourceslices", Err: err}
	default:
		return nil, fmt.Errorf("listing resourceslices: %w", err)
	}
}

// ResourceSlices returns the slices published by DRADriverName. The driver is
// filtered client-side: a field selector on spec.driver would cost a second
// pass over the same data the node classification already needs.
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

// Pods lists the pods in namespace matching selector. An empty namespace means
// every namespace.
func (l *Live) Pods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error) {
	pods, err := listAll[corev1.Pod](ctx, selector, func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return l.clientset.CoreV1().Pods(namespace).List(ctx, opts)
	})
	switch {
	case err == nil:
		return pods, nil
	case apierrors.IsForbidden(err):
		return nil, &PartialError{Resource: "pods", Err: err}
	default:
		return nil, fmt.Errorf("listing pods in namespace %q: %w", namespace, err)
	}
}

// ClusterPolicies returns the nvidia.com/v1 ClusterPolicy objects.
//
// A cluster without the GPU Operator has no such CRD, so the API server
// reports NotFound for the whole collection. That is reported as
// ErrNotInstalled rather than as a partial read: nothing was denied and
// nothing is missing, the operator is simply not there.
func (l *Live) ClusterPolicies(ctx context.Context) ([]unstructured.Unstructured, error) {
	items, err := listAll[unstructured.Unstructured](ctx, "", func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return l.dynamic.Resource(clusterPolicyGVR).List(ctx, opts)
	})
	switch {
	case err == nil:
		return items, nil
	case apierrors.IsNotFound(err):
		return nil, ErrNotInstalled
	case apierrors.IsForbidden(err):
		return nil, &PartialError{Resource: "clusterpolicies", Err: err}
	default:
		return nil, fmt.Errorf("listing clusterpolicies: %w", err)
	}
}
