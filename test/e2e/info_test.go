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
	"encoding/json"
	"strings"
	"testing"
)

// laneOperands are the components the Mokka lane runs on every worker. They
// are the verbatim "app" label values, which is what OperandPod.Component
// carries; see pkg/inventory/types_components.go.
var laneOperands = []string{
	"nvidia-device-plugin-daemonset",
	"gpu-feature-discovery",
	"nvidia-dcgm-exporter",
	"nvidia-operator-validator",
}

// infoNodeJSON is the part of one GPUNode this suite asserts on. It is the
// detailed shape: gpu info fills the conditions and the slice names, and gpu
// list does not.
type infoNodeJSON struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Product  string `json:"product"`
	Capacity int64  `json:"capacity"`
	Mode     string `json:"mode"`
	Hinted   bool   `json:"hinted"`
	MIG      struct {
		Strategy string `json:"strategy"`
	} `json:"mig"`
	Conditions []struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	} `json:"conditions"`
	Operands []struct {
		Name           string   `json:"name"`
		Namespace      string   `json:"namespace"`
		Component      string   `json:"component"`
		Phase          string   `json:"phase"`
		Images         []string `json:"images"`
		InitContainers []struct {
			Name     string `json:"name"`
			ExitCode *int32 `json:"exitCode"`
			Reason   string `json:"reason"`
		} `json:"initContainers"`
	} `json:"operands"`
	ResourceSlices []string `json:"resourceSlices"`
}

// infoJSON runs gpu info over a node and decodes it.
func infoJSON(t *testing.T, args ...string) infoNodeJSON {
	t.Helper()

	res := run(t, append([]string{"info"}, append(args, "-o", "json")...)...)
	if res.code != 0 {
		t.Fatalf("gpu info exited %d, want 0\nstderr:\n%s", res.code, res.stderr)
	}

	var node infoNodeJSON
	if err := json.Unmarshal([]byte(res.stdout), &node); err != nil {
		t.Fatalf("decoding gpu info output: %v\n%s", err, res.stdout)
	}

	return node
}

func TestInfoAgainstTheLane(t *testing.T) {
	waitForStartedPods(t)

	workers := workerNodeNames(t)
	if len(workers) == 0 {
		t.Fatal("the lane has no worker nodes")
	}

	worker := workers[0]
	node := infoJSON(t, worker)

	t.Run("it describes the node that was asked for", func(t *testing.T) {
		if node.Metadata.Name != worker {
			t.Errorf("name = %q, want %q", node.Metadata.Name, worker)
		}

		if node.Product != laneProduct {
			t.Errorf("product = %q, want %q", node.Product, laneProduct)
		}

		if node.Capacity != laneGPUCount {
			t.Errorf("capacity = %d, want %d", node.Capacity, laneGPUCount)
		}

		if node.Mode != "device-plugin" {
			t.Errorf("mode = %q, want device-plugin", node.Mode)
		}
	})

	t.Run("it fills the details gpu list leaves out", func(t *testing.T) {
		if len(node.Conditions) == 0 {
			t.Error("conditions are empty; gpu info is the command that fills them")
		}

		// Never null, so that a consumer can range without a null check first.
		if node.ResourceSlices == nil {
			t.Error("resourceSlices is null, want an empty array")
		}
	})

	t.Run("the operator's operands are on the node", func(t *testing.T) {
		byComponent := make(map[string]bool, len(node.Operands))
		for _, o := range node.Operands {
			byComponent[o.Component] = true

			if o.Namespace != operatorNamespace {
				t.Errorf("operand %s is in namespace %q, want %q", o.Name, o.Namespace, operatorNamespace)
			}

			if len(o.Images) == 0 {
				t.Errorf("operand %s reports no images; a version mismatch would be invisible", o.Name)
			}
		}

		for _, want := range laneOperands {
			if !byComponent[want] {
				t.Errorf("no %s operand on %s; the node has %v", want, worker, componentsOf(node))
			}
		}
	})

	t.Run("the validator carries its verdict", func(t *testing.T) {
		for _, o := range node.Operands {
			if o.Component != "nvidia-operator-validator" {
				continue
			}

			if len(o.InitContainers) == 0 {
				t.Fatalf("%s reports no init container results; its verdict is in them", o.Name)
			}

			for _, r := range o.InitContainers {
				if r.ExitCode == nil {
					t.Errorf("%s: %s has no exit code, so it has not finished", o.Name, r.Name)

					continue
				}

				if *r.ExitCode != 0 {
					t.Errorf("%s: %s exited %d (%s)", o.Name, r.Name, *r.ExitCode, r.Reason)
				}
			}

			return
		}

		t.Fatalf("no validator pod on %s; the node has %v", worker, componentsOf(node))
	})

	t.Run("the described view names the operands", func(t *testing.T) {
		res := run(t, "info", worker)
		if res.code != 0 {
			t.Fatalf("gpu info exited %d, want 0\nstderr:\n%s", res.code, res.stderr)
		}

		for _, want := range []string{worker, laneProduct, "device-plugin", "Conditions", "Operands"} {
			if !strings.Contains(res.stdout, want) {
				t.Errorf("the description does not mention %q:\n%s", want, res.stdout)
			}
		}
	})
}

// TestInfoOnANodeWithoutGPUs pins the exit code and the message a script
// branches on.
func TestInfoOnANodeWithoutGPUs(t *testing.T) {
	res := run(t, "info", "there-is-no-such-node")

	if res.code != 2 {
		t.Errorf("gpu info exited %d, want 2\nstderr:\n%s", res.code, res.stderr)
	}

	want := "node there-is-no-such-node has no GPUs and no GPU labels"
	if !strings.Contains(res.stderr, want) {
		t.Errorf("stderr does not say %q:\n%s", want, res.stderr)
	}
}

// TestInfoOnTheControlPlane pins that the hinted node is described rather than
// refused: that it claims GPUs and advertises none is the answer.
func TestInfoOnTheControlPlane(t *testing.T) {
	var controlPlane string

	for _, node := range clusterNodes(t) {
		if _, found := node.Labels[controlPlaneRoleLabel]; found {
			controlPlane = node.Name

			break
		}
	}

	if controlPlane == "" {
		t.Skip("the lane has no control plane node to ask about")
	}

	node := infoJSON(t, controlPlane)

	if !node.Hinted {
		t.Errorf("%s is not marked hinted; it carries an NVIDIA label and advertises no GPUs", controlPlane)
	}

	if node.Mode != "none" {
		t.Errorf("%s mode = %q, want none", controlPlane, node.Mode)
	}
}

// componentsOf names the operands a node reported, for a failure message.
func componentsOf(node infoNodeJSON) []string {
	out := make([]string, 0, len(node.Operands))
	for _, o := range node.Operands {
		out = append(out, o.Component)
	}

	return out
}
