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
	"errors"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

// gpuRequestingPod is a workload asking for GPUs, in a namespace the operator
// does not own. The inventory counts it to answer "requested", and a bundle
// that never collected it cannot say what is in use on a node.
func gpuRequestingPod(name, namespace, node, resourceName string, count int64) *v1.Pod {
	request := v1.ResourceList{v1.ResourceName(resourceName): *resource.NewQuantity(count, resource.DecimalSI)}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1.PodSpec{
			NodeName: node,
			Containers: []v1.Container{{
				Name:      "trainer",
				Resources: v1.ResourceRequirements{Requests: request, Limits: request},
			}},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}
}

// nodeEvent is an event about a node, which is what NV-UPG-001 reads to date a
// driver upgrade step.
func nodeEvent(name, node string) *v1.Event {
	return &v1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "default"},
		InvolvedObject: v1.ObjectReference{Kind: "Node", Name: node},
		Reason:         "NodeNotSchedulable",
		Message:        "the node is cordoned",
	}
}

// completeCluster is a cluster with GPU work running outside the operator
// namespace and events about its nodes.
func completeCluster() []runtime.Object {
	out := twoNodeCluster()

	return append(out,
		gpuRequestingPod("trainer", "research", "gpu-node-1", "nvidia.com/gpu", 2),
		gpuRequestingPod("shared", "team-b", "gpu-node-2", "nvidia.com/gpu.shared", 1),
		gpuRequestingPod("cpu-only", "research", "gpu-node-1", "cpu", 1),
		nodeEvent("upgrade-1", "gpu-node-1"),
		// A second node's event, so that --node has something to leave out. A
		// fixture with one node's events cannot tell a filter that works from
		// one that was never applied.
		nodeEvent("upgrade-2", "gpu-node-2"),
	)
}

// The inventory needs every pod that asks for GPUs, in every namespace, or it
// cannot say how many of a node's GPUs are in use. must-gather collected the
// operator namespace and nothing else, so a bundle could never answer it.
func TestAGatherCollectsTheGPURequestingPodsOfEveryNamespace(t *testing.T) {
	dir, names, err := runGather(t, &Options{ToolVersion: "v1.2.3"}, completeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !contains(names, gpuPodsFile) {
		t.Fatalf("the bundle holds no %s: %v", gpuPodsFile, names)
	}

	var list v1.PodList
	if err := yaml.Unmarshal([]byte(readArtifact(t, dir, gpuPodsFile)), &list); err != nil {
		t.Fatalf("%s does not parse: %v", gpuPodsFile, err)
	}

	got := map[string]string{}
	for _, pod := range list.Items {
		got[pod.Name] = pod.Namespace
	}

	for name, namespace := range map[string]string{"trainer": "research", "shared": "team-b"} {
		if got[name] != namespace {
			t.Errorf("%s does not hold %s/%s; it holds %v", gpuPodsFile, namespace, name, got)
		}
	}
	// A pod that asks for no GPU is not the inventory's business and would
	// make the file the whole cluster's pod list.
	if _, collected := got["cpu-only"]; collected {
		t.Errorf("%s holds a pod that requests no GPU: %v", gpuPodsFile, got)
	}
}

// health reads node events to date a driver upgrade step. Without them every
// bundle read is partial and every health --from-bundle exits 3.
func TestAGatherCollectsTheNodeEvents(t *testing.T) {
	dir, names, err := runGather(t, &Options{ToolVersion: "v1.2.3"}, completeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !contains(names, nodeEventsFile) {
		t.Fatalf("the bundle holds no %s: %v", nodeEventsFile, names)
	}

	var list v1.EventList
	if err := yaml.Unmarshal([]byte(readArtifact(t, dir, nodeEventsFile)), &list); err != nil {
		t.Fatalf("%s does not parse: %v", nodeEventsFile, err)
	}
	about := map[string]bool{}
	for _, event := range list.Items {
		about[event.InvolvedObject.Name] = true
	}
	// Both nodes, because nothing narrowed this gather: a collector that kept
	// one node's events whatever was asked would pass a test that only ever
	// looked for one.
	if len(about) != 2 || !about["gpu-node-1"] || !about["gpu-node-2"] {
		t.Errorf("%s holds events about %v, want both nodes", nodeEventsFile, about)
	}
}

// --node narrows what the bundle is about, and these two collectors are about
// the node: a bundle narrowed to one node that still carried the other node's
// workloads would be larger than asked for and misleading about where the
// evidence came from.
func TestTheNewCollectorsHonourTheNodeFlag(t *testing.T) {
	dir, _, err := runGather(t, &Options{Node: "gpu-node-1", ToolVersion: "v1.2.3"}, completeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var pods v1.PodList
	if err := yaml.Unmarshal([]byte(readArtifact(t, dir, gpuPodsFile)), &pods); err != nil {
		t.Fatalf("%s does not parse: %v", gpuPodsFile, err)
	}
	for _, pod := range pods.Items {
		if pod.Spec.NodeName != "gpu-node-1" {
			t.Errorf("%s holds %s/%s, which runs on %s", gpuPodsFile, pod.Namespace, pod.Name, pod.Spec.NodeName)
		}
	}
	if len(pods.Items) != 1 {
		t.Errorf("%s holds %d pods, want the one on gpu-node-1", gpuPodsFile, len(pods.Items))
	}

	var events v1.EventList
	if err := yaml.Unmarshal([]byte(readArtifact(t, dir, nodeEventsFile)), &events); err != nil {
		t.Fatalf("%s does not parse: %v", nodeEventsFile, err)
	}
	if len(events.Items) != 1 {
		t.Errorf("%s holds %d events, want the one about gpu-node-1", nodeEventsFile, len(events.Items))
	}
	for _, event := range events.Items {
		if event.InvolvedObject.Name != "gpu-node-1" {
			t.Errorf("%s holds an event about %s", nodeEventsFile, event.InvolvedObject.Name)
		}
	}
}

// A reader of the index has to be able to tell a bundle that holds these from
// one gathered before they existed, without opening the directory.
func TestTheManifestRecordsTheNewCollectors(t *testing.T) {
	dir, _, err := runGather(t, &Options{ToolVersion: "v1.2.3"}, completeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := readManifest(t, dir)
	for _, collector := range []string{"gpu-pods", "node-events"} {
		status := m.statusOf(t, collector)
		if status != StatusOK {
			t.Errorf("the manifest records collector %q as %q, want %q", collector, status, StatusOK)
		}
	}

	indexed := map[string]bool{}
	for _, f := range m.Files {
		indexed[f.Path] = true
	}
	for _, name := range []string{gpuPodsFile, nodeEventsFile} {
		if !indexed[name] {
			t.Errorf("the manifest does not index %s", name)
		}
	}
}

// A collector that was refused records why rather than failing the gather: a
// bundle without the GPU pods is a bundle that reads as partial, which is the
// honest outcome, and the rest of it is still worth sending.
func TestADeniedGPUPodReadIsRecordedRatherThanFatal(t *testing.T) {
	clientset := fake.NewSimpleClientset(completeCluster()...)
	clientset.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "events"}, "", errors.New("nope"))
	})

	_, names, err := runGatherWith(t, &Options{ToolVersion: "v1.2.3"}, clientset)
	if err == nil {
		t.Fatal("a denied read produced no error at all")
	}
	if !strings.Contains(err.Error(), "node-events") {
		t.Errorf("the error does not name the collector that was refused: %v", err)
	}
	if contains(names, nodeEventsFile) {
		t.Errorf("the bundle holds %s although the read was refused: %v", nodeEventsFile, names)
	}
}
