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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// noDriverPodsLine is what a gather says about a cluster whose GPU Operator
// runs without a driver daemonset, which is what the Mokka lane is: the
// simulated GPUs need no driver, so there is no pod to ask for a bug report.
//
// It names every label the collector looked under, so a reader can tell "this
// cluster installs its driver some other way" from "the collector only knows
// one of the four ways". Pinned exactly: a line that quietly stopped naming a
// label would be the first sign of the collector having stopped looking under
// it, and nothing else in the bundle would show that.
const noDriverPodsLine = "no driver pods found " +
	"(labels app=nvidia-driver-daemonset, app.kubernetes.io/component=nvidia-driver, " +
	"app=nvidia-vgpu-manager-daemonset, openshift.driver-toolkit); " +
	"skipping nvidia-bug-report"

// bundle is one must-gather run and the directory it wrote.
type bundle struct {
	dir   string
	run   result
	files map[string]os.FileInfo

	// podsBefore and podsAfter are the operator namespace as the cluster
	// reported it either side of the gather. A pod that came or went while the
	// gather was running is the cluster's doing, not the gatherer's, and the
	// two lists are what tells those apart.
	podsBefore []v1.Pod
	podsAfter  []v1.Pod
}

// gather runs a must-gather into a fresh directory under the test's temporary
// one. The directory is named rather than created, so that the gather creates
// it and the mode it chooses is what gets asserted.
func gather(t *testing.T) *bundle {
	t.Helper()

	// A container whose logs the API server will not serve yet fails the
	// gather, and that is the lane still coming up rather than anything about
	// kubectl-nv.
	waitForStartedPods(t)

	podsBefore := operatorPods(t)

	dir := filepath.Join(t.TempDir(), "artifacts")
	res := run(t, "adm", "must-gather", "--artifacts-dir", dir)
	if res.code != 0 {
		t.Fatalf("must-gather exited %d, want 0\nstderr:\n%s", res.code, res.stderr)
	}

	podsAfter := operatorPods(t)

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

	return &bundle{dir: dir, run: res, files: files, podsBefore: podsBefore, podsAfter: podsAfter}
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

	t.Run("the manifest satisfies its published schema", func(t *testing.T) {
		// manifest.json is what travels to support, and the published schema
		// is what whoever reads it there validates against. The struct the
		// subtests below decode into names the fields they care about and
		// ignores the rest, so a field added to the manifest without a schema
		// to describe it would pass every one of them.
		assertMatchesSchema(t, output.KindBundleManifest, string(b.read(t, "manifest.json")))
	})

	t.Run("the manifest and the bundle agree", func(t *testing.T) {
		// Driven by manifest.json rather than by a list written out here. A
		// hard-coded list of file names has to be edited every time a collector
		// is added, which is how it stops catching anything; the manifest is
		// the gatherer's own claim about what it wrote, and this checks that
		// claim against the directory on disk.
		m := readBundleManifest(t, b.dir)

		if len(m.Collectors) == 0 {
			t.Fatal("manifest.json records no collectors at all")
		}
		for _, c := range m.Collectors {
			if c.Status == "ok" && len(c.Artifacts) == 0 {
				t.Errorf("collector %q succeeded but claims no artifact", c.Name)
			}
			for _, artifact := range c.Artifacts {
				if b.files[artifact] == nil {
					t.Errorf("collector %q claims %s, which the bundle does not hold", c.Name, artifact)
				}
			}
		}

		// The bundle is exactly what the manifest indexes, plus the manifest.
		// A file nobody indexed is a file a reader cannot check; an indexed
		// file that is not there is a collector that reported what it meant to
		// write rather than what it wrote.
		indexed := map[string]bool{"manifest.json": true}
		for _, f := range m.Files {
			indexed[f.Path] = true
			if b.files[f.Path] == nil {
				t.Errorf("manifest.json indexes %s, which the bundle does not hold", f.Path)
			}
		}
		for name := range b.files {
			if !indexed[name] {
				t.Errorf("bundle holds %s, which manifest.json does not index", name)
			}
		}

		// Recomputed here from the files as they landed. This is the check a
		// support engineer makes on a bundle that has been through an archive
		// and an upload, and a gatherer that hashed its intent rather than its
		// output would pass every other assertion in this file.
		for _, f := range m.Files {
			raw, err := os.ReadFile(filepath.Join(b.dir, f.Path))
			if err != nil {
				continue // already reported above
			}
			if f.Size != int64(len(raw)) {
				t.Errorf("%s: manifest size = %d, the file is %d bytes", f.Path, f.Size, len(raw))
			}
			if got := sha256Hex(raw); got != f.SHA256 {
				t.Errorf("%s: manifest sha256 = %q, the file hashes to %q", f.Path, f.SHA256, got)
			}
		}
	})

	t.Run("the core artifacts are always there", func(t *testing.T) {
		// The floor under the manifest check above. A gatherer that dropped
		// every collector but one would still be self-consistent: its manifest
		// would index the one file it wrote and the directory would match. The
		// names below are the ones a bundle is useless without, so they are
		// listed by hand and a collector that stops writing one has to be
		// deleted from this list deliberately.
		for _, name := range []string{
			"nodes.yaml",
			"gpu_operand_pods.yaml",
			"cluster_policy.yaml",
			"daemonsets.yaml",
			"resourceslices.yaml",
			"must-gather.log",
			"must-gather.stderr.log",
			"manifest.json",
		} {
			if b.files[name] == nil {
				t.Errorf("bundle is missing %s\nbundle holds: %v", name, b.names())
			}
		}

		// The driver custom resource is either there or recorded as absent,
		// and exactly one of the two files says so. Both, or neither, means
		// the collector cannot make its mind up about the cluster.
		driverYAML := b.files["nvidia_driver.yaml"] != nil
		driverMissing := b.files["nvidia_driver.missing"] != nil
		if driverYAML == driverMissing {
			t.Errorf("bundle holds nvidia_driver.yaml=%v and nvidia_driver.missing=%v, want exactly one",
				driverYAML, driverMissing)
		}
	})

	t.Run("the operand pods are the cluster's pods", func(t *testing.T) {
		// Derived from the file, checked against the cluster: a gatherer that
		// listed one page of pods and stopped writes a bundle that is
		// self-consistent and wrong, and only the cluster can say so.
		got := podNames(podList(t, b, "gpu_operand_pods.yaml"))
		before := podNames(b.podsBefore)
		after := podNames(b.podsAfter)

		if strings.Join(before, ",") == strings.Join(after, ",") {
			if strings.Join(got, ",") != strings.Join(before, ",") {
				t.Errorf("gpu_operand_pods.yaml lists\n  %v\nthe cluster has\n  %v", got, before)
			}
			return
		}

		// The namespace changed under the gather. What was there throughout
		// still has to be in the bundle, and nothing may be in it that was
		// never in the namespace at all.
		t.Logf("the namespace changed during the gather: before %v, after %v", before, after)
		for _, name := range intersection(before, after) {
			if !contains(got, name) {
				t.Errorf("gpu_operand_pods.yaml is missing %s, which was there throughout", name)
			}
		}
		for _, name := range got {
			if !contains(before, name) && !contains(after, name) {
				t.Errorf("gpu_operand_pods.yaml lists %s, which the cluster never had", name)
			}
		}
	})

	t.Run("the operands cover every worker", func(t *testing.T) {
		// The two daemonsets that make a node a GPU node: one puts
		// nvidia.com/gpu into allocatable, the other puts the nvidia.com
		// labels on. A bundle collected from a lane where either is missing
		// from a worker is a bundle of a lane that is not what it claims.
		const (
			devicePlugin = "nvidia-device-plugin-daemonset"
			featureDisc  = "gpu-feature-discovery"
		)

		onNode := daemonSetPodNodes(b.podsBefore)
		for _, name := range []string{devicePlugin, featureDisc} {
			for _, worker := range workerNodeNames(t) {
				if !onNode[name][worker] {
					t.Errorf("no %s pod on worker %s", name, worker)
				}
			}
		}

		var sets appsv1.DaemonSetList
		data := b.read(t, "daemonsets.yaml")
		if err := yaml.Unmarshal(data, &sets); err != nil {
			t.Fatalf("daemonsets.yaml does not parse as a DaemonSetList: %v", err)
		}
		named := make(map[string]bool, len(sets.Items))
		for _, set := range sets.Items {
			named[set.Name] = true
		}
		for _, name := range []string{devicePlugin, featureDisc} {
			if !named[name] {
				t.Errorf("daemonsets.yaml does not list %s, it lists %v", name, sortedKeys(named))
			}
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
		//
		// Every container gets its current log and the previous one beside it.
		// A container that has never restarted has no previous log to serve,
		// and the file is still written carrying that: a reader has to be able
		// to tell a container that never crashed from one nobody looked at.
		want := map[string]bool{
			"must-gather.log":        true,
			"must-gather.stderr.log": true,
		}

		operators := podList(t, b, "gpu_operator_pod.yaml")
		if len(operators) == 0 {
			t.Fatal("gpu_operator_pod.yaml lists no pod")
		}
		operator := operators[0]
		for _, container := range containerNames(operator) {
			want["gpu_operator_logs_"+container+".log"] = true
			want["gpu_operator_logs_"+container+".previous.log"] = true
		}

		for _, pod := range podList(t, b, "gpu_operand_pods.yaml") {
			// The operator pod is in the namespace listing too, and its logs
			// are already collected under gpu_operator_logs_. Collecting them
			// a second time under a second name would double the largest file
			// in the bundle, and the gpu-operator script skips it here for the
			// same reason.
			if pod.Name == operator.Name && pod.Namespace == operator.Namespace {
				continue
			}
			for _, container := range containerNames(pod) {
				want[pod.Name+"_"+container+".log"] = true
				want[pod.Name+"_"+container+".previous.log"] = true
			}
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
		// Still exhaustive in the other direction. A log named after the wrong
		// pod, or after a container the pod does not run, lands here rather
		// than being absorbed by a suffix rule.
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

// contains reports whether names holds name.
func contains(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}

// intersection returns the names in both lists, sorted.
func intersection(a, b []string) []string {
	both := make([]string, 0, len(a))
	for _, name := range a {
		if contains(b, name) {
			both = append(both, name)
		}
	}
	sort.Strings(both)
	return both
}

// sortedKeys lists the keys of set, sorted, for a failure message.
func sortedKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
