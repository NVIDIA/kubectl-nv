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

package topology

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// NodeReader is the part of source.Source this package needs: the GPU nodes,
// and the pods holding GPUs on them.
type NodeReader interface {
	Nodes(ctx context.Context) ([]corev1.Node, error)
	Pods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error)
}

// BuildNodes fills the fields of the inventory model this package groups by:
// the node's name and labels, its topology labels, its allocatable GPUs and
// the GPUs the pods on it already hold.
//
// It fills those fields and no others. The full model, with the operands, the
// MIG configuration and the driver versions, is pkg/inventory's to build, and
// this command will read it from there instead; until that builder lands,
// grouping nodes needs four fields and this is the shortest way to four
// fields that does not wait on it.
//
// A denied pod list is not the end of the run: the nodes and their labels
// were still read, so they come back with every node's requested GPUs marked
// as never read, and the PartialError alongside them. A node list that failed
// is the end of it, because there is nothing to group.
func BuildNodes(ctx context.Context, src NodeReader) ([]inventory.GPUNode, error) {
	nodes, err := src.Nodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("reading the GPU nodes: %w", err)
	}

	requested, podErr := requestedByNode(ctx, src)
	known := podErr == nil

	out := make([]inventory.GPUNode, 0, len(nodes))
	for i := range nodes {
		node := buildNode(&nodes[i])
		node.RequestedKnown = known
		if known {
			node.Requested = requested[nodes[i].Name]
		}
		out = append(out, *node)
	}

	return out, podErr
}

// buildNode shapes one API node into the model.
func buildNode(n *corev1.Node) *inventory.GPUNode {
	out := inventory.NewGPUNode(n.Name)
	out.ObjectMeta = metav1.ObjectMeta{
		Name:              n.Name,
		Labels:            n.Labels,
		CreationTimestamp: n.CreationTimestamp,
	}

	if q, found := n.Status.Allocatable[source.ResourceGPU]; found {
		out.Allocatable = q.Value()
	}
	if q, found := n.Status.Capacity[source.ResourceGPU]; found {
		out.Capacity = q.Value()
	}

	out.Clique = n.Labels[LabelClique]
	out.Topology = map[string]string{}
	for label, value := range n.Labels {
		if isTopographLabel(label) {
			out.Topology[label] = value
		}
	}

	// Whether a pod could land here at all. A node missing either of these is
	// a node the fit has to leave out, so a builder that does not read them
	// hands back a cluster where nothing fits anywhere.
	out.Unschedulable = n.Spec.Unschedulable
	out.Ready = isReady(n)

	return out
}

// isReady reports whether the node's Ready condition is True. A node with no
// conditions at all is not Ready: nothing said it was, and reading an absent
// condition as healthy turns a node that never registered into a placement.
func isReady(n *corev1.Node) bool {
	for i := range n.Status.Conditions {
		if n.Status.Conditions[i].Type == corev1.NodeReady {
			return n.Status.Conditions[i].Status == corev1.ConditionTrue
		}
	}

	return false
}

// requestedByNode sums the GPUs the pods on each node are holding.
//
// A pod holds its GPUs from the moment it is scheduled until it reaches a
// terminal phase, so Pending and Running count and Succeeded and Failed do
// not. A pod with no node assigned is holding nothing anywhere.
func requestedByNode(ctx context.Context, src NodeReader) (map[string]int64, error) {
	pods, err := src.Pods(ctx, "", "")
	if err != nil {
		return nil, err
	}

	out := make(map[string]int64, len(pods))
	for i := range pods {
		pod := &pods[i]
		if pod.Spec.NodeName == "" {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		out[pod.Spec.NodeName] += podGPUs(&pod.Spec)
	}

	return out, nil
}
