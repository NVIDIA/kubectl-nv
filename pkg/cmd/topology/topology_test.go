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
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	cmdtopology "github.com/NVIDIA/kubectl-nv/pkg/cmd/topology"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// fakeFactory hands the command fake clients instead of real ones.
type fakeFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

func (f *fakeFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f *fakeFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f *fakeFactory) RESTConfig() (*rest.Config, error)               { return &rest.Config{}, nil }

// gpuNode builds a node advertising whole GPUs, in the given clique and
// fabric tier.
func gpuNode(name, clique string, allocatable int64) *corev1.Node {
	quantity := *resource.NewQuantity(allocatable, resource.DecimalSI)

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			"nvidia.com/gpu.clique":                       clique,
			"fabric.topograph.run/tier-1":                 "fabric-1",
			"feature.node.kubernetes.io/pci-10de.present": "true",
		}},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": quantity},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": quantity},
		},
	}
}

// gpuPod builds a running pod holding GPUs on a node.
func gpuPod(name, nodeName string, count int64) *corev1.Pod {
	request := corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(count, resource.DecimalSI)}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "research"},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:      "trainer",
				Resources: corev1.ResourceRequirements{Requests: request, Limits: request},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// gpuJob builds a Job whose pods each want count GPUs.
func gpuJob(name string, parallelism int32, count int64) *batchv1.Job {
	request := corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(count, resource.DecimalSI)}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "research"},
		Spec: batchv1.JobSpec{
			Parallelism: &parallelism,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:      "trainer",
					Resources: corev1.ResourceRequirements{Requests: request, Limits: request},
				}},
			}},
		},
	}
}

// run executes the topology command over a cluster of the given objects and
// returns what it printed on stdout.
func run(t *testing.T, args []string, objects ...runtime.Object) (string, error) {
	t.Helper()

	out := &bytes.Buffer{}
	streams := genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: out, ErrOut: &bytes.Buffer{}}

	namespace := "research"
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.Namespace = &namespace

	o := &cmd.Options{
		ConfigFlags: configFlags,
		Streams:     streams,
		Factory: &fakeFactory{
			clientset: fake.NewSimpleClientset(objects...),
			dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
				runtime.NewScheme(),
				map[schema.GroupVersionResource]string{
					{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "topologies"}:      "TopologyList",
					{Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "resourceflavors"}: "ResourceFlavorList",
					{Group: "kueue.x-k8s.io", Version: "v1beta1", Resource: "topologies"}:      "TopologyList",
					{Group: "kueue.x-k8s.io", Version: "v1beta1", Resource: "resourceflavors"}: "ResourceFlavorList",
				},
			),
		},
	}

	c := cmdtopology.NewCommand(o)
	c.SetArgs(args)
	c.SetOut(out)
	c.SetErr(streams.ErrOut)
	c.SilenceUsage = true
	c.SilenceErrors = true

	return out.String(), c.Execute()
}

// decode reads the JSON report the command printed.
func decode(t *testing.T, raw string) map[string]any {
	t.Helper()

	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}

	return got
}

// TestTopologyGroupsTheClusterByClique is the command end to end: two nodes
// in one clique, one of them holding a GPU already.
func TestTopologyGroupsTheClusterByClique(t *testing.T) {
	raw, err := run(t, []string{"-o", "json"},
		gpuNode("node-a", "clique-a", 4),
		gpuNode("node-b", "clique-a", 4),
		gpuPod("busy", "node-a", 3),
	)
	if err != nil {
		t.Fatalf("topology -o json: %v", err)
	}

	got := decode(t, raw)
	if got["kind"] != "TopologyReport" {
		t.Fatalf("kind is %v, want TopologyReport: %s", got["kind"], raw)
	}

	levels, _ := got["levels"].([]any)
	if len(levels) == 0 {
		t.Fatalf("the report carries no levels: %s", raw)
	}

	first, _ := levels[0].(map[string]any)
	if first["name"] != "clique" {
		t.Fatalf("the first level is %v, want the clique: %s", first["name"], raw)
	}

	groups, _ := first["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("the clique level has %d groups, want 1: %s", len(groups), raw)
	}

	group, _ := groups[0].(map[string]any)
	for field, want := range map[string]float64{
		"nodeCount":       2,
		"allocatable":     8,
		"requested":       3,
		"free":            5,
		"largestFreeNode": 4,
	} {
		if got, _ := group[field].(float64); got != want {
			t.Errorf("%s is %v, want %v: %s", field, group[field], want, raw)
		}
	}
}

// TestTopologyReportsKueueAsMissing: a cluster with no Kueue CRDs is an
// answer, not a failure.
func TestTopologyReportsKueueAsMissing(t *testing.T) {
	raw, err := run(t, []string{"-o", "json"}, gpuNode("node-a", "clique-a", 4))
	if err != nil {
		t.Fatalf("topology -o json: %v", err)
	}

	kueue, _ := decode(t, raw)["kueue"].(map[string]any)
	if kueue == nil {
		t.Fatalf("the report says nothing about Kueue: %s", raw)
	}
	if kueue["installed"] != false {
		t.Errorf("Kueue is reported as installed: %s", raw)
	}
	if reason, _ := kueue["reason"].(string); reason == "" {
		t.Errorf("the report gives no reason why there are no Kueue levels: %s", raw)
	}
}

// TestTopologyAnswersForAJob covers --for end to end: two pods of four GPUs
// against two idle nodes of four in one clique.
func TestTopologyAnswersForAJob(t *testing.T) {
	raw, err := run(t, []string{"-o", "json", "--for", "job/trainer"},
		gpuNode("node-a", "clique-a", 4),
		gpuNode("node-b", "clique-a", 4),
		gpuJob("trainer", 2, 4),
	)
	if err != nil {
		t.Fatalf("topology --for job/trainer: %v", err)
	}

	fit, _ := decode(t, raw)["fit"].(map[string]any)
	if fit == nil {
		t.Fatalf("the report carries no answer about the job: %s", raw)
	}
	if fit["smallest"] != "clique" {
		t.Errorf("the smallest level that fits is %v, want the clique: %s", fit["smallest"], raw)
	}
	if got, _ := fit["gpusPerPod"].(float64); got != 4 {
		t.Errorf("GPUs per pod is %v, want 4: %s", fit["gpusPerPod"], raw)
	}
}

// TestTopologyExitsTwoForAMissingJob: a job that is not there is something
// the user typed wrong, which is exit 2 and not a report of nothing.
func TestTopologyExitsTwoForAMissingJob(t *testing.T) {
	_, err := run(t, []string{"-o", "json", "--for", "job/absent"}, gpuNode("node-a", "clique-a", 4))
	if err == nil {
		t.Fatal("asking about a job that does not exist succeeded")
	}

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError so the command exits 2", err, err)
	}
	if !strings.Contains(err.Error(), "absent") {
		t.Errorf("the error is %q, want it to name the job", err)
	}
}

// TestTopologyExitsTwoForAJobWithoutGPUs.
func TestTopologyExitsTwoForAJobWithoutGPUs(t *testing.T) {
	_, err := run(t, []string{"-o", "json", "--for", "job/trainer"},
		gpuNode("node-a", "clique-a", 4),
		gpuJob("trainer", 2, 0),
	)

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError so the command exits 2", err, err)
	}
}

// TestTopologyRejectsAForTargetItCannotAnswer.
func TestTopologyRejectsAForTargetItCannotAnswer(t *testing.T) {
	_, err := run(t, []string{"--for", "pod/trainer"}, gpuNode("node-a", "clique-a", 4))

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
}

// TestTopologyPrintsATable is the default output: a reader who ran the
// command with no flags gets columns, not JSON.
func TestTopologyPrintsATable(t *testing.T) {
	raw, err := run(t, nil, gpuNode("node-a", "clique-a", 4))
	if err != nil {
		t.Fatalf("topology: %v", err)
	}

	for _, want := range []string{"LEVEL", "GROUP", "NODES", "FREE", "clique", "clique-a"} {
		if !strings.Contains(raw, want) {
			t.Errorf("the table does not carry %q:\n%s", want, raw)
		}
	}
}

// TestTopologyRejectsAnUnknownOutputFormat keeps -o a closed set, so a typo
// is exit 2 rather than a silent fall back to the table.
func TestTopologyRejectsAnUnknownOutputFormat(t *testing.T) {
	_, err := run(t, []string{"-o", "toml"}, gpuNode("node-a", "clique-a", 4))

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
}
