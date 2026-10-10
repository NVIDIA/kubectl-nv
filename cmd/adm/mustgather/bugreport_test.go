/**
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package mustgather

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// driverPod builds a running driver daemonset pod scheduled on the named node.
func driverPod(t *testing.T, namespace, name, nodeName string) *v1.Pod {
	t.Helper()
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": "nvidia-driver-daemonset"},
		},
		Spec:   v1.PodSpec{NodeName: nodeName},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}
}

// artifactNames lists the files the gatherer left in the artifact directory.
func artifactNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading artifact dir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// recordingFetcher writes each source pod's name into the destination file, so a
// report can be traced back to the pod it came from. Pods named in failFor fail
// instead, as a pod whose driver container is wedged would.
func recordingFetcher(failFor map[string]bool) podFileFetcher {
	return func(pod *v1.Pod, destPath string) error {
		if failFor[pod.Name] {
			return errors.New("command terminated with exit code 1")
		}
		return os.WriteFile(destPath, []byte(pod.Name), 0o600)
	}
}

// TestGatherBugReportCollectsEveryDriverPod drives the whole collector, from the
// labelled pod query down to the copy, against a cluster with three driver
// nodes. Stopping after the first pod of a daemonset, as the collector used to,
// ships support a one-node report for a three-node cluster.
func TestGatherBugReportCollectsEveryDriverPod(t *testing.T) {
	const namespace = "gpu-operator"

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	clientset := fake.NewClientset(
		driverPod(t, namespace, "nvidia-driver-daemonset-aaaaa", "worker-a"),
		driverPod(t, namespace, "nvidia-driver-daemonset-bbbbb", "worker-b"),
		driverPod(t, namespace, "nvidia-driver-daemonset-ccccc", "worker-c"),
	)

	g := &Gatherer{
		opts:         &Options{ArtifactDir: artifactDir},
		logFile:      logFile,
		errLogFile:   errLogFile,
		clientset:    clientset,
		fetchPodFile: recordingFetcher(nil),
	}

	if err := g.gatherBugReportFromDaemonSet(namespace); err != nil {
		t.Fatalf("gatherBugReportFromDaemonSet: %v", err)
	}

	want := []string{
		"nvidia-bug-report_worker-a.log.gz",
		"nvidia-bug-report_worker-b.log.gz",
		"nvidia-bug-report_worker-c.log.gz",
	}
	got := artifactNames(t, artifactDir)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("artifact dir holds %v, want exactly %v", got, want)
	}

	contents := map[string]string{
		"nvidia-bug-report_worker-a.log.gz": "nvidia-driver-daemonset-aaaaa",
		"nvidia-bug-report_worker-b.log.gz": "nvidia-driver-daemonset-bbbbb",
		"nvidia-bug-report_worker-c.log.gz": "nvidia-driver-daemonset-ccccc",
	}
	for name, wantContent := range contents {
		data, err := os.ReadFile(filepath.Join(artifactDir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(data) != wantContent {
			t.Errorf("%s holds report of pod %q, want %q", name, string(data), wantContent)
		}
	}
}

// vgpuPod builds a running vGPU manager daemonset pod scheduled on the named
// node.
func vgpuPod(t *testing.T, namespace, name, nodeName string) *v1.Pod {
	t.Helper()
	pod := driverPod(t, namespace, name, nodeName)
	pod.Labels = map[string]string{"app": "nvidia-vgpu-manager-daemonset"}
	return pod
}

// TestGatherBugReportReportsEveryFailure pins that the error handed back names
// every pod that failed, not just the first. An operator who reads only the
// first name re-runs the gather believing one node is at fault, and loses the
// evidence that the failure is cluster-wide.
func TestGatherBugReportReportsEveryFailure(t *testing.T) {
	const namespace = "gpu-operator"
	const firstFailure = "nvidia-driver-daemonset-bbbbb"
	const secondFailure = "nvidia-driver-daemonset-ddddd"

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	clientset := fake.NewClientset(
		driverPod(t, namespace, "nvidia-driver-daemonset-aaaaa", "worker-a"),
		driverPod(t, namespace, firstFailure, "worker-b"),
		driverPod(t, namespace, secondFailure, "worker-d"),
	)

	g := &Gatherer{
		opts:       &Options{ArtifactDir: artifactDir},
		logFile:    logFile,
		errLogFile: errLogFile,
		clientset:  clientset,
		fetchPodFile: recordingFetcher(map[string]bool{
			firstFailure:  true,
			secondFailure: true,
		}),
	}

	err := g.gatherBugReportFromDaemonSet(namespace)
	if err == nil {
		t.Fatal("gatherBugReportFromDaemonSet returned nil, want an error naming both failing pods")
	}
	for _, failed := range []string{firstFailure, secondFailure} {
		if !strings.Contains(err.Error(), failed) {
			t.Errorf("error does not name failing pod %s, holds:\n%s", failed, err.Error())
		}
	}

	// The healthy node is still collected.
	want := []string{"nvidia-bug-report_worker-a.log.gz"}
	got := artifactNames(t, artifactDir)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("artifact dir holds %v, want exactly %v", got, want)
	}
}

// TestGatherBugReportCollectsVgpuAfterDriverFailure pins that a driver daemonset
// that fails does not cost us the vGPU manager daemonset behind it. The two are
// separate collections and a cluster running vGPU needs both.
func TestGatherBugReportCollectsVgpuAfterDriverFailure(t *testing.T) {
	const namespace = "gpu-operator"
	const failingDriver = "nvidia-driver-daemonset-aaaaa"

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	clientset := fake.NewClientset(
		driverPod(t, namespace, failingDriver, "worker-a"),
		vgpuPod(t, namespace, "nvidia-vgpu-manager-daemonset-bbbbb", "worker-b"),
	)

	g := &Gatherer{
		opts:         &Options{ArtifactDir: artifactDir},
		logFile:      logFile,
		errLogFile:   errLogFile,
		clientset:    clientset,
		fetchPodFile: recordingFetcher(map[string]bool{failingDriver: true}),
	}

	err := g.gatherBugReportFromDaemonSet(namespace)
	if err == nil {
		t.Fatal("gatherBugReportFromDaemonSet returned nil, want an error naming the failing driver pod")
	}
	if !strings.Contains(err.Error(), failingDriver) {
		t.Errorf("error does not name failing pod %s, holds:\n%s", failingDriver, err.Error())
	}

	// The vGPU manager daemonset is queried after the driver one, so its report
	// only exists if the driver failure did not end the gather.
	want := []string{"nvidia-bug-report_worker-b.log.gz"}
	got := artifactNames(t, artifactDir)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("artifact dir holds %v, want exactly %v", got, want)
	}
	content := readFileAsString(t, filepath.Join(artifactDir, "nvidia-bug-report_worker-b.log.gz"))
	if content != "nvidia-vgpu-manager-daemonset-bbbbb" {
		t.Errorf("worker-b report holds %q, want the vGPU manager pod's report", content)
	}
}

// TestGatherBugReportKeepsDriverAndVgpuOnOneNode pins that a driver pod and a
// vGPU manager pod sharing a node keep separate files. The two daemonsets are
// collected in separate passes, so a name ledger scoped to one pass lets the
// second pass overwrite the first node's report.
func TestGatherBugReportKeepsDriverAndVgpuOnOneNode(t *testing.T) {
	const namespace = "gpu-operator"
	const vgpuName = "nvidia-vgpu-manager-daemonset-bbbbb"

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	clientset := fake.NewClientset(
		driverPod(t, namespace, "nvidia-driver-daemonset-aaaaa", "worker-a"),
		vgpuPod(t, namespace, vgpuName, "worker-a"),
	)

	g := &Gatherer{
		opts:         &Options{ArtifactDir: artifactDir},
		logFile:      logFile,
		errLogFile:   errLogFile,
		clientset:    clientset,
		fetchPodFile: recordingFetcher(nil),
	}

	if err := g.gatherBugReportFromDaemonSet(namespace); err != nil {
		t.Fatalf("gatherBugReportFromDaemonSet: %v", err)
	}

	want := []string{
		"nvidia-bug-report_worker-a.log.gz",
		"nvidia-bug-report_worker-a_" + vgpuName + ".log.gz",
	}
	got := artifactNames(t, artifactDir)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("artifact dir holds %v, want exactly %v", got, want)
	}

	// Each file must hold its own daemonset's report, not the later pass's.
	contents := map[string]string{
		"nvidia-bug-report_worker-a.log.gz":                  "nvidia-driver-daemonset-aaaaa",
		"nvidia-bug-report_worker-a_" + vgpuName + ".log.gz": vgpuName,
	}
	for name, wantContent := range contents {
		data, err := os.ReadFile(filepath.Join(artifactDir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(data) != wantContent {
			t.Errorf("%s holds report of pod %q, want %q", name, string(data), wantContent)
		}
	}
}

// TestCollectBugReportsContinuesAfterFailure pins that one unreachable driver pod
// does not cost the operator the reports of every node behind it, while still
// surfacing the failure to the caller.
func TestCollectBugReportsContinuesAfterFailure(t *testing.T) {
	const namespace = "gpu-operator"
	const failing = "nvidia-driver-daemonset-bbbbb"

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	pods := []v1.Pod{
		*driverPod(t, namespace, "nvidia-driver-daemonset-aaaaa", "worker-a"),
		*driverPod(t, namespace, failing, "worker-b"),
		*driverPod(t, namespace, "nvidia-driver-daemonset-ccccc", "worker-c"),
	}

	g := &Gatherer{
		opts:         &Options{ArtifactDir: artifactDir},
		logFile:      logFile,
		errLogFile:   errLogFile,
		fetchPodFile: recordingFetcher(map[string]bool{failing: true}),
	}

	err := g.collectBugReports("app=nvidia-driver-daemonset", pods)
	if err == nil {
		t.Fatal("collectBugReports returned nil, want an error naming the failing pod")
	}
	if !strings.Contains(err.Error(), failing) {
		t.Errorf("error %q does not name the failing pod %q", err.Error(), failing)
	}

	// worker-b produced nothing, but its neighbours must still be on disk.
	want := []string{
		"nvidia-bug-report_worker-a.log.gz",
		"nvidia-bug-report_worker-c.log.gz",
	}
	got := artifactNames(t, artifactDir)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("artifact dir holds %v, want exactly %v", got, want)
	}
}

// TestCollectBugReportsSkipsUncollectablePods pins that a pod with no node and a
// pod that is not running are passed over, each with a line in the error log,
// instead of producing a "nvidia-bug-report_.log.gz" or a failed exec.
func TestCollectBugReportsSkipsUncollectablePods(t *testing.T) {
	const namespace = "gpu-operator"

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	unscheduled := driverPod(t, namespace, "nvidia-driver-daemonset-pending", "")
	terminating := driverPod(t, namespace, "nvidia-driver-daemonset-failed", "worker-b")
	terminating.Status.Phase = v1.PodFailed

	pods := []v1.Pod{
		*driverPod(t, namespace, "nvidia-driver-daemonset-aaaaa", "worker-a"),
		*unscheduled,
		*terminating,
	}

	var fetched []string
	g := &Gatherer{
		opts:       &Options{ArtifactDir: artifactDir},
		logFile:    logFile,
		errLogFile: errLogFile,
		fetchPodFile: func(pod *v1.Pod, destPath string) error {
			fetched = append(fetched, pod.Name)
			return os.WriteFile(destPath, []byte(pod.Name), 0o600)
		},
	}

	if err := g.collectBugReports("app=nvidia-driver-daemonset", pods); err != nil {
		t.Fatalf("collectBugReports: %v", err)
	}

	if len(fetched) != 1 || fetched[0] != "nvidia-driver-daemonset-aaaaa" {
		t.Errorf("fetched from %v, want only [nvidia-driver-daemonset-aaaaa]", fetched)
	}
	want := []string{"nvidia-bug-report_worker-a.log.gz"}
	got := artifactNames(t, artifactDir)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("artifact dir holds %v, want exactly %v", got, want)
	}

	errLog := readFileAsString(t, errLogFile.Name())
	for _, skipped := range []string{"nvidia-driver-daemonset-pending", "nvidia-driver-daemonset-failed"} {
		if !strings.Contains(errLog, skipped) {
			t.Errorf("error log does not record skipping %s, holds:\n%s", skipped, errLog)
		}
	}
}

// TestCollectBugReportsKeepsBothReportsOnOneNode pins that a second driver pod on
// a node gets its own file. Both pods share a node name, so naming by node alone
// would leave one file holding the second pod's report.
func TestCollectBugReportsKeepsBothReportsOnOneNode(t *testing.T) {
	const namespace = "gpu-operator"
	const second = "nvidia-driver-daemonset-bbbbb"

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	pods := []v1.Pod{
		*driverPod(t, namespace, "nvidia-driver-daemonset-aaaaa", "worker-a"),
		*driverPod(t, namespace, second, "worker-a"),
	}

	g := &Gatherer{
		opts:         &Options{ArtifactDir: artifactDir},
		logFile:      logFile,
		errLogFile:   errLogFile,
		fetchPodFile: recordingFetcher(nil),
	}

	if err := g.collectBugReports("app=nvidia-driver-daemonset", pods); err != nil {
		t.Fatalf("collectBugReports: %v", err)
	}

	want := []string{
		"nvidia-bug-report_worker-a.log.gz",
		"nvidia-bug-report_worker-a_" + second + ".log.gz",
	}
	got := artifactNames(t, artifactDir)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("artifact dir holds %v, want exactly %v", got, want)
	}

	contents := map[string]string{
		"nvidia-bug-report_worker-a.log.gz":                "nvidia-driver-daemonset-aaaaa",
		"nvidia-bug-report_worker-a_" + second + ".log.gz": second,
	}
	for name, wantContent := range contents {
		data, err := os.ReadFile(filepath.Join(artifactDir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(data) != wantContent {
			t.Errorf("%s holds report of pod %q, want %q", name, string(data), wantContent)
		}
	}
}

// TestGatherBugReportNamesFilesPerNode pins the destination name of a bug report
// to the node the driver pod runs on. Naming by daemonset label instead leaves a
// single file holding the last pod's report, silently losing every other node.
func TestGatherBugReportNamesFilesPerNode(t *testing.T) {
	const namespace = "gpu-operator"

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	pods := []v1.Pod{
		*driverPod(t, namespace, "nvidia-driver-daemonset-aaaaa", "worker-a"),
		*driverPod(t, namespace, "nvidia-driver-daemonset-bbbbb", "worker-b"),
	}

	g := &Gatherer{
		opts:       &Options{ArtifactDir: artifactDir},
		logFile:    logFile,
		errLogFile: errLogFile,
		// The fetcher stands in for exec + copyFromPod: it records which pod the
		// report came from by writing that pod's name into the destination file.
		fetchPodFile: func(pod *v1.Pod, destPath string) error {
			return os.WriteFile(destPath, []byte(pod.Name), 0o600)
		},
	}

	if err := g.collectBugReports("app=nvidia-driver-daemonset", pods); err != nil {
		t.Fatalf("collectBugReports: %v", err)
	}

	entries, err := os.ReadDir(artifactDir)
	if err != nil {
		t.Fatalf("reading artifact dir: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)

	want := []string{
		"nvidia-bug-report_worker-a.log.gz",
		"nvidia-bug-report_worker-b.log.gz",
	}
	if len(got) != len(want) {
		t.Fatalf("artifact dir holds %v, want exactly %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("artifact dir holds %v, want exactly %v", got, want)
		}
	}

	// Each file must carry its own node's report, not a copy of the other's.
	contents := map[string]string{
		"nvidia-bug-report_worker-a.log.gz": "nvidia-driver-daemonset-aaaaa",
		"nvidia-bug-report_worker-b.log.gz": "nvidia-driver-daemonset-bbbbb",
	}
	for name, wantContent := range contents {
		data, err := os.ReadFile(filepath.Join(artifactDir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if string(data) != wantContent {
			t.Errorf("%s holds report of pod %q, want %q", name, string(data), wantContent)
		}
	}
}

// scratchLogFiles returns the two log files a Gatherer writes its own progress
// and errors to, outside the artifact directory under assertion.
func scratchLogFiles(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	dir := t.TempDir()
	logFile, err := os.Create(filepath.Join(dir, "must-gather.log"))
	if err != nil {
		t.Fatalf("creating log file: %v", err)
	}
	errLogFile, err := os.Create(filepath.Join(dir, "must-gather.stderr.log"))
	if err != nil {
		t.Fatalf("creating stderr log file: %v", err)
	}
	t.Cleanup(func() {
		logFile.Close()
		errLogFile.Close()
	})
	return logFile, errLogFile
}

// readFileAsString returns the contents of path, failing the test if unreadable.
func readFileAsString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(data)
}
