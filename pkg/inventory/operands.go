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
	"sort"

	corev1 "k8s.io/api/core/v1"
)

const (
	// appLabel is the label the GPU Operator names its own pods with, and the
	// one OperandPod.Component carries verbatim.
	appLabel = "app"
	// componentLabel is the newer name for the same thing. The operator has
	// used both over its lifetime, so a reader takes whichever is there.
	componentLabel = "app.kubernetes.io/component"
	// nameLabel is the last label to try. node-feature-discovery's worker, a
	// pod a GPU inventory does run into, carries neither of the two above.
	nameLabel = "app.kubernetes.io/name"

	// LabelComputeDomain is the only label the DRA driver's IMEX daemon pod
	// carries: the upstream template labels it with the compute domain UUID
	// and with nothing else, no "app" key of any kind
	// (templates/compute-domain-daemon.tmpl.yaml, and the key itself at
	// cmd/compute-domain-daemon/common.go:32, nvidia-dra-driver-gpu
	// fb8b96747). Only the key is matched; the value is a UUID.
	LabelComputeDomain = "resource.nvidia.com/computeDomain"

	// draKubeletPluginComponent is the component value the DRA driver's own
	// charts give the kubelet plugin.
	draKubeletPluginComponent = "kubelet-plugin"
)

// draComponentLabelKeys are the per-chart label keys the DRA driver's kubelet
// plugin is selected by. Both are "<chart name>-component", and the chart was
// renamed, so the same pod has shipped under two keys:
//
//   - nvidia-dra-driver-gpu-component, the standalone chart at v25.3.x
//     (deployments/helm/nvidia-dra-driver-gpu/templates/_helpers.tpl, which
//     builds the key from Chart.Name).
//   - dra-driver-nvidia-gpu-component, after the repository rename
//     (commit 7a5e8439c, first released in v0.4.1).
//
// The GPU Operator's own copy of the DaemonSet is not here: it gives the pod a
// plain "app" label, which the ordinary path already reads.
//
// These are matched as exact keys with the exact value above. A prefix match
// would claim the pods of any chart whose name ends the same way, and a
// value-only match would claim the driver's controller pod as a kubelet
// plugin.
var draComponentLabelKeys = []string{
	"nvidia-dra-driver-gpu-component",
	"dra-driver-nvidia-gpu-component",
}

// operatorNamespace finds where the GPU Operator is installed, from the
// namespace of its own pod. The label is the one pkg/mustgather already keys
// on, so the two commands cannot disagree about which namespace holds the
// operands.
//
// An empty result means no operator pod was seen. That is an answer: a cluster
// without the GPU Operator has no operands, and reporting that as a failure
// would make kubectl nv list refuse to describe a cluster running the device plugin
// on its own.
func operatorNamespace(pods []corev1.Pod) string {
	for i := range pods {
		pod := &pods[i]
		if componentOf(pod) == ComponentOperator {
			return pod.Namespace
		}
	}

	return ""
}

// operandsByNode groups the operands by the node they are scheduled on.
//
// Two things make a pod an operand. One is the GPU Operator's namespace, which
// is where everything the operator installs lives. The other is a DRA driver
// label, wherever the pod is: the standalone charts install the driver into a
// namespace of their own, so a namespace filter alone reports a DRA node with
// no kubelet plugin on it, which is the one fact that explains why the node
// advertises nothing.
//
// Neither test is a guess about a user's own pods. An unlabelled pod outside
// the operator's namespace is nobody's operand, and the DRA keys are matched
// exactly.
func operandsByNode(pods []corev1.Pod, namespace string) map[string][]OperandPod {
	var out map[string][]OperandPod

	for i := range pods {
		pod := &pods[i]
		if pod.Spec.NodeName == "" || !isOperand(pod, namespace) {
			continue
		}

		if out == nil {
			out = map[string][]OperandPod{}
		}

		out[pod.Spec.NodeName] = append(out[pod.Spec.NodeName], operandFromPod(pod))
	}

	for node := range out {
		operands := out[node]
		sort.Slice(operands, func(i, j int) bool { return operands[i].Name < operands[j].Name })
	}

	return out
}

// operandFromPod shapes one operator pod into the model.
func operandFromPod(pod *corev1.Pod) OperandPod {
	out := OperandPod{
		Name:      pod.Name,
		Namespace: pod.Namespace,
		Component: componentOf(pod),
		Phase:     string(pod.Status.Phase),
		Images:    images(pod),
		Ready:     allReady(pod.Status.ContainerStatuses),
	}

	for i := range pod.Status.ContainerStatuses {
		out.Restarts += pod.Status.ContainerStatuses[i].RestartCount
	}

	// Only the validator's init containers are a verdict. Every other
	// operand's are setup steps, and carrying them would bury the one set of
	// exit codes a reader is looking for under a dozen that mean nothing.
	if out.Component == ComponentOperatorValidator {
		out.InitContainers = initResults(pod.Status.InitContainerStatuses)
	}

	return out
}

// isOperand reports whether a pod belongs in a node's operand list.
func isOperand(pod *corev1.Pod, namespace string) bool {
	if draComponent(pod) != "" {
		return true
	}

	return namespace != "" && pod.Namespace == namespace
}

// draComponent classifies a DRA driver pod by label, or returns "" for a pod
// that is not one. See draComponentLabelKeys for why there are three
// spellings.
func draComponent(pod *corev1.Pod) string {
	if _, found := pod.Labels[LabelComputeDomain]; found {
		return ComponentIMEXDaemon
	}

	if pod.Labels[appLabel] == ComponentDRADriver {
		return ComponentDRADriver
	}

	for _, key := range draComponentLabelKeys {
		if pod.Labels[key] == draKubeletPluginComponent {
			return ComponentDRADriver
		}
	}

	return ""
}

// componentOf names which operand a pod is. The result is never empty: a pod
// reported with no component is one a reader cannot act on and cannot even
// look up, so there is a fallback for every case.
//
// The order is:
//
//  1. the "app" label, verbatim, which is what the GPU Operator writes and
//     what a health rule matches on.
//  2. a DRA driver label, which is how the standalone charts identify the
//     kubelet plugin and the only way the IMEX daemon can be identified at
//     all, neither of them carrying an "app" key.
//  3. app.kubernetes.io/component, the newer spelling of 1, kept because the
//     operand model's own contract names it.
//  4. app.kubernetes.io/name, which node-feature-discovery's worker carries
//     instead of any of the above.
//  5. the name of the controller that owns the pod, for a pod with no useful
//     label at all. It is the DaemonSet or ReplicaSet name, which is still
//     something a reader can type into kubectl.
//  6. the pod's own name, for a pod with no label and no controller, which is
//     what a standalone pod in the operator's namespace looks like. It names
//     one pod rather than a kind of pod, and that is the point: the
//     alternative is an operand a reader cannot look up at all.
//
// The constants in this package are the values the GPU Operator and the DRA
// driver are known to use; a pod labelled with something nothing here names
// still arrives under its own label value rather than under a guess.
func componentOf(pod *corev1.Pod) string {
	if app := pod.Labels[appLabel]; app != "" {
		return app
	}

	if dra := draComponent(pod); dra != "" {
		return dra
	}

	if component := pod.Labels[componentLabel]; component != "" {
		return component
	}

	if name := pod.Labels[nameLabel]; name != "" {
		return name
	}

	if owner := controllerName(pod); owner != "" {
		return owner
	}

	return pod.Name
}

// controllerName is the name of the controller that owns the pod. Only the
// controlling reference counts: a pod can carry several owner references and
// only one of them is what created it.
func controllerName(pod *corev1.Pod) string {
	for i := range pod.OwnerReferences {
		owner := &pod.OwnerReferences[i]
		if owner.Controller != nil && *owner.Controller {
			return owner.Name
		}
	}

	return ""
}

// images lists the container images a pod runs, init containers first and in
// pod order, which is how a driver or toolkit version mismatch becomes
// visible. An image used twice is listed once.
func images(pod *corev1.Pod) []string {
	out := make([]string, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	seen := make(map[string]struct{}, cap(out))

	add := func(containers []corev1.Container) {
		for i := range containers {
			image := containers[i].Image
			if image == "" {
				continue
			}

			if _, dup := seen[image]; dup {
				continue
			}

			seen[image] = struct{}{}
			out = append(out, image)
		}
	}

	add(pod.Spec.InitContainers)
	add(pod.Spec.Containers)

	return out
}

// allReady reports whether every container of the pod is ready. A pod with no
// container statuses at all has not started, which is not ready.
func allReady(statuses []corev1.ContainerStatus) bool {
	if len(statuses) == 0 {
		return false
	}

	for i := range statuses {
		if !statuses[i].Ready {
			return false
		}
	}

	return true
}

// initResults reads the init container outcomes, in pod order.
//
// A container that has not terminated carries no exit code, because "has not
// finished" and "exited 0" are different facts: reporting the first as the
// second turns a validation still running into one that passed.
func initResults(statuses []corev1.ContainerStatus) []InitResult {
	out := make([]InitResult, 0, len(statuses))

	for i := range statuses {
		s := &statuses[i]
		result := InitResult{Name: s.Name}

		switch {
		case s.State.Terminated != nil:
			code := s.State.Terminated.ExitCode
			result.ExitCode = &code
			result.Reason = s.State.Terminated.Reason
		case s.State.Waiting != nil:
			result.Reason = s.State.Waiting.Reason
		case s.State.Running != nil:
			result.Reason = "Running"
		}

		out = append(out, result)
	}

	return out
}
