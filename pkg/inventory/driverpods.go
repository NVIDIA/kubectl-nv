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

package inventory

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// timeFormat is how every time in this package reaches the wire: RFC3339 in
// UTC, which is the encoding pkg/output fixes for a finding's evidence, so a
// rule reporting one passes it through unchanged.
const timeFormat = time.RFC3339

// labelDriverToolkit is the label OpenShift's driver toolkit marks a driver
// DaemonSet with. It is matched on the key alone, which is what a selector of
// a bare key means to the API server, because the value is "true" today and
// the key's presence is the claim.
const labelDriverToolkit = "openshift.driver-toolkit"

// componentLabelDriver is the app.kubernetes.io/component value the GPU
// Operator gives every driver DaemonSet it manages, whichever way it was
// asked for (manifests/state-driver/0500_daemonset.yaml).
const componentLabelDriver = "nvidia-driver"

// driverSelector is one way a driver pod identifies itself.
//
// value empty means the key alone, which the API server reads as "this label
// is set, whatever it says".
type driverSelector struct {
	key   string
	value string
	// component is what a pod matching this selector is reported as: one of
	// the two driver operands, never the pod's own label value.
	component string
}

// driverSelectors are the four selectors a driver pod can be found through,
// in the order pkg/mustgather walks them. Each one is a different way of
// installing the driver, and dropping any one of them loses the bug report of
// every cluster installed that way, and here also loses NV-DRV-001 on it.
//
// Three are the ones the gpu-operator script uses, at lines 337 to 339 of
// hack/must-gather.sh. The fourth, app=nvidia-driver-daemonset, is
// kubectl-nv's own: the script does not look under it, and this plugin always
// has.
//
// This is the single list. pkg/mustgather reads it through DriverSelectors
// and pkg/health/rules matches on the component this assigns, so a selector
// added here reaches both at once.
var driverSelectors = []driverSelector{
	{key: appLabel, value: ComponentDriver, component: ComponentDriver},
	{key: componentLabel, value: componentLabelDriver, component: ComponentDriver},
	{key: appLabel, value: ComponentVGPUManager, component: ComponentVGPUManager},
	{key: labelDriverToolkit, component: ComponentDriver},
}

// DriverSelectors returns the label selectors a driver pod is found through,
// in "key=value" form, or "key" for the one matched on presence. The slice is
// fresh, so a caller may sort or truncate it.
func DriverSelectors() []string {
	out := make([]string, 0, len(driverSelectors))

	for _, selector := range driverSelectors {
		if selector.value == "" {
			out = append(out, selector.key)

			continue
		}

		out = append(out, selector.key+"="+selector.value)
	}

	return out
}

// DriverComponent reports which driver operand a pod's labels name, and
// whether they name one at all.
//
// The classification cannot be done on the "app" label's value, which is the
// trap this function exists for. The GPU Operator names each DaemonSet the
// NVIDIADriver CR produces nvidia-<driverType>-driver-<osTag>-<hash> and puts
// that name in the pod's "app" label
// (internal/state/driver.go getDriverAppName, and
// manifests/state-driver/0500_daemonset.yaml, which sets app to .Driver.AppName),
// and OpenShift's driver toolkit pods carry a per-RHCOS name of their own.
// Neither is ever the literal "nvidia-driver-daemonset", so comparing the
// selector values against the app label classified every driver pod on a
// modern cluster as an operand nothing recognised, and NV-DRV-001 could never
// fire on one.
//
// The selectors are tried in order and the first match wins. A vGPU manager
// pod carries neither of the two driver spellings, so it reaches its own
// selector; a pod carrying the OpenShift toolkit label and nothing else this
// knows is a driver, which is what that label means.
//
// What this deliberately does not claim: the vGPU host manager DaemonSet the
// NVIDIADriver CR produces, which carries
// app.kubernetes.io/component=nvidia-vgpu-host-manager and a generated app
// name. No selector in pkg/mustgather's list matches it either, so adding one
// here would change what a bug report collects, which is a decision for the
// collector rather than for a health rule.
func DriverComponent(labels map[string]string) (string, bool) {
	for _, selector := range driverSelectors {
		value, present := labels[selector.key]
		if !present {
			continue
		}

		// An empty selector value matches any value, including an empty one:
		// the key's presence is the whole claim.
		if selector.value == "" || selector.value == value {
			return selector.component, true
		}
	}

	return "", false
}

// Termination is what a container's last exit looked like.
//
// It is a value rather than a pointer because nothing in this package is ever
// null on the wire. Observed is what says whether there was an exit to read:
// without it a zero value would be indistinguishable from a container that
// exited 0, which is the difference between "it has not run" and "it passed".
type Termination struct {
	// Observed is whether the container status carried a termination at all.
	// Every other field means nothing when it is false.
	Observed bool `json:"observed" yaml:"observed"`
	// ExitCode is the code it exited with.
	ExitCode int32 `json:"exitCode" yaml:"exitCode"`
	// Reason is the termination reason, for instance "Error" or "OOMKilled".
	Reason string `json:"reason" yaml:"reason"`
	// FinishedAt is when it exited, RFC3339 in UTC, empty when the status
	// carried no finish time.
	//
	// A string rather than a metav1.Time because a zero metav1.Time marshals
	// as null, and never-null is the guarantee this package's consumers were
	// given first. It is the same encoding a finding's evidence uses, so a
	// rule reporting it passes it through, and a rule measuring a window
	// against it parses it once.
	FinishedAt string `json:"finishedAt" yaml:"finishedAt"`
}

// terminationFrom reads a container state's termination, if it has one.
func terminationFrom(state corev1.ContainerState) Termination {
	if state.Terminated == nil {
		return Termination{}
	}

	out := Termination{
		Observed: true,
		ExitCode: state.Terminated.ExitCode,
		Reason:   state.Terminated.Reason,
	}

	if !state.Terminated.FinishedAt.IsZero() {
		out.FinishedAt = state.Terminated.FinishedAt.UTC().Format(timeFormat)
	}

	return out
}

// lastTermination is the most recent exit of a container, whether it is the
// state it is in now or the one it was in before the restart it is waiting
// out.
//
// Both have to be read. A container in CrashLoopBackOff has a Waiting current
// state and carries the exit code of the run that failed in
// LastTerminationState, so a reader of the current state alone sees a pod with
// nothing wrong with it.
func lastTermination(status *corev1.ContainerStatus) Termination {
	current := terminationFrom(status.State)
	previous := terminationFrom(status.LastTerminationState)

	switch {
	case !current.Observed:
		return previous
	case !previous.Observed:
		return current
	case previous.FinishedAt > current.FinishedAt:
		// RFC3339 in UTC sorts lexically in time order, which is what makes
		// this comparison safe on strings. An empty finish time sorts first,
		// so a termination that carries one wins over one that does not.
		return previous
	default:
		return current
	}
}

// newestTermination is the most recent exit across a pod's containers, which
// is what dates a pod's last restart.
func newestTermination(statuses []corev1.ContainerStatus) Termination {
	var newest Termination

	for i := range statuses {
		candidate := lastTermination(&statuses[i])
		if !candidate.Observed {
			continue
		}

		if !newest.Observed || candidate.FinishedAt > newest.FinishedAt {
			newest = candidate
		}
	}

	return newest
}
