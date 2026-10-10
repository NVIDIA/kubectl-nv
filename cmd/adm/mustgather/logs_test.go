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
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const logLine = "2026-10-10T09:00:00.000000000Z line from %s\n"

// logRequest records one call into the pod log streamer.
type logRequest struct {
	namespace string
	pod       string
	container string
	previous  bool
}

// apiServerLogStreamer behaves like the API server's log endpoint for the given
// pods: it rejects a request that names no container when the pod runs more than
// one, and it only returns timestamped lines when timestamps were asked for.
func apiServerLogStreamer(pods []v1.Pod, recorded *[]logRequest) podLogStreamer {
	byName := make(map[string]v1.Pod, len(pods))
	for _, p := range pods {
		byName[p.Name] = p
	}
	return func(_ context.Context, namespace, podName string, opts *v1.PodLogOptions) (io.ReadCloser, error) {
		*recorded = append(*recorded, logRequest{
			namespace: namespace,
			pod:       podName,
			container: opts.Container,
			previous:  opts.Previous,
		})

		pod, ok := byName[podName]
		if !ok {
			return nil, fmt.Errorf("pods %q not found", podName)
		}
		names := containerNamesOf(pod)
		if opts.Container == "" && len(names) > 1 {
			return nil, fmt.Errorf("a container name must be specified for pod %s, choose one of: [%s]",
				podName, strings.Join(names, " "))
		}
		if !opts.Timestamps {
			return nil, errors.New("timestamps not requested")
		}
		return io.NopCloser(strings.NewReader(fmt.Sprintf(logLine, opts.Container))), nil
	}
}

// containerNamesOf lists the pod's containers the way the API server does in its
// "choose one of" error: regular containers only, in spec order.
func containerNamesOf(pod v1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.Containers))
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}

func podWithContainers(namespace, name string, initContainers, containers []string) v1.Pod {
	toContainers := func(names []string) []v1.Container {
		out := make([]v1.Container, 0, len(names))
		for _, n := range names {
			out = append(out, v1.Container{Name: n})
		}
		return out
	}
	return v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1.PodSpec{
			InitContainers: toContainers(initContainers),
			Containers:     toContainers(containers),
		},
	}
}

// TestGatherPodLogsPerContainerWithTimestamps pins log collection to one request
// and one file per container, with timestamps requested and kubectl's per-line
// pod/container prefix. A single request that names no container is refused by
// the API server for any multi-container pod, so the logs are never collected.
func TestGatherPodLogsPerContainerWithTimestamps(t *testing.T) {
	const namespace = "gpu-operator"
	const operatorName = "gpu-operator-7d9f6c4b8-x2kqs"
	const operandName = "nvidia-operator-validator-9z4rt"

	operatorPod := podWithContainers(namespace, operatorName,
		[]string{"driver-validation"}, []string{"gpu-operator", "gpu-operator-validator"})
	operandPod := podWithContainers(namespace, operandName,
		nil, []string{"toolkit", "validator"})

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	var requests []logRequest
	g := &Gatherer{
		opts:          &Options{ArtifactDir: artifactDir},
		logFile:       logFile,
		errLogFile:    errLogFile,
		streamPodLogs: apiServerLogStreamer([]v1.Pod{operatorPod, operandPod}, &requests),
	}

	podInfo := &PodInfo{
		OperatorPods: &v1.PodList{Items: []v1.Pod{operatorPod}},
		OperandPods:  &v1.PodList{Items: []v1.Pod{operandPod}},
	}
	if err := g.gatherPodLogs(podInfo, namespace); err != nil {
		t.Fatalf("gatherPodLogs: %v", err)
	}

	wantFiles := map[string]string{
		// Operand pod: one file per container, named "<pod>_<container>.log".
		operandName + "_toolkit.log": fmt.Sprintf("[pod/%s/toolkit] "+logLine, operandName, "toolkit"),
		operandName + "_validator.log": fmt.Sprintf("[pod/%s/validator] "+logLine, operandName,
			"validator"),
		// Operator pod, current logs.
		"gpu_operator_logs_driver-validation.log": fmt.Sprintf("[pod/%s/driver-validation] "+logLine,
			operatorName, "driver-validation"),
		"gpu_operator_logs_gpu-operator.log": fmt.Sprintf("[pod/%s/gpu-operator] "+logLine,
			operatorName, "gpu-operator"),
		"gpu_operator_logs_gpu-operator-validator.log": fmt.Sprintf("[pod/%s/gpu-operator-validator] "+logLine,
			operatorName, "gpu-operator-validator"),
		// Operator pod, previous logs.
		"gpu_operator_logs_driver-validation.previous.log": fmt.Sprintf("[pod/%s/driver-validation] "+logLine,
			operatorName, "driver-validation"),
		"gpu_operator_logs_gpu-operator.previous.log": fmt.Sprintf("[pod/%s/gpu-operator] "+logLine,
			operatorName, "gpu-operator"),
		"gpu_operator_logs_gpu-operator-validator.previous.log": fmt.Sprintf("[pod/%s/gpu-operator-validator] "+logLine,
			operatorName, "gpu-operator-validator"),
	}

	for name, want := range wantFiles {
		data, err := os.ReadFile(filepath.Join(artifactDir, name))
		if err != nil {
			t.Errorf("reading %s: %v", name, err)
			continue
		}
		if string(data) != want {
			t.Errorf("%s holds %q, want %q", name, string(data), want)
		}
	}

	// Init containers are collected before regular containers, in spec order, and
	// every request names its container.
	wantRequests := []logRequest{
		{namespace, operatorName, "driver-validation", false},
		{namespace, operatorName, "gpu-operator", false},
		{namespace, operatorName, "gpu-operator-validator", false},
		{namespace, operatorName, "driver-validation", true},
		{namespace, operatorName, "gpu-operator", true},
		{namespace, operatorName, "gpu-operator-validator", true},
		{namespace, operandName, "toolkit", false},
		{namespace, operandName, "validator", false},
	}
	if len(requests) != len(wantRequests) {
		t.Fatalf("made %d log requests (%v), want %d (%v)", len(requests), requests,
			len(wantRequests), wantRequests)
	}
	for i := range wantRequests {
		if requests[i] != wantRequests[i] {
			t.Errorf("log request %d is %+v, want %+v", i, requests[i], wantRequests[i])
		}
	}
}

// TestGatherPodLogsPrefixesEveryLine pins the prefix to every line of the
// stream, not just the first, and pins that a final line the container emitted
// without a trailing newline still reaches the artifact. A log ends mid-line
// whenever the container was killed partway through a write, which is exactly
// the case being diagnosed.
func TestGatherPodLogsPrefixesEveryLine(t *testing.T) {
	const namespace = "gpu-operator"
	const operandName = "nvidia-container-toolkit-daemonset-k7m2p"

	// Two complete lines and a third cut off without a trailing newline.
	const streamed = "first line\nsecond line\ntruncated tail"

	operandPod := podWithContainers(namespace, operandName, nil, []string{"toolkit"})
	operatorPod := podWithContainers(namespace, "gpu-operator-7d9f6c4b8-x2kqs", nil, []string{"gpu-operator"})

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	g := &Gatherer{
		opts:       &Options{ArtifactDir: artifactDir},
		logFile:    logFile,
		errLogFile: errLogFile,
		streamPodLogs: func(_ context.Context, _, podName string, opts *v1.PodLogOptions) (io.ReadCloser, error) {
			if podName != operandName {
				return io.NopCloser(strings.NewReader("")), nil
			}
			return io.NopCloser(strings.NewReader(streamed)), nil
		},
	}

	podInfo := &PodInfo{
		OperatorPods: &v1.PodList{Items: []v1.Pod{operatorPod}},
		OperandPods:  &v1.PodList{Items: []v1.Pod{operandPod}},
	}
	if err := g.gatherPodLogs(podInfo, namespace); err != nil {
		t.Fatalf("gatherPodLogs: %v", err)
	}

	prefix := fmt.Sprintf("[pod/%s/toolkit] ", operandName)
	want := prefix + "first line\n" + prefix + "second line\n" + prefix + "truncated tail"

	data, err := os.ReadFile(filepath.Join(artifactDir, operandName+"_toolkit.log"))
	if err != nil {
		t.Fatalf("reading toolkit log: %v", err)
	}
	if string(data) != want {
		t.Errorf("toolkit log holds %q, want %q", string(data), want)
	}
}

// TestGatherPodLogsKeepsPreviousLogsPlaceholder pins the existing handling of a
// container that has never restarted: the previous-log file is still written,
// carrying the placeholder rather than failing the whole collection.
func TestGatherPodLogsKeepsPreviousLogsPlaceholder(t *testing.T) {
	const namespace = "gpu-operator"
	const operatorName = "gpu-operator-7d9f6c4b8-x2kqs"

	operatorPod := podWithContainers(namespace, operatorName, nil, []string{"gpu-operator"})

	artifactDir := t.TempDir()
	logFile, errLogFile := scratchLogFiles(t)

	g := &Gatherer{
		opts:       &Options{ArtifactDir: artifactDir},
		logFile:    logFile,
		errLogFile: errLogFile,
		streamPodLogs: func(_ context.Context, _, podName string, opts *v1.PodLogOptions) (io.ReadCloser, error) {
			if opts.Previous {
				return nil, fmt.Errorf("previous terminated container %q in pod %q not found",
					opts.Container, podName)
			}
			return io.NopCloser(strings.NewReader(fmt.Sprintf(logLine, opts.Container))), nil
		},
	}

	podInfo := &PodInfo{
		OperatorPods: &v1.PodList{Items: []v1.Pod{operatorPod}},
		OperandPods:  &v1.PodList{},
	}
	if err := g.gatherPodLogs(podInfo, namespace); err != nil {
		t.Fatalf("gatherPodLogs: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(artifactDir, "gpu_operator_logs_gpu-operator.previous.log"))
	if err != nil {
		t.Fatalf("reading previous log file: %v", err)
	}
	if string(data) != "No previous logs available\n" {
		t.Errorf("previous log file holds %q, want %q", string(data), "No previous logs available\n")
	}
}
