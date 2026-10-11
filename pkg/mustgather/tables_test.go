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
	"bytes"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fieldsOf splits a rendered table into its header and the cells of each row.
// The tab writer pads with spaces, so the cells are read back the way a human
// reads them rather than by splitting on a separator that is no longer there.
func fieldsOf(t *testing.T, rendered string) ([]string, [][]string) {
	t.Helper()

	lines := strings.Split(strings.TrimRight(rendered, "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("the table has no header row:\n%s", rendered)
	}
	rows := make([][]string, 0, len(lines)-1)
	for _, line := range lines[1:] {
		rows = append(rows, strings.Fields(line))
	}
	return strings.Fields(lines[0]), rows
}

// TestNodeTableCarriesTheWideColumns covers the node .status file. A support
// engineer reads the kernel and the container runtime off it before anything
// else, and a table rendered without the wide columns sends them back to the
// customer for another gather.
func TestNodeTableCarriesTheWideColumns(t *testing.T) {
	node := v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "gpu-node-1",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-3 * time.Hour)),
			Labels:            map[string]string{"node-role.kubernetes.io/worker": ""},
		},
		Status: v1.NodeStatus{
			Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
			Addresses: []v1.NodeAddress{
				{Type: v1.NodeInternalIP, Address: "10.0.1.7"},
			},
			NodeInfo: v1.NodeSystemInfo{
				KubeletVersion:          "v1.33.1",
				OSImage:                 "Ubuntu 24.04.1 LTS",
				KernelVersion:           "6.8.0-45-generic",
				ContainerRuntimeVersion: "containerd://2.0.0",
			},
		},
	}

	var out bytes.Buffer
	if err := writeNodeTable(&out, []v1.Node{node}); err != nil {
		t.Fatalf("writeNodeTable: %v", err)
	}

	header, rows := fieldsOf(t, out.String())
	if strings.Join(header, " ") != "NAME STATUS ROLES AGE VERSION INTERNAL-IP EXTERNAL-IP OS-IMAGE KERNEL-VERSION CONTAINER-RUNTIME" {
		t.Errorf("header = %v, want kubectl's wide node columns", header)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1:\n%s", len(rows), out.String())
	}

	line := out.String()
	for _, want := range []string{"gpu-node-1", "Ready", "worker", "3h", "v1.33.1", "10.0.1.7", "6.8.0-45-generic", "containerd://2.0.0"} {
		if !strings.Contains(line, want) {
			t.Errorf("the node row is missing %q:\n%s", want, line)
		}
	}
	// A node with no external address prints kubectl's marker rather than an
	// empty cell, which would shift every column after it.
	if !strings.Contains(line, "<none>") {
		t.Errorf("the unset external address is not marked:\n%s", line)
	}
}

// TestNodeTableStatusReadsTheCondition covers the column a reader scans for
// first. A node that is NotReady and one that was never heard from are
// different problems, and both differ from a cordoned node.
func TestNodeTableStatusReadsTheCondition(t *testing.T) {
	cases := []struct {
		name          string
		condition     *v1.NodeCondition
		unschedulable bool
		want          string
	}{
		{name: "ready", condition: &v1.NodeCondition{Type: v1.NodeReady, Status: v1.ConditionTrue}, want: "Ready"},
		{name: "not ready", condition: &v1.NodeCondition{Type: v1.NodeReady, Status: v1.ConditionFalse}, want: "NotReady"},
		{name: "never reported", want: "Unknown"},
		{
			name:          "cordoned",
			condition:     &v1.NodeCondition{Type: v1.NodeReady, Status: v1.ConditionTrue},
			unschedulable: true,
			want:          "Ready,SchedulingDisabled",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			node := &v1.Node{Spec: v1.NodeSpec{Unschedulable: c.unschedulable}}
			if c.condition != nil {
				node.Status.Conditions = []v1.NodeCondition{*c.condition}
			}
			if got := nodeStatus(node); got != c.want {
				t.Errorf("nodeStatus = %q, want %q", got, c.want)
			}
		})
	}
}

// TestPodTableCarriesTheWideColumns covers the pod .status files, which are
// how a reader finds which node an unhappy operand is on without parsing the
// YAML.
func TestPodTableCarriesTheWideColumns(t *testing.T) {
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "nvidia-device-plugin-daemonset-aaaaa",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-90 * time.Minute)),
		},
		Spec: v1.PodSpec{
			NodeName:   "gpu-node-2",
			Containers: []v1.Container{{Name: "a"}, {Name: "b"}},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			PodIP: "10.244.1.5",
			ContainerStatuses: []v1.ContainerStatus{
				{Name: "a", Ready: true, RestartCount: 2},
				{Name: "b", Ready: false, RestartCount: 3},
			},
		},
	}

	var out bytes.Buffer
	if err := writePodTable(&out, []v1.Pod{pod}); err != nil {
		t.Fatalf("writePodTable: %v", err)
	}

	header, rows := fieldsOf(t, out.String())
	if strings.Join(header, " ") != "NAME READY STATUS RESTARTS AGE IP NODE NOMINATED NODE READINESS GATES" {
		t.Errorf("header = %v, want kubectl's wide pod columns", header)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1:\n%s", len(rows), out.String())
	}

	row := rows[0]
	// The cells are checked by position rather than by substring: a renderer
	// that puts the restart count in the ready column passes a substring check.
	want := map[int]string{0: "nvidia-device-plugin-daemonset-aaaaa", 1: "1/2", 2: "Running", 3: "5", 5: "10.244.1.5", 6: "gpu-node-2"}
	for index, value := range want {
		if index >= len(row) {
			t.Fatalf("the row has %d cells, want at least %d:\n%s", len(row), index+1, out.String())
		}
		if row[index] != value {
			t.Errorf("cell %d = %q, want %q (row: %v)", index, row[index], value, row)
		}
	}
}

// TestPodTableStatusPrefersTheWaitingReason keeps the column useful for the
// pods a gather is run about. A driver container stuck pulling its image is
// Pending by phase, and Pending is not a diagnosis.
func TestPodTableStatusPrefersTheWaitingReason(t *testing.T) {
	pod := &v1.Pod{
		Spec: v1.PodSpec{Containers: []v1.Container{{Name: "driver"}}},
		Status: v1.PodStatus{
			Phase: v1.PodPending,
			ContainerStatuses: []v1.ContainerStatus{{
				Name:  "driver",
				State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
			}},
		},
	}

	if got := podStatus(pod); got != "ImagePullBackOff" {
		t.Errorf("podStatus = %q, want ImagePullBackOff", got)
	}
}

// TestDaemonSetTableCountsTheRollout covers the daemonset .status file, which
// is where a half-rolled-out driver shows up as a gap between desired and
// ready.
func TestDaemonSetTableCountsTheRollout(t *testing.T) {
	set := appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "nvidia-driver-daemonset",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		},
		Spec: appsv1.DaemonSetSpec{
			Template: v1.PodTemplateSpec{
				Spec: v1.PodSpec{NodeSelector: map[string]string{"nvidia.com/gpu.present": "true"}},
			},
		},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: 4,
			CurrentNumberScheduled: 4,
			NumberReady:            2,
			UpdatedNumberScheduled: 3,
			NumberAvailable:        2,
		},
	}

	var out bytes.Buffer
	if err := writeDaemonSetTable(&out, []appsv1.DaemonSet{set}); err != nil {
		t.Fatalf("writeDaemonSetTable: %v", err)
	}

	header, rows := fieldsOf(t, out.String())
	if strings.Join(header, " ") != "NAME DESIRED CURRENT READY UP-TO-DATE AVAILABLE NODE SELECTOR AGE" {
		t.Errorf("header = %v, want kubectl's daemonset columns", header)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1:\n%s", len(rows), out.String())
	}

	row := rows[0]
	want := map[int]string{0: "nvidia-driver-daemonset", 1: "4", 2: "4", 3: "2", 4: "3", 5: "2", 6: "nvidia.com/gpu.present=true"}
	for index, value := range want {
		if index >= len(row) {
			t.Fatalf("the row has %d cells, want at least %d:\n%s", len(row), index+1, out.String())
		}
		if row[index] != value {
			t.Errorf("cell %d = %q, want %q (row: %v)", index, row[index], value, row)
		}
	}
}
