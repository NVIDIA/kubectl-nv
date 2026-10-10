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
	"os"
	"path/filepath"
	"sort"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// driverPod builds a driver daemonset pod scheduled on the named node.
func driverPod(t *testing.T, namespace, name, nodeName string) *v1.Pod {
	t.Helper()
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": "nvidia-driver-daemonset"},
		},
		Spec: v1.PodSpec{NodeName: nodeName},
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
