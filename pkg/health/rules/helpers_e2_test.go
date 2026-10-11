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

package rules_test

import (
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
)

// The node options the second set of rules needs. They are here rather than in
// helpers_test.go so that the two milestones' fixtures stay separable: a case
// written for one rule set is never silently changed by the other.

// withUpgradeState sets nvidia.com/gpu-driver-upgrade-state, which is what the
// GPU Operator's upgrade controller writes as it walks a node through an
// upgrade.
func withUpgradeState(state string) nodeOption {
	return func(n *inventory.GPUNode) { n.UpgradeState = state }
}

// withClique sets nvidia.com/gpu.clique, the NVLink or IMEX clique the node's
// GPUs belong to.
func withClique(clique string) nodeOption {
	return func(n *inventory.GPUNode) { n.Clique = clique }
}

// withMIG sets the MIG labels without touching the node's extended resources,
// which is what the single and none strategies look like: both keep
// advertising nvidia.com/gpu and publish no nvidia.com/mig-* at all.
func withMIG(strategy, config, configState string, capable bool) nodeOption {
	return func(n *inventory.GPUNode) {
		n.MIG.Strategy = strategy
		n.MIG.Config = config
		n.MIG.ConfigState = configState
		n.MIG.Capable = capable
	}
}

// withMIGResources adds the per-profile extended resources a mixed-strategy
// node advertises, leaving the labels to withMIG.
func withMIGResources(resources map[string]inventory.ResourceCount) nodeOption {
	return func(n *inventory.GPUNode) {
		if n.MIG.Resources == nil {
			n.MIG.Resources = map[string]inventory.ResourceCount{}
		}

		for name, count := range resources {
			n.MIG.Resources[name] = count
		}
	}
}

// withTopology sets the Topograph labels, keyed by the full label name as
// pkg/inventory keys them.
func withTopology(labels map[string]string) nodeOption {
	return func(n *inventory.GPUNode) {
		if n.Topology == nil {
			n.Topology = map[string]string{}
		}

		for k, v := range labels {
			n.Topology[k] = v
		}

		withLabels(labels)(n)
	}
}

// withConditions appends node conditions. kubectl nv health fills them,
// unlike kubectl nv list, because NV-NODE-005 reads them.
func withConditions(conditions ...corev1.NodeCondition) nodeOption {
	return func(n *inventory.GPUNode) { n.Conditions = append(n.Conditions, conditions...) }
}

// cordoned sets spec.unschedulable.
func cordoned() nodeOption {
	return func(n *inventory.GPUNode) { n.Unschedulable = true }
}

// withResourceSlices names the gpu.nvidia.com ResourceSlices published for the
// node.
func withResourceSlices(names ...string) nodeOption {
	return func(n *inventory.GPUNode) { n.ResourceSlices = append(n.ResourceSlices, names...) }
}

// withValidatorInit puts the operator validator pod on the node with its init
// container results spelled out, which is where the toolkit verdict lives.
func withValidatorInit(ready bool, results ...inventory.InitResult) nodeOption {
	return func(n *inventory.GPUNode) {
		phase := string(corev1.PodRunning)
		if !ready {
			phase = string(corev1.PodPending)
		}

		n.Operands = append(n.Operands, inventory.OperandPod{
			Name:           "nvidia-operator-validator-abcde",
			Namespace:      "gpu-operator",
			Component:      inventory.ComponentOperatorValidator,
			Phase:          phase,
			Ready:          ready,
			InitContainers: results,
		})
	}
}

// initResult builds one init container outcome. A nil code is a container that
// has not terminated, which is a different fact from exiting zero.
func initResult(name string, code *int32, reason string) inventory.InitResult {
	return inventory.InitResult{Name: name, ExitCode: code, Reason: reason}
}

// exitCode returns a pointer to an exit code, because InitResult carries one.
func exitCode(code int32) *int32 { return &code }

// nvsentinelCondition builds the node condition NVSentinel's platform
// connector writes: the type is the health check's name verbatim and the
// reason is that name with IsNotHealthy or IsHealthy appended
// (platform-connectors/pkg/connectors/kubernetes/process_node_events.go lines
// 193 and 950, NVSentinel 93fa8222).
func nvsentinelCondition(checkName string, failing bool, message string) corev1.NodeCondition {
	condition := corev1.NodeCondition{
		Type:    corev1.NodeConditionType(checkName),
		Status:  corev1.ConditionFalse,
		Reason:  checkName + "IsHealthy",
		Message: "No Health Failures",
	}

	if failing {
		condition.Status = corev1.ConditionTrue
		condition.Reason = checkName + "IsNotHealthy"
		condition.Message = message
	}

	return condition
}

// upgradeEvent builds the event the GPU Operator's upgrade controller records
// when it moves a node into a state: reason GPUDriverUpgrade, message
// "Successfully updated node state label to <state>"
// (k8s-operator-libs pkg/upgrade/node_upgrade_state_provider.go:129 and
// util.go:158).
func upgradeEvent(node, state string, when time.Time) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: node + ".1", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: node},
		Reason:         "GPUDriverUpgrade",
		Message:        "Successfully updated node state label to " + state,
		LastTimestamp:  metav1.NewTime(when),
	}
}

// computeDomain builds a ComputeDomain as the NVIDIA DRA driver serves it:
// resource.nvidia.com/v1beta1, namespaced, with status.nodes naming the nodes
// it covers and each one's IMEX daemon status
// (api/nvidia.com/resource/v1beta1/computedomain.go, k8s-dra-driver-gpu
// fb8b9674).
func computeDomain(namespace, name, status string, nodes ...map[string]any) unstructured.Unstructured {
	items := make([]any, 0, len(nodes))
	for _, node := range nodes {
		items = append(items, node)
	}

	return unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "resource.nvidia.com/v1beta1",
		"kind":       "ComputeDomain",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"status":     map[string]any{"status": status, "nodes": items},
	}}
}

// cdNode is one entry of a ComputeDomain's status.nodes.
func cdNode(name, cliqueID, status string) map[string]any {
	return map[string]any{"name": name, "cliqueID": cliqueID, "status": status, "ipAddress": "10.0.0.1"}
}
