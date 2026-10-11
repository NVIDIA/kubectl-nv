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
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// sentinelUnhealthySuffix is what NVSentinel appends to a health check's name
// to build the node condition's reason when that check is failing. The
// condition's own type is the check name verbatim, so the pair of them is the
// signature nothing else writes
// (platform-connectors/pkg/connectors/kubernetes/process_node_events.go:193
// builds the type and :950 builds the reason, NVSentinel 93fa8222).
const sentinelUnhealthySuffix = "IsNotHealthy"

// NodeConditionOrCordon is NV-NODE-005: an NVSentinel health check is
// reporting the node unhealthy, or the GPU node is cordoned.
//
// NVSentinel writes one node condition per health check it runs, and the
// condition type is the check's own name: GPUXidError, GPURowRemap, whatever
// the deployment configured. There is no fixed list to match against and no
// prefix on the type, so the recogniser is the reason instead, which the
// platform connector builds as the check name plus IsNotHealthy when the check
// is failing and plus IsHealthy when it is not.
//
// That pairing is what makes the match discriminate. Matching every
// non-standard condition at True would fire on every node of every cluster
// running a log shipper, a storage operator or anything else that writes a
// condition of its own, and the rule would be noise within a week. Matching
// the reason alone would claim a condition whose reason happens to name
// another check. Both halves have to agree, and the status has to be True:
// NVSentinel writes the IsNotHealthy reason only alongside True, so a False
// one is a check that has recovered.
//
// A cordon is reported separately and at info. A cordoned GPU node is
// invisible to the scheduler, which is worth saying in a report about why no
// work is landing, but it is usually somebody doing maintenance on purpose:
// an error there would make `kubectl nv health` fail a pipeline every time an
// operator drained a node.
type NodeConditionOrCordon struct{ versionRange }

// NewNodeConditionOrCordon returns the rule. Both version bounds are empty:
// NVSentinel is not a GPU Operator component and a cordon is Kubernetes' own,
// so no operator version decides whether either means anything.
func NewNodeConditionOrCordon() *NodeConditionOrCordon { return &NodeConditionOrCordon{} }

// Code implements health.Rule.
func (r *NodeConditionOrCordon) Code() string { return "NV-NODE-005" }

// Severity implements health.Rule. An error is what the rule is listed as,
// for the failing health check; the cordon finding lowers itself to info.
func (r *NodeConditionOrCordon) Severity() output.Severity { return output.SeverityError }

// Evaluate implements health.Rule.
func (r *NodeConditionOrCordon) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	for _, node := range in.Nodes {
		for i := range node.Conditions {
			condition := &node.Conditions[i]
			if !isSentinelFailure(condition) {
				continue
			}

			evidence := map[string]string{
				"condition": string(condition.Type),
				"status":    string(condition.Status),
				"reason":    condition.Reason,
			}

			setIf(evidence, "message", condition.Message)

			if !condition.LastTransitionTime.IsZero() {
				evidence["since"] = condition.LastTransitionTime.UTC().Format(time.RFC3339)
			}

			findings = append(findings, output.Finding{
				Code:      r.Code(),
				Severity:  r.Severity(),
				Node:      node.Name,
				Component: ComponentNode,
				Message:   "an NVSentinel health check is reporting the node unhealthy",
				Evidence:  evidence,
				Next:      infoCommand(node.Name),
			})
		}

		if !node.Unschedulable {
			continue
		}

		findings = append(findings, output.Finding{
			Code:      r.Code(),
			Severity:  output.SeverityInfo,
			Node:      node.Name,
			Component: ComponentNode,
			Message:   "the GPU node is cordoned, so nothing new will be scheduled on it",
			Evidence:  map[string]string{"unschedulable": btoa(true)},
			Next:      infoCommand(node.Name),
		})
	}

	return findings
}

// isSentinelFailure reports whether a condition is NVSentinel saying one of
// its checks is failing. See the type comment for why all three halves are
// needed.
func isSentinelFailure(condition *corev1.NodeCondition) bool {
	return condition.Status == corev1.ConditionTrue &&
		condition.Reason == string(condition.Type)+sentinelUnhealthySuffix
}
