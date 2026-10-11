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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The deep copies are written by hand rather than generated. There is no code
// generator in this repository's build, and these are flat types whose
// reference fields are a short, visible list; a generator would add a tool
// dependency and a verify step in order to copy six slices and two maps.
//
// Every reference field is copied here. A field added to a type and not added
// here is a shallow copy that still compiles, so deepcopy_test.go mutates
// every one of them through a copy and checks the original, and
// deepcopy_reflect_test.go does the same without naming any field: it refuses
// a fixture that leaves a reference field unset, then mutates everything
// reachable in a copy and requires the original to be untouched.
//
// TypeMeta and the scalars are covered by the *out = *n above. They are not
// reassigned afterwards: a line that copies a value field again reads as if it
// were doing something, and the next person has to work out that it is not.

// DeepCopyInto copies the receiver into out.
func (r *InitResult) DeepCopyInto(out *InitResult) {
	*out = *r

	if r.ExitCode != nil {
		code := *r.ExitCode
		out.ExitCode = &code
	}
}

// DeepCopy returns a copy of the receiver.
func (r *InitResult) DeepCopy() *InitResult {
	if r == nil {
		return nil
	}

	out := new(InitResult)
	r.DeepCopyInto(out)

	return out
}

// DeepCopyInto copies the receiver into out.
func (p *OperandPod) DeepCopyInto(out *OperandPod) {
	*out = *p

	if p.Images != nil {
		out.Images = make([]string, len(p.Images))
		copy(out.Images, p.Images)
	}

	if p.InitContainers != nil {
		out.InitContainers = make([]InitResult, len(p.InitContainers))
		for i := range p.InitContainers {
			p.InitContainers[i].DeepCopyInto(&out.InitContainers[i])
		}
	}
}

// DeepCopy returns a copy of the receiver.
func (p *OperandPod) DeepCopy() *OperandPod {
	if p == nil {
		return nil
	}

	out := new(OperandPod)
	p.DeepCopyInto(out)

	return out
}

// copyResourceCounts returns a copy of in, or nil when in is nil. ResourceCount
// holds only numbers, so copying the entries is enough.
func copyResourceCounts(in map[string]ResourceCount) map[string]ResourceCount {
	if in == nil {
		return nil
	}

	out := make(map[string]ResourceCount, len(in))
	for k, v := range in {
		out[k] = v
	}

	return out
}

// copyStringMap returns a copy of in, or nil when in is nil. Nil stays nil:
// MarshalJSON is what turns it into an empty object, and a copy that quietly
// allocated would make a value differ from its own copy.
func copyStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}

	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}

	return out
}

// DeepCopyInto copies the receiver into out.
func (n *GPUNode) DeepCopyInto(out *GPUNode) {
	*out = *n

	n.ObjectMeta.DeepCopyInto(&out.ObjectMeta)

	out.MIG.Resources = copyResourceCounts(n.MIG.Resources)
	out.Deploy = copyStringMap(n.Deploy)
	out.Topology = copyStringMap(n.Topology)

	if n.Taints != nil {
		out.Taints = make([]corev1.Taint, len(n.Taints))
		for i := range n.Taints {
			n.Taints[i].DeepCopyInto(&out.Taints[i])
		}
	}

	if n.Conditions != nil {
		out.Conditions = make([]corev1.NodeCondition, len(n.Conditions))
		for i := range n.Conditions {
			n.Conditions[i].DeepCopyInto(&out.Conditions[i])
		}
	}

	if n.Operands != nil {
		out.Operands = make([]OperandPod, len(n.Operands))
		for i := range n.Operands {
			n.Operands[i].DeepCopyInto(&out.Operands[i])
		}
	}

	if n.ResourceSlices != nil {
		out.ResourceSlices = make([]string, len(n.ResourceSlices))
		copy(out.ResourceSlices, n.ResourceSlices)
	}
}

// DeepCopy returns a copy of the receiver.
func (n *GPUNode) DeepCopy() *GPUNode {
	if n == nil {
		return nil
	}

	out := new(GPUNode)
	n.DeepCopyInto(out)

	return out
}

// DeepCopyObject implements runtime.Object.
func (n *GPUNode) DeepCopyObject() runtime.Object {
	if c := n.DeepCopy(); c != nil {
		return c
	}

	return nil
}

// DeepCopyInto copies the receiver into out.
func (l *GPUNodeList) DeepCopyInto(out *GPUNodeList) {
	*out = *l

	l.ListMeta.DeepCopyInto(&out.ListMeta)

	if l.Items != nil {
		out.Items = make([]GPUNode, len(l.Items))
		for i := range l.Items {
			l.Items[i].DeepCopyInto(&out.Items[i])
		}
	}
}

// DeepCopy returns a copy of the receiver.
func (l *GPUNodeList) DeepCopy() *GPUNodeList {
	if l == nil {
		return nil
	}

	out := new(GPUNodeList)
	l.DeepCopyInto(out)

	return out
}

// DeepCopyObject implements runtime.Object.
func (l *GPUNodeList) DeepCopyObject() runtime.Object {
	if c := l.DeepCopy(); c != nil {
		return c
	}

	return nil
}
