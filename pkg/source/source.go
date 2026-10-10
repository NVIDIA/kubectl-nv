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

// Package source is the single read path for every read-only command. A
// command talks to a Source and never to a client directly, so the same code
// serves a live cluster and a must-gather bundle replayed offline.
//
// Nothing in this package writes to a cluster: the interface has no verb that
// creates, updates, patches or deletes.
package source

import (
	"context"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// DRADriverName is the DRA driver the GPU operator publishes
	// ResourceSlices under.
	DRADriverName = "gpu.nvidia.com"

	// LabelPCIVendor is set by node-feature-discovery on any node carrying a
	// PCI device with NVIDIA's vendor id (0x10de).
	LabelPCIVendor = "feature.node.kubernetes.io/pci-10de.present"

	// LabelGPUPresent is set by the GPU operator's feature discovery.
	LabelGPUPresent = "nvidia.com/gpu.present"

	// ResourceGPU is the extended resource the device plugin advertises.
	ResourceGPU corev1.ResourceName = "nvidia.com/gpu"

	// labelValueTrue is the only value the discovery labels are honoured at.
	// node-feature-discovery writes "true"; anything else is not a claim that
	// the feature is present.
	labelValueTrue = "true"

	// pageLimit is the page size for every list. It matches the limit
	// cmd/adm/mustgather already uses, so a large cluster is walked in
	// bounded chunks instead of one unbounded response.
	pageLimit = 500
)

// Source reads the cluster state the read-only commands need. Every method
// takes a context and returns API objects: shaping those into an inventory
// model is the caller's job, not this package's.
type Source interface {
	// Nodes returns the GPU nodes, sorted by name and free of duplicates.
	Nodes(ctx context.Context) ([]corev1.Node, error)

	// Pods returns the pods in namespace matching selector. An empty
	// namespace means every namespace, and an empty selector means every pod.
	Pods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error)

	// ResourceSlices returns the ResourceSlices published by DRADriverName.
	ResourceSlices(ctx context.Context) ([]resourcev1.ResourceSlice, error)

	// ClusterPolicies returns the nvidia.com/v1 ClusterPolicy objects.
	ClusterPolicies(ctx context.Context) ([]unstructured.Unstructured, error)
}

// PartialError reports that a read succeeded for some resources and failed for
// others. The items returned alongside it are usable: a caller must not treat
// a PartialError as "no result". Resources names what could not be read, so a
// command can tell the user which RBAC rule or which bundle file is missing.
type PartialError struct {
	// Resources names what could not be read: an API resource for a live
	// cluster ("resourceslices"), a file name for a bundle ("nodes.yaml").
	Resources []string
	// Err is the underlying failure, kept so that callers can test it with
	// apierrors.IsForbidden and friends.
	Err error
}

func (e *PartialError) Error() string {
	return "could not read " + strings.Join(e.Resources, ", ") + ": " + e.Err.Error()
}

// Unwrap exposes the underlying failure to errors.Is and errors.As.
func (e *PartialError) Unwrap() error { return e.Err }

// matchesLabelOrCapacityRule reports whether node satisfies one of the first
// three GPU discovery rules. The fourth rule, a ResourceSlice naming the node,
// needs the slice set and is applied by selectGPUNodes.
func matchesLabelOrCapacityRule(node *corev1.Node) bool {
	if node.Labels[LabelPCIVendor] == labelValueTrue {
		return true
	}
	if node.Labels[LabelGPUPresent] == labelValueTrue {
		return true
	}
	if q, ok := node.Status.Capacity[ResourceGPU]; ok && q.Sign() > 0 {
		return true
	}
	return false
}

// sliceNodeNames collects the node names claimed by slices belonging to
// DRADriverName. Slices for any other driver are ignored, and a slice that
// names no node (a pool spanning nodes by selector) contributes nothing.
func sliceNodeNames(slices []resourcev1.ResourceSlice) map[string]struct{} {
	out := make(map[string]struct{}, len(slices))
	for i := range slices {
		s := &slices[i]
		if s.Spec.Driver != DRADriverName || s.Spec.NodeName == nil || *s.Spec.NodeName == "" {
			continue
		}
		out[*s.Spec.NodeName] = struct{}{}
	}
	return out
}

// selectGPUNodes applies the union of the four discovery rules, deduplicates
// by node name and returns the result sorted by name. fromSlices may be nil,
// which is the case for a bundle: must-gather does not collect ResourceSlices.
func selectGPUNodes(nodes []corev1.Node, fromSlices map[string]struct{}) []corev1.Node {
	out := make([]corev1.Node, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for i := range nodes {
		n := nodes[i]
		if _, dup := seen[n.Name]; dup {
			continue
		}
		_, bySlice := fromSlices[n.Name]
		if !bySlice && !matchesLabelOrCapacityRule(&n) {
			continue
		}
		seen[n.Name] = struct{}{}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
