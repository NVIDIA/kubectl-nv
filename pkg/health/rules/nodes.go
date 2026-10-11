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

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
)

// everyNode returns the GPU nodes followed by the hinted ones.
//
// The rules about a broken operand have to walk both, and getting this wrong
// made four of them unable to fire on the fault they exist for. A node whose
// driver or container toolkit is broken does not stay a GPU node: the device
// plugin's init container waits on toolkit-ready and never starts, so the
// plugin never advertises nvidia.com/gpu, and the kubelet zeroes the capacity
// of a resource whose plugin has gone. The node keeps its hint labels and
// drops out of Input.Nodes into Input.Hinted, which is to say that by the time
// NV-DRV-001 or NV-CTK-001 has something to report, the node is no longer one
// they were looking at.
//
// The same holds for MIG: a node the mig-manager is reconfiguring advertises
// its profiles at zero for a while, which is not membership either.
//
// This does not reopen the control plane problem the operator-targets gate was
// built for. The rules that walk both match on an operand pod or an operator
// written label, and a control plane carrying GPU hint labels has no driver
// pod, no validator pod and no upgrade state label: there is nothing on it for
// any of them to match.
//
// The slice is fresh, so a caller may reorder it. The nodes are not copied,
// which is safe because a rule must not mutate its input.
func everyNode(in health.Input) []inventory.GPUNode {
	out := make([]inventory.GPUNode, 0, len(in.Nodes)+len(in.Hinted))
	out = append(out, in.Nodes...)
	out = append(out, in.Hinted...)

	return out
}

// midUpgrade reports whether the GPU Operator's upgrade controller is in the
// middle of replacing the node's driver.
//
// An empty label is a node no upgrade has touched, and upgrade-done is one the
// controller has finished with. Everything between them is the controller
// deliberately taking the driver down and putting it back, which is why a
// driver pod that is not ready during one is information rather than a fault:
// reporting an error would make every rolling driver upgrade in the fleet look
// like an outage, and NV-UPG-001 is the rule that says whether the upgrade
// itself is in trouble.
func midUpgrade(node inventory.GPUNode) bool {
	return node.UpgradeState != "" && node.UpgradeState != upgradeStateDone
}

// insideWindow reports whether a termination happened within d of now.
//
// A termination with no finish time recorded is not inside any window. That is
// the conservative answer: the alternative is reading an absent time as the
// zero time, which is inside no window either, or as now, which would report
// every pod that ever restarted.
func insideWindow(termination inventory.Termination, now time.Time, d time.Duration) (time.Time, bool) {
	if !termination.Observed || termination.FinishedAt == "" {
		return time.Time{}, false
	}

	when, err := time.Parse(time.RFC3339, termination.FinishedAt)
	if err != nil {
		return time.Time{}, false
	}

	return when, now.Sub(when) < d
}
