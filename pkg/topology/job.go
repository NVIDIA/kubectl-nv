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
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// forJobPrefix is the only --for target this command answers about.
const forJobPrefix = "job/"

// ParseForTarget reads a --for value and returns the job name.
//
// Only job/<name> is accepted. A pod, a deployment or a bare name is refused
// rather than guessed at: the answer this command gives is read as a
// placement decision, and giving one about the wrong object is worse than
// giving none.
func ParseForTarget(value string) (string, error) {
	name, found := strings.CutPrefix(value, forJobPrefix)
	if !found || name == "" || strings.Contains(name, "/") {
		return "", output.NewUsageError(
			"cannot read --for %q: want job/<name>, for instance --for job/trainer", value)
	}

	return name, nil
}

// JobRequestFrom reads what a job needs from the cluster: how many pods run
// at once, and what one of them asks for.
func JobRequestFrom(job *batchv1.Job) (JobRequest, error) {
	req := JobRequest{
		Namespace: job.Namespace,
		Name:      job.Name,
		Pods:      podsOf(job),
		// Swap for pkg/inventory's exported effective-request function, which
		// takes a corev1.PodSpec, once D1 has merged it. podGPUs below is a
		// second copy of that rule and the two must not be left to drift.
		GPUsPerPod: podGPUs(&job.Spec.Template.Spec),
	}

	if req.Pods <= 0 {
		return JobRequest{}, output.NewUsageError(
			"job %s/%s runs %d pods: there is no placement to answer about",
			job.Namespace, job.Name, req.Pods)
	}

	if req.GPUsPerPod <= 0 {
		return JobRequest{}, output.NewUsageError(
			"job %s/%s requests no %s: there is no GPU topology answer to give about it",
			job.Namespace, job.Name, source.ResourceGPU)
	}

	return req, nil
}

// podsOf is how many pods the job runs at once: spec.parallelism, capped by
// spec.completions when that is set and smaller.
//
// The cap is what Kueue computes for the same question (podsCount in its Job
// integration): a job with twenty completions running eight at a time needs
// room for eight, but one with two completions never runs more than two
// however high its parallelism is, and reserving eight nodes for it would
// answer that a cluster is too small when it is not.
//
// An unset parallelism is one pod, which is the API's own default.
func podsOf(job *batchv1.Job) int64 {
	pods := int64(1)
	if job.Spec.Parallelism != nil {
		pods = int64(*job.Spec.Parallelism)
	}

	if job.Spec.Completions != nil && int64(*job.Spec.Completions) < pods {
		pods = int64(*job.Spec.Completions)
	}

	return pods
}

// podGPUs is the effective nvidia.com/gpu request of one pod, by the rule the
// scheduler uses (k8s.io/component-helpers/resource, which follows the
// sidecar KEP):
//
//	result = the containers, summed, plus every native sidecar
//	then    raised to the largest InitContainerUse(i), where
//	        InitContainerUse(i) = the i-th init container plus every native
//	        sidecar declared before it
//
// A native sidecar is an init container whose restart policy is Always. It
// does not finish before the containers start, it runs beside them, so its
// GPUs are held at the same time as theirs and add to them. A plain init
// container runs on its own, before the containers, so the pod needs
// whichever is larger: what it wants, or what the containers want.
//
// Taking the larger of the two groups and never the sum is the mistake worth
// naming: a 4-GPU trainer with a 1-GPU sidecar needs a node with five free,
// and answering four places it on a node where it will sit Pending.
func podGPUs(spec *corev1.PodSpec) int64 {
	var containers int64
	for i := range spec.Containers {
		containers += containerGPUs(&spec.Containers[i])
	}

	var sidecars, largestInit int64
	for i := range spec.InitContainers {
		init := &spec.InitContainers[i]
		need := containerGPUs(init)

		if isNativeSidecar(init) {
			// It runs alongside the containers, so it adds to them, and it is
			// also already running while any later init container runs.
			containers += need
			sidecars += need
			need = sidecars
		} else {
			need += sidecars
		}

		if need > largestInit {
			largestInit = need
		}
	}

	if largestInit > containers {
		return largestInit
	}

	return containers
}

// isNativeSidecar reports whether an init container is one: restartPolicy
// Always on an init container is what makes it run beside the containers
// instead of before them.
func isNativeSidecar(c *corev1.Container) bool {
	return c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways
}

// containerGPUs is what one container asks for.
//
// An extended resource cannot be overcommitted, so its request and its limit
// are the same number once a pod exists. A Job's pod template is not a pod:
// it is stored as written, and the defaulting that copies limits into
// requests runs when the pod is created from it, so a template carrying only
// limits is ordinary and asks for exactly those GPUs.
func containerGPUs(c *corev1.Container) int64 {
	if q, found := c.Resources.Requests[corev1.ResourceName(source.ResourceGPU)]; found {
		return q.Value()
	}
	if q, found := c.Resources.Limits[corev1.ResourceName(source.ResourceGPU)]; found {
		return q.Value()
	}

	return 0
}
