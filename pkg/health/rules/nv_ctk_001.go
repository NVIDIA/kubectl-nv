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

// ToolkitValidationFailed is NV-CTK-001: the validator's toolkit check exited
// non-zero on the node.
//
// The container toolkit is what makes a GPU reachable from inside a container.
// When its validation fails the node keeps advertising its GPUs and every pod
// placed on it fails to start, which is the worst shape a GPU node can be in:
// the scheduler still believes it.
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
// A container that has not terminated carries no exit code at all, and "has
// not finished" is not "failed": a validation still running, reported as a
// failure, is a finding that clears itself a second later and teaches a reader
// to ignore the rule.
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

	for _, node := range in.Nodes {
		validator, found := operandFor(node, inventory.ComponentOperatorValidator)
		if !found {
			continue
		}

		for _, result := range validator.InitContainers {
			if result.Name != initToolkitValidation || result.ExitCode == nil || *result.ExitCode == 0 {
				continue
			}

			evidence := map[string]string{
				inventory.DeployLabel(deployKeyContainerTKT): node.Deploy[deployKeyContainerTKT],
				"pod":           validator.Name,
				"initContainer": result.Name,
				"exitCode":      itoa(*result.ExitCode),
			}

			// A deploy label the node does not carry is left out rather than
			// written as an empty string, and so is a termination with no
			// reason recorded.
			if evidence[inventory.DeployLabel(deployKeyContainerTKT)] == "" {
				delete(evidence, inventory.DeployLabel(deployKeyContainerTKT))
			}

			setIf(evidence, "reason", result.Reason)

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
