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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// gpuContainer is a container asking for n whole GPUs. A container asking for
// none is written with no resources at all, which is what most pods on a GPU
// node look like.
func gpuContainer(name string, n int64) corev1.Container {
	c := corev1.Container{Name: name}
	if n > 0 {
		c.Resources = corev1.ResourceRequirements{
			Requests: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(n, resource.DecimalSI)},
			Limits:   corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(n, resource.DecimalSI)},
		}
	}

	return c
}

// sidecar is a restartable init container, which the scheduler counts into the
// pod's request for the whole of the pod's life rather than only while it runs.
func sidecar(name string, n int64) corev1.Container {
	c := gpuContainer(name, n)
	always := corev1.ContainerRestartPolicyAlways
	c.RestartPolicy = &always

	return c
}

func testPod(name, node string, phase corev1.PodPhase, init, containers []corev1.Container) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1.PodSpec{
			NodeName:       node,
			InitContainers: init,
			Containers:     containers,
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func TestPodGPURequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pod  corev1.Pod
		want int64
	}{
		{
			name: "a limit with no request still counts",
			pod: testPod("limit-only", "n1", corev1.PodRunning, nil, []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					// An extended resource may be written as a limit alone,
					// and the API server defaults the request to it. A pod
					// read out of a bundle, or admitted before that
					// defaulting, arrives with only the limit set, and
					// counting it as zero reports an occupied node as free.
					Limits: corev1.ResourceList{
						"nvidia.com/gpu": *resource.NewQuantity(2, resource.DecimalSI),
					},
				},
			}}),
			want: 2,
		},
		{
			name: "the request wins over a larger limit",
			pod: testPod("both", "n1", corev1.PodRunning, nil, []corev1.Container{{
				Name: "app",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						"nvidia.com/gpu": *resource.NewQuantity(1, resource.DecimalSI),
					},
					Limits: corev1.ResourceList{
						"nvidia.com/gpu": *resource.NewQuantity(4, resource.DecimalSI),
					},
				},
			}}),
			want: 1,
		},
		{
			name: "no container asks for a GPU",
			pod:  testPod("none", "n1", corev1.PodRunning, nil, []corev1.Container{gpuContainer("app", 0)}),
			want: 0,
		},
		{
			name: "containers are summed",
			pod: testPod("sum", "n1", corev1.PodRunning, nil, []corev1.Container{
				gpuContainer("a", 2), gpuContainer("b", 3),
			}),
			want: 5,
		},
		{
			name: "an init container larger than the containers wins",
			pod: testPod("init-wins", "n1", corev1.PodRunning,
				[]corev1.Container{gpuContainer("prepare", 8)},
				[]corev1.Container{gpuContainer("app", 1)}),
			want: 8,
		},
		{
			name: "containers larger than the init container win",
			pod: testPod("containers-win", "n1", corev1.PodRunning,
				[]corev1.Container{gpuContainer("prepare", 1)},
				[]corev1.Container{gpuContainer("a", 2), gpuContainer("b", 2)}),
			want: 4,
		},
		{
			name: "only the largest init container counts, they do not run together",
			pod: testPod("init-max", "n1", corev1.PodRunning,
				[]corev1.Container{gpuContainer("one", 3), gpuContainer("two", 5)},
				[]corev1.Container{gpuContainer("app", 1)}),
			want: 5,
		},
		{
			name: "a sidecar is added to the containers, not maximised against them",
			pod: testPod("sidecar", "n1", corev1.PodRunning,
				[]corev1.Container{sidecar("proxy", 1)},
				[]corev1.Container{gpuContainer("app", 2)}),
			want: 3,
		},
		{
			name: "a sidecar is still held while a later init container runs",
			pod: testPod("sidecar-then-init", "n1", corev1.PodRunning,
				[]corev1.Container{sidecar("proxy", 1), gpuContainer("prepare", 4)},
				[]corev1.Container{gpuContainer("app", 1)}),
			// 1 + 4 while prepare runs, against 1 + 1 once the pod settles.
			want: 5,
		},
		{
			name: "an init container before a sidecar does not carry it",
			pod: testPod("init-then-sidecar", "n1", corev1.PodRunning,
				[]corev1.Container{gpuContainer("prepare", 4), sidecar("proxy", 1)},
				[]corev1.Container{gpuContainer("app", 1)}),
			// prepare runs alone at 4, and the pod then settles at 1 + 1.
			want: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := podGPURequest(&tt.pod); got != tt.want {
				t.Errorf("podGPURequest = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestRequestedByNodeCountsEveryLivePhase pins which pods hold a GPU. A
// Succeeded or Failed pod has given its devices back; everything else,
// Pending included, is holding or about to hold them.
func TestRequestedByNodeCountsEveryLivePhase(t *testing.T) {
	t.Parallel()

	for _, phase := range []corev1.PodPhase{corev1.PodPending, corev1.PodRunning, corev1.PodUnknown} {
		t.Run(string(phase)+" counts", func(t *testing.T) {
			t.Parallel()

			pods := []corev1.Pod{testPod("p", "n1", phase, nil, []corev1.Container{gpuContainer("app", 2)})}
			if got := requestedByNode(pods)["n1"]; got != 2 {
				t.Errorf("requested on n1 = %d, want 2 for a %s pod", got, phase)
			}
		})
	}

	for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
		t.Run(string(phase)+" does not count", func(t *testing.T) {
			t.Parallel()

			pods := []corev1.Pod{testPod("p", "n1", phase, nil, []corev1.Container{gpuContainer("app", 2)})}
			if got := requestedByNode(pods); len(got) != 0 {
				t.Errorf("requestedByNode = %#v, want nothing for a %s pod", got, phase)
			}
		})
	}
}

// TestRequestedByNodeKeepsPodsOnTheirOwnNode is the mistake that reports a
// cluster's whole GPU demand against every node in it.
func TestRequestedByNodeKeepsPodsOnTheirOwnNode(t *testing.T) {
	t.Parallel()

	pods := []corev1.Pod{
		testPod("a", "n1", corev1.PodRunning, nil, []corev1.Container{gpuContainer("app", 2)}),
		testPod("b", "n2", corev1.PodRunning, nil, []corev1.Container{gpuContainer("app", 3)}),
		testPod("c", "n1", corev1.PodRunning, nil, []corev1.Container{gpuContainer("app", 1)}),
		// Unscheduled: it is nobody's yet.
		testPod("d", "", corev1.PodPending, nil, []corev1.Container{gpuContainer("app", 4)}),
	}

	got := requestedByNode(pods)
	want := map[string]int64{"n1": 3, "n2": 3}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("requestedByNode = %#v, want %#v", got, want)
	}
}

// TestRequestedByNodeIgnoresOtherResources pins that only nvidia.com/gpu is
// counted: a node's CPU demand is not its GPU demand, and a MIG profile is
// reported in MIG.Resources rather than folded into the whole-GPU number.
func TestRequestedByNodeIgnoresOtherResources(t *testing.T) {
	t.Parallel()

	pod := testPod("cpu-only", "n1", corev1.PodRunning, nil, []corev1.Container{{
		Name: "app",
		Resources: corev1.ResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:       resource.MustParse("8"),
				"nvidia.com/mig-1g.10gb": *resource.NewQuantity(2, resource.DecimalSI),
			},
		},
	}})

	if got := requestedByNode([]corev1.Pod{pod}); len(got) != 0 {
		t.Errorf("requestedByNode = %#v, want nothing: neither resource is a whole GPU", got)
	}
}

// TestGPUNodeRequestedComesFromThePods is the same rule seen through the
// exported surface: the number a reader gets off a GPUNode is the one the
// scheduler would have computed.
func TestGPUNodeRequestedComesFromThePods(t *testing.T) {
	t.Parallel()

	node := testNode("n1", nil,
		map[string]string{"nvidia.com/gpu": "8"}, map[string]string{"nvidia.com/gpu": "8"})
	pods := []corev1.Pod{
		testPod("running", "n1", corev1.PodRunning, nil, []corev1.Container{gpuContainer("app", 2)}),
		testPod("sidecar", "n1", corev1.PodRunning,
			[]corev1.Container{sidecar("proxy", 1)}, []corev1.Container{gpuContainer("app", 1)}),
		testPod("done", "n1", corev1.PodSucceeded, nil, []corev1.Container{gpuContainer("app", 4)}),
	}

	got := buildNode(node, nodeContext{requested: requestedByNode(pods)["n1"], requestedKnown: true})
	if got.Requested != 4 {
		t.Errorf("GPUNode.Requested = %d, want 4 (2 + (1 + 1), with the Succeeded pod left out)", got.Requested)
	}
}
