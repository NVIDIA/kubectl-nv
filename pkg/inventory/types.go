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

// Package inventory holds the model of a GPU capable node: one flat type that
// every read-only command shares, built from node labels, node status and the
// GPU operator's pods.
//
// The model is the contract between the command that fills it (gpu list and
// gpu info) and the commands that read it (gpu health, gpu topology). It is
// deliberately flat and free of spec and status halves: nothing here is ever
// applied to a cluster, so the split would carry no meaning.
//
// Two guarantees hold on the wire, both enforced by MarshalJSON rather than by
// a constructor, because a zero value reaches a printer often enough:
//
//   - The envelope is always stamped. A GPUNode marshals with the apiVersion
//     and kind of pkg/output's contract whether or not TypeMeta was set.
//   - Nothing is ever null. Every slice is an array and every map is an
//     object, so a consumer can range or index without a null check first.
//
// GPUNode's own fields carry no omitempty either: the field set a reader sees
// is the same on every node, so a jq expression written against one node works
// against all of them. The nested types keep omitempty, because a rule reads
// those through the Go fields rather than off the wire.
//
// Shaping cluster objects into this model is pkg/source's caller's job; this
// package defines the model, its deep copy and its JSON, and nothing else.
package inventory

import (
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// How a node advertises its GPUs. A node can run the device plugin, DRA, both
// during a migration, or neither while the operator is still rolling out.
const (
	// ModeDevicePlugin means the node advertises the nvidia.com/gpu extended
	// resource and publishes no ResourceSlice.
	ModeDevicePlugin = "device-plugin"
	// ModeDRA means the node publishes a gpu.nvidia.com ResourceSlice and
	// advertises no extended resource.
	ModeDRA = "dra"
	// ModeBoth means the node does both, which is what a cluster migrating
	// from the device plugin to DRA looks like mid-flight.
	ModeBoth = "both"
	// ModeNone means the node carries a GPU hint label but advertises its
	// GPUs neither way: the operator has not finished with it, or it failed.
	ModeNone = "none"
)

// MIG is the Multi-Instance GPU configuration of a node, read from the
// nvidia.com/mig.* labels.
type MIG struct {
	// Strategy is nvidia.com/mig.strategy: "none", "single" or "mixed".
	Strategy string `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	// Config is nvidia.com/mig.config, the profile the mig-manager was asked
	// to apply.
	Config string `json:"config,omitempty" yaml:"config,omitempty"`
	// ConfigState is nvidia.com/mig.config.state, where the mig-manager got
	// to with it: "success", "pending", "failed".
	ConfigState string `json:"configState,omitempty" yaml:"configState,omitempty"`
	// Capable is nvidia.com/mig.capable, whether the hardware supports MIG at
	// all. A node can be capable and unconfigured.
	Capable bool `json:"capable" yaml:"capable"`
}

// Sharing is how a node shares each physical GPU between pods, read from the
// nvidia.com/gpu.sharing-strategy and nvidia.com/gpu.replicas labels.
type Sharing struct {
	// Strategy is nvidia.com/gpu.sharing-strategy: "none", "time-slicing" or
	// "mps".
	Strategy string `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	// Replicas is nvidia.com/gpu.replicas, how many times each GPU is
	// advertised. It is 1, or absent, when nothing is shared.
	Replicas int `json:"replicas" yaml:"replicas"`
}

// InitResult is what one init container of an operand pod ended up doing. The
// operator's validator pods carry their verdict in init container exit codes,
// so this is where a failed validation is visible.
type InitResult struct {
	// Name is the init container name, for instance "toolkit-validation".
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
	// ExitCode is the code it terminated with. It is a pointer because "has
	// not finished" and "exited 0" are different facts and both are common.
	ExitCode *int32 `json:"exitCode,omitempty" yaml:"exitCode,omitempty"`
	// Reason is the termination reason, for instance "Error" or "Completed".
	Reason string `json:"reason,omitempty" yaml:"reason,omitempty"`
}

// OperandPod is one GPU operator pod running on the node.
type OperandPod struct {
	// Name is the pod name.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
	// Namespace is the namespace it runs in, usually gpu-operator.
	Namespace string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	// Component is which operand it is, read from the "app" label or from
	// "app.kubernetes.io/component". The operator has used both over its
	// lifetime, so a reader takes whichever is there.
	Component string `json:"component,omitempty" yaml:"component,omitempty"`
	// Phase is the pod phase.
	Phase string `json:"phase,omitempty" yaml:"phase,omitempty"`
	// Restarts is the restart count summed over the pod's containers.
	Restarts int32 `json:"restarts" yaml:"restarts"`
	// Images are the container images the pod runs, which is how a driver or
	// toolkit version mismatch becomes visible.
	Images []string `json:"images" yaml:"images"`
	// Ready is whether every container reports ready.
	Ready bool `json:"ready" yaml:"ready"`
	// InitContainers are the init container results, in pod order.
	InitContainers []InitResult `json:"initContainers" yaml:"initContainers"`
}

// operandPodJSON breaks the method set, so that MarshalJSON can marshal the
// value without recursing into itself.
type operandPodJSON OperandPod

// MarshalJSON renders the nil slices as empty arrays.
func (p OperandPod) MarshalJSON() ([]byte, error) {
	out := operandPodJSON(p)
	if out.Images == nil {
		out.Images = []string{}
	}

	if out.InitContainers == nil {
		out.InitContainers = []InitResult{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling operand pod %q: %w", p.Name, err)
	}

	return raw, nil
}

// GPUNode is one GPU capable node, as the read-only commands see it.
//
// ObjectMeta carries the node's name, its labels and its creation timestamp,
// and nothing else: the rest of a node's metadata (managed fields, resource
// version, annotations) is noise in a report and churn in a golden file, so a
// builder trims it rather than copying the node's metadata wholesale.
type GPUNode struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	// ObjectMeta is the apimachinery type so that the cli-runtime printers can
	// reach the name through meta.Accessor.
	metav1.ObjectMeta `json:"metadata,omitzero" yaml:"metadata,omitempty"`

	// Product is nvidia.com/gpu.product, the GFD device name, for instance
	// "NVIDIA-H100-80GB-HBM3".
	Product string `json:"product" yaml:"product"`
	// Count is nvidia.com/gpu.count, how many devices GFD found. It is 0 when
	// the label is absent, which is not the same as a node with no GPUs: a
	// node can have GPUs and no GFD.
	Count int `json:"count" yaml:"count"`

	// Capacity is the nvidia.com/gpu capacity the node advertises.
	Capacity int64 `json:"capacity" yaml:"capacity"`
	// Allocatable is the nvidia.com/gpu allocatable.
	Allocatable int64 `json:"allocatable" yaml:"allocatable"`
	// Requested is the nvidia.com/gpu summed over the pods scheduled on the
	// node. It is 0 and meaningless unless RequestedKnown is set.
	Requested int64 `json:"requested" yaml:"requested"`
	// RequestedKnown is whether Requested was computed. It is false under
	// --no-pods, which skips the pod listing; reporting 0 requested GPUs for a
	// busy node because nobody looked would be worse than reporting nothing.
	RequestedKnown bool `json:"requestedKnown" yaml:"requestedKnown"`

	// DriverVersion is the NVIDIA driver version GFD reports.
	DriverVersion string `json:"driverVersion" yaml:"driverVersion"`
	// CUDAVersion is the CUDA version GFD reports.
	CUDAVersion string `json:"cudaVersion" yaml:"cudaVersion"`

	// Mode is how the node advertises its GPUs: one of ModeDevicePlugin,
	// ModeDRA, ModeBoth or ModeNone.
	Mode string `json:"mode" yaml:"mode"`

	// MIG is the node's Multi-Instance GPU configuration.
	MIG MIG `json:"mig" yaml:"mig"`
	// Sharing is how the node shares each GPU between pods.
	Sharing Sharing `json:"sharing" yaml:"sharing"`

	// UpgradeState is nvidia.com/gpu-driver-upgrade-state, where the node is
	// in a driver upgrade: "upgrade-done", "cordon-required" and so on.
	UpgradeState string `json:"upgradeState" yaml:"upgradeState"`
	// Deploy holds the nvidia.com/gpu.deploy.<operand> labels, keyed by the
	// operand: "driver", "container-toolkit", "device-plugin". The operator
	// gates each operand on its own label, so this is what says why an operand
	// is not running on a node.
	Deploy map[string]string `json:"deploy" yaml:"deploy"`
	// Clique is nvidia.com/gpu.clique, the NVLink or IMEX clique the node's
	// GPUs belong to.
	Clique string `json:"clique" yaml:"clique"`
	// Topology holds every fabric.topograph.run/tier-* label and
	// accelerator.topograph.run/domain, keyed by the full label name. Track G
	// builds the fabric tree out of it, so the keys are kept verbatim rather
	// than parsed into tiers here.
	Topology map[string]string `json:"topology" yaml:"topology"`

	// Ready is whether the node's Ready condition is True.
	Ready bool `json:"ready" yaml:"ready"`
	// Unschedulable is spec.unschedulable, that is, whether it is cordoned.
	Unschedulable bool `json:"unschedulable" yaml:"unschedulable"`
	// Taints are the node's taints, which is how a GPU node that nothing can
	// land on explains itself.
	Taints []corev1.Taint `json:"taints" yaml:"taints"`
	// Conditions are the node conditions. gpu info fills them; gpu list leaves
	// them empty, because a list of nodes with every condition on each one is
	// not a list anyone reads.
	Conditions []corev1.NodeCondition `json:"conditions" yaml:"conditions"`

	// Operands are the GPU operator pods running on the node.
	Operands []OperandPod `json:"operands" yaml:"operands"`
	// ResourceSlices are the names of the gpu.nvidia.com ResourceSlices
	// published for the node. gpu info fills them.
	ResourceSlices []string `json:"resourceSlices" yaml:"resourceSlices"`

	// Hinted is set when the node is not a GPU node but carries a hint label,
	// that is, when it came from Source.HintedNodes rather than Source.Nodes.
	// A hinted node is a node that should have GPUs and does not advertise
	// any, which is the single most common thing a user is looking for.
	Hinted bool `json:"hinted" yaml:"hinted"`
}

// gpuNodeJSON breaks the method set so that MarshalJSON does not recurse.
type gpuNodeJSON GPUNode

// MarshalJSON stamps the envelope and renders every nil slice and map as an
// empty array or object. Both guarantees belong on the type: a GPUNode built
// by hand, or a zero value from a test, has to come off the wire looking like
// every other one.
func (n GPUNode) MarshalJSON() ([]byte, error) {
	out := gpuNodeJSON(n)
	out.TypeMeta = output.TypeMetaFor(output.KindGPUNode)

	if out.Deploy == nil {
		out.Deploy = map[string]string{}
	}

	if out.Topology == nil {
		out.Topology = map[string]string{}
	}

	if out.Taints == nil {
		out.Taints = []corev1.Taint{}
	}

	if out.Conditions == nil {
		out.Conditions = []corev1.NodeCondition{}
	}

	if out.Operands == nil {
		out.Operands = []OperandPod{}
	}

	if out.ResourceSlices == nil {
		out.ResourceSlices = []string{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling GPU node %q: %w", n.Name, err)
	}

	return raw, nil
}

// GPUNodeList is a list of GPU capable nodes.
type GPUNodeList struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	metav1.ListMeta `json:"metadata,omitzero" yaml:"metadata,omitempty"`

	// Items keeps the order the command produced, as output.List does.
	Items []GPUNode `json:"items" yaml:"items"`
}

// gpuNodeListJSON breaks the method set so that MarshalJSON does not recurse.
type gpuNodeListJSON GPUNodeList

// MarshalJSON stamps the envelope and renders a nil Items as an empty array.
func (l GPUNodeList) MarshalJSON() ([]byte, error) {
	out := gpuNodeListJSON(l)
	out.TypeMeta = output.TypeMetaFor(output.KindGPUNodeList)

	if out.Items == nil {
		out.Items = []GPUNode{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling the GPU node list: %w", err)
	}

	return raw, nil
}

// NewGPUNode returns a GPU node stamped with the API version and carrying the
// name. The envelope is stamped on marshal too, so this is a convenience for
// the in-memory value rather than the thing that makes the output correct.
func NewGPUNode(name string) *GPUNode {
	return &GPUNode{
		TypeMeta:   output.TypeMetaFor(output.KindGPUNode),
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

// NewGPUNodeList returns an empty list stamped with the API version.
func NewGPUNodeList() *GPUNodeList {
	return &GPUNodeList{
		TypeMeta: output.TypeMetaFor(output.KindGPUNodeList),
		Items:    []GPUNode{},
	}
}
