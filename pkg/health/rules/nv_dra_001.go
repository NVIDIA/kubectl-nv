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
	"sort"
	"strings"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// DevicePluginAndDRABoth is NV-DRA-001: the device plugin and the DRA driver
// both advertise the node's GPUs.
//
// The two advertisements are independent. The device plugin puts
// nvidia.com/gpu into the node's allocatable and the kubelet schedules against
// it; the DRA driver publishes a gpu.nvidia.com ResourceSlice and the
// scheduler allocates claims against that. Neither knows about the other, so a
// node in both modes can hand the same physical device to a pod holding a
// claim and to a pod holding an extended resource at the same time, and the
// second one to start gets a device already in use.
//
// It is a warning rather than an error because it is also exactly what a
// migration looks like mid-flight, and a cluster moving from the device plugin
// to DRA passes through it on purpose, node by node. What the finding says is
// that the node has not finished moving, not that it is broken.
type DevicePluginAndDRABoth struct{ versionRange }

// NewDevicePluginAndDRABoth returns the rule. Both version bounds are empty:
// the NVIDIA DRA driver installs from a chart of its own against any operator
// version, so a node can be in both modes whatever the operator is, and a
// floor read off the operator would silence the rule on exactly the clusters
// that installed the driver separately.
func NewDevicePluginAndDRABoth() *DevicePluginAndDRABoth { return &DevicePluginAndDRABoth{} }

// Code implements health.Rule.
func (r *DevicePluginAndDRABoth) Code() string { return "NV-DRA-001" }

// Severity implements health.Rule.
//
// Informational until the Phase 2 false-positive baseline. The finding is
// real, and nothing yet says how often a healthy fleet produces one; a rule
// that fails a pipeline before anybody has measured that is a rule operators
// switch off rather than read. The severity is raised once the baseline says
// what a clean cluster looks like.
func (r *DevicePluginAndDRABoth) Severity() output.Severity { return output.SeverityInfo }

// Evaluate implements health.Rule.
func (r *DevicePluginAndDRABoth) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	for _, node := range in.Nodes {
		if node.Mode != inventory.ModeBoth {
			continue
		}

		evidence := map[string]string{
			"mode":        node.Mode,
			"capacity":    itoa(node.Capacity),
			"allocatable": itoa(node.Allocatable),
		}

		setIf(evidence, "resourceSlices", sortedList(node.ResourceSlices))

		findings = append(findings, output.Finding{
			Code:      r.Code(),
			Severity:  r.Severity(),
			Node:      node.Name,
			Component: ComponentNode,
			Message: "the node advertises its GPUs through both the device plugin and " +
				"dynamic resource allocation",
			Evidence: evidence,
			Next:     infoCommand(node.Name),
		})
	}

	return findings
}

// sortedList is the evidence encoding for a list: sorted, comma joined, no
// spaces. The input is copied before sorting, because a rule must not mutate
// its input and the slice belongs to the node.
func sortedList(values []string) string {
	out := make([]string, len(values))
	copy(out, values)

	sort.Strings(out)

	return strings.Join(out, ",")
}
