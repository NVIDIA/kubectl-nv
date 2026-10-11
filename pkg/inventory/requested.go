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

package inventory

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// requestedByNode sums the effective nvidia.com/gpu request of every pod that
// is still holding devices, keyed by the node it is on.
//
// A pod that has reached Succeeded or Failed has given its devices back, so it
// is not counted. Everything else is: a Pending pod that the scheduler has
// already bound to a node is holding that node's GPUs as surely as a Running
// one, and an Unknown pod is a pod nobody can say has let go.
//
// A pod with no node is nobody's. It has not been scheduled, so counting it
// against any node would report demand the node is not carrying.
func requestedByNode(pods []corev1.Pod) map[string]int64 {
	var out map[string]int64

	for i := range pods {
		pod := &pods[i]
		if pod.Spec.NodeName == "" {
			continue
		}

		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}

		n := podGPURequest(pod)
		if n == 0 {
			continue
		}

		if out == nil {
			out = map[string]int64{}
		}

		out[pod.Spec.NodeName] += n
	}

	return out
}

// podGPURequest is the nvidia.com/gpu a pod asks the scheduler for, by the
// scheduler's own rule.
//
// The containers run together, so their requests add up. The init containers
// run one at a time and before the containers, so only the largest of them
// counts, and it counts against the containers rather than on top of them.
//
// A restartable init container, that is, a sidecar, is the exception: it keeps
// running once it has started, so it is held for the whole life of the pod. It
// adds to the container sum, and it also adds to every init container that
// starts after it, because those two do run at the same time.
//
// This is k8s.io/component-helpers' PodRequests narrowed to one resource.
// Reimplementing it rather than vendoring that package keeps the rule visible
// next to the tests that pin it; getting it wrong is how a half full node
// reads as a full one.
func podGPURequest(pod *corev1.Pod) int64 {
	var containers int64
	for i := range pod.Spec.Containers {
		containers += gpuRequest(&pod.Spec.Containers[i])
	}

	var sidecars, peak int64

	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if isRestartable(c) {
			sidecars += gpuRequest(c)
			peak = max(peak, sidecars)

			continue
		}

		// A plain init container runs alone, except for whatever sidecars
		// started before it.
		peak = max(peak, sidecars+gpuRequest(c))
	}

	return max(peak, containers+sidecars)
}

// isRestartable reports whether an init container is a sidecar, that is, one
// the kubelet keeps running for the life of the pod.
func isRestartable(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// gpuRequest is one container's nvidia.com/gpu request. Only whole GPUs are
// counted: a MIG profile is a resource of its own and is reported in
// MIG.Resources rather than folded into this number.
func gpuRequest(c *corev1.Container) int64 {
	q, ok := c.Resources.Requests[source.ResourceGPU]
	if !ok {
		// An extended resource may be written as a limit alone, and the
		// API server then defaults the request to it. A pod admitted before
		// that defaulting, or one read out of a bundle, can still arrive with
		// only the limit set.
		q, ok = c.Resources.Limits[source.ResourceGPU]
		if !ok {
			return 0
		}
	}

	return q.Value()
}
