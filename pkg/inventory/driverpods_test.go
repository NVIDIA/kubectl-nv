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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The four selectors are the ones pkg/mustgather collects a bug report
// through, and they are now served from here so that the two cannot drift.
// Written out rather than derived from the list, because a test that asked the
// list what the list holds would pass on an empty one.
func TestDriverSelectorsAreTheFourCollectedForms(t *testing.T) {
	t.Parallel()

	want := []string{
		"app=nvidia-driver-daemonset",
		"app.kubernetes.io/component=nvidia-driver",
		"app=nvidia-vgpu-manager-daemonset",
		"openshift.driver-toolkit",
	}

	if got := DriverSelectors(); !reflect.DeepEqual(got, want) {
		t.Errorf("DriverSelectors() =\n%v\nwant\n%v", got, want)
	}
}

func TestDriverSelectorsAreFresh(t *testing.T) {
	t.Parallel()

	first := DriverSelectors()
	first[0] = "tampered"

	if DriverSelectors()[0] == "tampered" {
		t.Error("DriverSelectors hands out the package's own slice")
	}
}

// Every shape a driver pod arrives in has to classify as one, because the
// component is what NV-DRV-001 matches on and what a reader greps for.
//
// The name-based shapes are the point. A DaemonSet managed by the NVIDIADriver
// CR is named nvidia-<driverType>-driver-<osTag>-<hash> and carries that name
// as its "app" label (gpu-operator manifests/state-driver/0500_daemonset.yaml,
// internal/state/driver.go getDriverAppName), and OpenShift's driver toolkit
// pods carry a per-RHCOS name of their own. Neither is ever the literal
// "nvidia-driver-daemonset", so matching the selector values against the app
// label would classify both as operands nothing recognises.
func TestDriverComponentClassifiesEveryDriverPodShape(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "the ClusterPolicy driver DaemonSet",
			labels: map[string]string{"app": "nvidia-driver-daemonset"},
			want:   ComponentDriver,
		},
		{
			name: "an NVIDIADriver CR pod, whose app label is its own generated name",
			labels: map[string]string{
				"app":                         "nvidia-gpu-driver-ubuntu22.04-7d9f4c2b1e",
				"app.kubernetes.io/component": "nvidia-driver",
				"nvidia.com/node.os-version":  "ubuntu22.04",
			},
			want: ComponentDriver,
		},
		{
			name: "an OpenShift driver toolkit pod",
			labels: map[string]string{
				"app":                      "nvidia-driver-daemonset-410.84.202203290245-0",
				"openshift.driver-toolkit": "true",
			},
			want: ComponentDriver,
		},
		{
			name: "an OpenShift driver toolkit pod carrying nothing else this knows",
			labels: map[string]string{
				"app":                      "nvidia-gpu-driver-rhcos4.14-abc123",
				"openshift.driver-toolkit": "true",
			},
			want: ComponentDriver,
		},
		{
			name:   "the vGPU manager DaemonSet",
			labels: map[string]string{"app": "nvidia-vgpu-manager-daemonset"},
			want:   ComponentVGPUManager,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, ok := DriverComponent(tc.labels)
			if !ok {
				t.Fatalf("DriverComponent(%v) found no driver", tc.labels)
			}

			if got != tc.want {
				t.Errorf("DriverComponent(%v) = %q, want %q", tc.labels, got, tc.want)
			}
		})
	}
}

// A pod that is not a driver must not be claimed as one, or every finding
// about a driver would name the wrong pod.
func TestDriverComponentIgnoresEverythingElse(t *testing.T) {
	t.Parallel()

	for _, labels := range []map[string]string{
		nil,
		make(map[string]string),
		{"app": "nvidia-device-plugin-daemonset"},
		{"app": "nvidia-operator-validator"},
		// A name that merely starts the same way.
		{"app": "nvidia-driver-daemonsetfoo"},
		// The component label naming something else.
		{"app.kubernetes.io/component": "nvidia-dcgm-exporter"},
		// A value that is a prefix of the selector's, not the selector's.
		{"app.kubernetes.io/component": "nvidia-driverx"},
	} {
		if component, ok := DriverComponent(labels); ok {
			t.Errorf("DriverComponent(%v) claimed %q, want no driver", labels, component)
		}
	}
}

// The classification has to reach OperandPod.Component, because that is the
// only field a rule sees.
func TestComponentOfPrefersTheDriverSelectors(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "nvidia-gpu-driver-ubuntu22.04-7d9f4c2b1e-abcde",
		Labels: map[string]string{
			"app":                         "nvidia-gpu-driver-ubuntu22.04-7d9f4c2b1e",
			"app.kubernetes.io/component": "nvidia-driver",
		},
	}}

	if got := componentOf(pod); got != ComponentDriver {
		t.Errorf("componentOf = %q, want %q: the app label wins over the driver selectors", got, ComponentDriver)
	}
}

// Everything that is not a driver keeps the component it had, or this change
// would rewrite every other operand's name.
func TestComponentOfLeavesOtherOperandsAlone(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:   "nvidia-device-plugin-daemonset-abcde",
		Labels: map[string]string{"app": "nvidia-device-plugin-daemonset"},
	}}

	if got := componentOf(pod); got != ComponentDevicePlugin {
		t.Errorf("componentOf = %q, want %q", got, ComponentDevicePlugin)
	}
}

// terminatedAt builds a container status that has terminated.
func terminatedAt(name string, code int32, reason string, when metav1.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name: name,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Reason: reason, FinishedAt: when,
		}},
	}
}

// crashLooping builds the shape a container in CrashLoopBackOff actually has:
// the current state is Waiting, and the exit code of the run that failed is in
// LastTerminationState. A reader of State alone sees nothing at all.
func crashLooping(name string, code int32, when metav1.Time) corev1.ContainerStatus {
	return corev1.ContainerStatus{
		Name:  name,
		State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			ExitCode: code, Reason: "Error", FinishedAt: when,
		}},
	}
}

func TestInitResultsCarryTheLastTermination(t *testing.T) {
	t.Parallel()

	when := metav1.Date(2026, 10, 11, 9, 30, 0, 0, time.UTC)
	results := initResults([]corev1.ContainerStatus{crashLooping("toolkit-validation", 1, when)})

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}

	result := results[0]

	// The current state is Waiting, so there is no exit code to report there.
	if result.ExitCode != nil {
		t.Errorf("ExitCode is %d, want nothing: the container has not terminated, it is waiting", *result.ExitCode)
	}

	if result.Reason != "CrashLoopBackOff" {
		t.Errorf("Reason is %q, want %q", result.Reason, "CrashLoopBackOff")
	}

	if !result.LastTermination.Observed {
		t.Fatal("LastTermination.Observed is false, so the crash loop's exit code is invisible")
	}

	if result.LastTermination.ExitCode != 1 {
		t.Errorf("LastTermination.ExitCode is %d, want 1", result.LastTermination.ExitCode)
	}

	if result.LastTermination.FinishedAt != "2026-10-11T09:30:00Z" {
		t.Errorf("LastTermination.FinishedAt is %q, want RFC3339 in UTC", result.LastTermination.FinishedAt)
	}
}

// A container that has never terminated has nothing to report, and a zero
// Termination must not read as an exit code of zero.
func TestInitResultsLeaveTheLastTerminationUnobserved(t *testing.T) {
	t.Parallel()

	results := initResults([]corev1.ContainerStatus{{
		Name:  "toolkit-validation",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}})

	if results[0].LastTermination.Observed {
		t.Error("LastTermination.Observed is true on a container that has never terminated")
	}
}

// The pod's last termination is the newest across its containers, because
// NV-DRV-001's window asks one question of the pod: when did it last die.
func TestOperandPodCarriesTheNewestContainerTermination(t *testing.T) {
	t.Parallel()

	older := metav1.Date(2026, 10, 11, 8, 0, 0, 0, time.UTC)
	newer := metav1.Date(2026, 10, 11, 11, 45, 0, 0, time.UTC)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-driver-daemonset-abcde",
			Labels: map[string]string{"app": "nvidia-driver-daemonset"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{
			terminatedAt("nvidia-driver-ctr", 1, "Error", older),
			crashLooping("openshift-driver-toolkit-ctr", 137, newer),
		}},
	}

	operand := operandFromPod(pod)

	if !operand.LastTermination.Observed {
		t.Fatal("LastTermination.Observed is false, so the pod's restart cannot be dated")
	}

	if operand.LastTermination.FinishedAt != "2026-10-11T11:45:00Z" {
		t.Errorf("LastTermination.FinishedAt is %q, want the newer of the two", operand.LastTermination.FinishedAt)
	}

	if operand.LastTermination.ExitCode != 137 {
		t.Errorf("LastTermination.ExitCode is %d, want 137, the newer one's", operand.LastTermination.ExitCode)
	}
}

func TestOperandPodWithoutATerminationReportsNone(t *testing.T) {
	t.Parallel()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-driver-daemonset-abcde",
			Labels: map[string]string{"app": "nvidia-driver-daemonset"}},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "nvidia-driver-ctr", Ready: true,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}

	if operandFromPod(pod).LastTermination.Observed {
		t.Error("LastTermination.Observed is true on a pod whose containers have never terminated")
	}
}
