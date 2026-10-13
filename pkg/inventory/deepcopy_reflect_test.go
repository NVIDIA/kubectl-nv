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
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
)

// The hand written deep copies are only as good as the fixture they are tested
// with: a field added to GPUNode and forgotten in DeepCopyInto is a shallow
// copy that compiles, and a test that never sets that field never notices.
//
// The two tests below close that gap without naming any field. The first
// refuses a fixture with an unset reference field, so a new slice, map or
// pointer has to be added to goldenNode before the suite goes green again. The
// second mutates every reachable value in the copy and requires the original
// to equal a freshly built fixture, so the new field is then covered by
// construction.

// referenceFieldPaths walks a struct type and returns the path of every slice,
// map and pointer field reachable through exported fields. Types outside this
// package are not descended into: their own DeepCopyInto is upstream's
// contract, not this package's.
func referenceFieldPaths(t *testing.T, typ reflect.Type, prefix string, out *[]string) {
	t.Helper()

	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}

		path := prefix + field.Name
		if field.Anonymous {
			path = prefix
		}

		switch field.Type.Kind() {
		case reflect.Slice, reflect.Map, reflect.Pointer:
			*out = append(*out, path)

			// Descend into the element type when it is one of ours, so that a
			// field added to OperandPod or InitResult is caught too.
			elem := field.Type.Elem()
			for elem.Kind() == reflect.Pointer {
				elem = elem.Elem()
			}

			if elem.Kind() == reflect.Struct && elem.PkgPath() == typ.PkgPath() {
				referenceFieldPaths(t, elem, path+"[].", out)
			}
		case reflect.Struct:
			if field.Type.PkgPath() != typ.PkgPath() {
				continue
			}

			referenceFieldPaths(t, field.Type, path+".", out)
		default:
		}
	}
}

// valueAtPath follows a dotted field path from v. A "[]" suffix on a step
// means "and then into the first element", which is how a field of OperandPod
// is reached from a GPUNode.
func valueAtPath(v reflect.Value, path string) (reflect.Value, bool) {
	for _, step := range strings.Split(path, ".") {
		if step == "" {
			continue
		}

		descend := strings.HasSuffix(step, "[]")
		name := strings.TrimSuffix(step, "[]")

		for v.Kind() == reflect.Pointer {
			if v.IsNil() {
				return reflect.Value{}, false
			}

			v = v.Elem()
		}

		if v.Kind() != reflect.Struct {
			return reflect.Value{}, false
		}

		v = v.FieldByName(name)
		if !v.IsValid() {
			return reflect.Value{}, false
		}

		if !descend {
			continue
		}

		switch v.Kind() {
		case reflect.Slice:
			if v.Len() == 0 {
				return reflect.Value{}, false
			}

			v = v.Index(0)
		case reflect.Map:
			keys := v.MapKeys()
			if len(keys) == 0 {
				return reflect.Value{}, false
			}

			v = v.MapIndex(keys[0])
		default:
			return reflect.Value{}, false
		}
	}

	return v, true
}

// The fixture has to populate every reference field GPUNode owns. A nil one
// means the deep copy of that field is asserted by nothing.
func TestGoldenNodePopulatesEveryReferenceField(t *testing.T) {
	t.Parallel()

	var paths []string
	referenceFieldPaths(t, reflect.TypeOf(GPUNode{}), "", &paths)

	if len(paths) == 0 {
		t.Fatal("found no reference fields on GPUNode: the walk is broken, not the fixture")
	}

	node := goldenNode()
	value := reflect.ValueOf(node)

	for _, path := range paths {
		field, ok := valueAtPath(value, path)
		if !ok {
			continue
		}

		switch field.Kind() {
		case reflect.Slice, reflect.Map:
			if field.IsNil() || field.Len() == 0 {
				t.Errorf("goldenNode leaves %s empty: add a value, or its deep copy is untested", path)
			}
		case reflect.Pointer:
			if field.IsNil() {
				t.Errorf("goldenNode leaves %s nil: set it, or its deep copy is untested", path)
			}
		default:
		}
	}
}

// mutateEverything walks v and changes every settable value it reaches:
// strings, numbers and booleans directly, and the contents of slices, maps and
// pointers recursively. Unexported fields are skipped, which is what keeps it
// off the unexported innards of time.Time and friends.
func mutateEverything(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			mutateEverything(v.Elem())
		}
	case reflect.Interface:
		if !v.IsNil() {
			mutateEverything(v.Elem())
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				mutateEverything(v.Field(i))
			}
		}
	case reflect.Slice, reflect.Array:
		for i := range v.Len() {
			mutateEverything(v.Index(i))
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			entry := reflect.New(v.Type().Elem()).Elem()
			entry.Set(v.MapIndex(key))
			mutateEverything(entry)
			v.SetMapIndex(key, entry)
		}
	case reflect.String:
		if v.CanSet() {
			v.SetString(v.String() + "-mutated")
		}
	case reflect.Bool:
		if v.CanSet() {
			v.SetBool(!v.Bool())
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		if v.CanSet() {
			v.SetInt(v.Int() + 1)
		}
	default:
	}
}

// Mutating everything reachable through the copy must leave the original equal
// to a freshly built fixture. This is the test a field added later is covered
// by without anybody remembering to extend it.
func TestGPUNodeDeepCopyIsDeepEverywhere(t *testing.T) {
	t.Parallel()

	original := goldenNode()
	copied := original.DeepCopy()

	mutateEverything(reflect.ValueOf(copied))

	if reflect.DeepEqual(*copied, original) {
		t.Fatal("mutating the copy changed nothing: the mutation walk is broken, not the deep copy")
	}

	if !reflect.DeepEqual(original, goldenNode()) {
		t.Errorf("mutating the copy changed the original:\ngot  %+v\nwant %+v", original, goldenNode())
	}
}

func TestGPUNodeListDeepCopyIsDeepEverywhere(t *testing.T) {
	t.Parallel()

	original := GPUNodeList{Items: []GPUNode{goldenNode()}}
	copied := original.DeepCopy()

	mutateEverything(reflect.ValueOf(copied))

	if reflect.DeepEqual(*copied, original) {
		t.Fatal("mutating the copy changed nothing: the mutation walk is broken, not the deep copy")
	}

	want := GPUNodeList{Items: []GPUNode{goldenNode()}}
	if !reflect.DeepEqual(original, want) {
		t.Errorf("mutating the copy changed the original:\ngot  %+v\nwant %+v", original, want)
	}
}

// DeepCopyObject is the method the printers and any runtime.Object consumer
// call, and returning the receiver from it satisfies the interface, compiles,
// and passes every test that only ever calls DeepCopy. So it is exercised
// through the interface here, with the same mutate-and-compare.
func TestDeepCopyObjectReturnsAnIndependentObject(t *testing.T) {
	t.Parallel()

	original := goldenNode()

	var obj runtime.Object = &original

	copied, ok := obj.DeepCopyObject().(*GPUNode)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *GPUNode", obj.DeepCopyObject())
	}

	if copied == &original {
		t.Fatal("DeepCopyObject returned the receiver itself")
	}

	mutateEverything(reflect.ValueOf(copied))

	if !reflect.DeepEqual(original, goldenNode()) {
		t.Errorf("mutating the DeepCopyObject result changed the original:\ngot  %+v", original)
	}
}

func TestListDeepCopyObjectReturnsAnIndependentObject(t *testing.T) {
	t.Parallel()

	original := GPUNodeList{Items: []GPUNode{goldenNode()}}

	var obj runtime.Object = &original

	copied, ok := obj.DeepCopyObject().(*GPUNodeList)
	if !ok {
		t.Fatalf("DeepCopyObject returned %T, want *GPUNodeList", obj.DeepCopyObject())
	}

	if copied == &original {
		t.Fatal("DeepCopyObject returned the receiver itself")
	}

	mutateEverything(reflect.ValueOf(copied))

	want := GPUNodeList{Items: []GPUNode{goldenNode()}}
	if !reflect.DeepEqual(original, want) {
		t.Errorf("mutating the DeepCopyObject result changed the original:\ngot  %+v", original)
	}
}

// A nil receiver must not panic: DeepCopy on a nil pointer is a thing
// apimachinery's generated code supports, and callers rely on it.
func TestDeepCopyOfNilIsNil(t *testing.T) {
	t.Parallel()

	var (
		node *GPUNode
		list *GPUNodeList
		pod  *OperandPod
		res  *InitResult
	)

	if node.DeepCopy() != nil || list.DeepCopy() != nil ||
		pod.DeepCopy() != nil || res.DeepCopy() != nil {
		t.Error("DeepCopy of a nil pointer returned something")
	}

	if node.DeepCopyObject() != nil || list.DeepCopyObject() != nil {
		t.Error("DeepCopyObject of a nil pointer returned something")
	}
}
