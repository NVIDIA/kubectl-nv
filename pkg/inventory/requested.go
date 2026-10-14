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

// PodSpecGPURequest is the number of GPUs one pod asks the scheduler for, by
// the scheduler's own rule.
//
// It takes a PodSpec rather than a Pod so that a caller holding a pod template
// can use it: a Job's spec.template.spec is a PodSpec, and the question "how
// many GPUs will each of this Job's pods need" has to be answered before any
// of those pods exist. The node inventory and that question then come out of
// one rule rather than two that agree until one of them is edited.
//
// The rule:
//
//   - The containers run together, so their requests add up.
//   - The init containers run one at a time and before the containers, so only
//     the largest of them counts, and it counts against the container sum
//     rather than on top of it.
//   - A restartable init container, that is, a sidecar, is the exception. It
//     keeps running once it has started, so it is held for the whole life of
//     the pod: it adds to the container sum, and it also adds to every init
//     container that starts after it, because those two do run at once.
//
// Two resource names are counted and summed, nvidia.com/gpu and
// nvidia.com/gpu.shared, because under renameByDefault the second is the only
// name a workload can ask for. A request written as a limit alone counts as
// that limit. A MIG profile does not count: it is a different unit.
//
// What this cannot do is decide whether the pod still holds those GPUs. A
// PodSpec carries no status, so the phase filter belongs to the caller;
// requestedByNode applies it for the node inventory.
//
// This is k8s.io/component-helpers' PodRequests narrowed to these resources.
// Reimplementing it rather than vendoring that package keeps the rule visible
// next to the tests that pin it; getting it wrong is how a half full node
// reads as a full one.
func PodSpecGPURequest(spec *corev1.PodSpec) int64 {
	if spec == nil {
		return 0
	}

	var containers int64
	for i := range spec.Containers {
		containers += gpuRequest(&spec.Containers[i])
	}

	var sidecars, peak int64

	for i := range spec.InitContainers {
		c := &spec.InitContainers[i]
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

// podGPURequest is PodSpecGPURequest over a whole pod. It is the one call the
// node inventory makes, so that the two can never be different rules.
func podGPURequest(pod *corev1.Pod) int64 {
	return PodSpecGPURequest(&pod.Spec)
}

// isRestartable reports whether an init container is a sidecar, that is, one
// the kubelet keeps running for the life of the pod.
func isRestartable(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// gpuRequest is one container's whole-GPU request, summed over the two names
// a whole GPU is advertised under.
//
// nvidia.com/gpu.shared is the only name a workload can ask for on a node
// configured with renameByDefault, so leaving it out reports every busy node
// on such a cluster as idle. A MIG profile is not counted: it is a resource of
// its own and is reported in MIG.Resources rather than folded into this
// number.
func gpuRequest(c *corev1.Container) int64 {
	return containerRequest(c, source.ResourceGPU) + containerRequest(c, source.ResourceGPUShared)
}

// containerRequest reads one resource off a container.
func containerRequest(c *corev1.Container, name corev1.ResourceName) int64 {
	q, ok := c.Resources.Requests[name]
	if !ok {
		// An extended resource may be written as a limit alone, and the
		// API server then defaults the request to it. A pod admitted before
		// that defaulting, or one read out of a bundle, can still arrive with
		// only the limit set, and counting it as zero reports an occupied node
		// as free.
		q, ok = c.Resources.Limits[name]
		if !ok {
			return 0
		}
	}

	return q.Value()
}
