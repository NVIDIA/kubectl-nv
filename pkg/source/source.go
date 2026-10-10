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
	// PCI device with NVIDIA's vendor id (0x10de). It is a hint, not
	// membership: see the discovery rule below.
	LabelPCIVendor = "feature.node.kubernetes.io/pci-10de.present"

	// LabelGPUPresent is set by the GPU operator's feature discovery. It is a
	// hint, not membership.
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

	// HintedNodes returns the nodes that carry a hint label but are not GPU
	// nodes, sorted by name and free of duplicates. A node is never in both
	// this set and Nodes.
	HintedNodes(ctx context.Context) ([]corev1.Node, error)

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

// GPU-node membership, and why the labels do not decide it.
//
// A node is a GPU node when the scheduler can actually place GPU work on it:
//
//  1. its capacity advertises nvidia.com/gpu greater than zero, or
//  2. a ResourceSlice for driver gpu.nvidia.com names it.
//
// feature.node.kubernetes.io/pci-10de.present and nvidia.com/gpu.present are
// hints. Both say that something NVIDIA-shaped was observed on the node; both
// are written by a DaemonSet, and neither is retracted when the thing stops
// working. On the Mokka lane the simulated-GPU DaemonSet tolerates every
// taint, so node-feature-discovery labelled the control plane, which has no
// allocatable GPU and never will. Counting that label as membership put the
// control plane in every GPU inventory.
//
// The same split earns its keep on real hardware in the opposite direction: a
// node whose device plugin is crash-looping keeps its labels and drops its
// capacity to zero, so it moves out of Nodes and into HintedNodes. That
// transition is the signal a health rule wants, which is why the hinted nodes
// are kept rather than discarded.

// isGPUNode reports whether node satisfies the capacity rule. The slice rule
// needs the slice set and is applied by classifyNodes.
func isGPUNode(node *corev1.Node) bool {
	q, ok := node.Status.Capacity[ResourceGPU]
	return ok && q.Sign() > 0
}

// hasHintLabel reports whether either NVIDIA discovery label is set on node.
func hasHintLabel(node *corev1.Node) bool {
	return node.Labels[LabelPCIVendor] == labelValueTrue ||
		node.Labels[LabelGPUPresent] == labelValueTrue
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

// classifyNodes splits one node list into the GPU nodes and the hinted nodes.
// Both are deduplicated by name and sorted by name, and the two sets are
// disjoint: membership wins, so a node that carries a hint and has capacity is
// a GPU node and nothing else.
//
// fromSlices may be nil, which is the case for a bundle: must-gather collects
// no ResourceSlices, so only the capacity rule can fire offline.
func classifyNodes(nodes []corev1.Node, fromSlices map[string]struct{}) (gpus, hinted []corev1.Node) {
	gpus = make([]corev1.Node, 0, len(nodes))
	hinted = make([]corev1.Node, 0, len(nodes))
	seen := make(map[string]struct{}, len(nodes))
	for i := range nodes {
		n := nodes[i]
		if _, dup := seen[n.Name]; dup {
			continue
		}
		seen[n.Name] = struct{}{}

		_, bySlice := fromSlices[n.Name]
		switch {
		case bySlice || isGPUNode(&n):
			gpus = append(gpus, n)
		case hasHintLabel(&n):
			hinted = append(hinted, n)
		}
	}
	sortByName(gpus)
	sortByName(hinted)
	return gpus, hinted
}

func sortByName(nodes []corev1.Node) {
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
}
