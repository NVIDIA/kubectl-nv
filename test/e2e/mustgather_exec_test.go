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
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// standInManifest is the driver stand-in this file applies to the lane.
const standInManifest = "testdata/driver-standin.yaml"

// standInDaemonSet is the name of the stand-in daemonset in the lane.
const standInDaemonSet = "nvidia-driver-standin"

// standInReadyTimeout bounds the wait for the stand-in to roll out. The image
// is a cold pull on a fresh lane, which is the slow part.
const standInReadyTimeout = 5 * time.Minute

// standInPoll is how often the rollout is asked about again.
const standInPoll = 5 * time.Second

// manifest is the bundle index as a reader parses it. Only the fields this
// file asserts on are named: the rest of the contract has its own unit tests,
// and repeating it here would make this file fail for reasons that have
// nothing to do with a live cluster.
type manifest struct {
	Collectors []struct {
		Name      string   `json:"name"`
		Status    string   `json:"status"`
		Artifacts []string `json:"artifacts"`
	} `json:"collectors"`
	Files []struct {
		Path   string `json:"path"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

// TestMustGatherExecAgainstTheLane covers the one path of must-gather that no
// unit test can reach: reaching into a running pod through the API server,
// running a command and copying a file back out.
//
// The lane installs the GPU Operator with driver.enabled=false, so it has no
// driver pod to exec into. This test puts one there: a stand-in daemonset
// carrying the driver labels and shipping a shell nvidia-bug-report.sh. The
// fixture is applied to the lane's own cluster through the lane's kubeconfig,
// and taken away again whichever way the test ends.
func TestMustGatherExecAgainstTheLane(t *testing.T) {
	applyStandIn(t)

	workers := workerNodeNames(t)
	if len(workers) == 0 {
		t.Fatal("the lane has no worker nodes")
	}
	waitForStandIn(t, len(workers))
	waitForStartedPods(t)

	t.Run("one bug report per worker, naming its own node", func(t *testing.T) {
		dir := gatherInto(t, 0)

		for _, worker := range workers {
			name := "nvidia-bug-report_" + worker + ".log.gz"
			// The report is a gzip of a line naming the node it came from. A
			// collector that copied one node's report into every file writes
			// the right number of files with the wrong contents, and only the
			// contents can tell the two apart.
			got := readGzip(t, filepath.Join(dir, name))
			if !strings.Contains(got, worker) {
				t.Errorf("%s holds %q, want it to name %s", name, got, worker)
			}
		}

		m := readBundleManifest(t, dir)

		// The stand-in ships no nvidia-smi, so the device usage section cannot
		// run. It has to say so rather than leaving the file out, because a
		// missing file reads the same as a collector that never ran.
		status := collectorStatus(t, m, "device-usage")
		if !strings.HasPrefix(status, "skipped:no nvidia-smi") {
			t.Errorf("device-usage status = %q, want it skipped for the missing nvidia-smi", status)
		}
		if ok := collectorStatus(t, m, "bug-report"); ok != "ok" {
			t.Errorf("bug-report status = %q, want ok", ok)
		}

		// The manifest is what a reader checks a bundle against after it has
		// travelled to support, so its digests are recomputed here from the
		// files that actually arrived.
		checkDigests(t, dir, m)
	})

	t.Run("--no-exec collects no report and succeeds", func(t *testing.T) {
		dir := gatherInto(t, 0, "--no-exec")

		for _, name := range bundleNames(t, dir) {
			if strings.HasPrefix(name, "nvidia-bug-report_") || strings.HasPrefix(name, "gpu_device_usage_") {
				t.Errorf("bundle holds %s, but --no-exec was given", name)
			}
		}

		m := readBundleManifest(t, dir)
		for _, collector := range []string{"bug-report", "device-usage"} {
			if got := collectorStatus(t, m, collector); got != "skipped:--no-exec" {
				t.Errorf("%s status = %q, want skipped:--no-exec", collector, got)
			}
		}
	})

	t.Run("--node collects only that worker's report", func(t *testing.T) {
		worker := workers[0]
		dir := gatherInto(t, 0, "--node", worker)

		var reports []string
		for _, name := range bundleNames(t, dir) {
			if strings.HasPrefix(name, "nvidia-bug-report_") {
				reports = append(reports, name)
			}
		}

		want := "nvidia-bug-report_" + worker + ".log.gz"
		if len(reports) != 1 || reports[0] != want {
			t.Fatalf("bundle holds reports %v, want exactly [%s]", reports, want)
		}
		if got := readGzip(t, filepath.Join(dir, want)); !strings.Contains(got, worker) {
			t.Errorf("%s holds %q, want it to name %s", want, got, worker)
		}
	})
}

// gatherInto runs a must-gather into a fresh directory and returns it, failing
// the test when the exit code is not the one expected.
func gatherInto(t *testing.T, wantCode int, args ...string) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "artifacts")
	full := append([]string{"adm", "must-gather", "--artifacts-dir", dir}, args...)
	res := run(t, full...)
	if res.code != wantCode {
		t.Fatalf("must-gather %v exited %d, want %d\nstderr:\n%s", args, res.code, wantCode, res.stderr)
	}
	return dir
}

// bundleNames lists what a bundle holds.
func bundleNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the bundle: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// readBundleManifest parses the bundle index.
func readBundleManifest(t *testing.T, dir string) *manifest {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		t.Fatalf("reading manifest.json: %v\nbundle holds: %v", err, bundleNames(t, dir))
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("manifest.json does not parse: %v\n%s", err, data)
	}
	return &m
}

// collectorStatus returns what the manifest recorded for one collector.
func collectorStatus(t *testing.T, m *manifest, name string) string {
	t.Helper()

	for _, c := range m.Collectors {
		if c.Name == name {
			return c.Status
		}
	}
	t.Fatalf("the manifest has no entry for collector %q", name)
	return ""
}

// checkDigests recomputes the manifest's sizes and digests from the files on
// disk. It is the check a support engineer makes on a bundle that has been
// through an archive and an upload, done here against the bundle as written.
func checkDigests(t *testing.T, dir string, m *manifest) {
	t.Helper()

	if len(m.Files) == 0 {
		t.Fatal("the manifest indexes no files")
	}
	for _, entry := range m.Files {
		raw, err := os.ReadFile(filepath.Join(dir, entry.Path))
		if err != nil {
			t.Errorf("the manifest indexes %s, which the bundle does not hold: %v", entry.Path, err)
			continue
		}
		if entry.Size != int64(len(raw)) {
			t.Errorf("%s: manifest size = %d, the file is %d bytes", entry.Path, entry.Size, len(raw))
		}
		if got := sha256Hex(raw); got != entry.SHA256 {
			t.Errorf("%s: manifest sha256 = %q, the file hashes to %q", entry.Path, entry.SHA256, got)
		}
	}
}

// readGzip returns the decompressed contents of a gzip artifact.
func readGzip(t *testing.T, path string) string {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", filepath.Base(path), err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("%s is not gzip: %v", filepath.Base(path), err)
	}
	defer reader.Close()

	text, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompressing %s: %v", filepath.Base(path), err)
	}
	return string(text)
}

// applyStandIn puts the driver stand-in on the lane and takes it away again
// when the test ends. This is the one thing the suite does to its cluster: the
// lane is its own, created and destroyed by the make target around it, and the
// exec path cannot be reached on a cluster with no driver pod at all.
func applyStandIn(t *testing.T) {
	t.Helper()

	kubectl(t, "apply", "-f", standInManifest)
	t.Cleanup(func() {
		// --ignore-not-found so that a cleanup after a failed apply is not a
		// second failure on top of the first.
		kubectl(t, "delete", "-f", standInManifest, "--ignore-not-found", "--wait=true")
	})
}

// kubectl runs one kubectl command against the lane's kubeconfig.
func kubectl(t *testing.T, args ...string) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	full := append([]string{"--kubeconfig", kubeconfigPath}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("kubectl %s: %v\n%s", strings.Join(args, " "), err, out.String())
	}
	t.Logf("kubectl %s\n%s", strings.Join(args, " "), out.String())
}

// waitForStandIn blocks until the stand-in has a ready pod on every worker.
// Running the gather before it has rolled out collects a bug report from
// fewer nodes than the lane has, which would read as a collector that misses
// nodes rather than as a test that did not wait.
func waitForStandIn(t *testing.T, workers int) {
	t.Helper()

	deadline := time.Now().Add(standInReadyTimeout)
	var last string
	for {
		set, err := clusterClient(t).AppsV1().DaemonSets(operatorNamespace).
			Get(context.Background(), standInDaemonSet, metav1.GetOptions{})
		if err != nil {
			last = fmt.Sprintf("cannot read the daemonset: %v", err)
		} else {
			last = fmt.Sprintf("desired %d, ready %d (want %d workers)",
				set.Status.DesiredNumberScheduled, set.Status.NumberReady, workers)
			if int(set.Status.NumberReady) >= workers &&
				set.Status.NumberReady == set.Status.DesiredNumberScheduled {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the driver stand-in did not roll out within %s: %s", standInReadyTimeout, last)
		}
		time.Sleep(standInPoll)
	}
}

// sha256Hex is the digest form the manifest records.
func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
