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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// taintedFixture is the YAML a gather writes for a cluster whose GPU nodes are
// tainted and whose ClusterPolicy tolerates the taint. Every value in it is
// one a redaction must not touch, and every one of them sits under a key that
// a credential pattern matches: a toleration's key is spelled "key", and
// automountServiceAccountToken is spelled with "token" in it.
const taintedFixture = `apiVersion: v1
kind: NodeList
items:
- apiVersion: v1
  kind: Node
  metadata:
    name: gpu-node-1
  spec:
    taints:
    - key: nvidia.com/gpu
      value: present
      effect: NoSchedule
  status:
    capacity:
      nvidia.com/gpu: "4"
`

// policyFixture is the ClusterPolicy as the gather writes it.
const policyFixture = `apiVersion: nvidia.com/v1
kind: ClusterPolicy
metadata:
  name: cluster-policy
spec:
  daemonsets:
    tolerations:
    - key: nvidia.com/gpu
      operator: Exists
      effect: NoSchedule
  driver:
    enabled: true
    repository: nvcr.io/nvidia
status:
  state: ready
`

// podFixture holds the two booleans a credential pattern would take for
// credentials if it read the key and not the value.
const podFixture = `apiVersion: v1
kind: PodList
items:
- apiVersion: v1
  kind: Pod
  metadata:
    name: dp-xyz
    namespace: gpu-operator
  spec:
    nodeName: gpu-node-1
    automountServiceAccountToken: false
    serviceAccountName: nvidia-device-plugin
    containers:
    - name: plugin
      securityContext:
        privileged: true
  status:
    phase: Running
`

// A redaction that changed an answer would be worse than no redaction: a
// support engineer reading the redacted bundle would be told something about
// the cluster that is not true.
//
// These are the values a generous credential pattern reaches for and must
// leave alone: a toleration and a taint are keyed by a field literally called
// "key", and automountServiceAccountToken is a boolean whose name holds
// "token". A redaction that wrote REDACTED over either changes which pods the
// operator can place.
func TestRedactLeavesTheValuesThatAreNotSecrets(t *testing.T) {
	dir := t.TempDir()
	for name, body := range map[string]string{
		"nodes.yaml":            taintedFixture,
		"cluster_policy.yaml":   policyFixture,
		"gpu_operand_pods.yaml": podFixture,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting: %v", err)
	}

	for _, tc := range []struct {
		file string
		keep []string
	}{
		{file: "nodes.yaml", keep: []string{
			"key: nvidia.com/gpu", "value: present", "effect: NoSchedule",
		}},
		{file: "cluster_policy.yaml", keep: []string{
			"key: nvidia.com/gpu", "operator: Exists", "enabled: true", "state: ready",
		}},
		{file: "gpu_operand_pods.yaml", keep: []string{
			"automountServiceAccountToken: false", "privileged: true",
			"serviceAccountName: nvidia-device-plugin",
		}},
	} {
		raw, rerr := os.ReadFile(filepath.Join(dir, tc.file))
		if rerr != nil {
			t.Fatalf("reading %s: %v", tc.file, rerr)
		}
		for _, keep := range tc.keep {
			if !strings.Contains(string(raw), keep) {
				t.Errorf("%s lost %q, which is not a secret:\n%s", tc.file, keep, raw)
			}
		}
	}
}

// A node name is a name, not a substring. A cluster with a node called "gpu"
// is unusual and legal, and a redaction that replaced every occurrence of the
// three letters would rewrite nvidia.com/gpu in every file of the bundle:
// every node would stop advertising GPUs and the whole report would change.
func TestRedactReplacesANodeNameOnTokenBoundariesOnly(t *testing.T) {
	dir := t.TempDir()
	body := `apiVersion: v1
kind: NodeList
items:
- apiVersion: v1
  kind: Node
  metadata:
    name: gpu
    labels:
      nvidia.com/gpu.present: "true"
      nvidia.com/gpu.count: "8"
  spec:
    taints:
    - key: nvidia.com/gpu
      effect: NoSchedule
  status:
    capacity:
      nvidia.com/gpu: "8"
      nvidia.com/gpu.shared: "16"
`
	if err := os.WriteFile(filepath.Join(dir, "nodes.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing nodes.yaml: %v", err)
	}

	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "nodes.yaml"))
	if err != nil {
		t.Fatalf("reading the redacted nodes.yaml: %v", err)
	}
	text := string(raw)

	for _, keep := range []string{
		"nvidia.com/gpu.present", "nvidia.com/gpu.count", "nvidia.com/gpu.shared",
		"key: nvidia.com/gpu",
	} {
		if !strings.Contains(text, keep) {
			t.Errorf("the redaction rewrote %q, which is a resource name and not the node:\n%s", keep, text)
		}
	}
	// And the node itself is still redacted: a boundary rule that gave up on
	// the whole name would pass every assertion above.
	if strings.Contains(text, "name: gpu\n") {
		t.Errorf("the node is still called gpu:\n%s", text)
	}
}
