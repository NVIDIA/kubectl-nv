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

package inventory

import (
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// The node labels this package reads. Every one of them is written by
// something outside kubectl-nv, and docs/labels.md records which component
// writes which and from which version, so that a name that moved is traced to
// the release it moved in rather than guessed at.
const (
	// LabelProduct is the GFD device name, for instance "NVIDIA-T4". Under the
	// single MIG strategy GFD overrides it with the profile, for instance
	// "A100-SXM4-40GB-MIG-1g.5gb".
	LabelProduct = "nvidia.com/gpu.product"
	// LabelCount is how many devices GFD found.
	LabelCount = "nvidia.com/gpu.count"
	// LabelClique is the NVLink fabric clique, "<ClusterUUID>.<CliqueID>".
	LabelClique = "nvidia.com/gpu.clique"

	// LabelDriverVersionFull is the whole driver version in one label, which
	// is what GFD has written since k8s-device-plugin v0.15.0.
	LabelDriverVersionFull = "nvidia.com/cuda.driver-version.full"
	// LabelRuntimeVersionFull is the whole CUDA runtime version, from the same
	// release.
	LabelRuntimeVersionFull = "nvidia.com/cuda.runtime-version.full"

	// LabelDriverMajor, LabelDriverMinor and LabelDriverRev are the
	// deprecated driver version parts. GFD still writes them, and a cluster
	// running an older GPU Operator writes nothing else.
	LabelDriverMajor = "nvidia.com/cuda.driver.major"
	LabelDriverMinor = "nvidia.com/cuda.driver.minor"
	LabelDriverRev   = "nvidia.com/cuda.driver.rev"
	// LabelRuntimeMajor and LabelRuntimeMinor are the deprecated CUDA runtime
	// version parts.
	LabelRuntimeMajor = "nvidia.com/cuda.runtime.major"
	LabelRuntimeMinor = "nvidia.com/cuda.runtime.minor"

	// LabelMIGStrategy is "none", "single" or "mixed".
	LabelMIGStrategy = "nvidia.com/mig.strategy"
	// LabelMIGCapable is whether the hardware supports MIG at all.
	LabelMIGCapable = "nvidia.com/mig.capable"
	// LabelMIGConfig is the profile the mig-manager was asked to apply.
	LabelMIGConfig = "nvidia.com/mig.config"
	// LabelMIGConfigState is where the mig-manager got to with it.
	LabelMIGConfigState = "nvidia.com/mig.config.state"

	// LabelSharingStrategy is "none", "time-slicing" or "mps".
	LabelSharingStrategy = "nvidia.com/gpu.sharing-strategy"
	// LabelReplicas is how many times each physical GPU is advertised.
	LabelReplicas = "nvidia.com/gpu.replicas"

	// LabelUpgradeState is where the node is in a driver upgrade. The GPU
	// Operator builds the name from the driver it manages, and "gpu" is the
	// only driver it manages today.
	LabelUpgradeState = "nvidia.com/gpu-driver-upgrade-state"

	// TopologyFabricPrefix is the prefix of Topograph's fabric tier labels,
	// one per tier, with tier 0 closest to the node.
	TopologyFabricPrefix = "fabric.topograph.run/tier-"
	// TopologyAcceleratorDomain is Topograph's accelerator interconnect
	// locality.
	TopologyAcceleratorDomain = "accelerator.topograph.run/domain"
	// TopologyAcceleratorSubDomain is the optional level nested inside it.
	TopologyAcceleratorSubDomain = "accelerator.topograph.run/sub-domain"

	// labelValueTrue is the only value a boolean label is honoured at, as
	// pkg/source already has it: anything else is not a claim.
	labelValueTrue = "true"
)

// nodeContext is everything about a node that does not come off the node
// object: what the rest of the cluster says about it, and how much of it the
// caller asked for.
type nodeContext struct {
	// hinted marks a node that carries a GPU hint label and advertises no
	// GPUs, that is, one that came from Source.HintedNodes.
	hinted bool
	// sliceNames are the gpu.nvidia.com ResourceSlices published for the node,
	// sorted. They decide the DRA half of Mode whether or not details were
	// asked for; only the names themselves are held back.
	sliceNames []string
	// requested is the nvidia.com/gpu summed over the pods on the node, and
	// requestedKnown is whether anybody looked.
	requested      int64
	requestedKnown bool
	// operands are the GPU Operator and DRA driver pods on the node.
	operands []OperandPod
	// withDetails turns on the fields gpu info shows and gpu list does not.
	withDetails bool
}

// buildNode shapes one node, plus what the cluster says about it, into the
// model. It reads nothing and asks nobody: every input is already in hand, so
// that building a thousand nodes costs a thousand loops rather than a thousand
// round trips.
func buildNode(node *corev1.Node, ctx nodeContext) *GPUNode {
	out := NewGPUNode(node.Name)
	// The rest of a node's metadata is noise in a report and churn in a golden
	// file, so it is named rather than copied wholesale.
	out.Labels = copyStringMap(node.Labels)
	out.CreationTimestamp = node.CreationTimestamp

	labels := node.Labels

	out.Hinted = ctx.hinted
	out.Product = labels[LabelProduct]
	out.Count = atoi(labels[LabelCount])
	out.Clique = labels[LabelClique]
	out.UpgradeState = labels[LabelUpgradeState]

	// The whole-GPU count is the two names summed. With renameByDefault set
	// the device plugin advertises the replicas under nvidia.com/gpu.shared
	// and nvidia.com/gpu not at all, and mid-rollout a node carries both; a
	// reader wants what the scheduler can place either way.
	out.Capacity = wholeGPUs(node.Status.Capacity)
	out.Allocatable = wholeGPUs(node.Status.Allocatable)

	out.Requested, out.RequestedKnown = 0, ctx.requestedKnown
	if ctx.requestedKnown {
		out.Requested = ctx.requested
	}

	out.DriverVersion = driverVersion(labels)
	out.CUDAVersion = cudaVersion(labels)

	out.MIG = MIG{
		Strategy:    labels[LabelMIGStrategy],
		Config:      labels[LabelMIGConfig],
		ConfigState: labels[LabelMIGConfigState],
		Capable:     labels[LabelMIGCapable] == labelValueTrue,
		Resources:   migResources(node),
	}

	out.Sharing = Sharing{
		Strategy: labels[LabelSharingStrategy],
		Replicas: atoi(labels[LabelReplicas]),
	}

	out.Mode = mode(node, len(ctx.sliceNames) > 0)
	out.Deploy = deployLabels(labels)
	out.Topology = topologyLabels(labels)

	out.Ready = isReady(node)
	out.Unschedulable = node.Spec.Unschedulable
	out.Taints = append([]corev1.Taint(nil), node.Spec.Taints...)
	out.Operands = ctx.operands

	// Conditions and slice names are what gpu info shows and gpu list does
	// not: a list of nodes with every condition on each one is not a list
	// anyone reads.
	if ctx.withDetails {
		out.Conditions = append([]corev1.NodeCondition(nil), node.Status.Conditions...)
		out.ResourceSlices = append([]string(nil), ctx.sliceNames...)
	}

	return out
}

// mode reports how the node advertises its GPUs.
//
// The extended resource half is the same test pkg/source uses for membership,
// MIG profiles included: a node running MIG in the mixed strategy advertises
// one resource per profile and no nvidia.com/gpu at all, and calling that node
// "none" would say the device plugin is not running on the very nodes it is
// running on.
func mode(node *corev1.Node, hasSlice bool) string {
	byPlugin := false

	for name, q := range node.Status.Capacity {
		isGPU := name == source.ResourceGPU ||
			name == source.ResourceGPUShared ||
			strings.HasPrefix(string(name), source.ResourceMIGPrefix)
		if isGPU && q.Sign() > 0 {
			byPlugin = true

			break
		}
	}

	switch {
	case byPlugin && hasSlice:
		return ModeBoth
	case byPlugin:
		return ModeDevicePlugin
	case hasSlice:
		return ModeDRA
	default:
		return ModeNone
	}
}

// migResources collects what the node advertises per MIG profile. It is empty
// unless the mixed strategy is in use, because that is the only strategy that
// advertises a resource per profile.
func migResources(node *corev1.Node) map[string]ResourceCount {
	var out map[string]ResourceCount

	for name, q := range node.Status.Capacity {
		// Only the MIG profile prefix, which nvidia.com/gpu.shared does not
		// carry: a shared whole GPU is not a MIG profile.
		if !strings.HasPrefix(string(name), source.ResourceMIGPrefix) {
			continue
		}

		if out == nil {
			out = map[string]ResourceCount{}
		}

		out[string(name)] = ResourceCount{
			Capacity:    q.Value(),
			Allocatable: quantity(node.Status.Allocatable, name),
		}
	}

	return out
}

// deployLabels reads the nvidia.com/gpu.deploy.<key> labels, keyed by the
// suffix alone so that ComponentForDeployKey can be applied to them.
func deployLabels(labels map[string]string) map[string]string {
	var out map[string]string

	for name, value := range labels {
		key, found := strings.CutPrefix(name, DeployLabelPrefix)
		if !found || key == "" {
			continue
		}

		if out == nil {
			out = map[string]string{}
		}

		out[key] = value
	}

	return out
}

// topologyLabels collects Topograph's locality labels, keyed by the full label
// name. Track G builds the fabric tree out of them, so nothing is parsed into
// a tier here: the keys that reach a reader are the keys the cluster carries.
func topologyLabels(labels map[string]string) map[string]string {
	var out map[string]string

	for name, value := range labels {
		isTopology := strings.HasPrefix(name, TopologyFabricPrefix) ||
			name == TopologyAcceleratorDomain ||
			name == TopologyAcceleratorSubDomain
		if !isTopology {
			continue
		}

		if out == nil {
			out = map[string]string{}
		}

		out[name] = value
	}

	return out
}

// driverVersion reads the NVIDIA driver version, preferring the label GFD has
// written since k8s-device-plugin v0.15.0 and falling back to the deprecated
// parts, which are all an older GPU Operator writes.
func driverVersion(labels map[string]string) string {
	if full := labels[LabelDriverVersionFull]; full != "" {
		return full
	}

	return joinVersion(labels[LabelDriverMajor], labels[LabelDriverMinor], labels[LabelDriverRev])
}

// cudaVersion reads the CUDA runtime version the same way.
func cudaVersion(labels map[string]string) string {
	if full := labels[LabelRuntimeVersionFull]; full != "" {
		return full
	}

	return joinVersion(labels[LabelRuntimeMajor], labels[LabelRuntimeMinor])
}

// joinVersion assembles a dotted version from its parts, dropping the empty
// trailing ones. GFD writes nvidia.com/cuda.driver.rev as an empty label when
// the driver version has only two parts, and "455.06." is not a version.
func joinVersion(parts ...string) string {
	kept := make([]string, 0, len(parts))

	for _, p := range parts {
		if p == "" {
			break
		}

		kept = append(kept, p)
	}

	return strings.Join(kept, ".")
}

// isReady reports whether the node's Ready condition is True.
func isReady(node *corev1.Node) bool {
	for i := range node.Status.Conditions {
		c := &node.Status.Conditions[i]
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}

	return false
}

// wholeGPUs sums the resources that are a whole GPU, as opposed to a MIG
// profile: nvidia.com/gpu and the renameByDefault spelling of it. The two are
// matched exactly, never by prefix, because nvidia.com/gpu is a prefix of
// every other nvidia.com/gpu* resource there is.
func wholeGPUs(list corev1.ResourceList) int64 {
	return quantity(list, source.ResourceGPU) + quantity(list, source.ResourceGPUShared)
}

// quantity reads one resource off a resource list as a whole number, which is
// what every resource this package reads is.
func quantity(list corev1.ResourceList, name corev1.ResourceName) int64 {
	q, ok := list[name]
	if !ok {
		return 0
	}

	return q.Value()
}

// atoi reads a label that holds a number, and reports an unreadable one as
// zero: a report is not the place to fail over a malformed label.
func atoi(value string) int {
	n, err := strconv.Atoi(value)
	if err != nil {
		return 0
	}

	return n
}

// sortedKeys returns the keys of a map, sorted, so that output built from one
// is the same on every run.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
