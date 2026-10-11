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

// Package output carries the machine readable output contract of kubectl-nv:
// the API version and kinds every command stamps its output with, the printers
// behind -o, the exit code mapping, a stderr logger, and a deterministic order
// for findings.
//
// The kinds themselves live in the packages that own them. A command defines
// its own typed payload, for instance
//
//	type GPUNode struct {
//		metav1.TypeMeta   `json:",inline"`
//		metav1.ObjectMeta `json:"metadata,omitzero"`
//		Spec              GPUNodeSpec   `json:"spec,omitzero"`
//		Status            GPUNodeStatus `json:"status,omitzero"`
//	}
//
// stamps it with TypeMetaFor(KindGPUNode), implements TableRows on it, and
// hands it to a Printer from this package. Object and List here are the small
// generic forms used by this package's own tests and by output that carries
// nothing but names.
//
// Machine readable output is rendered by the k8s.io/cli-runtime printers, which
// marshal through sigs.k8s.io/yaml. That package reads the JSON struct tags for
// both JSON and YAML output, so the JSON tags are what the wire format follows;
// the yaml tags are there for callers that reach for a plain YAML marshaller.
package output

import (
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	// GroupName is the API group every kubectl-nv envelope is stamped with.
	GroupName = "kubectl-nv.nvidia.com"
	// Version is the API version of the envelope.
	Version = "v1alpha1"
	// APIVersion is the group and version as they appear on the wire.
	APIVersion = GroupName + "/" + Version
)

// The kinds kubectl-nv emits. The payload of each one is defined by the package
// that produces it; the kinds are fixed here so that every consumer, and every
// track, can switch on them from day one.
const (
	// KindGPUNodeList is a list of GPU capable nodes.
	KindGPUNodeList = "GPUNodeList"
	// KindGPUNode is a single GPU capable node.
	KindGPUNode = "GPUNode"
	// KindHealthReport is the result of a health check run.
	KindHealthReport = "HealthReport"
	// KindTopologyReport is the device topology of one node or of the cluster.
	KindTopologyReport = "TopologyReport"
	// KindBundleManifest is the index of a must-gather bundle.
	KindBundleManifest = "BundleManifest"
	// KindDebugReport is one node's inventory next to the must-gather bundle
	// collected for it.
	KindDebugReport = "DebugReport"
)

// GroupVersion is the group and version of every kind in this contract.
var GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// TypeMetaFor returns the TypeMeta a kind of this contract is stamped with. It
// is how a payload type in another package joins the contract.
func TypeMetaFor(kind string) metav1.TypeMeta {
	return metav1.TypeMeta{APIVersion: APIVersion, Kind: kind}
}

// Object is the generic envelope entry: a kind and a name, with no payload. A
// command with a payload defines its own type instead.
type Object struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	// ObjectMeta carries the name the output is keyed by. It is the apimachinery
	// type so that the printers can read the name through meta.Accessor.
	metav1.ObjectMeta `json:"metadata,omitzero" yaml:"metadata,omitempty"`
}

// List is the generic envelope holding several Objects of one kind.
type List struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`
	metav1.ListMeta `json:"metadata,omitzero" yaml:"metadata,omitempty"`

	// Items keeps the order the command produced. Nothing downstream reorders
	// it, so a command that wants a stable order sorts before handing it over.
	Items []Object `json:"items" yaml:"items"`
}

// NewObject returns an item of the given kind, stamped with the API version.
func NewObject(kind, name string) *Object {
	return &Object{
		TypeMeta:   TypeMetaFor(kind),
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
}

// NewList returns an empty list of the given kind, stamped with the API version.
func NewList(kind string) *List {
	return &List{TypeMeta: TypeMetaFor(kind), Items: []Object{}}
}

// MarshalJSON renders a nil Items as an empty array. A consumer that has to
// special case null before it can range over items is a consumer that will get
// it wrong, and the guarantee belongs on the type rather than on NewList.
func (l List) MarshalJSON() ([]byte, error) {
	type listJSON List

	out := listJSON(l)
	if out.Items == nil {
		out.Items = []Object{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling %s: %w", l.Kind, err)
	}

	return raw, nil
}

// TableColumns implements TableRows.
func (o *Object) TableColumns(wide bool) []Column {
	return nameAndAgeColumns(wide)
}

// TableRows implements TableRows.
func (o *Object) TableRows(wide bool) [][]string {
	return [][]string{nameAndAgeRow(o, wide)}
}

// TableColumns implements TableRows.
func (l *List) TableColumns(wide bool) []Column {
	return nameAndAgeColumns(wide)
}

// TableRows implements TableRows.
func (l *List) TableRows(wide bool) [][]string {
	rows := make([][]string, 0, len(l.Items))
	for i := range l.Items {
		rows = append(rows, nameAndAgeRow(&l.Items[i], wide))
	}

	return rows
}

// DeepCopyInto copies the receiver into out.
func (o *Object) DeepCopyInto(out *Object) {
	*out = *o
	out.TypeMeta = o.TypeMeta
	o.ObjectMeta.DeepCopyInto(&out.ObjectMeta)
}

// DeepCopy returns a copy of the receiver.
func (o *Object) DeepCopy() *Object {
	if o == nil {
		return nil
	}

	out := new(Object)
	o.DeepCopyInto(out)

	return out
}

// DeepCopyObject implements runtime.Object.
func (o *Object) DeepCopyObject() runtime.Object {
	if c := o.DeepCopy(); c != nil {
		return c
	}

	return nil
}

// DeepCopyInto copies the receiver into out.
func (l *List) DeepCopyInto(out *List) {
	*out = *l
	out.TypeMeta = l.TypeMeta
	l.ListMeta.DeepCopyInto(&out.ListMeta)
	if l.Items != nil {
		out.Items = make([]Object, len(l.Items))
		for i := range l.Items {
			l.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a copy of the receiver.
func (l *List) DeepCopy() *List {
	if l == nil {
		return nil
	}

	out := new(List)
	l.DeepCopyInto(out)

	return out
}

// DeepCopyObject implements runtime.Object.
func (l *List) DeepCopyObject() runtime.Object {
	if c := l.DeepCopy(); c != nil {
		return c
	}

	return nil
}
