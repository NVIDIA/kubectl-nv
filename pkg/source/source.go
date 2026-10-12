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
	"errors"
	"sort"

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
	// pkg/mustgather already uses, so a large cluster is walked in
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

	// ClassifyNodes returns both sets from one read. Nodes and HintedNodes
	// are the halves of it, and a caller that wants both should ask for both
	// here rather than call them in turn: that is one pass over the cluster
	// instead of two, and the two halves are then guaranteed to describe the
	// same moment.
	ClassifyNodes(ctx context.Context) (gpu, hinted []corev1.Node, err error)

	// Pods returns the pods in namespace matching selector. An empty
	// namespace means every namespace, and an empty selector means every pod.
	Pods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error)

	// ResourceSlices returns the ResourceSlices published by DRADriverName.
	ResourceSlices(ctx context.Context) ([]resourcev1.ResourceSlice, error)

	// ClusterPolicies returns the nvidia.com/v1 ClusterPolicy objects.
	ClusterPolicies(ctx context.Context) ([]unstructured.Unstructured, error)
}

// PartialError reports that one resource could not be read while the rest of
// the run succeeded. The items returned alongside it are usable: a caller must
// not treat a PartialError as "no result".
//
// It is deliberately narrow. Only two things produce one: an RBAC denial on a
// secondary resource (pods, resourceslices, clusterpolicies), and a file a
// must-gather bundle does not contain. Everything else, a node list that
// failed, a transport error, a dead context, an expired or rejected
// credential, means the run did not happen and is reported as a plain error.
// The difference is the exit code, and the exit code is the contract: 3 says
// "these findings are incomplete, here is what I could not see", 4 says
// "conclude nothing from this run".
//
// When more than one resource is lost, each gets its own PartialError and they
// are combined with errors.Join, so every name survives and errors.As still
// reaches one.
type PartialError struct {
	// Resource names what could not be read: an API resource for a live
	// cluster ("resourceslices"), a file name for a bundle ("nodes.yaml").
	Resource string
	// Err is the underlying failure, kept so that callers can test it with
	// apierrors.IsForbidden and friends.
	Err error
}

func (e *PartialError) Error() string {
	return "could not read " + e.Resource + ": " + e.Err.Error()
}

// Unwrap exposes the underlying failure to errors.Is and errors.As.
func (e *PartialError) Unwrap() error { return e.Err }

// ExitCode makes this error carry its own exit status, which is what
// output.ExitCodeFor reads. The value is output.ExitPartial; it is written out
// rather than imported so that this package does not depend on the one that
// formats its results.
func (e *PartialError) ExitCode() int { return 3 }

// ErrNotInstalled reports that the GPU Operator is not installed: the cluster
// serves no nvidia.com/v1 clusterpolicies resource, because the CRD was never
// applied. That is an answer, not a failure, so it carries no exit code and
// the caller decides what to make of it. A command that only reports on the
// operator will want to say so plainly; one that also checks nodes can carry
// on without it.
var ErrNotInstalled = errors.New("the GPU Operator is not installed: the cluster serves no nvidia.com/v1 clusterpolicies")

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
// fromSlices may be nil, which is what a bundle gathered before must-gather
// started writing resourceslices.yaml looks like. Only the capacity rule can
// fire then, and the bundle reader reports that as a partial read rather than
// letting a slice-only GPU node quietly go missing.
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
