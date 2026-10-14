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

package rules_test

import (
	"testing"
	"time"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// validatorNode is a worker carrying the validator pod with the init
// container results the case is about.
func validatorNode(name string, ready bool, results ...inventory.InitResult) inventory.GPUNode {
	return buildNode(name,
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withEveryOperandDeployed(),
		withCount(4),
		withGPUs(4, 4),
		withOperand(inventory.ComponentDevicePlugin, true),
		withValidatorInit(ready, results...),
	)
}

func ctkInput(node inventory.GPUNode) health.Input {
	return health.Input{Nodes: []inventory.GPUNode{node}, OperatorInstalled: true}
}

// ctkNow is the injected clock the crash loop cases are dated against.
var ctkNow = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

// Any non-zero code is a failure, not just the one the happy path produces.
// A rule comparing against 1 would pass a toolkit that could not find its
// runtime (127) or rejected its configuration (2).
func TestToolkitValidationFailedReportsEveryNonZeroExit(t *testing.T) {
	t.Parallel()

	for _, code := range []int32{1, 2, 127, 255} {
		t.Run("exit="+itoaForTest(code), func(t *testing.T) {
			t.Parallel()

			node := validatorNode("worker-1", false,
				initResult("toolkit-validation", exitCode(code), "Error"))

			finding := only(t, evaluate(rules.NewToolkitValidationFailed(), ctkInput(node)))
			if got := finding.Evidence["exitCode"]; got != itoaForTest(code) {
				t.Errorf("evidence exitCode is %q, want %q", got, itoaForTest(code))
			}
		})
	}
}

// The shape a failing validation actually has under a restart policy of
// Always: the init container is Waiting in CrashLoopBackOff, carries no
// current exit code at all, and the code of the run that failed is in the
// last termination. A rule reading only the current state saw a validator
// with nothing wrong with it.
func TestToolkitValidationFailedReportsACrashLoop(t *testing.T) {
	t.Parallel()

	rule := rules.NewToolkitValidationFailed()
	node := validatorNode("worker-1", false,
		crashLoopInit("toolkit-validation", 1, ctkNow.Add(-90*time.Second)))

	finding := only(t, evaluate(rule, ctkInput(node)))

	checkFinding(t, rule, finding, inventory.ComponentContainerToolkit,
		"the toolkit-validation init container of the validator pod exited non-zero",
		"kubectl nv info worker-1")

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/gpu.deploy.container-toolkit": "true",
		"pod":               "nvidia-operator-validator-abcde",
		"initContainer":     "toolkit-validation",
		"exitCode":          "1",
		"reason":            "CrashLoopBackOff",
		"lastTerminationAt": "2026-10-11T11:58:30Z",
	})
}

// A crash loop whose last run exited zero is not a failed validation: the
// container is being restarted for some other reason, and reporting the exit
// code would say the toolkit failed when it passed.
func TestToolkitValidationFailedStaysQuietOnACrashLoopThatExitedZero(t *testing.T) {
	t.Parallel()

	node := validatorNode("worker-1", false, crashLoopInit("toolkit-validation", 0, ctkNow))

	if findings := evaluate(rules.NewToolkitValidationFailed(), ctkInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A waiting reason that is not a crash loop is a container that has not run
// this time, so the previous run's exit code says nothing about it.
func TestToolkitValidationFailedStaysQuietOnAnotherWaitingReason(t *testing.T) {
	t.Parallel()

	result := crashLoopInit("toolkit-validation", 1, ctkNow)
	result.Reason = "PodInitializing"

	node := validatorNode("worker-1", false, result)

	if findings := evaluate(rules.NewToolkitValidationFailed(), ctkInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A node whose toolkit validation fails never gets a device plugin, so it
// advertises no nvidia.com/gpu and is hinted by the time there is anything to
// report. This is the case the rule exists for.
func TestToolkitValidationFailedReportsAHintedWorker(t *testing.T) {
	t.Parallel()

	rule := rules.NewToolkitValidationFailed()
	node := validatorNode("worker-1", false, initResult("toolkit-validation", exitCode(1), "Error"))
	node.Capacity, node.Allocatable, node.Mode = 0, 0, inventory.ModeNone
	hinted()(&node)

	in := health.Input{Hinted: []inventory.GPUNode{node}, OperatorInstalled: true, Now: ctkNow}
	finding := only(t, evaluate(rule, in))

	if finding.Node != "worker-1" {
		t.Errorf("the finding names %q, want worker-1", finding.Node)
	}
}

// The Mokka control plane runs no validator pod, so walking the hinted nodes
// does not put it back into a report.
func TestToolkitValidationFailedStaysQuietOnTheControlPlane(t *testing.T) {
	t.Parallel()

	in := health.Input{
		Hinted:            []inventory.GPUNode{mokkaControlPlane()},
		OperatorInstalled: true,
		Now:               ctkNow,
	}

	if findings := evaluate(rules.NewToolkitValidationFailed(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

func TestToolkitValidationFailedReportsANonZeroExit(t *testing.T) {
	t.Parallel()

	rule := rules.NewToolkitValidationFailed()
	node := validatorNode("worker-1", false,
		initResult("driver-validation", exitCode(0), "Completed"),
		initResult("toolkit-validation", exitCode(1), "Error"))

	finding := only(t, evaluate(rule, ctkInput(node)))

	checkFinding(t, rule, finding, inventory.ComponentContainerToolkit,
		"the toolkit-validation init container of the validator pod exited non-zero",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityError {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityError)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/gpu.deploy.container-toolkit": "true",
		"pod":           "nvidia-operator-validator-abcde",
		"initContainer": "toolkit-validation",
		"exitCode":      "1",
		"reason":        "Error",
	})
}

// A validator that passed is the ordinary state of every working node.
func TestToolkitValidationFailedStaysQuietOnAZeroExit(t *testing.T) {
	t.Parallel()

	node := validatorNode("worker-1", true, initResult("toolkit-validation", exitCode(0), "Completed"))

	if findings := evaluate(rules.NewToolkitValidationFailed(), ctkInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A container that has not terminated carries no exit code, and "has not
// finished" is not "failed": a validation still running reported as a failure
// is a finding that clears itself a second later.
func TestToolkitValidationFailedStaysQuietWhileTheContainerIsRunning(t *testing.T) {
	t.Parallel()

	node := validatorNode("worker-1", false, initResult("toolkit-validation", nil, "Running"))

	if findings := evaluate(rules.NewToolkitValidationFailed(), ctkInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// Another init container failing is another finding. Matching any failed init
// container would report a driver fault as a toolkit fault and send the
// reader to the wrong component.
func TestToolkitValidationFailedIgnoresAnotherInitContainer(t *testing.T) {
	t.Parallel()

	node := validatorNode("worker-1", false, initResult("driver-validation", exitCode(1), "Error"))

	if findings := evaluate(rules.NewToolkitValidationFailed(), ctkInput(node)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A node with no validator pod has nothing to read a verdict from. That the
// validator is missing is NV-OP-002's business.
func TestToolkitValidationFailedStaysQuietWithoutAValidatorPod(t *testing.T) {
	t.Parallel()

	if findings := evaluate(rules.NewToolkitValidationFailed(), healthyLane()); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

func TestToolkitValidationFailedStaysQuietWithoutTheOperator(t *testing.T) {
	t.Parallel()

	node := validatorNode("worker-1", false, initResult("toolkit-validation", exitCode(1), "Error"))
	in := health.Input{Nodes: []inventory.GPUNode{node}}

	if findings := evaluate(rules.NewToolkitValidationFailed(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

func TestToolkitValidationFailedAppliesToEveryOperatorVersion(t *testing.T) {
	t.Parallel()

	rule := rules.NewToolkitValidationFailed()
	if !rule.Applies("v25.10.0") || !rule.Applies("") {
		t.Error("the rule has to apply to every operator version and to an unknown one")
	}

	bounded := rules.NewToolkitValidationFailed()
	bounded.MinVersion = "v99.0.0"

	if bounded.Applies("v25.10.0") {
		t.Error("the version gate is not wired: a bounded rule still applies below its minimum")
	}
}
