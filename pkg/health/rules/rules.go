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

// Package rules holds the health checks themselves, one file per rule.
//
// Every rule is a pure function of a health.Input: it reads no cluster, no
// clock and no file, which is what lets a cluster's worth of findings be
// reproduced from a table in a test. Each one declares the GPU Operator
// version range it means anything over and the severity it reports at, and
// every finding it emits carries the evidence it was concluded from and the
// command to run next.
//
// Rules are collected by All rather than registered from an init function, so
// that a caller says which checks ran and a test can run one rule on its own.
package rules

import (
	"sort"
	"strconv"
	"strings"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
)

// ComponentNode is what a finding about the node itself names, as against the
// operand components, which are the verbatim "app" label values pkg/inventory
// holds as constants.
const ComponentNode = "node"

// The node labels the rules read. They are written out here rather than taken
// from pkg/source's constants of the same value, because these are what a rule
// puts in its evidence verbatim: a reader has to be able to go and look at the
// same label on the same node.
const (
	// LabelPCIVendor is written by node feature discovery on any node carrying
	// a PCI device with NVIDIA's vendor id.
	LabelPCIVendor = "feature.node.kubernetes.io/pci-10de.present"
	// LabelGPUPresent is written by GPU feature discovery, a GPU Operator
	// operand, which is why its absence says something about the operator.
	LabelGPUPresent = "nvidia.com/gpu.present"
	// LabelGPUCount is how many devices GPU feature discovery found.
	LabelGPUCount = "nvidia.com/gpu.count"
	// LabelGPUReplicas is how many times each device is advertised under time
	// slicing or MPS.
	LabelGPUReplicas = "nvidia.com/gpu.replicas"
	// LabelSharingStrategy is how a node shares each device between pods.
	LabelSharingStrategy = "nvidia.com/gpu.sharing-strategy"
	// LabelWorkloadConfig is what kind of workload the node's GPUs are set up
	// for: container, vm-passthrough or vm-vgpu. A node set up for virtual
	// machines advertises nothing to the container scheduler by design.
	LabelWorkloadConfig = "nvidia.com/gpu.workload.config"

	// labelValueTrue is the only value a hint label is honoured at. The
	// DaemonSets that write these labels write "true" and nothing else, so
	// anything else is not a claim that the feature is present.
	labelValueTrue = "true"
)

// All returns the health checks, one fresh instance each, in code order.
//
// It is a function rather than a package variable so that two callers never
// share a rule, and the rules are collected here rather than registered from
// init functions so that the set of checks can be read off one list.
func All() []health.Rule {
	all := []health.Rule{
		NewNodePCIWithoutGPULabel(),
		NewNodeNoAllocatableGPUs(),
		NewNodeAllocatableBelowCapacity(),
		NewNodeCountDiffersFromCapacity(),
		NewOperatorClusterPolicyNotReady(),
		NewOperatorOperandDisabled(),
		NewDevicePluginNotReady(),
		NewDCGMExporterNotReady(),
	}

	sort.Slice(all, func(i, j int) bool { return all[i].Code() < all[j].Code() })

	return all
}

// Register adds every rule to a registry, which panics on a duplicate or a
// malformed code. A caller that wants those guarantees uses this; one that
// only wants to run the checks uses All.
func Register(registry *health.Registry) {
	for _, rule := range All() {
		registry.Register(rule)
	}
}

// versionRange is the declared GPU Operator version range of a rule, embedded
// by every rule type so that the gate is written once.
//
// Both bounds empty means the rule applies to every version, which is the
// honest answer for a check about a label or a field that predates this
// plugin. The fields are exported and settable so that a test can bound a rule
// and prove the gate is wired, rather than leaving the gate untested until the
// first rule that happens to need a bound.
type versionRange struct {
	// MinVersion is the first operator version the rule means anything over,
	// inclusive.
	MinVersion string
	// MaxVersion is the first version it no longer means anything over,
	// exclusive.
	MaxVersion string
}

// Applies implements the version half of health.Rule. An unknown or unreadable
// version means the rule applies: a rule skipped on a cluster whose version
// has an unusual shape reports clean for the wrong reason, which is the worse
// of the two failures.
func (r versionRange) Applies(operatorVersion string) bool {
	return health.VersionInRange(operatorVersion, r.MinVersion, r.MaxVersion)
}

// infoCommand is the next command for a finding about one node.
func infoCommand(node string) string { return "kubectl nv info " + node }

// itoa renders an integer as evidence: base 10, no separators, no units.
func itoa[T int | int32 | int64](n T) string { return strconv.FormatInt(int64(n), 10) }

// btoa renders a boolean as evidence: "true" or "false", never "True" or "1".
func btoa(b bool) string { return strconv.FormatBool(b) }

// labelIsTrue reports whether a hint label is making its claim.
func labelIsTrue(node inventory.GPUNode, name string) bool {
	return node.Labels[name] == labelValueTrue
}

// The values of nvidia.com/gpu.workload.config that mean the node's GPUs are
// handed to virtual machines rather than to containers. Such a node advertises
// no nvidia.com/gpu to the container scheduler on purpose.
//
// Phase 2 of the operator's sandbox workload support is what these are about,
// and gpu health has no rules for them yet: the only thing the current rules
// have to do is not report a working one as broken.
const (
	workloadVMPassthrough = "vm-passthrough"
	workloadVMvGPU        = "vm-vgpu"
)

// isVirtualMachineWorkload reports whether the node's GPUs are set up for
// virtual machines.
func isVirtualMachineWorkload(node inventory.GPUNode) bool {
	switch node.Labels[LabelWorkloadConfig] {
	case workloadVMPassthrough, workloadVMvGPU:
		return true
	default:
		return false
	}
}

// operandComponents returns the components of the operand pods on a node,
// sorted, deduplicated and comma joined, which is the evidence encoding for a
// list. It is empty when the node runs none, and setIf leaves an empty value
// out of the evidence map rather than writing it as "".
//
// Deduplicated because a node runs two pods of the same component for as long
// as a rollout takes, and sorted because the builder hands the pods over in
// whatever order the API server listed them: without both, two runs over the
// same cluster print different bytes. A pod whose labels named no component is
// dropped rather than joined as an empty string, which would read as a stray
// comma.
func operandComponents(node inventory.GPUNode) string {
	seen := make(map[string]bool, len(node.Operands))
	names := make([]string, 0, len(node.Operands))

	for _, pod := range node.Operands {
		if pod.Component == "" || seen[pod.Component] {
			continue
		}

		seen[pod.Component] = true

		names = append(names, pod.Component)
	}

	sort.Strings(names)

	return strings.Join(names, ",")
}

// operandFor returns the first operand pod on the node with that component.
// The component is a verbatim "app" label value, so callers pass one of
// pkg/inventory's Component constants rather than a string literal.
func operandFor(node inventory.GPUNode, component string) (inventory.OperandPod, bool) {
	for _, pod := range node.Operands {
		if pod.Component == component {
			return pod, true
		}
	}

	return inventory.OperandPod{}, false
}

// operandDisabledOn reports whether the nvidia.com/gpu.deploy.<key> label says
// the operand is not expected to run on the node. The key is the suffix alone,
// which is how inventory.GPUNode.Deploy is keyed. A missing label is not a
// claim either way, so it reads as "not disabled".
func operandDisabledOn(node inventory.GPUNode, key string) bool {
	value, ok := node.Deploy[key]

	return ok && DeployDisabled(value)
}

// migAllocatable sums a node's allocatable MIG slices over every profile. It
// is what makes a mixed-strategy node's zero whole-GPU allocatable
// legitimate: such a node advertises one extended resource per profile and no
// nvidia.com/gpu at all.
func migAllocatable(node inventory.GPUNode) int64 {
	var total int64
	for _, count := range node.MIG.Resources {
		total += count.Allocatable
	}

	return total
}

// isMixedMIG reports whether the node advertises MIG profiles as its extended
// resources, which is the case where GPUNode.Capacity is legitimately zero.
func isMixedMIG(node inventory.GPUNode) bool { return len(node.MIG.Resources) > 0 }

// setIf puts a key in an evidence map only when the value is not empty.
// Absent is the encoding for "not there": the key is left out, never set to ""
// or "<none>".
func setIf(evidence map[string]string, key, value string) {
	if value != "" {
		evidence[key] = value
	}
}
