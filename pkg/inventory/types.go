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
// Build every value through NewGPUNode and NewGPUNodeList. That is not a
// convenience: the cli-runtime printers behind -o json, -o yaml and -o name
// read TypeMeta off the object and refuse one whose GroupVersionKind is empty,
// and they do it before any marshalling, so MarshalJSON stamping the envelope
// does not reach them. A GPUNode literal prints as an error at -o json.
// types_printer_test.go asserts both halves of that.
//
// Two guarantees then hold on the wire, enforced by MarshalJSON rather than by
// the constructors, because a zero value still reaches encoding/json often
// enough:
//
//   - The envelope is always stamped. A GPUNode marshals with the apiVersion
//     and kind of pkg/output's contract whether or not TypeMeta was set.
//   - Nothing is ever null. Every slice is an array and every map is an
//     object, so a consumer can range or index without a null check first.
//     The single documented exception is InitResult.ExitCode, where the
//     alternative to an absent key is a null.
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

// ResourceCount is what a node advertises of one extended resource.
type ResourceCount struct {
	// Capacity is the resource's capacity on the node.
	Capacity int64 `json:"capacity" yaml:"capacity"`
	// Allocatable is what is left for the scheduler to place on it.
	Allocatable int64 `json:"allocatable" yaml:"allocatable"`
}

// MIG is the Multi-Instance GPU configuration of a node, read from the
// nvidia.com/mig.* labels and, in the mixed strategy, from the node's own
// extended resources.
type MIG struct {
	// Strategy is nvidia.com/mig.strategy: "none", "single" or "mixed".
	Strategy string `json:"strategy" yaml:"strategy"`
	// Config is nvidia.com/mig.config, the profile the mig-manager was asked
	// to apply.
	Config string `json:"config" yaml:"config"`
	// ConfigState is nvidia.com/mig.config.state, where the mig-manager got
	// to with it: "success", "pending", "failed".
	ConfigState string `json:"configState" yaml:"configState"`
	// Capable is nvidia.com/mig.capable, whether the hardware supports MIG at
	// all. A node can be capable and unconfigured.
	Capable bool `json:"capable" yaml:"capable"`
	// Resources is what the node advertises per MIG profile, keyed by the
	// full resource name, for instance "nvidia.com/mig-1g.10gb".
	//
	// It is only ever populated under the mixed strategy, which is the one
	// where it is the only description of the node's capacity: a mixed node
	// advertises one extended resource per profile and no nvidia.com/gpu at
	// all, so GPUNode.Capacity is zero on it and these counts are the real
	// numbers. The single strategy keeps advertising nvidia.com/gpu and
	// leaves this empty.
	//
	// The keys are the same strings pkg/source reads membership from, kept
	// whole rather than split into a profile name, so that the two cannot
	// disagree about what counts as a MIG resource.
	Resources map[string]ResourceCount `json:"resources" yaml:"resources"`
}

// migJSON breaks the method set so that MarshalJSON does not recurse.
type migJSON MIG

// MarshalJSON renders a nil Resources as an empty object.
func (m MIG) MarshalJSON() ([]byte, error) {
	out := migJSON(m)
	if out.Resources == nil {
		out.Resources = map[string]ResourceCount{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling the MIG configuration: %w", err)
	}

	return raw, nil
}

// Sharing is how a node shares each physical GPU between pods, read from the
// nvidia.com/gpu.sharing-strategy and nvidia.com/gpu.replicas labels.
type Sharing struct {
	// Strategy is nvidia.com/gpu.sharing-strategy: "none", "time-slicing" or
	// "mps".
	Strategy string `json:"strategy" yaml:"strategy"`
	// Replicas is nvidia.com/gpu.replicas, how many times each GPU is
	// advertised. It is 1, or absent, when nothing is shared.
	Replicas int `json:"replicas" yaml:"replicas"`
}

// InitResult is what one init container of an operand pod ended up doing. The
// operator's validator pods carry their verdict in init container exit codes,
// so this is where a failed validation is visible.
type InitResult struct {
	// Name is the init container name, for instance "toolkit-validation".
	Name string `json:"name" yaml:"name"`
	// ExitCode is the code it terminated with. It is a pointer because "has
	// not finished" and "exited 0" are different facts and both are common.
	//
	// This is the one field in the package that keeps omitempty, and so the
	// one field whose presence varies between nodes. The alternative for a nil
	// pointer is a null, and never-null is the guarantee consumers were given
	// first; an absent key and a null both have to be handled, and an absent
	// key is the one that does not break a reader that trusted the rest.
	ExitCode *int32 `json:"exitCode,omitempty" yaml:"exitCode,omitempty"`
	// Reason is the termination reason, for instance "Error" or "Completed".
	Reason string `json:"reason" yaml:"reason"`
}

// OperandPod is one GPU operator pod running on the node.
type OperandPod struct {
	// Name is the pod name.
	Name string `json:"name" yaml:"name"`
	// Namespace is the namespace it runs in, usually gpu-operator.
	Namespace string `json:"namespace" yaml:"namespace"`
	// Component is which operand it is: the verbatim value of the pod's "app"
	// label, or of "app.kubernetes.io/component" when there is no "app". The
	// operator has used both over its lifetime, so a reader takes whichever is
	// there and does not rewrite it.
	//
	// Verbatim is the contract. The Component* constants in this package are
	// the values the GPU Operator and the NVIDIA DRA driver are known to use,
	// and a rule matching on a component compares against one of them; a pod
	// labelled with something this package has never heard of still arrives
	// here with its own label value rather than an empty string, so an
	// unrecognised operand is visible rather than silently dropped.
	Component string `json:"component" yaml:"component"`
	// Phase is the pod phase.
	Phase string `json:"phase" yaml:"phase"`
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

	// Capacity is the nvidia.com/gpu capacity the node advertises. It is zero
	// on a node running MIG in the mixed strategy, which advertises one
	// resource per profile instead; MIG.Resources carries those.
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
	// Conditions are the node conditions. gpu info and gpu health both fill
	// them, the latter because NV-NODE-005 reads them; gpu list leaves them
	// empty, because a list of nodes with every condition on each one is not a
	// list anyone reads.
	Conditions []corev1.NodeCondition `json:"conditions" yaml:"conditions"`

	// Operands are the GPU Operator's pods running on the node, plus the
	// NVIDIA DRA driver's kubelet-plugin and IMEX daemon pods on it. The DRA
	// pods are operands in every sense that matters to a reader: they are what
	// makes the node's GPUs allocatable under DRA, and a node whose
	// kubelet-plugin is not running advertises nothing even though every GPU
	// Operator pod is healthy.
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
// name. Every GPUNode handed to a printer has to come from here or carry the
// same TypeMeta: the printers read it off the struct, not off the bytes.
func NewGPUNode(name string) *GPUNode {
	return &GPUNode{
		TypeMeta:   output.TypeMetaFor(output.KindGPUNode),
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

// NewGPUNodeList returns an empty list stamped with the API version. As with
// NewGPUNode, a list handed to a printer has to come from here.
func NewGPUNodeList() *GPUNodeList {
	return &GPUNodeList{
		TypeMeta: output.TypeMetaFor(output.KindGPUNodeList),
		Items:    []GPUNode{},
	}
}
