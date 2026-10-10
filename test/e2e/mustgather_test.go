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
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/yaml"
)

// noDriverPodsLine is what a gather says about a cluster whose GPU Operator
// runs without a driver daemonset, which is what the Mokka lane is: the
// simulated GPUs need no driver, so there is no pod to ask for a bug report.
const noDriverPodsLine = "no driver pods found " +
	"(labels app=nvidia-driver-daemonset, app=nvidia-vgpu-manager-daemonset); " +
	"skipping nvidia-bug-report"

// bundle is one must-gather run and the directory it wrote.
type bundle struct {
	dir   string
	run   result
	files map[string]os.FileInfo
}

// gather runs a must-gather into a fresh directory under the test's temporary
// one. The directory is named rather than created, so that the gather creates
// it and the mode it chooses is what gets asserted.
func gather(t *testing.T) *bundle {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "artifacts")
	res := run(t, "adm", "must-gather", "--artifacts-dir", dir)
	if res.code != 0 {
		t.Fatalf("must-gather exited %d, want 0\nstderr:\n%s", res.code, res.stderr)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the bundle: %v", err)
	}
	files := make(map[string]os.FileInfo, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			t.Fatalf("stat %s: %v", e.Name(), err)
		}
		files[e.Name()] = info
	}

	return &bundle{dir: dir, run: res, files: files}
}

// read returns the named artifact, failing the test when it is missing: every
// caller here is asserting about a file the bundle is supposed to hold.
func (b *bundle) read(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(b.dir, name))
	if err != nil {
		t.Fatalf("reading %s from the bundle: %v\nbundle holds: %v", name, err, b.names())
	}
	return data
}

// names lists what the bundle holds, sorted, for a failure message.
func (b *bundle) names() []string {
	names := make([]string, 0, len(b.files))
	for name := range b.files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestMustGatherAgainstTheLane is the whole command as a support engineer runs
// it: one invocation against a live cluster, and every assertion below made
// against the single bundle it produced. Running it once keeps the test honest
// about what one gather collects, and keeps a suite that takes a cluster to run
// down to one gather's worth of time.
func TestMustGatherAgainstTheLane(t *testing.T) {
	b := gather(t)

	t.Run("stdout stays empty", func(t *testing.T) {
		// The bundle goes to disk and the commentary goes to the error stream.
		// Anything on stdout breaks the first pipeline someone writes around
		// the plugin, and is the kind of thing only a real run catches.
		if b.run.stdout != "" {
			t.Errorf("stdout = %q, want empty", b.run.stdout)
		}
	})

	t.Run("the cluster policy is readable", func(t *testing.T) {
		var policy struct {
			Kind   string `json:"kind"`
			Status struct {
				State string `json:"state"`
			} `json:"status"`
		}
		data := b.read(t, "cluster_policy.yaml")
		if err := yaml.Unmarshal(data, &policy); err != nil {
			t.Fatalf("cluster_policy.yaml does not parse as YAML: %v\n%s", err, data)
		}
		if policy.Kind != "ClusterPolicy" {
			t.Errorf("cluster_policy.yaml kind = %q, want ClusterPolicy", policy.Kind)
		}
		// The state is what support reads first; a ClusterPolicy captured
		// without its status says nothing about whether the operator settled.
		if policy.Status.State == "" {
			t.Errorf("cluster_policy.yaml carries no status.state:\n%s", data)
		}
	})

	t.Run("every node of the cluster is in nodes.yaml", func(t *testing.T) {
		var list v1.NodeList
		data := b.read(t, "nodes.yaml")
		if err := yaml.Unmarshal(data, &list); err != nil {
			t.Fatalf("nodes.yaml does not parse as a NodeList: %v", err)
		}
		got := make([]string, 0, len(list.Items))
		for _, node := range list.Items {
			got = append(got, node.Name)
		}
		sort.Strings(got)

		want := clusterNodeNames(t)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("nodes.yaml lists %v, the cluster has %v", got, want)
		}
	})

	t.Run("the resource slices are recorded", func(t *testing.T) {
		var slices resourcev1.ResourceSliceList
		data := b.read(t, "resourceslices.yaml")
		if err := yaml.Unmarshal(data, &slices); err != nil {
			t.Fatalf("resourceslices.yaml does not parse as a ResourceSliceList: %v\n%s", err, data)
		}
		if slices.Kind != "ResourceSliceList" {
			t.Errorf("resourceslices.yaml kind = %q, want ResourceSliceList", slices.Kind)
		}
		// A cluster without the NVIDIA DRA driver has no slices, and the empty
		// list is the answer. It has to marshal as a list all the same, so that
		// a reader can range over it without a null check.
		if slices.Items == nil {
			t.Errorf("resourceslices.yaml has a null items, want a list:\n%s", data)
		}
	})

	t.Run("one log file per container", func(t *testing.T) {
		// The expectation is derived from the bundle's own pod lists rather
		// than written out here: the operands of a GPU Operator release are
		// its business, and a hard-coded list would have to be edited on every
		// bump, which is how it stops catching anything.
		want := map[string]bool{
			"must-gather.log":        true,
			"must-gather.stderr.log": true,
		}
		for _, pod := range podList(t, b, "gpu_operand_pods.yaml") {
			for _, container := range containerNames(pod) {
				want[pod.Name+"_"+container+".log"] = true
			}
		}
		operators := podList(t, b, "gpu_operator_pod.yaml")
		if len(operators) == 0 {
			t.Fatal("gpu_operator_pod.yaml lists no pod")
		}
		for _, container := range containerNames(operators[0]) {
			want["gpu_operator_logs_"+container+".log"] = true
			want["gpu_operator_logs_"+container+".previous.log"] = true
		}

		got := map[string]bool{}
		for name := range b.files {
			if strings.HasSuffix(name, ".log") {
				got[name] = true
			}
		}

		for name := range want {
			if !got[name] {
				t.Errorf("bundle is missing %s", name)
			}
		}
		for name := range got {
			if !want[name] {
				t.Errorf("bundle holds an unexpected log file %s", name)
			}
		}
	})

	t.Run("a driverless cluster says so and ships no bug report", func(t *testing.T) {
		for name := range b.files {
			if strings.HasPrefix(name, "nvidia-bug-report_") && strings.HasSuffix(name, ".log.gz") {
				t.Errorf("bundle holds %s, but this cluster runs no driver pods", name)
			}
		}
		if !strings.Contains(b.run.stderr, noDriverPodsLine) {
			t.Errorf("stderr does not say why no bug report was collected, want %q:\n%s",
				noDriverPodsLine, b.run.stderr)
		}
	})

	t.Run("the bundle is not readable by other accounts", func(t *testing.T) {
		// The bundle holds cluster configuration and pod logs, and its default
		// home is /tmp, where every account on the machine can look.
		info, err := os.Stat(b.dir)
		if err != nil {
			t.Fatalf("stat %s: %v", b.dir, err)
		}
		if got := info.Mode().Perm(); got != 0o700 {
			t.Errorf("artifact directory mode = %04o, want 0700", got)
		}
		for name, fi := range b.files {
			if !fi.Mode().IsRegular() {
				t.Errorf("%s is not a regular file (mode %v)", name, fi.Mode())
				continue
			}
			if got := fi.Mode().Perm(); got != 0o600 {
				t.Errorf("%s mode = %04o, want 0600", name, got)
			}
		}
	})
}

// podList parses one of the bundle's pod lists.
func podList(t *testing.T, b *bundle, name string) []v1.Pod {
	t.Helper()
	var list v1.PodList
	data := b.read(t, name)
	if err := yaml.Unmarshal(data, &list); err != nil {
		t.Fatalf("%s does not parse as a PodList: %v", name, err)
	}
	return list.Items
}

// containerNames lists the containers whose logs a gather collects: the init
// containers and then the regular ones, matching what the gatherer walks.
func containerNames(pod v1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}

// clusterNodeNames asks the cluster itself what it is made of, so that
// nodes.yaml is checked against the cluster rather than against itself. The
// call is a read, as every call this suite makes is.
func clusterNodeNames(t *testing.T) []string {
	t.Helper()

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		t.Fatalf("building a client configuration from %s: %v", kubeconfigPath, err)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatalf("building a kubernetes client: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	nodes, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing the cluster's nodes: %v", err)
	}
	names := make([]string, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		names = append(names, node.Name)
	}
	sort.Strings(names)
	return names
}
