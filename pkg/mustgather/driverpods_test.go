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
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
)

// errUnreachable stands in for whatever the API server says when an exec or a
// log stream cannot be served.
var errUnreachable = errors.New("the pod cannot be reached")

// driverPodWith builds a running driver pod carrying one of the labels the
// gather looks for.
func driverPodWith(name, node string, labels map[string]string, containers ...v1.Container) *v1.Pod {
	if len(containers) == 0 {
		containers = []v1.Container{{Name: "nvidia-driver-ctr"}}
	}
	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "gpu-operator", Labels: labels},
		Spec:       v1.PodSpec{NodeName: node, Containers: containers},
		Status:     v1.PodStatus{Phase: v1.PodRunning},
	}
}

// TestDriverPodsAreFoundByEveryLabel pins the four selectors the gpu-operator
// script uses. Each one is a different way of installing the driver: the
// operator's own daemonset, the app.kubernetes.io form, the vGPU manager, and
// the OpenShift driver toolkit. Dropping any one of them loses the bug report
// of every cluster installed that way, and the bundle says nothing about it.
func TestDriverPodsAreFoundByEveryLabel(t *testing.T) {
	pods := []runtime.Object{
		driverPodWith("driver-a", "node-a", map[string]string{"app": "nvidia-driver-daemonset"}),
		driverPodWith("driver-b", "node-b", map[string]string{"app.kubernetes.io/component": "nvidia-driver"}),
		driverPodWith("driver-c", "node-c", map[string]string{"app": "nvidia-vgpu-manager-daemonset"}),
		driverPodWith("driver-d", "node-d", map[string]string{"openshift.driver-toolkit": "true"}),
		// Not a driver pod: it must not be exec'd into.
		driverPodWith("device-plugin", "node-e", map[string]string{"app": "nvidia-device-plugin-daemonset"}),
	}

	g, _ := newTestGatherer(t, pods...)

	found, err := g.findDriverPods("gpu-operator")
	if err != nil {
		t.Fatalf("findDriverPods: %v", err)
	}

	got := make([]string, 0, len(found))
	for _, p := range found {
		got = append(got, p.pod.Name)
	}
	sort.Strings(got)

	want := []string{"driver-a", "driver-b", "driver-c", "driver-d"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("driver pods = %v, want %v", got, want)
	}
}

// TestDriverPodsAreNotCollectedTwice covers a pod that carries more than one of
// the selectors, which the operator's own daemonset does: the script
// deduplicates with "sort -u" and so must we, or the node's report is collected
// twice and the second run overwrites the first under a longer name.
func TestDriverPodsAreNotCollectedTwice(t *testing.T) {
	both := driverPodWith("driver-a", "node-a", map[string]string{
		"app":                         "nvidia-driver-daemonset",
		"app.kubernetes.io/component": "nvidia-driver",
	})

	g, _ := newTestGatherer(t, both)

	found, err := g.findDriverPods("gpu-operator")
	if err != nil {
		t.Fatalf("findDriverPods: %v", err)
	}
	if len(found) != 1 {
		names := make([]string, 0, len(found))
		for _, p := range found {
			names = append(names, p.pod.Name)
		}
		t.Errorf("a pod with two driver labels was found %d times: %v", len(found), names)
	}
}

// TestExecTargetsTheDefaultContainer is the open item the handoff left: the
// exec named no container, so the API server picked the first one, which on a
// driver pod carrying a sidecar is not the one holding nvidia-bug-report.sh.
// The annotation kubectl itself honours is what says which container to use.
func TestExecTargetsTheDefaultContainer(t *testing.T) {
	cases := []struct {
		name        string
		annotations map[string]string
		containers  []v1.Container
		want        string
	}{
		{
			name:       "no annotation falls back to the first container",
			containers: []v1.Container{{Name: "nvidia-driver-ctr"}, {Name: "sidecar"}},
			want:       "nvidia-driver-ctr",
		},
		{
			name:        "the annotation wins over the order",
			annotations: map[string]string{"kubectl.kubernetes.io/default-container": "sidecar"},
			containers:  []v1.Container{{Name: "nvidia-driver-ctr"}, {Name: "sidecar"}},
			want:        "sidecar",
		},
		{
			name:        "an annotation naming no container of the pod is ignored",
			annotations: map[string]string{"kubectl.kubernetes.io/default-container": "gone"},
			containers:  []v1.Container{{Name: "nvidia-driver-ctr"}},
			want:        "nvidia-driver-ctr",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod := driverPodWith("driver-a", "node-a",
				map[string]string{"app": "nvidia-driver-daemonset"}, c.containers...)
			pod.Annotations = c.annotations

			if got := execContainer(pod); got != c.want {
				t.Errorf("exec container = %q, want %q", got, c.want)
			}
		})
	}
}

// TestBothDriverCollectionFailuresAreNamed is the second open item: when the
// driver daemonset and the vGPU manager both fail, the error named one of them
// and the other was lost. A support engineer reading that error concludes the
// cluster has no vGPU manager.
func TestBothDriverCollectionFailuresAreNamed(t *testing.T) {
	pods := []runtime.Object{
		driverPodWith("driver-a", "node-a", map[string]string{"app": "nvidia-driver-daemonset"}),
		driverPodWith("vgpu-a", "node-b", map[string]string{"app": "nvidia-vgpu-manager-daemonset"}),
	}

	dir := filepath.Join(t.TempDir(), "artifacts")
	g, err := NewGatherer(&Options{
		ArtifactDir: dir,
		FetchPodFile: func(_ context.Context, pod *v1.Pod, _ string) error {
			return fmt.Errorf("%s: %w", pod.Name, errUnreachable)
		},
	}, stubFactory{clientset: fake.NewSimpleClientset(pods...), config: offlineConfig()}, discardProgress{})
	if err != nil {
		t.Fatalf("NewGatherer: %v", err)
	}
	t.Cleanup(g.Close)

	cerr := g.gatherBugReportFromDaemonSet(&gatherState{operatorNamespace: "gpu-operator"})
	if cerr == nil {
		t.Fatal("the bug report collector reported success with both daemonsets failing")
	}
	for _, want := range []string{"driver-a", "vgpu-a"} {
		if !strings.Contains(cerr.Error(), want) {
			t.Errorf("the error does not name %s:\n%v", want, cerr)
		}
	}
}

// TestPerContainerLogsContinuePastAFailure is the first open item: one
// container whose logs the API server will not serve ended the whole log
// collection, so the containers after it were silently lost.
func TestPerContainerLogsContinuePastAFailure(t *testing.T) {
	pod := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "operand-0", Namespace: "gpu-operator"},
		Spec: v1.PodSpec{
			NodeName: "node-a",
			Containers: []v1.Container{
				{Name: "first"}, {Name: "broken"}, {Name: "last"},
			},
		},
		Status: v1.PodStatus{Phase: v1.PodRunning},
	}

	dir := filepath.Join(t.TempDir(), "artifacts")
	g, err := NewGatherer(&Options{ArtifactDir: dir},
		stubFactory{clientset: fake.NewSimpleClientset(pod), config: offlineConfig()},
		discardProgress{})
	if err != nil {
		t.Fatalf("NewGatherer: %v", err)
	}
	t.Cleanup(g.Close)

	g.streamPodLogs = func(_ context.Context, _, _ string, opts *v1.PodLogOptions) (io.ReadCloser, error) {
		if opts.Container == "broken" {
			return nil, errUnreachable
		}
		return io.NopCloser(strings.NewReader("hello\n")), nil
	}

	if err := g.collectPodLogs("gpu-operator", pod, false); err != nil {
		t.Fatalf("collectPodLogs returned %v, want it to carry on past the broken container", err)
	}

	for _, container := range []string{"first", "last"} {
		if got := readArtifact(t, dir, "operand-0_"+container+".log"); !strings.Contains(got, "hello") {
			t.Errorf("operand-0_%s.log = %q, want the container's log", container, got)
		}
	}
	// The file for the failing container is still created, so that a reader can
	// tell a container whose logs could not be read from one that was never
	// looked at, and the error log says which it was.
	errLog := readArtifact(t, dir, "must-gather.stderr.log")
	if !strings.Contains(errLog, "broken") {
		t.Errorf("the error log does not record the failing container:\n%s", errLog)
	}
}
