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
)

// operatorNamespace finds where the GPU Operator is installed, from the
// namespace of its own pod. The label is the one pkg/mustgather already keys
// on, so the two commands cannot disagree about which namespace holds the
// operands.
//
// An empty result means no operator pod was seen. That is an answer: a cluster
// without the GPU Operator has no operands, and reporting that as a failure
// would make gpu list refuse to describe a cluster running the device plugin
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

// operandsByNode groups the operator's pods by the node they are scheduled on.
// A namespace is required: without one every pod in the cluster would be an
// operand, and a user's own pod carrying an "app" label is not one.
func operandsByNode(pods []corev1.Pod, namespace string) map[string][]OperandPod {
	if namespace == "" {
		return nil
	}

	var out map[string][]OperandPod

	for i := range pods {
		pod := &pods[i]
		if pod.Namespace != namespace || pod.Spec.NodeName == "" {
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

// componentOf names which operand a pod is, verbatim. The constants in this
// package are the values the GPU Operator and the NVIDIA DRA driver are known
// to use; a pod labelled with something nothing here names still arrives under
// its own label value rather than as an empty string.
func componentOf(pod *corev1.Pod) string {
	if app := pod.Labels[appLabel]; app != "" {
		return app
	}

	return pod.Labels[componentLabel]
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
