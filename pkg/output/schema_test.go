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

package output

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// healthReport stands in for a kind a later track defines in its own package.
// It carries the three shapes the generated schema has to get right: a
// metav1.Time, a slice that must never render as null, and the ObjectMeta every
// kind of this contract embeds.
type healthReport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitzero"`

	Status healthReportStatus `json:"status,omitzero"`
}

type healthReportStatus struct {
	LastChecked metav1.Time `json:"lastChecked,omitempty"`
	Findings    []Finding   `json:"findings"`
}

func (h *healthReport) DeepCopyObject() runtime.Object {
	clone := *h

	return &clone
}

// schemaOf generates a schema and decodes it, which is how every test below
// reads what was produced.
func schemaOf(t *testing.T, kind string, prototype any) map[string]any {
	t.Helper()

	r := NewSchemaRegistry()
	r.Register(kind, prototype)

	raw, err := r.Schema(kind)
	if err != nil {
		t.Fatalf("generating the schema for %s: %v", kind, err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decoding the schema for %s: %v\n%s", kind, err, raw)
	}

	return decoded
}

// at walks a decoded schema down a path of keys, failing when one is missing.
func at(t *testing.T, schema map[string]any, path ...string) map[string]any {
	t.Helper()

	current := schema
	for i, key := range path {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("no object at %q: %v", strings.Join(path[:i+1], "."), current[key])
		}
		current = next
	}

	return current
}

// propertyNames returns the keys of a schema's properties, sorted.
func propertyNames(t *testing.T, schema map[string]any) []string {
	t.Helper()

	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no properties: %v", schema)
	}
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sortStrings(names)

	return names
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func TestRegisterRejectsADuplicateKind(t *testing.T) {
	t.Parallel()

	r := NewSchemaRegistry()
	r.Register(KindHealthReport, &healthReport{})

	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("registering a kind twice did not panic: the second prototype would silently win")
		}
		want := `output: schema for kind "HealthReport" is already registered`
		if got, ok := recovered.(string); !ok || got != want {
			t.Errorf("panicked with %v, want %q", recovered, want)
		}
	}()

	r.Register(KindHealthReport, &healthReport{})
}

func TestRegisterCommandRejectsADuplicateCommand(t *testing.T) {
	t.Parallel()

	r := NewSchemaRegistry()
	r.Register(KindGPUNodeList, &gpuNodeList{})
	r.RegisterCommand("list", KindGPUNodeList)

	defer func() {
		if recover() == nil {
			t.Fatal("mapping a command twice did not panic")
		}
	}()

	r.RegisterCommand("list", KindGPUNodeList)
}

func TestRegisterCommandRejectsAnUnregisteredKind(t *testing.T) {
	t.Parallel()

	r := NewSchemaRegistry()

	defer func() {
		if recover() == nil {
			t.Fatal("mapping a command to an unknown kind did not panic")
		}
	}()

	r.RegisterCommand("list", KindGPUNodeList)
}

func TestSchemaResolvesACommandName(t *testing.T) {
	t.Parallel()

	r := NewSchemaRegistry()
	r.Register(KindGPUNodeList, &gpuNodeList{})
	r.RegisterCommand("list", KindGPUNodeList)

	byKind, err := r.Schema(KindGPUNodeList)
	if err != nil {
		t.Fatalf("by kind: %v", err)
	}
	byCommand, err := r.Schema("list")
	if err != nil {
		t.Fatalf("by command: %v", err)
	}
	if !reflect.DeepEqual(byKind, byCommand) {
		t.Error("the command name and the kind produced different schemas")
	}
}

func TestSchemaForAnUnknownNameIsAUsageErrorListingWhatThereIs(t *testing.T) {
	t.Parallel()

	r := NewSchemaRegistry()
	r.Register(KindGPUNodeList, &gpuNodeList{})
	r.Register(KindHealthReport, &healthReport{})
	r.RegisterCommand("list", KindGPUNodeList)

	_, err := r.Schema("GPUNodes")
	if err == nil {
		t.Fatal("an unknown name was accepted")
	}
	want := `unknown kind or command "GPUNodes": kinds are GPUNodeList, HealthReport; commands are list`
	if err.Error() != want {
		t.Errorf("error is %q,\nwant %q", err.Error(), want)
	}
	if got := ExitCodeFor(err); got != ExitUsage {
		t.Errorf("exit code is %d, want %d", got, ExitUsage)
	}
}

func TestKindsAreSorted(t *testing.T) {
	t.Parallel()

	r := NewSchemaRegistry()
	r.Register(KindHealthReport, &healthReport{})
	r.Register(KindGPUNodeList, &gpuNodeList{})
	r.Register(KindGPUNode, &gpuNode{})

	want := []string{KindGPUNode, KindGPUNodeList, KindHealthReport}
	if got := r.Kinds(); !reflect.DeepEqual(got, want) {
		t.Errorf("Kinds is %v, want %v: the order decides the order of the generated files", got, want)
	}
}

// apiVersion and kind are what a consumer switches on, so a schema that leaves
// them as free strings tells it nothing.
func TestSchemaPinsAPIVersionAndKindWithConstValues(t *testing.T) {
	t.Parallel()

	schema := schemaOf(t, KindGPUNodeList, &gpuNodeList{})

	apiVersion := at(t, schema, "properties", "apiVersion")
	if apiVersion["const"] != APIVersion {
		t.Errorf("apiVersion const is %v, want %q", apiVersion["const"], APIVersion)
	}
	if apiVersion["type"] != "string" {
		t.Errorf("apiVersion type is %v, want string", apiVersion["type"])
	}

	kind := at(t, schema, "properties", "kind")
	if kind["const"] != KindGPUNodeList {
		t.Errorf("kind const is %v, want %q", kind["const"], KindGPUNodeList)
	}

	if !requires(schema, "apiVersion") || !requires(schema, "kind") {
		t.Errorf("required is %v, want it to carry apiVersion and kind", schema["required"])
	}
}

// The items of a list carry their own identity, and it is the item kind rather
// than the list kind.
func TestSchemaPinsTheItemKindOnTheItemsOfAList(t *testing.T) {
	t.Parallel()

	schema := schemaOf(t, KindGPUNodeList, &gpuNodeList{})
	item := at(t, schema, "properties", "items", "items")

	kind := at(t, item, "properties", "kind")
	if kind["const"] != KindGPUNode {
		t.Errorf("item kind const is %v, want %q", kind["const"], KindGPUNode)
	}
	apiVersion := at(t, item, "properties", "apiVersion")
	if apiVersion["const"] != APIVersion {
		t.Errorf("item apiVersion const is %v, want %q", apiVersion["const"], APIVersion)
	}
	if !requires(item, "apiVersion") || !requires(item, "kind") {
		t.Errorf("item required is %v, want it to carry apiVersion and kind", item["required"])
	}
}

// requires reports whether a decoded schema lists name in its required array.
func requires(schema map[string]any, name string) bool {
	required, ok := schema["required"].([]any)
	if !ok {
		return false
	}
	for _, entry := range required {
		if entry == name {
			return true
		}
	}

	return false
}

// metav1.Time marshals as an RFC3339 string, or as null when it is the zero
// value: its MarshalJSON writes null, and omitempty does not drop a struct.
// Reflected as a plain struct it comes out as an empty object instead, which
// tells a consumer to send "{}" where a timestamp belongs.
func TestSchemaRendersATimestampAsAnRFC3339StringOrNull(t *testing.T) {
	t.Parallel()

	schema := schemaOf(t, KindHealthReport, &healthReport{})
	timestamp := at(t, schema, "properties", "status", "properties", "lastChecked")

	assertRFC3339OrNull(t, "lastChecked", timestamp)
}

// assertRFC3339OrNull holds a property to the one shape every timestamp of
// this contract has: an RFC3339 string, or null.
func assertRFC3339OrNull(t *testing.T, name string, property map[string]any) {
	t.Helper()

	alternatives, ok := property["oneOf"].([]any)
	if !ok || len(alternatives) != 2 {
		t.Fatalf("%s is %v, want a choice of an RFC3339 string and null", name, property)
	}

	str, _ := alternatives[0].(map[string]any)
	if str["type"] != "string" || str["format"] != "date-time" {
		t.Errorf("%s first alternative is %v, want an RFC3339 string", name, str)
	}

	null, _ := alternatives[1].(map[string]any)
	if null["type"] != "null" {
		t.Errorf("%s second alternative is %v, want null", name, null)
	}
}

// ObjectMeta reflected whole carries managedFields, ownerReferences, uid and a
// dozen other fields no kubectl-nv kind ever sets. Publishing them as the
// contract would invite a consumer to depend on them.
func TestSchemaTrimsObjectMetaToWhatTheContractUses(t *testing.T) {
	t.Parallel()

	schema := schemaOf(t, KindHealthReport, &healthReport{})
	meta := at(t, schema, "properties", "metadata")

	want := append([]string(nil), objectMetaPaths...)
	if got := propertyNames(t, meta); !reflect.DeepEqual(got, want) {
		t.Errorf("metadata properties are %v,\nwant %v: the trimmed set has to be the one --fields offers", got, want)
	}
	if meta["additionalProperties"] != false {
		t.Errorf("metadata additionalProperties is %v, want false", meta["additionalProperties"])
	}

	assertRFC3339OrNull(t, "metadata.creationTimestamp", at(t, meta, "properties", "creationTimestamp"))
	labels := at(t, meta, "properties", "labels")
	if labels["type"] != "object" {
		t.Errorf("metadata.labels type is %v, want object", labels["type"])
	}
}

// The list envelope carries ListMeta, which reflected whole offers selfLink,
// resourceVersion and shardInfo. The only field this contract sets on it is the
// one --limit writes.
func TestSchemaTrimsListMetaToTheTruncationCount(t *testing.T) {
	t.Parallel()

	schema := schemaOf(t, KindGPUNodeList, &gpuNodeList{})
	meta := at(t, schema, "properties", "metadata")

	want := []string{"continue", "remainingItemCount"}
	if got := propertyNames(t, meta); !reflect.DeepEqual(got, want) {
		t.Errorf("list metadata properties are %v, want %v", got, want)
	}
	remaining := at(t, meta, "properties", "remainingItemCount")
	if remaining["type"] != "integer" {
		t.Errorf("remainingItemCount type is %v, want integer", remaining["type"])
	}
}

// A schema that types a slice as an array is a promise the Go type has to keep:
// a nil slice marshals as null unless the kind says otherwise, and null is not
// an array.
func TestSliceFieldsAreArraysInTheSchemaAndNeverNullOnTheWire(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		kind      string
		prototype runtime.Object
		path      []string
		marshal   func() ([]byte, error)
		wantEmpty string
	}{
		{
			name:      "the items of a list",
			kind:      KindGPUNodeList,
			prototype: &gpuNodeList{},
			path:      []string{"properties", "items"},
			marshal:   func() ([]byte, error) { return json.Marshal(NewList(KindGPUNodeList)) },
			wantEmpty: `"items":[]`,
		},
		{
			name:      "the findings of a report",
			kind:      KindHealthReport,
			prototype: &healthReport{},
			path:      []string{"properties", "status", "properties", "findings"},
			marshal:   func() ([]byte, error) { return json.Marshal(ReportStatus{}) },
			wantEmpty: `"findings":[]`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			schema := schemaOf(t, tc.kind, tc.prototype)
			field := at(t, schema, tc.path...)
			if field["type"] != "array" {
				t.Errorf("%s is typed %v, want array", strings.Join(tc.path, "."), field["type"])
			}
			if _, ok := field["items"]; !ok {
				t.Errorf("%s is an array with no items schema", strings.Join(tc.path, "."))
			}

			raw, err := tc.marshal()
			if err != nil {
				t.Fatalf("marshalling the empty value: %v", err)
			}
			if !strings.Contains(string(raw), tc.wantEmpty) {
				t.Errorf("the empty value marshalled to %s, want it to contain %s: the schema promised an array",
					raw, tc.wantEmpty)
			}
		})
	}
}

func TestSchemaCarriesTheDraftAndRejectsUnknownProperties(t *testing.T) {
	t.Parallel()

	schema := schemaOf(t, KindGPUNodeList, &gpuNodeList{})
	if want := "https://json-schema.org/draft/2020-12/schema"; schema["$schema"] != want {
		t.Errorf("$schema is %v, want %q", schema["$schema"], want)
	}
	if schema["additionalProperties"] != false {
		t.Errorf("additionalProperties is %v, want false", schema["additionalProperties"])
	}
}

// Two runs have to produce the same bytes, or the checked-in schemas drift on
// their own and the drift test becomes noise.
func TestSchemaGenerationIsDeterministic(t *testing.T) {
	t.Parallel()

	r := NewSchemaRegistry()
	r.Register(KindHealthReport, &healthReport{})

	first, err := r.Schema(KindHealthReport)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	for i := 0; i < 5; i++ {
		again, err := r.Schema(KindHealthReport)
		if err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
		if string(again) != string(first) {
			t.Fatalf("run %d produced different bytes", i)
		}
	}
	if !strings.HasSuffix(string(first), "\n") {
		t.Error("the schema does not end in a newline, which every checked-in file has to")
	}
}

// The package level functions are what a kind in another package registers
// through, so a kind registered with RegisterSchema has to be the kind
// SchemaFor and RegisteredKinds answer about. Three names on one registry
// would otherwise be three registries.
//
// The default registry is swapped for the duration and put back afterwards.
// Registering into the real one would make this test the only reason a kind is
// in it: the run would leak into whatever else reads it, and a second run in
// the same process would panic on the duplicate.
func TestThePackageLevelFunctionsShareOneRegistry(t *testing.T) {
	const probe = "DefaultRegistryProbe"

	restore := swapDefaultSchemaRegistry(NewSchemaRegistry())
	t.Cleanup(restore)

	RegisterSchema(probe, &healthReport{})

	kinds := RegisteredKinds()
	if !slices.Contains(kinds, probe) {
		t.Fatalf("RegisteredKinds is %v, want it to carry %q", kinds, probe)
	}
	if !reflect.DeepEqual(kinds, DefaultSchemaRegistry().Kinds()) {
		t.Errorf("RegisteredKinds is %v, want the default registry's %v", kinds, DefaultSchemaRegistry().Kinds())
	}

	fromPackage, err := SchemaFor(probe)
	if err != nil {
		t.Fatalf("SchemaFor: %v", err)
	}
	fromRegistry, err := DefaultSchemaRegistry().Schema(probe)
	if err != nil {
		t.Fatalf("Schema: %v", err)
	}
	if string(fromPackage) != string(fromRegistry) {
		t.Error("SchemaFor and the default registry answered differently")
	}

	RegisterCommandKind("default-registry-probe", probe)
	if !slices.Contains(DefaultSchemaRegistry().Commands(), "default-registry-probe") {
		t.Error("RegisterCommandKind did not write to the default registry")
	}
}

// The swap has to put the registry back, or the next test in this package sees
// the probe and the drift check sees a kind nothing registers.
func TestSwappingTheDefaultRegistryIsReversible(t *testing.T) {
	before := DefaultSchemaRegistry()

	func() {
		restore := swapDefaultSchemaRegistry(NewSchemaRegistry())
		defer restore()

		RegisterSchema("SwapProbe", &healthReport{})
		if DefaultSchemaRegistry() == before {
			t.Fatal("the swap did not take effect")
		}
	}()

	if DefaultSchemaRegistry() != before {
		t.Fatal("the swap was not undone")
	}
	if slices.Contains(RegisteredKinds(), "SwapProbe") {
		t.Error("a kind registered during the swap is still in the real registry")
	}
}

// A schema is only worth publishing if what the command actually prints
// validates against it. The helper is what a command package calls in its own
// test, so a kind that drifts from its schema fails where it is defined.
func TestValidateAgainstSchemaAcceptsASampleAndRejectsAWrongOne(t *testing.T) {
	t.Parallel()

	r := NewSchemaRegistry()
	r.Register(KindGPUNodeList, &gpuNodeList{})

	good := &gpuNodeList{
		TypeMeta: TypeMetaFor(KindGPUNodeList),
		Items: []gpuNode{{
			TypeMeta:   TypeMetaFor(KindGPUNode),
			ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
			Status:     gpuNodeStatus{GPUs: 4},
		}},
	}
	if err := r.ValidateAgainstSchema(KindGPUNodeList, good); err != nil {
		t.Fatalf("a sample the command would print did not validate: %v", err)
	}

	// The schema types items as an array, and a kind that renders an empty one
	// as null does not satisfy it. That is the whole reason for validating a
	// sample rather than trusting the generator: the Go type and the published
	// schema disagree, and nothing else would say so.
	plain := &gpuNodeList{TypeMeta: TypeMetaFor(KindGPUNodeList)}
	if err := r.ValidateAgainstSchema(KindGPUNodeList, plain); err == nil {
		t.Error("a list whose empty items marshals as null satisfied a schema that says array")
	}

	// A kind that guarantees the empty array does satisfy it.
	stamped := NewSchemaRegistry()
	stamped.Register(KindGPUNodeList, &stampedList{})
	if err := stamped.ValidateAgainstSchema(KindGPUNodeList, &stampedList{}); err != nil {
		t.Errorf("an empty list that renders items as [] did not validate: %v", err)
	}

	// A kind stamped with the wrong apiVersion is exactly what the const in the
	// schema is there to catch.
	wrong := good.DeepCopyObject().(*gpuNodeList)
	wrong.APIVersion = "v1"
	if err := r.ValidateAgainstSchema(KindGPUNodeList, wrong); err == nil {
		t.Error("a list stamped with the wrong apiVersion validated")
	}

	if err := r.ValidateAgainstSchema("NoSuchKind", good); err == nil {
		t.Error("validating against a kind nobody registered was accepted")
	}
}

// A schema that only says apiVersion and kind are required when the kind has
// them keeps a payload type that carries neither from claiming fields it does
// not have.
func TestRequiredOnlyNamesPropertiesTheKindHas(t *testing.T) {
	t.Parallel()

	schema := schemaOf(t, "PlainPayload", &plainPayload{})
	if required, present := schema["required"]; present {
		for _, name := range required.([]any) {
			if name == "apiVersion" || name == "kind" {
				t.Errorf("required names %v, which this kind has no property for", name)
			}
		}
	}
}

// plainPayload is a payload type with no TypeMeta, as a type nested inside a
// kind is.
type plainPayload struct {
	Note string `json:"note,omitempty"`
}

// A slice without omitempty marshals as [] and never disappears, so the schema
// has to require it rather than leave a consumer to guess.
func TestANonOmitemptySliceIsRequiredInTheSchema(t *testing.T) {
	t.Parallel()

	schema := schemaOf(t, KindHealthReport, &healthReport{})
	status := at(t, schema, "properties", "status")
	if !requires(status, "findings") {
		t.Errorf("status.required is %v, want it to carry findings: the field always marshals", status["required"])
	}
}
