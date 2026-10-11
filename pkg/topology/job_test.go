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

package topology_test

import (
	"errors"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// gpus returns a resource list asking for n GPUs, and an empty one for zero,
// which is how a container that wants no GPU is written.
func gpus(n int64) corev1.ResourceList {
	if n == 0 {
		return corev1.ResourceList{}
	}

	return corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(n, resource.DecimalSI)}
}

// job builds a batch/v1 Job with the given parallelism and per-container GPU
// requests. A nil completions leaves the field unset.
func job(parallelism, completions *int32, containers, initContainers []int64) *batchv1.Job {
	spec := corev1.PodSpec{}
	for _, n := range containers {
		spec.Containers = append(spec.Containers, corev1.Container{
			Name:      "main",
			Resources: corev1.ResourceRequirements{Requests: gpus(n), Limits: gpus(n)},
		})
	}
	for _, n := range initContainers {
		spec.InitContainers = append(spec.InitContainers, corev1.Container{
			Name:      "init",
			Resources: corev1.ResourceRequirements{Requests: gpus(n), Limits: gpus(n)},
		})
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "trainer", Namespace: "research"},
		Spec: batchv1.JobSpec{
			Parallelism: parallelism,
			Completions: completions,
			Template:    corev1.PodTemplateSpec{Spec: spec},
		},
	}
}

// sidecar appends a native sidecar: an init container whose restart policy is
// Always, which runs alongside the ordinary containers rather than before
// them, and whose GPUs are therefore held at the same time as theirs.
func sidecar(j *batchv1.Job, n int64) *batchv1.Job {
	always := corev1.ContainerRestartPolicyAlways
	j.Spec.Template.Spec.InitContainers = append(j.Spec.Template.Spec.InitContainers, corev1.Container{
		Name:          "sidecar",
		RestartPolicy: &always,
		Resources:     corev1.ResourceRequirements{Requests: gpus(n), Limits: gpus(n)},
	})

	return j
}

func int32Ptr(n int32) *int32 { return &n }

// TestJobRequestSumsTheContainers: the pod's GPU request is the sum over its
// containers, because they all run at once.
func TestJobRequestSumsTheContainers(t *testing.T) {
	t.Parallel()

	got, err := topology.JobRequestFrom(job(int32Ptr(3), nil, []int64{2, 1}, nil))
	if err != nil {
		t.Fatalf("reading the job: %v", err)
	}

	if got.GPUsPerPod != 3 {
		t.Errorf("GPUs per pod is %d, want 3", got.GPUsPerPod)
	}
	if got.Pods != 3 {
		t.Errorf("pods is %d, want 3", got.Pods)
	}
	if got.Namespace != "research" || got.Name != "trainer" {
		t.Errorf("the request is about %s/%s, want research/trainer", got.Namespace, got.Name)
	}
}

// TestJobRequestTakesTheLargestInitContainerWhenItWins: init containers run
// one at a time and before the rest, so the pod needs whichever is bigger,
// the largest init container or the sum of the containers.
func TestJobRequestTakesTheLargestInitContainerWhenItWins(t *testing.T) {
	t.Parallel()

	got, err := topology.JobRequestFrom(job(nil, nil, []int64{1}, []int64{4, 2}))
	if err != nil {
		t.Fatalf("reading the job: %v", err)
	}

	if got.GPUsPerPod != 4 {
		t.Errorf("GPUs per pod is %d, want 4: the largest init container asks for four", got.GPUsPerPod)
	}
}

// TestJobRequestKeepsTheContainerSumWhenItWins is the other half of the same
// rule, and the one a max-of-everything implementation gets wrong.
func TestJobRequestKeepsTheContainerSumWhenItWins(t *testing.T) {
	t.Parallel()

	got, err := topology.JobRequestFrom(job(nil, nil, []int64{3, 3}, []int64{4}))
	if err != nil {
		t.Fatalf("reading the job: %v", err)
	}

	if got.GPUsPerPod != 6 {
		t.Errorf("GPUs per pod is %d, want 6: two containers of three outweigh one init container of four", got.GPUsPerPod)
	}
}

// TestJobRequestDefaultsParallelismToOne pins the API default: an unset
// spec.parallelism means one pod.
func TestJobRequestDefaultsParallelismToOne(t *testing.T) {
	t.Parallel()

	got, err := topology.JobRequestFrom(job(nil, nil, []int64{1}, nil))
	if err != nil {
		t.Fatalf("reading the job: %v", err)
	}

	if got.Pods != 1 {
		t.Errorf("pods is %d, want 1", got.Pods)
	}
}

// TestJobRequestRejectsAJobWithoutGPUs is the exit 2 case: there is no
// topology answer to give about a job that asks for no GPUs, and reporting
// "it fits everywhere" would be a wrong answer rather than no answer.
func TestJobRequestRejectsAJobWithoutGPUs(t *testing.T) {
	t.Parallel()

	_, err := topology.JobRequestFrom(job(int32Ptr(2), nil, []int64{0}, nil))
	if err == nil {
		t.Fatal("a job asking for no GPUs was accepted")
	}

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError so the command exits 2", err, err)
	}
	if usage.ExitCode() != output.ExitUsage {
		t.Errorf("exit code is %d, want %d", usage.ExitCode(), output.ExitUsage)
	}
}

// TestJobRequestRejectsParallelismZero: a suspended job scaled to zero pods
// has nothing to place, and dividing the cluster into groups that fit nothing
// is not an answer either.
func TestJobRequestRejectsParallelismZero(t *testing.T) {
	t.Parallel()

	_, err := topology.JobRequestFrom(job(int32Ptr(0), nil, []int64{1}, nil))

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError so the command exits 2", err, err)
	}
}

// TestParseForTargetAcceptsAJob covers the --for parsing, which is the only
// form the command takes.
func TestParseForTargetAcceptsAJob(t *testing.T) {
	t.Parallel()

	got, err := topology.ParseForTarget("job/trainer")
	if err != nil {
		t.Fatalf("parsing job/trainer: %v", err)
	}

	if got != "trainer" {
		t.Errorf("the job name is %q, want %q", got, "trainer")
	}
}

// TestParseForTargetRejectsAnythingElse: a pod or a deployment is not
// something this command answers about yet, and reading "pod/x" as a job
// would send the user a confident wrong answer.
func TestParseForTargetRejectsAnythingElse(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"pod/trainer", "trainer", "job/", "job", "jobs/trainer", "job/a/b"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			_, err := topology.ParseForTarget(value)

			var usage *output.UsageError
			if !errors.As(err, &usage) {
				t.Fatalf("--for %q gave %v, want an output.UsageError", value, err)
			}
		})
	}
}

// TestJobRequestCountsANativeSidecar: an init container whose restart policy
// is Always is a native sidecar, which runs alongside the ordinary containers
// instead of before them. The scheduler adds its resources to theirs, so a
// 4-GPU trainer with a 1-GPU sidecar needs a node with five free, and an
// implementation that takes the larger of the two would place it on a node
// with four and watch it sit Pending.
func TestJobRequestCountsANativeSidecar(t *testing.T) {
	t.Parallel()

	got, err := topology.JobRequestFrom(sidecar(job(nil, nil, []int64{4}, nil), 1))
	if err != nil {
		t.Fatalf("reading the job: %v", err)
	}

	if got.GPUsPerPod != 5 {
		t.Errorf("GPUs per pod is %d, want 5: a native sidecar holds its GPUs at the same time as the containers", got.GPUsPerPod)
	}
}

// TestJobRequestCountsASidecarUnderAPlainInitContainer is the rule's other
// half, from the upstream formula: a plain init container runs after the
// sidecars have started, so what it needs is its own request plus theirs.
// Here that is 2 + 1 = 3, which is less than the 4 + 1 the containers and the
// sidecar need together, so the containers still decide.
func TestJobRequestCountsASidecarUnderAPlainInitContainer(t *testing.T) {
	t.Parallel()

	got, err := topology.JobRequestFrom(sidecar(job(nil, nil, []int64{4}, []int64{2}), 1))
	if err != nil {
		t.Fatalf("reading the job: %v", err)
	}

	if got.GPUsPerPod != 5 {
		t.Errorf("GPUs per pod is %d, want 5", got.GPUsPerPod)
	}
}

// TestJobRequestReadsTheLimitWhenThereIsNoRequest: a Job's pod template is
// stored as written and the defaulting that copies limits into requests
// happens when the pod is created, so a template with limits and no requests
// is ordinary and asks for exactly those GPUs.
func TestJobRequestReadsTheLimitWhenThereIsNoRequest(t *testing.T) {
	t.Parallel()

	j := job(nil, nil, []int64{4}, nil)
	j.Spec.Template.Spec.Containers[0].Resources.Requests = nil

	got, err := topology.JobRequestFrom(j)
	if err != nil {
		t.Fatalf("reading the job: %v", err)
	}

	if got.GPUsPerPod != 4 {
		t.Errorf("GPUs per pod is %d, want 4: the limit is what the pod will ask for", got.GPUsPerPod)
	}
}

// TestJobRequestTakesCompletionsWhenItIsSmaller pins the pod count Kueue
// computes: parallelism, capped by completions when that is set, because a
// job that only has two pods left to complete never runs eight at once.
func TestJobRequestTakesCompletionsWhenItIsSmaller(t *testing.T) {
	t.Parallel()

	got, err := topology.JobRequestFrom(job(int32Ptr(8), int32Ptr(2), []int64{1}, nil))
	if err != nil {
		t.Fatalf("reading the job: %v", err)
	}

	if got.Pods != 2 {
		t.Errorf("pods is %d, want 2: completions caps parallelism", got.Pods)
	}
}

// TestJobRequestKeepsParallelismWhenCompletionsIsLarger is the other
// direction: twenty pods to complete, eight at a time, needs room for eight.
func TestJobRequestKeepsParallelismWhenCompletionsIsLarger(t *testing.T) {
	t.Parallel()

	got, err := topology.JobRequestFrom(job(int32Ptr(8), int32Ptr(20), []int64{1}, nil))
	if err != nil {
		t.Fatalf("reading the job: %v", err)
	}

	if got.Pods != 8 {
		t.Errorf("pods is %d, want 8", got.Pods)
	}
}

// TestJobWithoutGPUsNamesItselfAndTheResource pins the message, not just the
// type: it is what the user reads when the command exits 2, and a message
// that does not name the job leaves them guessing which of their jobs it
// means.
func TestJobWithoutGPUsNamesItselfAndTheResource(t *testing.T) {
	t.Parallel()

	_, err := topology.JobRequestFrom(job(int32Ptr(2), nil, []int64{0}, nil))
	if err == nil {
		t.Fatal("a job asking for no GPUs was accepted")
	}

	want := `job research/trainer requests no nvidia.com/gpu: there is no GPU topology answer to give about it`
	if err.Error() != want {
		t.Errorf("the error is\n  %q\nwant\n  %q", err.Error(), want)
	}
}

// TestJobWithNoPodsNamesItself pins the other exit-2 message.
func TestJobWithNoPodsNamesItself(t *testing.T) {
	t.Parallel()

	_, err := topology.JobRequestFrom(job(int32Ptr(0), nil, []int64{1}, nil))
	if err == nil {
		t.Fatal("a job running no pods was accepted")
	}

	want := `job research/trainer runs 0 pods: there is no placement to answer about`
	if err.Error() != want {
		t.Errorf("the error is\n  %q\nwant\n  %q", err.Error(), want)
	}
}

// TestParseForTargetNamesWhatItWanted pins the --for message.
func TestParseForTargetNamesWhatItWanted(t *testing.T) {
	t.Parallel()

	_, err := topology.ParseForTarget("pod/trainer")
	if err == nil {
		t.Fatal("--for pod/trainer was accepted")
	}

	want := `cannot read --for "pod/trainer": want job/<name>, for instance --for job/trainer`
	if err.Error() != want {
		t.Errorf("the error is\n  %q\nwant\n  %q", err.Error(), want)
	}
}
