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
	"bufio"
	"context"
	"errors"
	"fmt"
	"path/filepath"

	v1 "k8s.io/api/core/v1"
)

// gatherPodLogs collects the current and the previous log of every container of
// the operator pod and of every operand pod.
func (g *Gatherer) gatherPodLogs(s *gatherState) error {
	if err := g.prepareState(s); err != nil {
		return err
	}

	var errs []error
	if s.operatorPod != nil {
		for _, previous := range []bool{false, true} {
			if err := g.collectOperatorLogs(s.operatorNamespace, s.operatorPod, previous); err != nil {
				errs = append(errs, err)
			}
		}
	}

	for i := range s.operandPods {
		pod := s.operandPods[i]
		// The operator pod's log is already collected under its own name, and
		// collecting it twice doubles the largest file in the bundle.
		if s.operatorPod != nil && pod.Name == s.operatorPod.Name && pod.Namespace == s.operatorPod.Namespace {
			continue
		}
		for _, previous := range []bool{false, true} {
			if err := g.collectPodLogs(pod.Namespace, &pod, previous); err != nil {
				errs = append(errs, err)
			}
		}
	}

	return errors.Join(errs...)
}

// collectOperatorLogs writes the operator pod's logs under the names the
// bundle has used since Track B, one file per container.
func (g *Gatherer) collectOperatorLogs(namespace string, pod *v1.Pod, previous bool) error {
	var errs []error
	for _, container := range containerNames(pod) {
		fileName := fmt.Sprintf("gpu_operator_logs_%s.log", container)
		if previous {
			fileName = fmt.Sprintf("gpu_operator_logs_%s.previous.log", container)
		}
		if err := g.collectContainerLogs(namespace, pod, container, fileName, previous); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// collectPodLogs writes one file per container of a pod.
//
// A container whose logs the API server will not serve used to end the whole
// collection, so every container after it was silently lost: a pod with a
// crash-looping sidecar took the driver's own log with it. Each container is
// now collected on its own and the failures are reported together.
func (g *Gatherer) collectPodLogs(namespace string, pod *v1.Pod, previous bool) error {
	var errs []error
	for _, container := range containerNames(pod) {
		fileName := fmt.Sprintf("%s_%s.log", pod.Name, container)
		if previous {
			fileName = fmt.Sprintf("%s_%s.previous.log", pod.Name, container)
		}
		if err := g.collectContainerLogs(namespace, pod, container, fileName, previous); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// collectContainerLogs writes the logs of a single container to fileName under
// the artifact directory. The log request always names its container, because
// the API server refuses a request without one for a pod running more than one
// container, and always asks for timestamps.
//
// A container the API server will not serve leaves its file behind holding the
// reason, so that a reader can tell it apart from a container nobody looked at.
func (g *Gatherer) collectContainerLogs(namespace string, pod *v1.Pod, container, fileName string, previous bool) error {
	logPath := filepath.Join(g.opts.ArtifactDir, fileName)
	logFile, err := g.createArtifact(logPath)
	if err != nil {
		return g.logError(fmt.Sprintf("error creating %s: %%v", fileName), err)
	}
	committed := false
	defer func() {
		if !committed {
			logFile.discard()
		}
	}()
	commit := func(cause error) error {
		if cerr := logFile.commit(); cerr != nil {
			return g.logError(fmt.Sprintf("error writing %s: %%v", fileName), cerr)
		}
		committed = true
		return cause
	}

	podLogOpts := v1.PodLogOptions{
		Container:  container,
		Timestamps: true,
		Previous:   previous,
	}
	// The window goes on the current log only. The previous log ended when the
	// container died, so a window measured from now would empty exactly the
	// file a crash loop is diagnosed from.
	if !previous && g.opts.Since > 0 {
		seconds := int64(g.opts.Since.Seconds())
		podLogOpts.SinceSeconds = &seconds
	}

	stream, err := g.podLogs(context.Background(), namespace, pod.Name, &podLogOpts)
	if err != nil || stream == nil {
		what := "logs"
		if previous {
			what = "previous logs"
		}
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting %s for pod %s container %s: %v\n",
			what, pod.Name, container, err); lerr != nil {
			return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		if previous {
			return commit(writeStringToFile(logFile, "No previous logs available\n"))
		}
		return commit(writeStringToFile(logFile, fmt.Sprintf("No logs available: %v\n", err)))
	}
	defer stream.Close()

	writer := bufio.NewWriter(logFile)
	if err := writePrefixedLines(writer, stream, pod.Name, container); err != nil {
		return g.logError(fmt.Sprintf("error writing %s: %%v", fileName), err)
	}
	// The buffer holds the tail of the log until this point, so a failure here
	// truncates the artifact silently.
	if err := writer.Flush(); err != nil {
		return g.logError(fmt.Sprintf("error flushing %s: %%v", fileName), err)
	}
	return commit(nil)
}
