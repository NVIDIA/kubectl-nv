//go:build e2e

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

package e2e

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// operatorNamespace is where the Mokka lane installs the GPU Operator and its
// operands, and so the namespace the gather collects pods and logs from.
const operatorNamespace = "gpu-operator"

// controlPlaneRoleLabel marks the nodes that carry no operands. Mokka hints
// the control plane as GPU capable without giving it allocatable GPUs, so
// "has an nvidia.com label" does not separate a worker from it; the role does.
const controlPlaneRoleLabel = "node-role.kubernetes.io/control-plane"

// podsReadyTimeout bounds the wait for the namespace to settle. The lane
// scripts have already waited for the operand DaemonSets by the time the suite
// runs, so this is the margin for the validator pods that follow them, not the
// budget for an install.
const podsReadyTimeout = 10 * time.Minute

// podsReadyPoll is how often the namespace is asked again while it settles.
const podsReadyPoll = 5 * time.Second

var (
	clientOnce sync.Once
	client     kubernetes.Interface
	clientErr  error
)

// clusterClient returns a read-only client for the cluster under test. The
// suite only ever lists and gets through it: nothing in kubectl-nv writes to a
// cluster, and neither may the tests that check it.
func clusterClient(t *testing.T) kubernetes.Interface {
	t.Helper()

	clientOnce.Do(func() {
		config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
		if err != nil {
			clientErr = fmt.Errorf("building a client configuration from %s: %w", kubeconfigPath, err)
			return
		}
		client, err = kubernetes.NewForConfig(config)
		if err != nil {
			clientErr = fmt.Errorf("building a kubernetes client: %w", err)
		}
	})
	if clientErr != nil {
		t.Fatal(clientErr)
	}

	return client
}

// clusterNodeNames lists the cluster's nodes, so that what the bundle says
// about the cluster is checked against the cluster rather than against itself.
func clusterNodeNames(t *testing.T) []string {
	t.Helper()

	names := make([]string, 0)
	for _, node := range clusterNodes(t) {
		names = append(names, node.Name)
	}
	sort.Strings(names)

	return names
}

// clusterNodes lists the cluster's nodes.
func clusterNodes(t *testing.T) []v1.Node {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	nodes, err := clusterClient(t).CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing the cluster's nodes: %v", err)
	}

	return nodes.Items
}

// workerNodeNames lists the nodes the operands are expected to run on.
func workerNodeNames(t *testing.T) []string {
	t.Helper()

	names := make([]string, 0)
	for _, node := range clusterNodes(t) {
		if _, isControlPlane := node.Labels[controlPlaneRoleLabel]; isControlPlane {
			continue
		}
		names = append(names, node.Name)
	}
	sort.Strings(names)

	return names
}

// operatorPods lists the pods in the operator namespace.
func operatorPods(t *testing.T) []v1.Pod {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	pods, err := clusterClient(t).CoreV1().Pods(operatorNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing the pods in %s: %v", operatorNamespace, err)
	}

	return pods.Items
}

// podNames lists the names of pods, sorted.
func podNames(pods []v1.Pod) []string {
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	sort.Strings(names)

	return names
}

// waitForStartedPods blocks until every pod in the operator namespace has
// either finished or started all of its containers.
//
// A gather reads container logs, and the API server refuses the logs of a
// container that has not started yet with ContainerCreating. That is not a bug
// in kubectl-nv and must not be reported as one: the lane scripts wait for the
// operand DaemonSets to roll out, and the validator pods that follow them are
// still coming up when the suite starts.
func waitForStartedPods(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(podsReadyTimeout)
	var unready []string
	for {
		unready = unstartedPods(t)
		if len(unready) == 0 {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(podsReadyPoll)
	}

	t.Fatalf("after %s, %d pod(s) in %s have containers that never started:\n  %s",
		podsReadyTimeout, len(unready), operatorNamespace, strings.Join(unready, "\n  "))
}

// unstartedPods names the pods whose logs cannot be read yet, each with the
// containers that are holding it up, so that a timeout says which operand is
// stuck rather than only that something is.
func unstartedPods(t *testing.T) []string {
	t.Helper()

	var unready []string
	for _, pod := range operatorPods(t) {
		// A pod that ran to completion keeps its logs, so it is readable
		// whatever its containers say now.
		if pod.Status.Phase == v1.PodSucceeded {
			continue
		}

		var waiting []string
		waiting = append(waiting, unstartedContainers(pod.Spec.InitContainers, pod.Status.InitContainerStatuses)...)
		waiting = append(waiting, unstartedContainers(pod.Spec.Containers, pod.Status.ContainerStatuses)...)
		if len(waiting) > 0 {
			unready = append(unready, fmt.Sprintf("%s (%s): %s",
				pod.Name, pod.Status.Phase, strings.Join(waiting, ", ")))
		}
	}
	sort.Strings(unready)

	return unready
}

// unstartedContainers names the containers of one pod that have no logs to
// read: a container with no status yet, or one still waiting to start.
func unstartedContainers(spec []v1.Container, statuses []v1.ContainerStatus) []string {
	byName := make(map[string]v1.ContainerStatus, len(statuses))
	for _, status := range statuses {
		byName[status.Name] = status
	}

	var waiting []string
	for _, container := range spec {
		status, reported := byName[container.Name]
		if !reported {
			waiting = append(waiting, container.Name+" (no status)")
			continue
		}
		// Running and terminated containers both have logs. Waiting ones do
		// not, and neither does one the kubelet has not spoken about yet.
		if status.State.Running == nil && status.State.Terminated == nil {
			reason := "waiting"
			if status.State.Waiting != nil && status.State.Waiting.Reason != "" {
				reason = status.State.Waiting.Reason
			}
			waiting = append(waiting, container.Name+" ("+reason+")")
		}
	}

	return waiting
}

// daemonSetPodNodes maps a daemonset's name to the nodes it has a pod on,
// taken from the pods' owner references rather than from a label, because the
// owner is what actually says which daemonset a pod came from.
func daemonSetPodNodes(pods []v1.Pod) map[string]map[string]bool {
	nodes := make(map[string]map[string]bool)
	for _, pod := range pods {
		for _, owner := range pod.OwnerReferences {
			if owner.Kind != "DaemonSet" {
				continue
			}
			if nodes[owner.Name] == nil {
				nodes[owner.Name] = make(map[string]bool)
			}
			nodes[owner.Name][pod.Spec.NodeName] = true
		}
	}

	return nodes
}
