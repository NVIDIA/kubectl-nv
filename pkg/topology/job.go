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

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
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
		// The effective request is pkg/inventory's rule, applied to the pod
		// template rather than to a running pod: the sidecars, the init
		// container peak, the limits fallback for a template that was never
		// defaulted, and nvidia.com/gpu plus nvidia.com/gpu.shared. A second
		// copy of that here is a second thing to keep in step with the
		// scheduler.
		GPUsPerPod: inventory.PodSpecGPURequest(&job.Spec.Template.Spec),
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
