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

// The nvidia.com/mig.config.state values. Only one of them means the
// mig-manager finished.
const (
	migStateSuccess = "success"
	migStateFailed  = "failed"
)

// The MIG strategies, which decide what a node advertises and so what a
// healthy node of each kind looks like.
const (
	// migStrategyNone leaves MIG alone: the node advertises whole GPUs, and
	// MIG-capable hardware under it is hardware nobody has partitioned.
	migStrategyNone = "none"
	// migStrategySingle publishes every partition as nvidia.com/gpu, so a
	// healthy single node advertises no nvidia.com/mig-* resource at all.
	migStrategySingle = "single"
	// migStrategyMixed publishes one nvidia.com/mig-<profile> resource per
	// partition and no nvidia.com/gpu.
	migStrategyMixed = "mixed"
)

// migConfigDisabled is the config that asks for no partitions. A mixed node
// under it advertising no nvidia.com/mig-* is doing exactly what it was told,
// and a rule that did not know this name would report every cluster that
// switched MIG off through the label rather than through the strategy.
const migConfigDisabled = "all-disabled"

// MIGConfigNotApplied is NV-MIG-001: the MIG configuration a node was asked
// for has not been applied, or its strategy cannot be serving what it
// advertises.
//
// Two different faults under one code, because a reader looking at a node
// whose MIG is wrong does not yet know which of the two it is:
//
//  1. nvidia.com/mig.config names a profile and nvidia.com/mig.config.state is
//     not "success". failed is an error; pending, rebooting, an absent state
//     and anything else are warnings, because those are states a node passes
//     through and the reader is being told to look rather than that the node
//     is lost.
//
//  2. The strategy and the node's extended resources disagree. This is the
//     half that is easy to get backwards, so it is written out:
//
//     single  publishes every partition as nvidia.com/gpu. A healthy single
//     node therefore has no nvidia.com/mig-* capacity at all, and one
//     that has it is advertising names its own strategy does not serve.
//     mixed   publishes nvidia.com/mig-<profile>, one per partition. A mixed
//     node that applied a partitioning config and advertises none of
//     them is offering nothing a MIG workload can ask for.
//     none    does not partition. MIG-capable hardware under it is the Mokka
//     main lane, gb300 with nvidia.com/mig.capable=true and strategy
//     none, and it is healthy.
//
// The second fault is only asked about MIG-capable hardware and only when a
// config was applied: a mixed node nobody has configured has nothing to
// disagree with, and hardware that cannot do MIG cannot be misconfigured for
// it.
type MIGConfigNotApplied struct{ versionRange }

// NewMIGConfigNotApplied returns the rule. Both version bounds are empty: the
// mig.config labels and the three strategies predate every operator version
// this plugin supports.
func NewMIGConfigNotApplied() *MIGConfigNotApplied { return &MIGConfigNotApplied{} }

// Code implements health.Rule.
func (r *MIGConfigNotApplied) Code() string { return "NV-MIG-001" }

// Severity implements health.Rule. A warning is what the rule is listed as; a
// failed config raises its own finding to an error.
func (r *MIGConfigNotApplied) Severity() output.Severity { return output.SeverityWarning }

// Evaluate implements health.Rule.
func (r *MIGConfigNotApplied) Evaluate(in health.Input) []output.Finding {
	var findings []output.Finding

	tolerations := PolicyTolerations(in)

	for _, node := range in.Nodes {
		if isVirtualMachineWorkload(node) || !OperatorTargets(node, tolerations) {
			continue
		}

		if finding, found := r.configFinding(node); found {
			findings = append(findings, finding)

			// A node whose config has not been applied is a node mid-change,
			// and its resources are allowed to disagree with its strategy
			// while that is true. Reporting both would be reporting the same
			// fault twice.
			continue
		}

		if finding, found := r.strategyFinding(node); found {
			findings = append(findings, finding)
		}
	}

	return findings
}

// configFinding is the first fault: a config that has not been applied.
func (r *MIGConfigNotApplied) configFinding(node inventory.GPUNode) (output.Finding, bool) {
	if node.MIG.Config == "" || node.MIG.ConfigState == migStateSuccess {
		return output.Finding{}, false
	}

	severity := r.Severity()
	if node.MIG.ConfigState == migStateFailed {
		severity = output.SeverityError
	}

	return output.Finding{
		Code:      r.Code(),
		Severity:  severity,
		Node:      node.Name,
		Component: inventory.ComponentMIGManager,
		Message:   "the MIG configuration the node was asked for has not been applied",
		Evidence:  migEvidence(node, false),
		Next:      infoCommand(node.Name),
	}, true
}

// strategyFinding is the second: a strategy that cannot be serving what the
// node advertises.
func (r *MIGConfigNotApplied) strategyFinding(node inventory.GPUNode) (output.Finding, bool) {
	if !node.MIG.Capable {
		return output.Finding{}, false
	}

	advertisesMIG := isMixedMIG(node)

	switch node.MIG.Strategy {
	case migStrategySingle:
		if !advertisesMIG {
			return output.Finding{}, false
		}
	case migStrategyMixed:
		if advertisesMIG || node.MIG.Config == "" || node.MIG.Config == migConfigDisabled {
			return output.Finding{}, false
		}
	case migStrategyNone:
		// The Mokka main lane: gb300 with nvidia.com/mig.capable=true and
		// strategy none. Nothing was asked for and nothing is wrong.
		return output.Finding{}, false
	default:
		// An empty strategy, and anything this code has not seen. A strategy
		// nothing here names is not a claim about what the node advertises,
		// and guessing would report a working node as broken.
		return output.Finding{}, false
	}

	return output.Finding{
		Code:      r.Code(),
		Severity:  r.Severity(),
		Node:      node.Name,
		Component: ComponentNode,
		Message:   "the node's MIG strategy and the resources it advertises disagree",
		Evidence:  migEvidence(node, true),
		Next:      infoCommand(node.Name),
	}, true
}

// migEvidence is the MIG labels, and for the strategy fault the resources they
// are being compared against.
func migEvidence(node inventory.GPUNode, withResources bool) map[string]string {
	evidence := map[string]string{
		inventory.LabelMIGCapable: btoa(node.MIG.Capable),
	}

	setIf(evidence, inventory.LabelMIGStrategy, node.MIG.Strategy)
	setIf(evidence, inventory.LabelMIGConfig, node.MIG.Config)
	setIf(evidence, inventory.LabelMIGConfigState, node.MIG.ConfigState)

	if withResources {
		setIf(evidence, "migResources", migResourceNames(node))
		evidence["capacity"] = itoa(node.Capacity)
	}

	return evidence
}

// migResourceNames is the nvidia.com/mig-* resources the node advertises,
// sorted and comma joined, which is the evidence encoding for a list. It is
// empty when there are none, and setIf then leaves the key out.
func migResourceNames(node inventory.GPUNode) string {
	names := make([]string, 0, len(node.MIG.Resources))
	for name := range node.MIG.Resources {
		names = append(names, name)
	}

	sort.Strings(names)

	return strings.Join(names, ",")
}
