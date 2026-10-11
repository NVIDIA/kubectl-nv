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
	"os"
	"path/filepath"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
)

// driverLabels are the four selectors a driver pod can carry. Each one is a
// different way of installing the driver: the operator's own daemonset, the
// app.kubernetes.io form newer charts use, the vGPU manager, and the OpenShift
// driver toolkit. Dropping any one of them loses the bug report of every
// cluster installed that way, and the bundle says nothing about it.
var driverLabels = []string{
	"app=nvidia-driver-daemonset",
	"app.kubernetes.io/component=nvidia-driver",
	"app=nvidia-vgpu-manager-daemonset",
	"openshift.driver-toolkit",
}

// defaultContainerAnnotation is the annotation kubectl reads to decide which
// container of a pod a command without --container runs in. A driver pod
// carrying a sidecar has nvidia-bug-report.sh in only one of its containers,
// and the first in spec order is not always that one.
const defaultContainerAnnotation = "kubectl.kubernetes.io/default-container"

// driverTarget is one pod a bug report and the device usage are collected from,
// with the label it was found through and the container a command runs in.
type driverTarget struct {
	pod       v1.Pod
	label     string
	container string
}

// findDriverPods lists the driver pods of a namespace under every label, with
// each pod reported once however many of the labels it carries, narrowed to
// --node when one was given.
func (g *Gatherer) findDriverPods(namespace string) ([]driverTarget, error) {
	seen := make(map[string]bool)
	var found []driverTarget

	for _, label := range driverLabels {
		list, err := g.listPods(namespace, label)
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting pods for label %s: %v\n", label, err); lerr != nil {
				return nil, fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return nil, err
		}
		for _, pod := range g.podsOnNode(list.Items) {
			key := pod.Namespace + "/" + pod.Name
			if seen[key] {
				continue
			}
			seen[key] = true
			found = append(found, driverTarget{pod: pod, label: label, container: execContainer(&pod)})
		}
	}

	sort.Slice(found, func(i, j int) bool { return found[i].pod.Name < found[j].pod.Name })
	return found, nil
}

// execContainer names the container a command runs in: the one the pod's own
// annotation points at, or the first container when there is no annotation or
// it names a container the pod does not have.
func execContainer(pod *v1.Pod) string {
	named := pod.Annotations[defaultContainerAnnotation]
	for _, container := range pod.Spec.Containers {
		if container.Name == named {
			return named
		}
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}
	return ""
}

// collectable reports whether a pod can answer an exec, and why not when it
// cannot. A pod that never got scheduled has no node to name a report after,
// and one that is not running cannot answer at all; spending a doomed exec on
// either costs a timeout and tells the reader nothing.
func collectable(pod *v1.Pod) (bool, string) {
	if pod.Spec.NodeName == "" {
		return false, "not scheduled to a node"
	}
	if pod.Status.Phase != v1.PodRunning {
		return false, fmt.Sprintf("pod is %s, not Running", pod.Status.Phase)
	}
	// The pod and node names become file names in a bundle somebody else
	// unpacks. Every name a cluster accepted is a DNS-1123 subdomain, so one
	// that is not has either been tampered with or come from a confused
	// reader, and must not be used to build a path.
	if !usableInAName(pod.Name) {
		return false, fmt.Sprintf("pod name %q is not a usable name", pod.Name)
	}
	if !usableInAName(pod.Spec.NodeName) {
		return false, fmt.Sprintf("node name %q is not a usable name", pod.Spec.NodeName)
	}
	return true, ""
}

// gatherBugReportFromDaemonSet runs nvidia-bug-report.sh in every driver pod
// and copies the result out, one file per node.
func (g *Gatherer) gatherBugReportFromDaemonSet(s *gatherState) error {
	if err := g.prepareDriverPods(s); err != nil {
		return err
	}
	if g.opts.NoExec {
		return skip("--no-exec")
	}
	if len(s.driverPods) == 0 {
		g.note("no driver pods found (labels %s); skipping nvidia-bug-report", strings.Join(driverLabels, ", "))
		return skip("no driver pods")
	}

	// One ledger of destination names across every label: a node can run both a
	// driver pod and a vGPU manager pod, and the second must not overwrite the
	// first.
	taken := make(map[string]bool)
	var errs []error
	denied := false
	collected := 0
	reachable := 0

	for _, driver := range s.driverPods {
		pod := driver.pod
		if ok, why := collectable(&pod); !ok {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Skipping pod %s (label %s): %s\n",
				pod.Name, driver.label, why); lerr != nil {
				errs = append(errs, fmt.Errorf("error writing to stderr log file: %w", lerr))
			}
			continue
		}

		reachable++

		// The report is named after the node the pod runs on, as the script does.
		// A node already collected from, because it runs a second driver pod
		// mid-rollout or a vGPU manager pod too, gets the pod name as well.
		destFileName := fmt.Sprintf("nvidia-bug-report_%s.log.gz", pod.Spec.NodeName)
		if taken[destFileName] {
			destFileName = fmt.Sprintf("nvidia-bug-report_%s_%s.log.gz", pod.Spec.NodeName, pod.Name)
		}
		taken[destFileName] = true

		// One unreachable pod must not cost us the reports of the nodes behind
		// it, so the failures are collected and reported together at the end
		// naming every pod, not only the first.
		destPath := filepath.Join(g.opts.ArtifactDir, destFileName)
		ctx, cancel, timeout := g.execContext()
		err := g.fetchBugReport(ctx, &pod, destPath)
		cancel()
		if err != nil {
			if forbiddenExec(err) {
				denied = true
			}
			err = execFailure(ctx, timeout,
				fmt.Sprintf("collecting bug report (label %s)", driver.label), &pod, err)
			if _, lerr := fmt.Fprintf(g.errLogFile, "%v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			errs = append(errs, err)
			g.discardArtifact(destPath)
			continue
		}
		g.recordArtifact(destPath)
		collected++
	}

	switch {
	case denied:
		s.execDenied = append(s.execDenied, "bug-report")
		return skip("forbidden: %v", errors.Join(errs...))
	case reachable == 0:
		// Every driver pod was passed over, because none of them is running on
		// a node yet. The bundle holds no report and the reason belongs in the
		// manifest rather than only in the error log: "this cluster runs no
		// driver" and "its driver pods could not be asked" are different
		// clusters.
		return skip("no driver pod could be reached")
	case collected == 0 && len(errs) == 0:
		return skip("no driver pod could be reached")
	}
	return errors.Join(errs...)
}

// prepareDriverPods fills the driver pods into the state, once, so that the bug
// report and the device usage ask the cluster the same question and agree about
// which pods answered it.
func (g *Gatherer) prepareDriverPods(s *gatherState) error {
	if s.driverPods != nil {
		return nil
	}
	if err := g.prepareState(s); err != nil {
		return err
	}
	pods, err := g.findDriverPods(s.operatorNamespace)
	if err != nil {
		return err
	}
	if pods == nil {
		pods = []driverTarget{}
	}
	s.driverPods = pods
	return nil
}

// deviceUsageCommands are the commands the script runs on a driver pod's node,
// in the order it runs them. Everything but nvidia-smi runs in the host's
// namespaces, because the processes holding a GPU open are the host's, not the
// driver container's.
var deviceUsageCommands = []struct {
	heading string
	command string
}{
	{"nvidia-smi", "nvidia-smi"},
	{
		"lsof /run/nvidia/driver/dev/nvidia*",
		"nsenter --target 1 --mount --pid -- sh -c 'lsof /run/nvidia/driver/dev/nvidia* 2>&1 || true'",
	},
	{
		"process details for the PIDs using GPU devices",
		"nsenter --target 1 --mount --pid -- sh -c '" +
			`pids=$(lsof /run/nvidia/driver/dev/nvidia* 2>/dev/null | awk "NR>1 {print \$2}" | sort -u); ` +
			`if [ -n "$pids" ]; then for pid in $pids; do echo "=== Process $pid ==="; ` +
			`ps -p $pid -o pid,ppid,user,stat,start,etime,pcpu,pmem,vsz,rss,args 2>&1 || echo "Process $pid not found"; ` +
			`echo ""; done; else echo "No processes found using GPU devices"; fi'`,
	},
	{
		"fuser -v /run/nvidia/driver/dev/nvidia*",
		"nsenter --target 1 --mount --pid -- sh -c 'fuser -v /run/nvidia/driver/dev/nvidia* 2>&1 || true'",
	},
	{
		"ls -la /run/nvidia/driver/dev/nvidia*",
		"nsenter --target 1 --mount --pid -- sh -c 'ls -la /run/nvidia/driver/dev/nvidia* 2>&1 || true'",
	},
}

// nvidiaSMIProbe is what decides whether the device usage section can run at
// all. A driver pod on a node whose driver is preinstalled on the host, and the
// stand-in used in the end to end lane, have no nvidia-smi, and every command
// below it would produce a file of error messages.
const nvidiaSMIProbe = "command -v nvidia-smi"

// gatherDeviceUsage records which processes are holding each node's GPUs open,
// which is what a "the GPU is busy and nothing is using it" report is settled
// with.
func (g *Gatherer) gatherDeviceUsage(s *gatherState) error {
	if err := g.prepareDriverPods(s); err != nil {
		return err
	}
	if g.opts.NoExec {
		return skip("--no-exec")
	}
	if len(s.driverPods) == 0 {
		return skip("no driver pods")
	}

	var errs []error
	var withoutSMI []string
	collected := 0
	denied := false
	reachable := 0
	taken := make(map[string]bool)

	for _, driver := range s.driverPods {
		pod := driver.pod
		if ok, why := collectable(&pod); !ok {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Skipping device usage for pod %s: %s\n", pod.Name, why); lerr != nil {
				errs = append(errs, fmt.Errorf("error writing to stderr log file: %w", lerr))
			}
			continue
		}
		reachable++

		ctx, cancel, timeout := g.execContext()
		_, probeErr := g.execInPod(ctx, &pod, driver.container, nvidiaSMIProbe)
		cancel()
		if probeErr != nil {
			switch {
			case forbiddenExec(probeErr):
				denied = true
				errs = append(errs, execFailure(ctx, timeout, "device usage", &pod, probeErr))
			case errors.Is(probeErr, context.DeadlineExceeded):
				// A probe that ran out of time says nothing about whether the
				// container has nvidia-smi. Recording it as "no nvidia-smi"
				// would tell a reader the driver is not there.
				errs = append(errs, execFailure(ctx, timeout, "nvidia-smi probe", &pod, probeErr))
			default:
				withoutSMI = append(withoutSMI, pod.Name)
			}
			continue
		}

		// One file per node, as the script names them. A node running both a
		// driver pod and a vGPU manager pod gets a second file under the pod
		// name, rather than the second pass overwriting the first.
		fileName := fmt.Sprintf("gpu_device_usage_%s.log", pod.Spec.NodeName)
		if taken[fileName] {
			fileName = fmt.Sprintf("gpu_device_usage_%s_%s.log", pod.Spec.NodeName, pod.Name)
		}
		taken[fileName] = true

		if err := g.writeDeviceUsage(&driver, fileName); err != nil {
			errs = append(errs, err)
			continue
		}
		collected++
	}

	switch {
	case denied:
		s.execDenied = append(s.execDenied, "device-usage")
		return skip("forbidden: %v", errors.Join(errs...))
	case collected == 0 && len(withoutSMI) > 0 && len(errs) == 0:
		return skip("no nvidia-smi in %s", strings.Join(withoutSMI, ", "))
	case reachable == 0:
		return skip("no driver pod could be reached")
	case collected == 0 && len(errs) == 0:
		return skip("no driver pod could be reached")
	}
	return errors.Join(errs...)
}

// writeDeviceUsage runs the device usage commands in one driver pod and writes
// what they said to that node's file. A command that fails is recorded in the
// file rather than ending the section: the output of the ones that did run is
// still what the report is read for.
func (g *Gatherer) writeDeviceUsage(driver *driverTarget, fileName string) error {
	node := driver.pod.Spec.NodeName

	file, err := g.createArtifact(filepath.Join(g.opts.ArtifactDir, fileName))
	if err != nil {
		return g.logError(fmt.Sprintf("error creating %s: %%v", fileName), err)
	}
	committed := false
	defer func() {
		if !committed {
			file.discard()
		}
	}()

	for _, step := range deviceUsageCommands {
		if _, werr := fmt.Fprintf(file, "# %s output from %s\n", step.heading, node); werr != nil {
			return fmt.Errorf("writing %s: %w", fileName, werr)
		}
		ctx, cancel, timeout := g.execContext()
		out, cerr := g.execInPod(ctx, &driver.pod, driver.container, step.command)
		cancel()
		if cerr != nil {
			out = fmt.Sprintf("Failed to run %s on %s: %v\n",
				step.heading, node, execFailure(ctx, timeout, step.heading, &driver.pod, cerr))
		}
		if _, werr := fmt.Fprintf(file, "%s\n", strings.TrimRight(out, "\n")); werr != nil {
			return fmt.Errorf("writing %s: %w", fileName, werr)
		}
	}

	if cerr := file.commit(); cerr != nil {
		return g.logError(fmt.Sprintf("error writing %s: %%v", fileName), cerr)
	}
	committed = true
	return nil
}

// execInPod runs a command in one container of a pod, using the injected
// runner when a test set one.
func (g *Gatherer) execInPod(ctx context.Context, pod *v1.Pod, container, command string) (string, error) {
	if g.runInPod != nil {
		return g.runInPod(ctx, pod, container, command)
	}
	out, _, err := executeRemoteCommand(ctx, g.config, pod, container, command)
	return out, err
}

// discardArtifact takes back a file a failed collection left behind, so that a
// reader does not mistake a truncated or zero-length file for the answer. The
// file is only ever one this gather just created.
func (g *Gatherer) discardArtifact(path string) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		_, _ = fmt.Fprintf(g.errLogFile, "Error removing the partial artifact %s: %v\n", filepath.Base(path), err)
	}
}
