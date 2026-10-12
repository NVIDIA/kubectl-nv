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

package mustgather_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/engine"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// roundTripBundle writes the smallest bundle the readers accept: the node list
// and the operand pods, which is what list --from-bundle and health
// --from-bundle are built on.
func roundTripBundle(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	quantity := *resource.NewQuantity(4, resource.DecimalSI)
	nodes := &corev1.NodeList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "NodeList"},
		Items: []corev1.Node{{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Node"},
			ObjectMeta: metav1.ObjectMeta{
				Name: "gpu-worker-01.prod.example.com",
				Labels: map[string]string{
					"kubernetes.io/hostname":              "gpu-worker-01",
					"nvidia.com/gpu.present":              "true",
					"nvidia.com/gpu.count":                "4",
					"nvidia.com/gpu.product":              "NVIDIA-H100-80GB-HBM3",
					"nvidia.com/gpu.deploy.device-plugin": "true",
				},
			},
			Status: corev1.NodeStatus{
				Capacity:    corev1.ResourceList{"nvidia.com/gpu": quantity},
				Allocatable: corev1.ResourceList{"nvidia.com/gpu": quantity},
				Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Addresses:   []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "10.42.7.11"}},
			},
		}},
	}
	pods := &corev1.PodList{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
		Items: []corev1.Pod{{
			TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
			ObjectMeta: metav1.ObjectMeta{
				Name:      "nvidia-device-plugin-daemonset-xyz",
				Namespace: "gpu-operator",
				Labels:    map[string]string{"app": "nvidia-device-plugin-daemonset"},
			},
			Spec: corev1.PodSpec{
				NodeName:   "gpu-worker-01.prod.example.com",
				Containers: []corev1.Container{{Name: "plugin", Image: "nvcr.io/nvidia/k8s-device-plugin:v0.17.0"}},
			},
			Status: corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "plugin", Ready: true}},
			},
		}},
	}

	for name, object := range map[string]any{"nodes.yaml": nodes, "gpu_operand_pods.yaml": pods} {
		raw, err := yaml.Marshal(object)
		if err != nil {
			t.Fatalf("marshalling %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	return dir
}

// answers is what list --from-bundle and health --from-bundle say about a
// bundle, with the node names left out: the names are what redaction changes,
// and everything else is what it must not.
type answers struct {
	nodes    int
	gpus     []string
	findings []string
}

// readBundle is the read path both commands take: source.NewBundle, then the
// inventory, then the rules over it.
func readBundle(t *testing.T, dir string) (answers, []string) {
	t.Helper()

	src, err := source.NewBundle(dir)
	if err != nil {
		t.Fatalf("reading the bundle at %s: %v", dir, err)
	}

	list, _, buildErr := inventory.BuildWithHinted(context.Background(), src,
		inventory.BuildOptions{IncludeHinted: true, WithDetails: true})
	if list == nil {
		t.Fatalf("the bundle produced no inventory: %v", buildErr)
	}

	got := answers{nodes: len(list.Items)}
	var names []string
	in := health.Input{Now: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)}
	for i := range list.Items {
		item := list.Items[i]
		names = append(names, item.Name)
		got.gpus = append(got.gpus, strings.Join([]string{
			item.Product,
			item.Mode,
			itoa(item.Capacity), itoa(item.Allocatable), itoa(item.Requested),
		}, "/"))
		in.Nodes = append(in.Nodes, item)
	}

	report, err := engine.Run(rules.All(), in, engine.Options{})
	if err != nil {
		t.Fatalf("running the rules over the bundle: %v", err)
	}
	for _, f := range report.Findings {
		got.findings = append(got.findings, f.Code+":"+string(f.Severity))
	}
	sort.Strings(got.gpus)
	sort.Strings(got.findings)
	sort.Strings(names)

	return got, names
}

// itoa renders a count for the comparison key.
func itoa(n int64) string {
	return strings.TrimSpace(resource.NewQuantity(n, resource.DecimalSI).String())
}

// A redacted bundle is still a bundle. It loads through source.NewBundle and
// answers the same questions, because the only thing redaction may change is
// which name a node goes by.
func TestARedactedBundleReadsTheSame(t *testing.T) {
	t.Parallel()

	plain := roundTripBundle(t)
	before, beforeNames := readBundle(t, plain)

	redacted := roundTripBundle(t)
	r, err := mustgather.NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(redacted); err != nil {
		t.Fatalf("redacting the bundle: %v", err)
	}
	after, afterNames := readBundle(t, redacted)

	if before.nodes != after.nodes {
		t.Errorf("the plain bundle holds %d nodes, the redacted one %d", before.nodes, after.nodes)
	}
	if !equal(before.gpus, after.gpus) {
		t.Errorf("the GPU answers changed:\n  plain:    %v\n  redacted: %v", before.gpus, after.gpus)
	}
	if !equal(before.findings, after.findings) {
		t.Errorf("the findings changed:\n  plain:    %v\n  redacted: %v", before.findings, after.findings)
	}

	// Modulo the renamed nodes: the names are exactly what did change, and a
	// redaction that left them alone would pass every assertion above.
	if equal(beforeNames, afterNames) {
		t.Errorf("the node names are unchanged: %v", afterNames)
	}
	for _, name := range afterNames {
		if strings.Contains(name, "example.com") || strings.Contains(name, "gpu-worker") {
			t.Errorf("the redacted bundle still names %q", name)
		}
	}
	if len(output.RegisteredKinds()) == 0 {
		t.Fatal("no kind is registered: this test links the contract and would not notice")
	}
}

// equal compares two sorted string slices.
func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
