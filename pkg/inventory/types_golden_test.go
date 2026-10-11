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
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// goldenTime is the one timestamp every golden fixture uses, so that the
// expected bytes below do not move with the clock.
func goldenTime() metav1.Time {
	return metav1.NewTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
}

// goldenNode is a GPUNode with a value in every field, including one of every
// nested type. It is the fixture the expected bytes below describe.
//
// The node conditions carry real timestamps on purpose: metav1.Time is a
// struct, so the omitempty on NodeCondition's two time fields does not apply
// to them, and a zero one marshals as null. That null is upstream's, not
// this package's, and setting the times is what keeps a realistic node free
// of nulls.
func goldenNode() GPUNode {
	exitCode := int32(0)

	return GPUNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "node-a",
			Labels:            map[string]string{"nvidia.com/gpu.present": "true"},
			CreationTimestamp: goldenTime(),
		},
		Product:        "NVIDIA-H100-80GB-HBM3",
		Count:          8,
		Capacity:       8,
		Allocatable:    8,
		Requested:      3,
		RequestedKnown: true,
		DriverVersion:  "580.65.06",
		CUDAVersion:    "13.0",
		Mode:           ModeBoth,
		MIG: MIG{
			Strategy:    "mixed",
			Config:      "all-1g.10gb",
			ConfigState: "success",
			Capable:     true,
			Resources: map[string]ResourceCount{
				"nvidia.com/mig-1g.10gb": {Capacity: 7, Allocatable: 7},
			},
		},
		Sharing:       Sharing{Strategy: "time-slicing", Replicas: 4},
		UpgradeState:  "upgrade-done",
		Deploy:        map[string]string{"driver": "true"},
		Clique:        "clique-0",
		Topology:      map[string]string{"fabric.topograph.run/tier-1": "block-1"},
		Ready:         true,
		Unschedulable: false,
		Taints: []corev1.Taint{
			{Key: "nvidia.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule},
		},
		Conditions: []corev1.NodeCondition{{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  goldenTime(),
			LastTransitionTime: goldenTime(),
			Reason:             "KubeletReady",
		}},
		Operands: []OperandPod{{
			Name:           "nvidia-driver-daemonset-xyz",
			Namespace:      "gpu-operator",
			Component:      ComponentDriver,
			Phase:          "Running",
			Restarts:       1,
			Images:         []string{"nvcr.io/nvidia/driver:580"},
			Ready:          true,
			InitContainers: []InitResult{{Name: "k8s-driver-manager", ExitCode: &exitCode, Reason: "Completed"}},
		}},
		ResourceSlices: []string{"node-a-gpu.nvidia.com-abcde"},
		Hinted:         false,
	}
}

// goldenNodeJSON is the wire form of goldenNode, written out by hand from the
// struct tags rather than captured from a run. It is the whole contract in one
// string: a renamed tag anywhere in the tree, a nested type that stopped
// emitting a field, or a list item that lost its envelope all land here.
const goldenNodeJSON = `{"kind":"GPUNode","apiVersion":"kubectl-nv.nvidia.com/v1alpha1",` +
	`"metadata":{"name":"node-a","creationTimestamp":"2026-01-02T03:04:05Z",` +
	`"labels":{"nvidia.com/gpu.present":"true"}},` +
	`"product":"NVIDIA-H100-80GB-HBM3","count":8,` +
	`"capacity":8,"allocatable":8,"requested":3,"requestedKnown":true,` +
	`"driverVersion":"580.65.06","cudaVersion":"13.0","mode":"both",` +
	`"mig":{"strategy":"mixed","config":"all-1g.10gb","configState":"success","capable":true,` +
	`"resources":{"nvidia.com/mig-1g.10gb":{"capacity":7,"allocatable":7}}},` +
	`"sharing":{"strategy":"time-slicing","replicas":4},` +
	`"upgradeState":"upgrade-done","deploy":{"driver":"true"},"clique":"clique-0",` +
	`"topology":{"fabric.topograph.run/tier-1":"block-1"},` +
	`"ready":true,"unschedulable":false,` +
	`"taints":[{"key":"nvidia.com/gpu","value":"present","effect":"NoSchedule"}],` +
	`"conditions":[{"type":"Ready","status":"True",` +
	`"lastHeartbeatTime":"2026-01-02T03:04:05Z","lastTransitionTime":"2026-01-02T03:04:05Z",` +
	`"reason":"KubeletReady"}],` +
	`"operands":[{"name":"nvidia-driver-daemonset-xyz","namespace":"gpu-operator",` +
	`"component":"nvidia-driver-daemonset","phase":"Running","restarts":1,` +
	`"images":["nvcr.io/nvidia/driver:580"],"ready":true,` +
	`"lastTermination":{"observed":false,"exitCode":0,"reason":"","finishedAt":""},` +
	`"initContainers":[{"name":"k8s-driver-manager","exitCode":0,"reason":"Completed",` +
	`"lastTermination":{"observed":false,"exitCode":0,"reason":"","finishedAt":""}}]}],` +
	`"resourceSlices":["node-a-gpu.nvidia.com-abcde"],"hinted":false}`

func TestGPUNodeGoldenJSON(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(goldenNode())
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	if string(raw) != goldenNodeJSON {
		t.Errorf("json  =\n%s\nwant  =\n%s", raw, goldenNodeJSON)
	}
}

// The list golden is what catches an item marshalled without its envelope:
// Items holds GPUNode values, and a value whose MarshalJSON stopped being
// reached would lose its kind and apiVersion here and nowhere else.
func TestGPUNodeListGoldenJSON(t *testing.T) {
	t.Parallel()

	list := GPUNodeList{Items: []GPUNode{goldenNode()}}

	raw, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	want := `{"kind":"GPUNodeList","apiVersion":"kubectl-nv.nvidia.com/v1alpha1","items":[` +
		goldenNodeJSON + `]}`

	if string(raw) != want {
		t.Errorf("json  =\n%s\nwant  =\n%s", raw, want)
	}
}

// The golden round trips: every field above comes back out of the bytes. A
// golden that only ever marshals would not notice a tag that two fields now
// share.
func TestGoldenRoundTrips(t *testing.T) {
	t.Parallel()

	var got GPUNode
	if err := json.Unmarshal([]byte(goldenNodeJSON), &got); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("remarshalling: %v", err)
	}

	if string(raw) != goldenNodeJSON {
		t.Errorf("round trip =\n%s\nwant\n%s", raw, goldenNodeJSON)
	}
}

// Schema stability is the reason the nested types carry no omitempty: every
// node has the same field set whatever is set on it, so a consumer can read
// mig.strategy or operands[].component without first checking whether the key
// is there.
//
// The golden above cannot show that, because every field in it has a value and
// omitempty only hides an empty one. This is the zero-value half: the exact
// bytes of each nested type with nothing set.
func TestNestedTypesHaveTheSameFieldSetWhenEmpty(t *testing.T) {
	t.Parallel()

	node := GPUNode{Operands: []OperandPod{{InitContainers: []InitResult{{}}}}}

	raw, err := json.Marshal(node)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}

	var got struct {
		MIG      json.RawMessage   `json:"mig"`
		Sharing  json.RawMessage   `json:"sharing"`
		Operands []json.RawMessage `json:"operands"`
	}

	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshalling: %v", err)
	}

	const (
		wantMIG     = `{"strategy":"","config":"","configState":"","capable":false,"resources":{}}`
		wantSharing = `{"strategy":"","replicas":0}`
		wantOperand = `{"name":"","namespace":"","component":"","phase":"","restarts":0,` +
			`"images":[],"ready":false,"lastTermination":{"observed":false,"exitCode":0,"reason":"","finishedAt":""},"initContainers":[{"name":"","reason":"","lastTermination":{"observed":false,"exitCode":0,"reason":"","finishedAt":""}}]}`
	)

	if string(got.MIG) != wantMIG {
		t.Errorf("mig  = %s\nwant = %s", got.MIG, wantMIG)
	}

	if string(got.Sharing) != wantSharing {
		t.Errorf("sharing = %s\nwant    = %s", got.Sharing, wantSharing)
	}

	if len(got.Operands) != 1 {
		t.Fatalf("operands = %d, want 1", len(got.Operands))
	}

	if string(got.Operands[0]) != wantOperand {
		t.Errorf("operand = %s\nwant    = %s", got.Operands[0], wantOperand)
	}
}

// InitResult.ExitCode is the one field whose presence varies, and it varies on
// purpose: the alternative for a nil pointer is a null. Both halves are pinned
// so that a change to either is a deliberate one.
func TestExitCodeIsTheOnlyVaryingKey(t *testing.T) {
	t.Parallel()

	exitCode := int32(3)

	for name, tc := range map[string]struct {
		result InitResult
		want   string
	}{
		"unfinished": {InitResult{Name: "toolkit-validation"}, `{"name":"toolkit-validation","reason":"","lastTermination":{"observed":false,"exitCode":0,"reason":"","finishedAt":""}}`},
		"exited": {
			InitResult{Name: "toolkit-validation", ExitCode: &exitCode, Reason: "Error"},
			`{"name":"toolkit-validation","exitCode":3,"reason":"Error","lastTermination":{"observed":false,"exitCode":0,"reason":"","finishedAt":""}}`,
		},
		"exited zero": {
			InitResult{Name: "toolkit-validation", ExitCode: new(int32), Reason: "Completed"},
			`{"name":"toolkit-validation","exitCode":0,"reason":"Completed","lastTermination":{"observed":false,"exitCode":0,"reason":"","finishedAt":""}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			raw, err := json.Marshal(tc.result)
			if err != nil {
				t.Fatalf("marshalling: %v", err)
			}

			if string(raw) != tc.want {
				t.Errorf("json = %s, want %s", raw, tc.want)
			}
		})
	}
}
