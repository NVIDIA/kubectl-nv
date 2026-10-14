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
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// The driver upgrade states this rule is about, verbatim from
// k8s-operator-libs pkg/upgrade/consts.go.
const (
	// upgradeStateFailed is terminal: the operator tried and could not.
	upgradeStateFailed = "upgrade-failed"
	// upgradeStateDone is the other terminal state, and the one every node of
	// a cluster that is not upgrading sits in.
	upgradeStateDone = "upgrade-done"
	// The three transitional states a node can sit in indefinitely when
	// whatever the operator is waiting for never happens: a cordon that
	// cannot be taken, a pod that will not go away, a validation that never
	// passes.
	upgradeStateCordonRequired     = "cordon-required"
	upgradeStatePodRestartRequired = "pod-restart-required"
	upgradeStateValidationRequired = "validation-required"
)

// upgradeStuckStates are the transitional states, in the order the operator
// walks them.
var upgradeStuckStates = []string{
	upgradeStateCordonRequired,
	upgradeStatePodRestartRequired,
	upgradeStateValidationRequired,
}

// upgradeEventReason is what the GPU Operator's upgrade controller records its
// node events under: strings.ToUpper(DriverName) + "DriverUpgrade", and the
// driver name behind nvidia.com/gpu-driver-upgrade-state is "gpu"
// (k8s-operator-libs pkg/upgrade/util.go:158 and consts.go:23).
const upgradeEventReason = "GPUDriverUpgrade"

// upgradeEventPrefix is the start of the message that announces a state. The
// controller formats it as "Successfully updated node state label to %s", so
// the state follows verbatim (node_upgrade_state_provider.go:129).
const upgradeEventPrefix = "Successfully updated node state label to "

// UpgradeStuck is NV-UPG-001: a node that a driver upgrade failed on, or that
// has been sitting in one step of an upgrade for too long.
//
// The difficulty is that nvidia.com/gpu-driver-upgrade-state carries no
// transition time. A node that entered cordon-required a minute ago and one
// that entered it last Tuesday carry exactly the same label, and an upgrade in
// progress is not a fault. So the duration is read from the event the upgrade
// controller recorded when it set the state, and a node with no such event is
// reported at info rather than given a duration nobody measured.
//
// Three severities, which is one per quality of evidence:
//
//	upgrade-failed              error    the operator says it failed
//	stuck, with an event        warning  measured against --upgrade-stuck-after
//	stuck, with no event        info     the state is there, the duration is not
//
// The transitional states are only three of the twelve the controller names
// (k8s-operator-libs pkg/upgrade/consts.go). The rest are either terminal
// (upgrade-done, upgrade-failed) or steps the controller drives itself within
// seconds; reporting those would be reporting every node of every cluster
// mid-upgrade.
//
// One consequence of reading the duration off an event is worth knowing
// about: the API server expires events after an hour by default, so a node
// that has been wedged for longer than that has no event left to be dated
// from and drops to info. That is the honest answer rather than a wrong
// duration, and the state itself is still reported.
//
// Both the GPU nodes and the hinted ones are walked. An upgrade takes the
// driver down, so a node part way through one often advertises no
// nvidia.com/gpu and is hinted exactly while this rule has something to say
// about it. See everyNode.
type UpgradeStuck struct {
	versionRange
	// after is how long a transitional state has to have lasted before it
	// counts as stuck. It comes from --upgrade-stuck-after.
	after time.Duration
}

// DefaultUpgradeStuckAfter is the window --upgrade-stuck-after defaults to.
// Half an hour is longer than any step of a healthy upgrade on hardware and
// short enough that a node wedged overnight is reported in the morning.
const DefaultUpgradeStuckAfter = 30 * time.Minute

// NewUpgradeStuck returns the rule with the given window. A zero or negative
// window is a caller that did not set one, and reads as the default: a window
// of zero would report every node the instant it entered a state.
func NewUpgradeStuck(after time.Duration) *UpgradeStuck {
	if after <= 0 {
		after = DefaultUpgradeStuckAfter
	}

	return &UpgradeStuck{after: after}
}

// Code implements health.Rule.
func (r *UpgradeStuck) Code() string { return "NV-UPG-001" }

// Severity implements health.Rule. A warning is what the rule is listed as:
// the stuck states are the common case, and a failed upgrade raises its own
// finding to an error.
func (r *UpgradeStuck) Severity() output.Severity { return output.SeverityWarning }

// Evaluate implements health.Rule.
func (r *UpgradeStuck) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	for _, node := range everyNode(in) {
		finding, found := r.evaluateNode(in, node)
		if found {
			findings = append(findings, finding)
		}
	}

	return findings
}

// evaluateNode is one node's verdict.
func (r *UpgradeStuck) evaluateNode(in health.Input, node inventory.GPUNode) (output.Finding, bool) {
	state := node.UpgradeState

	evidence := map[string]string{
		inventory.LabelUpgradeState: state,
		"upgradeStuckAfter":         r.after.String(),
	}

	finding := output.Finding{
		Code:      r.Code(),
		Node:      node.Name,
		Component: ComponentNode,
		Evidence:  evidence,
		Next:      infoCommand(node.Name),
	}

	if state == upgradeStateFailed {
		finding.Severity = output.SeverityError
		finding.Message = "the node's driver upgrade failed"

		return finding, true
	}

	if !isStuckState(state) {
		return output.Finding{}, false
	}

	since, dated := enteredState(in, node.Name, state)
	if !dated {
		finding.Severity = output.SeverityInfo
		finding.Message = "the node is in driver upgrade state " + state + " and no event says since when"

		return finding, true
	}

	elapsed := in.Now.Sub(since)
	if elapsed < r.after {
		return output.Finding{}, false
	}

	evidence["since"] = since.UTC().Format(time.RFC3339)
	evidence["for"] = elapsed.Truncate(time.Second).String()

	finding.Severity = output.SeverityWarning
	finding.Message = "the node has been in driver upgrade state " + state +
		" for longer than --upgrade-stuck-after"

	return finding, true
}

// isStuckState reports whether a state is one a node can sit in indefinitely.
func isStuckState(state string) bool {
	for _, stuck := range upgradeStuckStates {
		if state == stuck {
			return true
		}
	}

	return false
}

// enteredState returns when the upgrade controller last announced this state
// for this node.
//
// Three things have to match, and each one of them has silenced a wrong
// answer: the event's involved object is this node, its reason is the upgrade
// controller's, and its message announces this state. An event about another
// node would date the state from somebody else's upgrade; an event from
// another controller would date it from the kubelet's last heartbeat; and the
// event announcing the state the node passed through on its way here is older
// than the one that matters.
//
// The newest wins, because a node that re-entered a state two minutes ago has
// been there two minutes whatever it did this morning.
func enteredState(in health.Input, node, state string) (time.Time, bool) {
	var newest time.Time

	for i := range in.Events {
		event := &in.Events[i]
		if event.InvolvedObject.Name != node || event.Reason != upgradeEventReason {
			continue
		}

		if !strings.HasSuffix(event.Message, upgradeEventPrefix+state) {
			continue
		}

		if when := eventTime(event); when.After(newest) {
			newest = when
		}
	}

	return newest, !newest.IsZero()
}

// eventTime is when an event last happened.
//
// Three fields can carry it and which one is filled depends on who recorded
// it: the events/v1 recorders write EventTime and leave LastTimestamp empty,
// the core/v1 ones do the opposite, and a brand new event has only its
// FirstTimestamp. Reading one field alone would silently date half the
// clusters in the field from the zero time.
func eventTime(event *corev1.Event) time.Time {
	switch {
	case !event.LastTimestamp.IsZero():
		return event.LastTimestamp.Time
	case !event.EventTime.IsZero():
		return event.EventTime.Time
	default:
		return event.FirstTimestamp.Time
	}
}
