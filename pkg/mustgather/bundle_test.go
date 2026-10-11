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
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// TestRefusesANonEmptyArtifactDir covers the bundle that gets sent to support
// holding two runs' files. A second gather into the same directory leaves the
// first run's artifacts beside the second's wherever a collector was skipped
// this time, and nothing in the bundle says which run a file came from: the
// manifest indexes both and its digests check out.
func TestRefusesANonEmptyArtifactDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "artifacts")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "nvidia-bug-report_old-node.log.gz")
	if err := os.WriteFile(stale, []byte("from a previous run"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := Run(&Options{ArtifactDir: dir}, stubFactory{
		clientset: fake.NewSimpleClientset(twoNodeCluster()...),
		dynamic:   emptyDynamicClient(),
		config:    offlineConfig(),
	}, discardProgress{})
	if err == nil {
		t.Fatal("Run returned nil over a directory that already holds a bundle")
	}
	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitUsage, err)
	}

	want := dir + " is not empty: refusing to mix this bundle with what is already there"
	if err.Error() != want {
		t.Errorf("error = %q, want exactly %q", err.Error(), want)
	}

	// The refusal has to leave what was there alone. Deleting someone else's
	// files to make room is a worse failure than the one being prevented.
	if _, serr := os.Stat(stale); serr != nil {
		t.Errorf("the refusal removed the file that was already there: %v", serr)
	}
}

// TestAnEmptyOrAbsentArtifactDirIsFine keeps the check from refusing the two
// cases a user actually hits: a fresh path, and a directory they made.
func TestAnEmptyOrAbsentArtifactDirIsFine(t *testing.T) {
	t.Run("a path that does not exist", func(t *testing.T) {
		if _, _, err := runGather(t, &Options{}, twoNodeCluster()...); err != nil {
			t.Fatalf("Run: %v", err)
		}
	})

	t.Run("a directory the user made", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "artifacts")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		err := Run(&Options{ArtifactDir: dir}, stubFactory{
			clientset: fake.NewSimpleClientset(twoNodeCluster()...),
			dynamic:   emptyDynamicClient(),
			config:    offlineConfig(),
		}, discardProgress{})
		if err != nil {
			t.Fatalf("Run over an empty directory: %v", err)
		}
	})
}

// TestDefaultArtifactDirIsUniquePerRun covers two gathers started in the same
// minute, which is what a support engineer does when the first one came out
// wrong. The name was minute-resolution, so the second run landed in the first
// run's directory.
func TestDefaultArtifactDirIsUniquePerRun(t *testing.T) {
	at := time.Date(2026, time.October, 11, 14, 59, 30, 0, time.UTC)

	first := DefaultArtifactDir(at)
	second := DefaultArtifactDir(at)

	if first == second {
		t.Errorf("two runs at the same instant both chose %s", first)
	}

	// The shape still has to be the one support reads by name, with the
	// seconds and the suffix that make it unique.
	pattern := regexp.MustCompile(`^/tmp/nvidia-gpu-operator_20261011_145930_[a-z0-9]{6}$`)
	for _, name := range []string{first, second} {
		if !pattern.MatchString(name) {
			t.Errorf("%s does not match %s", name, pattern)
		}
	}
}

// TestIndexSkipsNonRegularFiles covers a bundle directory holding something
// that is not a file. Hashing it either blocks forever on a fifo or fails the
// index, and either way the manifest is the one file a reader needs most.
func TestIndexSkipsNonRegularFiles(t *testing.T) {
	dir, _, err := runGather(t, &Options{}, twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The index is recomputed here over a directory that has since grown a
	// symlink, which is the cheapest non-regular file to make portably.
	link := filepath.Join(dir, "elsewhere.yaml")
	if lerr := os.Symlink(filepath.Join(dir, "nodes.yaml"), link); lerr != nil {
		t.Skipf("this filesystem does not do symlinks: %v", lerr)
	}

	g, gerr := NewGatherer(&Options{ArtifactDir: dir}, stubFactory{
		clientset: fake.NewSimpleClientset(), dynamic: emptyDynamicClient(), config: offlineConfig(),
	}, discardProgress{})
	if gerr != nil {
		t.Fatalf("NewGatherer: %v", gerr)
	}
	t.Cleanup(g.Close)

	files, ierr := g.indexBundle()
	if ierr != nil {
		t.Fatalf("indexBundle: %v", ierr)
	}
	for _, f := range files {
		if f.Path == "elsewhere.yaml" {
			t.Error("the index holds a symlink, which is not a file of the bundle")
		}
	}
}

// TestUnknownNodeIsAUsageError covers a typo in --node. The gather used to run
// to the end and produce a bundle with nothing in it about any node, which
// reads as a cluster with no GPUs rather than as a name that does not exist.
func TestUnknownNodeIsAUsageError(t *testing.T) {
	_, _, err := runGather(t, &Options{Node: "gpu-node-404"}, twoNodeCluster()...)
	if err == nil {
		t.Fatal("Run returned nil for a node the cluster does not have")
	}
	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitUsage, err)
	}
	if !strings.Contains(err.Error(), "gpu-node-404") {
		t.Errorf("error = %q, want it to name the node that was asked for", err.Error())
	}
}

// TestArtifactNamesAreRejectedWhenUnusable covers the names that reach the
// filesystem. Pod and node names are DNS-1123 on any cluster that accepted
// them, so one that is not has either been tampered with or come from a
// confused reader, and either way it must not be used to build a path.
func TestArtifactNamesAreRejectedWhenUnusable(t *testing.T) {
	cases := map[string]bool{
		"gpu-node-1":                    true,
		"nvidia-driver-daemonset-abcde": true,
		"node.with.dots":                true,
		"":                              false,
		"../../etc/passwd":              false,
		"node/with/slashes":             false,
		"Node-With-Capitals":            false,
		"-leading-dash":                 false,
		strings.Repeat("a", 254):        false,
	}

	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			if got := usableInAName(name); got != want {
				t.Errorf("usableInAName(%q) = %v, want %v", name, got, want)
			}
		})
	}
}

// TestBugReportSkipsAPodWithAnUnusableName keeps such a name out of the bundle
// rather than letting it build a path.
func TestBugReportSkipsAPodWithAnUnusableName(t *testing.T) {
	pod := driverPodWith("driver-a", "../../escape", map[string]string{"app": "nvidia-driver-daemonset"})

	g, dir := newTestGatherer(t, pod)
	err := g.gatherBugReportFromDaemonSet(&gatherState{operatorNamespace: "gpu-operator", prepared: true})

	var skipped errSkipped
	if !errors.As(err, &skipped) {
		t.Fatalf("the collector returned %v, want the pod passed over", err)
	}
	for _, name := range artifactNames(t, dir) {
		if strings.Contains(name, "escape") {
			t.Errorf("the bundle holds %s, built from a node name that is not a usable name", name)
		}
	}
}

// TestDeviceUsageSkipWhenThereIsNoNvidiaSMI covers the container that cannot
// answer nvidia-smi: the stand-in in the end to end lane, and any cluster whose
// driver is preinstalled on the host. Every command after the probe would write
// a file of error messages, so the collector records why instead.
func TestDeviceUsageSkipWhenThereIsNoNvidiaSMI(t *testing.T) {
	pod := driverPodWith("driver-a", "node-a", map[string]string{"app": "nvidia-driver-daemonset"})

	dir := filepath.Join(t.TempDir(), "artifacts")
	g, err := NewGatherer(&Options{
		ArtifactDir: dir,
		RunInPod: func(_ context.Context, _ *v1.Pod, _, command string) (string, error) {
			if strings.Contains(command, "nvidia-smi") {
				return "", errors.New("sh: nvidia-smi: not found")
			}
			return "", nil
		},
	}, stubFactory{clientset: fake.NewSimpleClientset(pod), config: offlineConfig()}, discardProgress{})
	if err != nil {
		t.Fatalf("NewGatherer: %v", err)
	}
	t.Cleanup(g.Close)

	var skipped errSkipped
	cerr := g.gatherDeviceUsage(&gatherState{operatorNamespace: "gpu-operator", prepared: true})
	if !errors.As(cerr, &skipped) {
		t.Fatalf("the device usage collector returned %v, want it recorded as skipped", cerr)
	}
	if skipped.reason != "no nvidia-smi in driver-a" {
		t.Errorf("skip reason = %q, want %q", skipped.reason, "no nvidia-smi in driver-a")
	}
	for _, name := range artifactNames(t, dir) {
		if strings.HasPrefix(name, "gpu_device_usage_") {
			t.Errorf("the bundle holds %s for a container with no nvidia-smi", name)
		}
	}
}

// TestPodImagesCarryInitContainers covers the file that says which release is
// running. The driver validation runs as an init container, and a list that
// skipped init containers left out the image support asks about first on a
// cluster whose validation is failing.
func TestPodImagesCarryInitContainers(t *testing.T) {
	pod := v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "nvidia-operator-validator-aaaaa", Namespace: "gpu-operator"},
		Spec: v1.PodSpec{
			InitContainers: []v1.Container{{Name: "driver-validation", Image: "nvcr.io/nvidia/validator:v1"}},
			Containers:     []v1.Container{{Name: "nvidia-operator-validator", Image: "nvcr.io/nvidia/validator:v2"}},
		},
	}

	var out strings.Builder
	if err := writePodImages(&out, []v1.Pod{pod}); err != nil {
		t.Fatalf("writePodImages: %v", err)
	}

	got := out.String()
	for _, want := range []string{"nvcr.io/nvidia/validator:v1", "nvcr.io/nvidia/validator:v2"} {
		if !strings.Contains(got, want) {
			t.Errorf("the image list is missing %q:\n%s", want, got)
		}
	}
	if !strings.HasPrefix(got, "nvidia-operator-validator-aaaaa:\t") {
		t.Errorf("the line does not start with the pod name and a tab:\n%q", got)
	}
}
