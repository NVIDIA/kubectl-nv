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
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// stubNodes is a NodeReader over fixed objects, with an error either read can
// be made to return.
type stubNodes struct {
	nodes    []corev1.Node
	pods     []corev1.Pod
	nodesErr error
	podsErr  error
}

func (s *stubNodes) Nodes(context.Context) ([]corev1.Node, error) {
	return s.nodes, s.nodesErr
}

func (s *stubNodes) Pods(context.Context, string, string) ([]corev1.Pod, error) {
	return s.pods, s.podsErr
}

func apiNode(name string, allocatable int64, labels map[string]string) corev1.Node {
	quantity := *resource.NewQuantity(allocatable, resource.DecimalSI)

	return corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": quantity},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": quantity},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// TestBuildNodesCarriesWhetherAPodCouldLandThere: the fit leaves out cordoned
// and not-Ready nodes, so a builder that does not read those two fields hands
// it a cluster where every node looks unusable and every job fits nowhere.
func TestBuildNodesCarriesWhetherAPodCouldLandThere(t *testing.T) {
	t.Parallel()

	ready := apiNode("ready", 8, nil)

	cordoned := apiNode("cordoned", 8, nil)
	cordoned.Spec.Unschedulable = true

	notReady := apiNode("not-ready", 8, nil)
	notReady.Status.Conditions = []corev1.NodeCondition{
		{Type: corev1.NodeReady, Status: corev1.ConditionFalse},
	}

	silent := apiNode("silent", 8, nil)
	silent.Status.Conditions = nil

	got, err := topology.BuildNodes(context.Background(), &stubNodes{
		nodes: []corev1.Node{ready, cordoned, notReady, silent},
	})
	if err != nil {
		t.Fatalf("BuildNodes: %v", err)
	}

	byName := map[string]inventory.GPUNode{}
	for _, n := range got {
		byName[n.Name] = n
	}

	if !byName["ready"].Ready || byName["ready"].Unschedulable {
		t.Errorf("the ready node came back ready=%v unschedulable=%v, want true and false",
			byName["ready"].Ready, byName["ready"].Unschedulable)
	}
	if !byName["cordoned"].Unschedulable {
		t.Error("the cordoned node did not come back cordoned")
	}
	if byName["not-ready"].Ready {
		t.Error("the node whose Ready condition is False came back ready")
	}
	if byName["silent"].Ready {
		t.Error("a node with no conditions at all came back ready: nothing said it was")
	}
}

func apiPod(name, nodeName string, phase corev1.PodPhase, count int64) corev1.Pod {
	request := corev1.ResourceList{}
	if count > 0 {
		request["nvidia.com/gpu"] = *resource.NewQuantity(count, resource.DecimalSI)
	}

	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "research"},
		Spec: corev1.PodSpec{
			NodeName:   nodeName,
			Containers: []corev1.Container{{Name: "c", Resources: corev1.ResourceRequirements{Requests: request, Limits: request}}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

// TestBuildNodesSplitsTheTopologyLabels: the clique goes to its own field and
// the Topograph labels to Topology, which is where Build reads them from.
func TestBuildNodesSplitsTheTopologyLabels(t *testing.T) {
	t.Parallel()

	src := &stubNodes{nodes: []corev1.Node{apiNode("node-a", 8, map[string]string{
		topology.LabelClique:           "clique-a",
		topology.LabelTierPrefix + "1": "fabric-1",
		topology.LabelDomain:           "domain-1",
		"kubernetes.io/hostname":       "node-a",
	})}}

	got, err := topology.BuildNodes(context.Background(), src)
	if err != nil {
		t.Fatalf("BuildNodes: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("BuildNodes returned %d nodes, want 1", len(got))
	}

	n := got[0]
	if n.Kind != "GPUNode" {
		t.Errorf("the node is not stamped: kind is %q", n.Kind)
	}
	if n.Clique != "clique-a" {
		t.Errorf("clique is %q, want clique-a", n.Clique)
	}
	if n.Topology[topology.LabelTierPrefix+"1"] != "fabric-1" {
		t.Errorf("the tier label is missing from %v", n.Topology)
	}
	if n.Topology[topology.LabelDomain] != "domain-1" {
		t.Errorf("the domain label is missing from %v", n.Topology)
	}
	if _, found := n.Topology["kubernetes.io/hostname"]; found {
		t.Errorf("a label that is not a topology label landed in %v", n.Topology)
	}
	if n.Labels["kubernetes.io/hostname"] != "node-a" {
		t.Errorf("the node's labels were dropped: %v", n.Labels)
	}
	if n.Allocatable != 8 {
		t.Errorf("allocatable is %d, want 8", n.Allocatable)
	}
}

// TestBuildNodesCountsOnlyThePodsHoldingGPUs: a finished pod has given its
// GPUs back, and a pod on another node was never holding these.
func TestBuildNodesCountsOnlyThePodsHoldingGPUs(t *testing.T) {
	t.Parallel()

	src := &stubNodes{
		nodes: []corev1.Node{apiNode("node-a", 8, nil), apiNode("node-b", 8, nil)},
		pods: []corev1.Pod{
			apiPod("running", "node-a", corev1.PodRunning, 2),
			apiPod("pending", "node-a", corev1.PodPending, 1),
			apiPod("succeeded", "node-a", corev1.PodSucceeded, 4),
			apiPod("failed", "node-a", corev1.PodFailed, 4),
			apiPod("elsewhere", "node-b", corev1.PodRunning, 3),
			apiPod("unscheduled", "", corev1.PodPending, 8),
		},
	}

	got, err := topology.BuildNodes(context.Background(), src)
	if err != nil {
		t.Fatalf("BuildNodes: %v", err)
	}

	byName := map[string]int64{}
	for _, n := range got {
		if !n.RequestedKnown {
			t.Errorf("%s reports its requested GPUs as unknown", n.Name)
		}
		byName[n.Name] = n.Requested
	}

	// Running plus pending: both hold a place on the node. The terminal pods
	// do not.
	if byName["node-a"] != 3 {
		t.Errorf("node-a has %d GPUs requested, want 3", byName["node-a"])
	}
	if byName["node-b"] != 3 {
		t.Errorf("node-b has %d GPUs requested, want 3", byName["node-b"])
	}
}

// TestBuildNodesKeepsTheNodesWhenThePodReadIsDenied: the topology is still
// worth printing, with the requested GPUs marked as never read rather than
// reported as zero.
func TestBuildNodesKeepsTheNodesWhenThePodReadIsDenied(t *testing.T) {
	t.Parallel()

	src := &stubNodes{
		nodes:   []corev1.Node{apiNode("node-a", 8, nil)},
		podsErr: &source.PartialError{Resource: "pods", Err: errors.New("denied")},
	}

	got, err := topology.BuildNodes(context.Background(), src)

	var pe *source.PartialError
	if !errors.As(err, &pe) {
		t.Fatalf("BuildNodes returned %v (%T), want a PartialError", err, err)
	}
	if len(got) != 1 {
		t.Fatalf("BuildNodes returned %d nodes, want the nodes it did read", len(got))
	}
	if got[0].RequestedKnown {
		t.Error("the node reports its requested GPUs as known after a denied pod read")
	}
}

// TestBuildNodesFailsWhenTheNodesCannotBeRead: without the nodes there is no
// partial answer to give.
func TestBuildNodesFailsWhenTheNodesCannotBeRead(t *testing.T) {
	t.Parallel()

	wanted := errors.New("the API server is unreachable")
	_, err := topology.BuildNodes(context.Background(), &stubNodes{nodesErr: wanted})

	if !errors.Is(err, wanted) {
		t.Fatalf("BuildNodes returned %v, want the node read failure", err)
	}
}
