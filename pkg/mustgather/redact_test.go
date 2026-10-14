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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// The strings planted in the fixture bundle. Every one of them is something a
// bundle must not carry to NVIDIA support, and every assertion below is made
// against these exact strings rather than against a pattern: a redactor that
// rewrote them into something else that still held the original would satisfy
// a pattern and fail here.
const (
	plantedNodeA    = "gpu-worker-01.prod.example.com"
	plantedNodeB    = "gpu-worker-02.prod.example.com"
	plantedHostA    = "gpu-worker-01"
	plantedIPA      = "10.42.7.11"
	plantedIPB      = "10.42.7.12"
	plantedPodIP    = "10.244.3.5"
	plantedIPv6     = "fd00:dead:beef::17"
	plantedAPIKey   = "planted-api-key-0ff1ce"
	plantedPassword = "planted-password-hunter2"
	plantedBearer   = "eyJhbGciOiJIUzI1NiJ9.plantedbearertoken"
	plantedKubeTok  = "planted-kubeconfig-token-abc123"
	plantedSecret   = "cGxhbnRlZC1zZWNyZXQtZGF0YQ=="
)

// plantedStrings is everything the redacted bundle must no longer hold.
func plantedStrings() []string {
	return []string{
		plantedNodeA, plantedNodeB, plantedHostA,
		plantedIPA, plantedIPB, plantedPodIP, plantedIPv6,
		plantedAPIKey, plantedPassword, plantedBearer, plantedKubeTok, plantedSecret,
	}
}

// resourceQuantity is a whole-number quantity, for the fixture capacities.
func resourceQuantity(n int64) resource.Quantity {
	return *resource.NewQuantity(n, resource.DecimalSI)
}

// fixtureNodes is the node list a gather writes, with the planted identities on
// it: the name, the hostname label, and the addresses.
func fixtureNodes() *corev1.NodeList {
	node := func(name, host, ip string) corev1.Node {
		return corev1.Node{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
			ObjectMeta: metav1.ObjectMeta{
				Name: name,
				Labels: map[string]string{
					"kubernetes.io/hostname": host,
					"nvidia.com/gpu.present": "true",
				},
			},
			Status: corev1.NodeStatus{
				Capacity:    corev1.ResourceList{"nvidia.com/gpu": resourceQuantity(4)},
				Allocatable: corev1.ResourceList{"nvidia.com/gpu": resourceQuantity(4)},
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Addresses: []corev1.NodeAddress{
					{Type: corev1.NodeInternalIP, Address: ip},
					{Type: corev1.NodeHostName, Address: host},
				},
			},
		}
	}

	return &corev1.NodeList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"},
		Items: []corev1.Node{
			node(plantedNodeA, plantedHostA, plantedIPA),
			node(plantedNodeB, "gpu-worker-02", plantedIPB),
		},
	}
}

// fixturePods are the operand pods, which is where the node cross-reference
// lives: spec.nodeName names a node the node list also names, and a reader
// follows it from one file to the other.
func fixturePods() *corev1.PodList {
	return &corev1.PodList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
		Items: []corev1.Pod{{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nvidia-driver-daemonset-abcde",
				Namespace: "gpu-operator",
				Labels:    map[string]string{"app": "nvidia-driver-daemonset"},
			},
			Spec: corev1.PodSpec{
				NodeName: plantedNodeA,
				Containers: []corev1.Container{{
					Name:  "nvidia-driver-ctr",
					Image: "nvcr.io/nvidia/driver:550.54.15",
					Env: []corev1.EnvVar{
						{Name: "NVIDIA_API_KEY", Value: plantedAPIKey},
						{Name: "REGISTRY_PASSWORD", Value: plantedPassword},
						{Name: "NVIDIA_VISIBLE_DEVICES", Value: "all"},
					},
				}},
			},
			Status: corev1.PodStatus{
				Phase:  corev1.PodRunning,
				HostIP: plantedIPA,
				PodIP:  plantedPodIP,
			},
		}},
	}
}

// fixtureBundle writes a bundle holding every planted string, in the files a
// real gather writes them in.
func fixtureBundle(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	nodes, err := yaml.Marshal(fixtureNodes())
	if err != nil {
		t.Fatalf("marshalling the fixture nodes: %v", err)
	}
	write("nodes.yaml", nodes)

	pods, err := yaml.Marshal(fixturePods())
	if err != nil {
		t.Fatalf("marshalling the fixture pods: %v", err)
	}
	write("gpu_operand_pods.yaml", pods)

	write("must-gather.log", []byte(strings.Join([]string{
		"collecting node " + plantedNodeA,
		"kubelet on " + plantedHostA + " answered from " + plantedIPA,
		"the IMEX daemon reached " + plantedIPv6,
		"GET /api with Authorization: Bearer " + plantedBearer,
		"",
	}, "\n")))

	write("kubeconfig.fragment", []byte(strings.Join([]string{
		"users:",
		"- name: admin",
		"  user:",
		"    token: " + plantedKubeTok,
		"",
	}, "\n")))

	write("pull-secret.yaml", []byte(strings.Join([]string{
		"apiVersion: v1",
		"kind: Secret",
		"metadata:",
		"  name: ngc-secret",
		"data:",
		"  .dockerconfigjson: " + plantedSecret,
		"",
	}, "\n")))

	return dir
}

// bundleText reads every file of a bundle, keyed by name.
func bundleText(t *testing.T, dir string) map[string]string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the bundle: %v", err)
	}
	out := map[string]string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatalf("reading %s: %v", entry.Name(), err)
		}
		out[entry.Name()] = string(raw)
	}

	return out
}

// Nothing planted survives, anywhere. The assertion is per file so that a
// failure names the one that leaked rather than saying the bundle did.
func TestRedactLeavesNoPlantedStringInAnyFile(t *testing.T) {
	t.Parallel()

	dir := fixtureBundle(t)
	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting the bundle: %v", err)
	}

	for name, text := range bundleText(t, dir) {
		for _, planted := range plantedStrings() {
			if strings.Contains(text, planted) {
				t.Errorf("%s still holds %q", name, planted)
			}
		}
	}
}

// A bundle whose cross-references no longer line up is a bundle nobody can
// read: the pod says which node it runs on, and that has to be a node the node
// list names.
func TestRedactKeepsNodeCrossReferences(t *testing.T) {
	t.Parallel()

	dir := fixtureBundle(t)
	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting the bundle: %v", err)
	}

	var nodes corev1.NodeList
	if err := yaml.Unmarshal([]byte(bundleText(t, dir)["nodes.yaml"]), &nodes); err != nil {
		t.Fatalf("the redacted nodes.yaml does not parse: %v", err)
	}
	var pods corev1.PodList
	if err := yaml.Unmarshal([]byte(bundleText(t, dir)["gpu_operand_pods.yaml"]), &pods); err != nil {
		t.Fatalf("the redacted gpu_operand_pods.yaml does not parse: %v", err)
	}

	if len(nodes.Items) != 2 || len(pods.Items) != 1 {
		t.Fatalf("the redaction changed the shape of the bundle: %d nodes, %d pods",
			len(nodes.Items), len(pods.Items))
	}

	named := map[string]bool{}
	for _, node := range nodes.Items {
		named[node.Name] = true
	}
	if !named[pods.Items[0].Spec.NodeName] {
		t.Errorf("the pod runs on %q, which the redacted node list does not name: %v",
			pods.Items[0].Spec.NodeName, named)
	}

	// The same input maps to the same pseudonym wherever it appears, which is
	// what lets a reader follow it between files at all.
	first := nodes.Items[0]
	if first.Name == "" {
		t.Fatal("a node came back with no name")
	}
	if got := first.Labels["kubernetes.io/hostname"]; got == "" || got == plantedHostA {
		t.Errorf("the hostname label is %q: it was not redacted", got)
	}
}

// Two bundles must not be comparable. A fixed mapping, or one derived from the
// input alone, would let anyone holding two bundles confirm that a node they
// guessed at is in both.
func TestRedactIsNotComparableAcrossBundles(t *testing.T) {
	t.Parallel()

	pseudonymOf := func(t *testing.T) string {
		t.Helper()

		dir := fixtureBundle(t)
		r, err := NewRedactor()
		if err != nil {
			t.Fatalf("building the redactor: %v", err)
		}
		if err := r.RedactBundle(dir); err != nil {
			t.Fatalf("redacting the bundle: %v", err)
		}
		var nodes corev1.NodeList
		if err := yaml.Unmarshal([]byte(bundleText(t, dir)["nodes.yaml"]), &nodes); err != nil {
			t.Fatalf("the redacted nodes.yaml does not parse: %v", err)
		}

		return nodes.Items[0].Name
	}

	if first, second := pseudonymOf(t), pseudonymOf(t); first == second {
		t.Errorf("two runs gave the same node the same pseudonym %q: the mapping is not keyed per run", first)
	}
}

// The key is what makes the mapping unreversible, so it is never written. A
// bundle carrying it would be a bundle anyone could undo.
func TestTheRedactionKeyIsNeverWritten(t *testing.T) {
	t.Parallel()

	dir := fixtureBundle(t)
	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting the bundle: %v", err)
	}

	key := r.keyHexForTest()
	if key == "" {
		t.Fatal("the redactor has no key")
	}
	for name, text := range bundleText(t, dir) {
		if strings.Contains(text, key) {
			t.Errorf("%s holds the redaction key", name)
		}
	}
}

// The things a reader needs are not identities and must survive: the image a
// driver pod runs is the single most useful line in a bundle.
func TestRedactKeepsWhatTheBundleIsReadFor(t *testing.T) {
	t.Parallel()

	dir := fixtureBundle(t)
	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting the bundle: %v", err)
	}

	pods := bundleText(t, dir)["gpu_operand_pods.yaml"]
	for _, want := range []string{
		"nvcr.io/nvidia/driver:550.54.15",
		"nvidia-driver-daemonset",
		"NVIDIA_VISIBLE_DEVICES",
		"all",
	} {
		if !strings.Contains(pods, want) {
			t.Errorf("the redaction dropped %q, which is what the bundle is read for", want)
		}
	}
}
