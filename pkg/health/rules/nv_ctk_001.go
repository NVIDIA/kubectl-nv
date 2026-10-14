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

package rules

import (
	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// initToolkitValidation is the validator's init container that checks the
// container toolkit. The operator's validator runs one init container per
// thing it validates and carries each verdict in that container's exit code
// rather than in a log line.
const initToolkitValidation = "toolkit-validation"

// reasonCrashLoop is the waiting reason the kubelet gives a container it is
// backing off from restarting. Under a pod restart policy of Always, which is
// what the operator's validator runs with, this is the state a failing init
// container actually sits in: the current state carries no exit code at all
// and the one that matters is in the last termination.
const reasonCrashLoop = "CrashLoopBackOff"

// ToolkitValidationFailed is NV-CTK-001: the validator's toolkit check exited
// non-zero on the node.
//
// The container toolkit is what makes a GPU reachable from inside a container.
// When its validation fails the device plugin never starts, so the node stops
// advertising nvidia.com/gpu and drops out of the GPU inventory into the
// hinted one: a node that was running work a minute ago and now looks, to
// anything reading capacity alone, like a node that never had GPUs at all.
//
// The finding names the toolkit rather than the validator. The validator is
// the messenger, and a reader sent to nvidia-operator-validator spends the
// first ten minutes finding out that the pod it names is working perfectly;
// the validator pod is in the evidence instead.
//
// Only this init container is matched. Every other one is a different
// component's verdict, and reporting a failed driver validation as a toolkit
// fault sends a reader to the wrong place with the right level of confidence.
//
// A container that is running carries no exit code at all, and "has not
// finished" is not "failed": a validation still running, reported as a
// failure, is a finding that clears itself a second later and teaches a reader
// to ignore the rule.
//
// A container that is crash looping is a different thing again, and it is the
// shape this rule meets most often. Under a pod restart policy of Always the
// validator does not sit terminated: the init container is Waiting with reason
// CrashLoopBackOff and the exit code of the run that failed is in
// LastTerminationState. A rule reading only the current state saw a validator
// with nothing wrong with it, which is why pkg/inventory carries the last
// termination beside the current one.
//
// Both the GPU nodes and the hinted ones are walked: a node whose toolkit
// validation fails never gets a device plugin, so it advertises no
// nvidia.com/gpu and is hinted by the time there is anything to report. See
// everyNode.
type ToolkitValidationFailed struct{ versionRange }

// NewToolkitValidationFailed returns the rule. Both version bounds are empty:
// the validator and its toolkit-validation init container predate every
// operator version this plugin supports.
func NewToolkitValidationFailed() *ToolkitValidationFailed { return &ToolkitValidationFailed{} }

// Code implements health.Rule.
func (r *ToolkitValidationFailed) Code() string { return "NV-CTK-001" }

// Severity implements health.Rule. An error: the node takes GPU work it
// cannot run.
func (r *ToolkitValidationFailed) Severity() output.Severity { return output.SeverityError }

// Evaluate implements health.Rule.
func (r *ToolkitValidationFailed) Evaluate(in health.Input) []output.Finding {
	if !in.OperatorInstalled {
		return nil
	}

	var findings []output.Finding

	for _, node := range everyNode(in) {
		validator, found := operandFor(node, inventory.ComponentOperatorValidator)
		if !found {
			continue
		}

		for _, result := range validator.InitContainers {
			if result.Name != initToolkitValidation {
				continue
			}

			code, failed := toolkitFailure(result)
			if !failed {
				continue
			}

			evidence := map[string]string{
				"pod":           validator.Name,
				"initContainer": result.Name,
				"exitCode":      itoa(code),
			}

			setIf(evidence, inventory.DeployLabel(deployKeyContainerTKT), node.Deploy[deployKeyContainerTKT])
			setIf(evidence, "reason", result.Reason)

			// Only when the verdict came from the last termination rather than
			// the current state, which is also what says the container is
			// still failing now rather than having failed once.
			if result.ExitCode == nil {
				setIf(evidence, "lastTerminationAt", result.LastTermination.FinishedAt)
			}

			findings = append(findings, output.Finding{
				Code:      r.Code(),
				Severity:  r.Severity(),
				Node:      node.Name,
				Component: inventory.ComponentContainerToolkit,
				Message:   "the " + initToolkitValidation + " init container of the validator pod exited non-zero",
				Evidence:  evidence,
				Next:      infoCommand(node.Name),
			})
		}
	}

	return findings
}

// toolkitFailure returns the exit code the validation failed with, and whether
// it failed at all.
//
// Two places carry one, and which is filled depends only on the pod's restart
// policy. A container that has terminated has its code in the current state; a
// container the kubelet is backing off from restarting is Waiting, and the
// code of the run that failed is the last termination's.
//
// A last termination is only read under CrashLoopBackOff. Any other waiting
// reason is a container that has not got as far as running this time, and the
// previous run's exit code says nothing about it.
func toolkitFailure(result inventory.InitResult) (int32, bool) {
	if result.ExitCode != nil {
		return *result.ExitCode, *result.ExitCode != 0
	}

	if result.Reason != reasonCrashLoop || !result.LastTermination.Observed {
		return 0, false
	}

	return result.LastTermination.ExitCode, result.LastTermination.ExitCode != 0
}
