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
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// operandPod is one GPU Operator pod as the API serves it.
func operandPod(name, namespace, node, app string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": app},
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: app, Image: "nvcr.io/nvidia/" + app + ":v26.3.3"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: app, Ready: true, RestartCount: 0, Image: "nvcr.io/nvidia/" + app + ":v26.3.3"},
			},
		},
	}
}

// TestOperatorNamespace pins how the operator's namespace is found: from the
// pod labelled app=gpu-operator, which is the same thing must-gather keys on.
func TestOperatorNamespace(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		pods []corev1.Pod
		want string
	}{
		{
			name: "the default namespace",
			pods: []corev1.Pod{
				operandPod("kube-proxy-x", "kube-system", "worker-1", "kube-proxy"),
				operandPod("gpu-operator-0", "gpu-operator", "worker-1", ComponentOperator),
			},
			want: "gpu-operator",
		},
		{
			name: "the OpenShift namespace",
			pods: []corev1.Pod{
				operandPod("gpu-operator-0", "nvidia-gpu-operator", "worker-1", ComponentOperator),
			},
			want: "nvidia-gpu-operator",
		},
		{
			name: "no operator pod at all",
			pods: []corev1.Pod{operandPod("kube-proxy-x", "kube-system", "worker-1", "kube-proxy")},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := operatorNamespace(tt.pods); got != tt.want {
				t.Errorf("operatorNamespace = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestOperandsByNodeKeepsTheOperatorNamespace pins the filter. Every pod in
// the cluster reaches this function, and a pod of the user's that happens to
// carry an app label is not an operand.
func TestOperandsByNodeKeepsTheOperatorNamespace(t *testing.T) {
	t.Parallel()

	pods := []corev1.Pod{
		operandPod("device-plugin-a", "gpu-operator", "worker-1", ComponentDevicePlugin),
		operandPod("gfd-a", "gpu-operator", "worker-1", ComponentGFD),
		operandPod("device-plugin-b", "gpu-operator", "worker-2", ComponentDevicePlugin),
		// Same label, someone else's namespace.
		operandPod("impostor", "team-ml", "worker-1", ComponentDevicePlugin),
		// In the namespace but not scheduled anywhere yet.
		operandPod("pending", "gpu-operator", "", ComponentDCGM),
	}

	got := operandsByNode(pods, "gpu-operator")

	names := make(map[string][]string, len(got))
	for node, operands := range got {
		for _, o := range operands {
			names[node] = append(names[node], o.Name)
		}
	}

	want := map[string][]string{
		"worker-1": {"device-plugin-a", "gfd-a"},
		"worker-2": {"device-plugin-b"},
	}
	if !reflect.DeepEqual(names, want) {
		t.Errorf("operands = %#v, want %#v", names, want)
	}
}

// TestOperandsByNodeWithoutANamespace pins that a cluster with no GPU Operator
// has no operands rather than every pod in it.
func TestOperandsByNodeWithoutANamespace(t *testing.T) {
	t.Parallel()

	pods := []corev1.Pod{operandPod("device-plugin-a", "gpu-operator", "worker-1", ComponentDevicePlugin)}

	if got := operandsByNode(pods, ""); len(got) != 0 {
		t.Errorf("operands = %#v, want none when the operator namespace is unknown", got)
	}
}

// TestOperandFromPod pins the fields a reader acts on: which operand it is,
// what it is running, and how often it has fallen over.
func TestOperandFromPod(t *testing.T) {
	t.Parallel()

	pod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "nvidia-driver-daemonset-abcde",
			Namespace: "gpu-operator",
			Labels:    map[string]string{"app": ComponentDriver},
		},
		Spec: corev1.PodSpec{
			NodeName:       "worker-1",
			InitContainers: []corev1.Container{{Name: "k8s-driver-manager", Image: "nvcr.io/nvidia/k8s-driver-manager:v0.8.0"}},
			Containers:     []corev1.Container{{Name: "nvidia-driver-ctr", Image: "nvcr.io/nvidia/driver:550.107.02"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "nvidia-driver-ctr", Ready: true, RestartCount: 3},
			},
		},
	}

	got := operandFromPod(&pod)

	if got.Component != ComponentDriver {
		t.Errorf("Component = %q, want %q", got.Component, ComponentDriver)
	}

	if got.Phase != string(corev1.PodRunning) {
		t.Errorf("Phase = %q, want Running", got.Phase)
	}

	if got.Restarts != 3 {
		t.Errorf("Restarts = %d, want 3", got.Restarts)
	}

	if !got.Ready {
		t.Error("Ready = false, want true when every container reports ready")
	}

	want := []string{"nvcr.io/nvidia/k8s-driver-manager:v0.8.0", "nvcr.io/nvidia/driver:550.107.02"}
	if !reflect.DeepEqual(got.Images, want) {
		t.Errorf("Images = %v, want %v", got.Images, want)
	}
}

// TestOperandFromPodNotReady pins the half of Ready that matters: one
// container out of three being down is not a ready pod.
func TestOperandFromPodNotReady(t *testing.T) {
	t.Parallel()

	pod := operandPod("dcgm-exporter-a", "gpu-operator", "worker-1", ComponentDCGMExporter)
	pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses,
		corev1.ContainerStatus{Name: "second", Ready: false, RestartCount: 7})

	got := operandFromPod(&pod)
	if got.Ready {
		t.Error("Ready = true with a container that is not ready")
	}

	if got.Restarts != 7 {
		t.Errorf("Restarts = %d, want 7 summed over the containers", got.Restarts)
	}
}

// TestOperandFromPodUnlabelled pins the fallback. The operator has used both
// "app" and "app.kubernetes.io/component" over its lifetime, and a pod this
// package has never heard of still has to arrive under its own name.
func TestOperandFromPodUnlabelled(t *testing.T) {
	t.Parallel()

	pod := operandPod("something-new", "gpu-operator", "worker-1", "")
	pod.Labels = map[string]string{"app.kubernetes.io/component": "nvidia-something-new"}

	if got := operandFromPod(&pod); got.Component != "nvidia-something-new" {
		t.Errorf("Component = %q, want the verbatim label value", got.Component)
	}
}

// TestValidatorInitResults is why InitResult exists: the validator carries its
// verdict in init container exit codes, not in its logs.
func TestValidatorInitResults(t *testing.T) {
	t.Parallel()

	pod := operandPod("nvidia-operator-validator-xyz", "gpu-operator", "worker-1", ComponentOperatorValidator)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{
		{
			Name:  "driver-validation",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}},
		},
		{
			Name:  "toolkit-validation",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Reason: "Error"}},
		},
		{
			Name:  "cuda-validation",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "PodInitializing"}},
		},
	}

	got := operandFromPod(&pod)
	if len(got.InitContainers) != 3 {
		t.Fatalf("InitContainers = %#v, want three results in pod order", got.InitContainers)
	}

	if got.InitContainers[0].ExitCode == nil || *got.InitContainers[0].ExitCode != 0 {
		t.Errorf("driver-validation exit code = %v, want 0", got.InitContainers[0].ExitCode)
	}

	if got.InitContainers[1].ExitCode == nil || *got.InitContainers[1].ExitCode != 1 {
		t.Errorf("toolkit-validation exit code = %v, want 1", got.InitContainers[1].ExitCode)
	}

	if got.InitContainers[1].Reason != "Error" {
		t.Errorf("toolkit-validation reason = %q, want Error", got.InitContainers[1].Reason)
	}

	// "has not finished" and "exited 0" are different facts, and reporting the
	// first as the second is how a stuck validation reads as a passing one.
	if got.InitContainers[2].ExitCode != nil {
		t.Errorf("cuda-validation exit code = %v, want none: it has not terminated", *got.InitContainers[2].ExitCode)
	}

	if got.InitContainers[2].Reason != "PodInitializing" {
		t.Errorf("cuda-validation reason = %q, want PodInitializing", got.InitContainers[2].Reason)
	}
}

// TestInitResultsOnlyForTheValidator pins the scope: every operand would
// otherwise carry its init containers, and the driver pod's are not a verdict.
func TestInitResultsOnlyForTheValidator(t *testing.T) {
	t.Parallel()

	pod := operandPod("nvidia-driver-daemonset-abcde", "gpu-operator", "worker-1", ComponentDriver)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  "k8s-driver-manager",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0, Reason: "Completed"}},
	}}

	if got := operandFromPod(&pod); len(got.InitContainers) != 0 {
		t.Errorf("InitContainers = %#v, want none for an operand that is not the validator", got.InitContainers)
	}
}

// TestBuildAttachesOperands is the whole path: the one pod list feeds both the
// requested GPUs and the operands, and the operands land on their own node.
func TestBuildAttachesOperands(t *testing.T) {
	t.Parallel()

	src := &countingSource{
		gpu: []corev1.Node{
			*testNode("worker-1", nil, map[string]string{"nvidia.com/gpu": "4"}, nil),
			*testNode("worker-2", nil, map[string]string{"nvidia.com/gpu": "4"}, nil),
		},
		pods: []corev1.Pod{
			operandPod("gpu-operator-0", "gpu-operator", "worker-1", ComponentOperator),
			operandPod("device-plugin-1", "gpu-operator", "worker-1", ComponentDevicePlugin),
			operandPod("device-plugin-2", "gpu-operator", "worker-2", ComponentDevicePlugin),
			operandPod("training-job", "team-ml", "worker-1", "trainer"),
		},
	}

	list, err := Build(context.Background(), src, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	got := make([]string, 0, 2)
	for _, o := range itemNamed(t, list, "worker-1").Operands {
		got = append(got, o.Name)
	}

	want := []string{"device-plugin-1", "gpu-operator-0"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("worker-1 operands = %v, want %v sorted by name", got, want)
	}
}

// TestBuildWithoutTheOperatorIsNotAnError pins that a cluster without the GPU
// Operator is an answer, not a failure.
func TestBuildWithoutTheOperatorIsNotAnError(t *testing.T) {
	t.Parallel()

	src := &countingSource{
		gpu:  []corev1.Node{*testNode("worker-1", nil, map[string]string{"nvidia.com/gpu": "4"}, nil)},
		pods: []corev1.Pod{operandPod("training-job", "team-ml", "worker-1", "trainer")},
	}

	list, err := Build(context.Background(), src, BuildOptions{})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	if operands := itemNamed(t, list, "worker-1").Operands; len(operands) != 0 {
		t.Errorf("operands = %#v, want none without a GPU Operator", operands)
	}
}

// draPod is a pod carrying one of the DRA driver's identifying labels and
// nothing else, which is what the standalone charts produce: they label by
// component rather than with an "app" key.
func draPod(name, namespace, node string, labels map[string]string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: "plugin", Image: "nvcr.io/nvidia/dra-driver:v25.3.0"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "plugin", Ready: true}},
		},
	}
}

// TestDRAComponentLabelVariants pins the three label spellings the DRA
// kubelet plugin has shipped under, and the one the IMEX daemon uses.
//
// A node whose kubelet plugin is not running advertises nothing under DRA
// however healthy every GPU Operator pod on it looks, so a spelling this
// package does not recognise hides exactly the pod a reader needs.
func TestDRAComponentLabelVariants(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "operator managed, app label",
			labels: map[string]string{"app": "nvidia-dra-driver-kubelet-plugin"},
			want:   ComponentDRADriver,
		},
		{
			name:   "standalone chart v25.x",
			labels: map[string]string{"nvidia-dra-driver-gpu-component": "kubelet-plugin"},
			want:   ComponentDRADriver,
		},
		{
			name:   "renamed chart, v0.4.x",
			labels: map[string]string{"dra-driver-nvidia-gpu-component": "kubelet-plugin"},
			want:   ComponentDRADriver,
		},
		{
			name:   "IMEX daemon, keyed by the compute domain",
			labels: map[string]string{"resource.nvidia.com/computeDomain": "0b1d7a1e-dead-beef-cafe-000000000001"},
			want:   ComponentIMEXDaemon,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pod := draPod("dra-abc", "nvidia-dra-driver-gpu", "worker-1", tt.labels)
			if got := operandFromPod(&pod).Component; got != tt.want {
				t.Errorf("Component = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestDRAComponentLabelsAreExact pins that the match is on the exact keys and
// values. A prefix or substring match would claim every pod of an unrelated
// chart that happens to use a "-component" key.
func TestDRAComponentLabelsAreExact(t *testing.T) {
	t.Parallel()

	for _, labels := range []map[string]string{
		{"nvidia-dra-driver-gpu-component": "controller"},
		{"some-other-chart-component": "kubelet-plugin"},
		{"nvidia-dra-driver-gpu-component-extra": "kubelet-plugin"},
		{"resource.nvidia.com/computeDomain.cliqueID": "1"},
	} {
		pod := draPod("not-dra", "elsewhere", "worker-1", labels)
		if got := operandFromPod(&pod).Component; got == ComponentDRADriver || got == ComponentIMEXDaemon {
			t.Errorf("labels %v were read as %q, want no DRA classification", labels, got)
		}
	}
}

// TestOperandsByNodeReachesDRAOutsideTheOperatorNamespace is the whole point
// of classifying by label: the standalone DRA driver installs into its own
// namespace, so a namespace filter alone drops it.
func TestOperandsByNodeReachesDRAOutsideTheOperatorNamespace(t *testing.T) {
	t.Parallel()

	pods := []corev1.Pod{
		operandPod("device-plugin-1", "gpu-operator", "worker-1", ComponentDevicePlugin),
		draPod("dra-in-operator-ns", "gpu-operator", "worker-1",
			map[string]string{"app": "nvidia-dra-driver-kubelet-plugin"}),
		draPod("dra-standalone", "nvidia-dra-driver-gpu", "worker-1",
			map[string]string{"nvidia-dra-driver-gpu-component": "kubelet-plugin"}),
		draPod("dra-sigs", "dra-driver", "worker-1",
			map[string]string{"dra-driver-nvidia-gpu-component": "kubelet-plugin"}),
		draPod("imex-daemon", "nvidia-dra-driver-gpu", "worker-1",
			map[string]string{"resource.nvidia.com/computeDomain": "0b1d7a1e"}),
		// Somebody else's pod, in somebody else's namespace, with no DRA and
		// no operator label: not an operand.
		draPod("training-job", "team-ml", "worker-1", map[string]string{"app.kubernetes.io/name": "trainer"}),
	}

	got := make([]string, 0, 5)
	for _, o := range operandsByNode(pods, "gpu-operator")["worker-1"] {
		got = append(got, o.Name)
	}

	want := []string{"device-plugin-1", "dra-in-operator-ns", "dra-sigs", "dra-standalone", "imex-daemon"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("operands = %v, want %v", got, want)
	}
}

// TestDRAOperandsWithoutAnOperator pins that a cluster running the standalone
// DRA driver and no GPU Operator still reports its operands.
func TestDRAOperandsWithoutAnOperator(t *testing.T) {
	t.Parallel()

	pods := []corev1.Pod{
		draPod("dra-standalone", "nvidia-dra-driver-gpu", "worker-1",
			map[string]string{"nvidia-dra-driver-gpu-component": "kubelet-plugin"}),
		draPod("training-job", "team-ml", "worker-1", nil),
	}

	operands := operandsByNode(pods, operatorNamespace(pods))["worker-1"]
	if len(operands) != 1 || operands[0].Component != ComponentDRADriver {
		t.Fatalf("operands = %#v, want the one kubelet plugin", operands)
	}
}

// TestComponentPrecedence pins the order the component name is resolved in.
// Every operand has to get a non-empty one: a pod reported with an empty
// component is a pod a reader cannot act on.
func TestComponentPrecedence(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		labels map[string]string
		owners []metav1.OwnerReference
		want   string
	}{
		{
			name: "app wins over every other label",
			labels: map[string]string{
				"app":                         ComponentDCGM,
				"app.kubernetes.io/component": "something-else",
				"app.kubernetes.io/name":      "another-thing",
			},
			want: ComponentDCGM,
		},
		{
			name: "app.kubernetes.io/component when there is no app",
			labels: map[string]string{
				"app.kubernetes.io/component": "nvidia-something-new",
				"app.kubernetes.io/name":      "another-thing",
			},
			want: "nvidia-something-new",
		},
		{
			name:   "app.kubernetes.io/name when there is neither",
			labels: map[string]string{"app.kubernetes.io/name": "node-feature-discovery"},
			want:   "node-feature-discovery",
		},
		{
			name: "the controlling owner when there is no label at all",
			owners: []metav1.OwnerReference{
				{Kind: "ReplicaSet", Name: "not-the-controller"},
				{Kind: "DaemonSet", Name: "nvidia-node-status-exporter", Controller: ptrTo(true)},
			},
			want: "nvidia-node-status-exporter",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			pod := draPod("p", "gpu-operator", "worker-1", tt.labels)
			pod.OwnerReferences = tt.owners

			if got := operandFromPod(&pod).Component; got != tt.want {
				t.Errorf("Component = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestEveryOperandHasAComponent is the guarantee behind the precedence: a pod
// with nothing to go on at all still reports something a reader can name.
func TestEveryOperandHasAComponent(t *testing.T) {
	t.Parallel()

	pods := []corev1.Pod{
		draPod("bare-pod", "gpu-operator", "worker-1", nil),
		draPod("named-pod", "gpu-operator", "worker-1", map[string]string{"app.kubernetes.io/name": "nfd-worker"}),
	}

	for _, o := range operandsByNode(pods, "gpu-operator")["worker-1"] {
		if o.Component == "" {
			t.Errorf("operand %s has an empty component", o.Name)
		}
	}
}

// TestValidatorInitResultRunning pins the third state an init container can be
// in. "Running" is not "exited 0" and not "waiting", and a validation in
// flight must not read as one that passed.
func TestValidatorInitResultRunning(t *testing.T) {
	t.Parallel()

	pod := operandPod("nvidia-operator-validator-xyz", "gpu-operator", "worker-1", ComponentOperatorValidator)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  "cuda-validation",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}}

	results := operandFromPod(&pod).InitContainers
	if len(results) != 1 {
		t.Fatalf("InitContainers = %#v, want one result", results)
	}

	if results[0].ExitCode != nil {
		t.Errorf("ExitCode = %d, want none: the container is still running", *results[0].ExitCode)
	}

	if results[0].Reason != "Running" {
		t.Errorf("Reason = %q, want Running", results[0].Reason)
	}
}

// ptrTo is the one-line helper the owner reference fixtures need.
func ptrTo[T any](v T) *T { return &v }
