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

// deployKeyDCGMExporter is the nvidia.com/gpu.deploy.<key> suffix the operator
// gates the DCGM exporter on.
const deployKeyDCGMExporter = "dcgm-exporter"

// DCGMExporterNotReady is NV-DCGM-001: a GPU node whose dcgm-exporter pod is
// missing or not ready where it should be running.
//
// "Where it should be running" is the whole difficulty, and it is what makes
// this rule stricter about firing than NV-DP-001 is. dcgm-exporter is
// optional: plenty of installs switch it off in the chart values, and this
// repository's own Mokka lane switches off the standalone host engine, the
// container toolkit, the mig-manager and the node status exporter that way. So
// the rule needs the node's own nvidia.com/gpu.deploy.dcgm-exporter label to
// say the operand belongs there before its absence is worth reporting. The
// device plugin needs no such evidence, because it is what makes a GPU node a
// GPU node.
//
// The cost of the stricter test is a cluster whose operator writes no deploy
// label for an exporter it does run: there, a dead exporter goes unreported.
// The alternative is a warning on every cluster that chose not to run one, and
// a check that fires on a healthy cluster is a check nobody reads.
type DCGMExporterNotReady struct{ versionRange }

// NewDCGMExporterNotReady returns the rule. Both version bounds are empty: the
// dcgm-exporter operand and its deploy label predate every operator version
// this plugin supports.
func NewDCGMExporterNotReady() *DCGMExporterNotReady { return &DCGMExporterNotReady{} }

// Code implements health.Rule.
func (r *DCGMExporterNotReady) Code() string { return "NV-DCGM-001" }

// Severity implements health.Rule. A warning rather than an error: a node with
// no exporter still runs GPU work, it just runs it unobserved.
func (r *DCGMExporterNotReady) Severity() output.Severity { return output.SeverityWarning }

// Evaluate implements health.Rule.
func (r *DCGMExporterNotReady) Evaluate(in health.Input) []output.Finding {
	if !in.OperatorInstalled || !OperandEnabledInPolicy(in, PolicyFieldDCGMExporter) {
		return nil
	}

	var findings []output.Finding

	for _, node := range in.Nodes {
		if operandDisabledOn(node, deployKeyDCGMExporter) {
			continue
		}

		pod, found := operandFor(node, inventory.ComponentDCGMExporter)
		if found && pod.Ready {
			continue
		}

		message := "the dcgm-exporter pod on the node is not ready"
		if !found {
			message = "the node runs no dcgm-exporter pod and the ClusterPolicy enables it"
		}

		findings = append(findings, output.Finding{
			Code:      r.Code(),
			Severity:  r.Severity(),
			Node:      node.Name,
			Component: inventory.ComponentDCGMExporter,
			Message:   message,
			Evidence:  operandEvidence(node, deployKeyDCGMExporter, pod, found),
			Next:      infoCommand(node.Name),
		})
	}

	return findings
}
