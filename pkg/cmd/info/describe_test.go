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

package info

import (
	"bytes"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
)

// TestNewCommandFlags pins the flags the command offers. A flag that silently
// stops being registered is a flag every script using it starts failing on.
func TestNewCommandFlags(t *testing.T) {
	t.Parallel()

	c := NewCommand(&cmd.Options{})

	for _, name := range []string{"from-bundle", "output"} {
		if c.Flags().Lookup(name) == nil {
			t.Errorf("gpu info has no --%s flag", name)
		}
	}

	if short := c.Flags().ShorthandLookup("o"); short == nil || short.Name != "output" {
		t.Error("gpu info has no -o shorthand for --output")
	}

	if err := c.Args(c, []string{"worker-1", "worker-2"}); err == nil {
		t.Error("gpu info accepted two nodes, want exactly one")
	}
}

// fullNode is a node with something in every section, so that one description
// exercises all of them.
func fullNode() *inventory.GPUNode {
	n := inventory.NewGPUNode("worker-1")
	n.Labels = map[string]string{"nvidia.com/gpu.product": "A100-SXM4-40GB"}
	n.Product = "A100-SXM4-40GB"
	n.Count = 8
	n.Capacity, n.Allocatable = 8, 6
	n.Requested, n.RequestedKnown = 6, true
	n.DriverVersion, n.CUDAVersion = "550.107.02", "12.5"
	n.Mode = inventory.ModeBoth
	n.Clique = "7b968a6d-c8aa-45e1-9e07-e1e51be99c31.1"
	n.UpgradeState = "upgrade-done"
	n.MIG = inventory.MIG{
		Strategy: "mixed",
		Capable:  true,
		Config:   "all-balanced",
		Resources: map[string]inventory.ResourceCount{
			"nvidia.com/mig-1g.10gb": {Capacity: 14, Allocatable: 12},
		},
	}
	n.Sharing = inventory.Sharing{Strategy: "time-slicing", Replicas: 4}
	n.Deploy = map[string]string{"driver": "true"}
	n.Topology = map[string]string{"fabric.topograph.run/tier-0": "switch-a"}
	n.Ready = true
	n.Unschedulable = true
	n.Taints = []corev1.Taint{{
		Key: "nvidia.com/gpu", Value: "present", Effect: corev1.TaintEffectNoSchedule,
	}}
	n.Conditions = []corev1.NodeCondition{{
		Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady",
	}}
	n.ResourceSlices = []string{"worker-1-gpu-pool-0"}

	exit := int32(1)
	n.Operands = []inventory.OperandPod{{
		Name:      "nvidia-operator-validator-abc",
		Namespace: "gpu-operator",
		Component: inventory.ComponentOperatorValidator,
		Phase:     "Running",
		Restarts:  2,
		Ready:     false,
		Images:    []string{"nvcr.io/nvidia/cloud-native/gpu-operator-validator:v26.3.3"},
		InitContainers: []inventory.InitResult{
			{Name: "toolkit-validation", ExitCode: &exit, Reason: "Error"},
			{Name: "cuda-validation", Reason: "PodInitializing"},
		},
	}}

	return n
}

func TestDescribe(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := describe(&buf, fullNode()); err != nil {
		t.Fatalf("describe: %v", err)
	}

	got := buf.String()

	for _, want := range []string{
		"worker-1",
		"Ready,SchedulingDisabled",
		"A100-SXM4-40GB",
		"550.107.02",
		"both",
		"7b968a6d-c8aa-45e1-9e07-e1e51be99c31.1",
		"upgrade-done",
		"nvidia.com/mig-1g.10gb",
		"time-slicing",
		"fabric.topograph.run/tier-0",
		"nvidia.com/gpu=present:NoSchedule",
		"KubeletReady",
		"worker-1-gpu-pool-0",
		"nvidia-operator-validator-abc",
		"nvcr.io/nvidia/cloud-native/gpu-operator-validator:v26.3.3",
		// The validator's verdict, which is the whole reason InitResult exists.
		"toolkit-validation",
		"exit 1",
		"Error",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("description does not mention %q:\n%s", want, got)
		}
	}

	// "has not finished" and "exited 0" are different facts, and a view that
	// renders the first as "exit 0" turns a stuck validation into a passing one.
	if !strings.Contains(got, "cuda-validation: not finished") {
		t.Errorf("description does not say the unfinished init container is unfinished:\n%s", got)
	}
}

// TestDescribeStaysReadable pins that one long line does not push every value
// in the description to the right. A tab writer aligns a whole column block to
// its widest cell, and an operand image line is sixty characters wide, so the
// obvious rendering puts every value half a terminal away from its key.
func TestDescribeStaysReadable(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := describe(&buf, fullNode()); err != nil {
		t.Fatalf("describe: %v", err)
	}

	line := sectionLine(t, buf.String(), "Product:")

	gap := strings.Index(line, "A100-SXM4-40GB") - len("Product:")
	if gap > 20 {
		t.Errorf("the value of Product sits %d columns past its key:\n%s", gap, line)
	}
}

// TestDescribeRequestedUnknown pins that a node whose pods were not listed
// says so rather than reporting 0 requested GPUs.
func TestDescribeRequestedUnknown(t *testing.T) {
	t.Parallel()

	n := fullNode()
	n.Requested, n.RequestedKnown = 0, false

	var buf bytes.Buffer
	if err := describe(&buf, n); err != nil {
		t.Fatalf("describe: %v", err)
	}

	line := sectionLine(t, buf.String(), "Requested")
	if strings.Contains(line, "0") {
		t.Errorf("Requested line reads %q, want it to say nobody counted", line)
	}
}

// TestDescribeEmptySections pins that an empty section says <none> rather than
// disappearing, so that "no operands at all" and "a section this command
// forgot" read differently.
func TestDescribeEmptySections(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	if err := describe(&buf, inventory.NewGPUNode("bare-1")); err != nil {
		t.Fatalf("describe: %v", err)
	}

	got := buf.String()
	for _, heading := range []string{"Taints", "Conditions", "Operands", "Resource slices", "Labels"} {
		if !strings.Contains(got, heading) {
			t.Errorf("description has no %s section:\n%s", heading, got)
		}
	}

	if strings.Count(got, "<none>") < 5 {
		t.Errorf("empty sections are not marked <none>:\n%s", got)
	}
}

// sectionLine returns the line carrying the key, for the assertions that are
// about one line rather than about the whole description.
func sectionLine(t *testing.T, text, key string) string {
	t.Helper()

	for _, line := range strings.Split(text, "\n") {
		if strings.Contains(line, key) {
			return line
		}
	}

	t.Fatalf("no line mentions %q in:\n%s", key, text)

	return ""
}
