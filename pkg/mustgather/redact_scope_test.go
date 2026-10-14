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

package mustgather

import (
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"
)

// What a tenant's pod carries that a support bundle has no business holding.
// Every one of these is asserted against by its exact value: a projection that
// kept the field and rewrote it would still be carrying it.
const (
	plantedPodEnv        = "planted-env-value-0ff1ce"
	plantedPodArg        = "planted-arg-s3cr3t"
	plantedPodCommand    = "planted-command-hunter2"
	plantedPodAnnotation = "planted-annotation-private"
)

// tenantGPUPod is somebody else's workload: it asks for GPUs, so the inventory
// counts it, and everything else on it belongs to them.
func tenantGPUPod(name, namespace, node string) *v1.Pod {
	request := v1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(2, resource.DecimalSI)}
	always := v1.ContainerRestartPolicyAlways

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			UID:         types.UID("11111111-2222-3333-4444-555555555555"),
			Labels:      map[string]string{"app": "trainer"},
			Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": plantedPodAnnotation},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "batch/v1", Kind: "Job", Name: "train", UID: types.UID("deadbeef"),
			}},
		},
		Spec: v1.PodSpec{
			NodeName: node,
			Overhead: v1.ResourceList{"cpu": *resource.NewMilliQuantity(250, resource.DecimalSI)},
			Volumes:  []v1.Volume{{Name: "dataset", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}}},
			InitContainers: []v1.Container{{
				Name:          "fetch",
				RestartPolicy: &always,
				Command:       []string{"/bin/sh", "-c", plantedPodCommand},
			}},
			Containers: []v1.Container{{
				Name:      "trainer",
				Image:     "registry.example.com/team/trainer:v1",
				Command:   []string{plantedPodCommand},
				Args:      []string{"--dataset", plantedPodArg},
				Env:       []v1.EnvVar{{Name: "DATASET_URL", Value: plantedPodEnv}},
				Resources: v1.ResourceRequirements{Requests: request, Limits: request},
			}},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning, HostIP: "10.42.7.11", PodIP: "10.244.3.5"},
	}
}

// tenantCluster is the fixture cluster with somebody else's workload on it.
func tenantCluster() []runtime.Object {
	return append(twoNodeCluster(), tenantGPUPod("trainer", "research", "gpu-node-1"))
}

// plantedPodSecrets is everything of a tenant's pod that must not reach a
// bundle at all.
func plantedPodSecrets() []string {
	return []string{plantedPodEnv, plantedPodArg, plantedPodCommand, plantedPodAnnotation}
}

// A bundle goes to another company. The inventory needs a GPU pod's node, its
// resource requests and its phase to say how many of a node's GPUs are in use;
// it needs none of the command a tenant runs, the arguments it runs it with,
// its environment or its annotations, and a support bundle that carries them
// is carrying what nobody meant to send.
//
// Redaction is not the answer here and is not what this checks: these fields
// are not collected, with or without --redact, so there is nothing to rewrite
// and nothing a pattern can miss.
func TestAGPUPodIsCollectedAsTheInventoryReadsItAndNoMore(t *testing.T) {
	for _, redact := range []bool{false, true} {
		name := "without --redact"
		if redact {
			name = "with --redact"
		}

		t.Run(name, func(t *testing.T) {
			dir, names, err := runGather(t,
				&Options{Redact: redact, ToolVersion: "v1.2.3"}, tenantCluster()...)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			for _, file := range names {
				text := readArtifact(t, dir, file)
				for _, planted := range plantedPodSecrets() {
					if strings.Contains(text, planted) {
						t.Errorf("%s holds %q, which no reader of a bundle needs", file, planted)
					}
				}
			}
		})
	}
}

// The projection has to keep what the readers read. Each field below is one a
// reader reaches for, named with the reader that reaches for it: dropping any
// of them makes a bundle that reads as a cluster with nothing running on it.
func TestAGPUPodKeepsWhatTheReadersRead(t *testing.T) {
	dir, _, err := runGather(t, &Options{ToolVersion: "v1.2.3"}, tenantCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var list v1.PodList
	if err := yaml.Unmarshal([]byte(readArtifact(t, dir, gpuPodsFile)), &list); err != nil {
		t.Fatalf("%s does not parse: %v", gpuPodsFile, err)
	}

	var pod *v1.Pod
	for i := range list.Items {
		if list.Items[i].Name == "trainer" {
			pod = &list.Items[i]
		}
	}
	if pod == nil {
		t.Fatalf("%s does not hold the tenant pod", gpuPodsFile)
	}

	// inventory.requestedByNode keys by this and skips a pod with none.
	if pod.Spec.NodeName != "gpu-node-1" {
		t.Errorf("spec.nodeName is %q, want gpu-node-1", pod.Spec.NodeName)
	}
	// inventory.requestedByNode drops a Succeeded or Failed pod by it.
	if pod.Status.Phase != v1.PodRunning {
		t.Errorf("status.phase is %q, want Running", pod.Status.Phase)
	}
	// inventory.PodSpecGPURequest reads these two, and the init container's
	// restartPolicy is what tells a sidecar from a plain init container.
	if got := pod.Spec.Containers[0].Resources.Requests["nvidia.com/gpu"]; got.Value() != 2 {
		t.Errorf("the container requests %v GPUs, want 2", got.Value())
	}
	if len(pod.Spec.InitContainers) != 1 || pod.Spec.InitContainers[0].RestartPolicy == nil {
		t.Errorf("the init container lost its restartPolicy: %+v", pod.Spec.InitContainers)
	}
	// inventory.operandFromPod reads these when the pod is an operand.
	if pod.Namespace != "research" || pod.Labels["app"] != "trainer" {
		t.Errorf("the pod lost its namespace or labels: %q %v", pod.Namespace, pod.Labels)
	}
	if len(pod.OwnerReferences) != 1 || pod.UID == "" {
		t.Errorf("the pod lost its owner or uid: %+v %q", pod.OwnerReferences, pod.UID)
	}

	// And nothing else of the spec: a projection that kept a field nobody
	// reads is a projection that will keep the next one too.
	if len(pod.Spec.Volumes) != 0 {
		t.Errorf("the projection kept %d volumes", len(pod.Spec.Volumes))
	}
	if len(pod.Spec.Containers[0].Env) != 0 || len(pod.Spec.Containers[0].Args) != 0 {
		t.Errorf("the projection kept the container's env or args: %+v", pod.Spec.Containers[0])
	}
	if pod.Spec.Containers[0].Image != "" {
		t.Errorf("the projection kept the image %q, which only an operand's is read for",
			pod.Spec.Containers[0].Image)
	}
	if len(pod.Annotations) != 0 {
		t.Errorf("the projection kept the annotations: %v", pod.Annotations)
	}
}
