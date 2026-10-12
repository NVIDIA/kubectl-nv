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
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	validator "github.com/google/jsonschema-go/jsonschema"
	"github.com/invopop/jsonschema"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SchemaRegistry holds the prototype each kind's JSON Schema is generated from,
// and the command names that stand in for a kind on the command line.
//
// A kind is registered by the package that defines it, which is the only place
// that can keep the prototype and the type a command actually prints in step.
// Registering the same kind twice is a programming error rather than a
// configuration one: the second prototype would silently win, and the published
// schema would describe a type nothing emits.
type SchemaRegistry struct {
	mu sync.RWMutex
	// kinds maps a kind to the value its schema is reflected from.
	kinds map[string]any
	// commands maps a command name, for instance "list", onto the kind it
	// prints. The commands fill it, so that "kubectl nv schema list" works
	// without this package knowing the command tree.
	commands map[string]string
	// schemaless maps a command name onto why it has no kind. A command that
	// prints prose has no schema to publish, and a caller walking the command
	// tree still asks for one: answered with "unknown command" it would go
	// looking for a typo in a name it read off --help.
	schemaless map[string]string
}

// NewSchemaRegistry returns an empty registry. Tests use their own rather than
// the package level one, so that what one test registers cannot be what another
// test sees.
func NewSchemaRegistry() *SchemaRegistry {
	return &SchemaRegistry{
		kinds:      map[string]any{},
		commands:   map[string]string{},
		schemaless: map[string]string{},
	}
}

// defaultSchemaRegistry is the registry the commands share.
var defaultSchemaRegistry = NewSchemaRegistry()

// DefaultSchemaRegistry returns the registry RegisterSchema writes to and the
// schema command reads from.
func DefaultSchemaRegistry() *SchemaRegistry { return defaultSchemaRegistry }

// swapDefaultSchemaRegistry puts another registry in the default's place and
// returns the call that puts the real one back.
//
// It exists for the tests of this package, which have to exercise the package
// level functions without registering anything into the registry the drift
// check compares against docs/schema. A test that registered there would make
// itself the only reason a kind is published, and a second run in the same
// process would panic on the duplicate.
func swapDefaultSchemaRegistry(r *SchemaRegistry) func() {
	previous := defaultSchemaRegistry
	defaultSchemaRegistry = r

	return func() { defaultSchemaRegistry = previous }
}

// RegisterSchema records the prototype the JSON Schema of a kind is generated
// from. It panics when the kind is already registered.
func RegisterSchema(kind string, prototype any) { defaultSchemaRegistry.Register(kind, prototype) }

// RegisterCommandKind records which kind a command prints, so that a caller can
// ask for a schema by the command they were going to run.
func RegisterCommandKind(command, kind string) {
	defaultSchemaRegistry.RegisterCommand(command, kind)
}

// RegisterSchemalessCommand records that a command prints something that is
// not a kind of this contract, and why.
func RegisterSchemalessCommand(command, reason string) {
	defaultSchemaRegistry.RegisterSchemaless(command, reason)
}

// RegisteredKinds returns the kinds of the default registry, sorted.
func RegisteredKinds() []string { return defaultSchemaRegistry.Kinds() }

// SchemaFor returns the JSON Schema of a kind or of a command, from the default
// registry.
func SchemaFor(name string) ([]byte, error) { return defaultSchemaRegistry.Schema(name) }

// Register records a prototype for a kind.
func (r *SchemaRegistry) Register(kind string, prototype any) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if prototype == nil {
		panic(fmt.Sprintf("output: schema for kind %q needs a prototype", kind))
	}
	if _, taken := r.kinds[kind]; taken {
		panic(fmt.Sprintf("output: schema for kind %q is already registered", kind))
	}
	r.kinds[kind] = prototype
}

// RegisterCommand maps a command name onto a kind. The kind has to be
// registered first: a command pointing at a kind nobody defined would answer
// "unknown kind" at the one moment the caller needs an answer.
func (r *SchemaRegistry) RegisterCommand(command, kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, known := r.kinds[kind]; !known {
		panic(fmt.Sprintf("output: command %q maps to kind %q, which is not registered", command, kind))
	}
	if existing, taken := r.commands[command]; taken {
		panic(fmt.Sprintf("output: command %q already maps to kind %q", command, existing))
	}
	if _, taken := r.schemaless[command]; taken {
		panic(fmt.Sprintf("output: command %q is already registered as having no schema", command))
	}
	r.commands[command] = kind
}

// RegisterSchemaless records a command that prints no kind of this contract,
// with the reason to give whoever asks for its schema.
//
// It is the same programming error as a duplicate kind to register a command
// twice, or to register one both ways: which of the two answers a caller got
// would depend on the order the packages happened to be linked in.
func (r *SchemaRegistry) RegisterSchemaless(command, reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if reason == "" {
		panic(fmt.Sprintf("output: command %q needs a reason for having no schema", command))
	}
	if kind, taken := r.commands[command]; taken {
		panic(fmt.Sprintf("output: command %q already maps to kind %q", command, kind))
	}
	if _, taken := r.schemaless[command]; taken {
		panic(fmt.Sprintf("output: command %q is already registered as having no schema", command))
	}
	r.schemaless[command] = reason
}

// Kinds returns the registered kinds, sorted. The order is the order the
// generated files are written in, so it has to be stable.
func (r *SchemaRegistry) Kinds() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return sortedKeys(r.kinds)
}

// Commands returns the registered command names, sorted.
func (r *SchemaRegistry) Commands() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return sortedKeys(r.commands)
}

// sortedKeys returns the keys of a map, sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Strings(out)

	return out
}

// Schema returns the JSON Schema of a kind, or of a command name standing in
// for one. An unknown name is a usage error listing what there is, because the
// caller typed it.
func (r *SchemaRegistry) Schema(name string) ([]byte, error) {
	r.mu.RLock()
	prototype, known := r.kinds[name]
	kind := name
	if !known {
		if mapped, ok := r.commands[name]; ok {
			kind = mapped
			prototype, known = r.kinds[mapped]
		}
	}
	kinds, commands := sortedKeys(r.kinds), sortedKeys(r.commands)
	reason, schemaless := r.schemaless[name]
	r.mu.RUnlock()

	if schemaless {
		return nil, NewUsageError("%s has no schema: %s", name, reason)
	}

	if !known {
		return nil, NewUsageError("unknown kind or command %q: kinds are %s; commands are %s",
			name, listOrNone(kinds), listOrNone(commands))
	}

	return GenerateSchema(kind, prototype)
}

// listOrNone renders a list for an error message.
func listOrNone(values []string) string {
	if len(values) == 0 {
		return "none"
	}

	return strings.Join(values, ", ")
}

// GenerateSchema renders the JSON Schema of one kind. It is exported so that a
// package registering a kind can check its own prototype in a test without
// going through a registry.
func GenerateSchema(kind string, prototype any) ([]byte, error) {
	reflector := &jsonschema.Reflector{
		// The schema of one kind is one self contained file: a consumer that has
		// to resolve $defs to find out what an item looks like is a consumer that
		// will not bother.
		ExpandedStruct: true,
		DoNotReference: true,
		Mapper:         contractMapper,
	}

	schema := reflector.Reflect(prototype)
	if schema == nil {
		return nil, fmt.Errorf("reflecting the schema of %s produced nothing", kind)
	}

	stampIdentity(schema, kind)
	requireAlwaysMarshalled(schema, reflect.TypeOf(prototype))

	raw, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshalling the schema of %s: %w", kind, err)
	}

	return append(raw, '\n'), nil
}

// The apimachinery types this contract embeds but does not reflect whole.
var (
	timeType       = reflect.TypeOf(metav1.Time{})
	microTimeType  = reflect.TypeOf(metav1.MicroTime{})
	objectMetaType = reflect.TypeOf(metav1.ObjectMeta{})
	listMetaType   = reflect.TypeOf(metav1.ListMeta{})
)

// contractMapper replaces the apimachinery types whose Go shape and wire shape
// disagree, or whose Go shape carries far more than this contract sets.
//
// Reflected as plain structs, metav1.Time and metav1.MicroTime come out as
// empty objects, which tells a consumer to send {} where a timestamp belongs,
// and ObjectMeta comes out with managedFields, ownerReferences, uid and a dozen
// other fields no kubectl-nv kind ever writes. Publishing those as the contract
// would invite a consumer to depend on them.
func contractMapper(t reflect.Type) *jsonschema.Schema {
	switch t {
	case timeType, microTimeType:
		return rfc3339Schema()
	case objectMetaType:
		return trimmedObjectMeta()
	case listMetaType:
		return trimmedListMeta()
	}

	return nil
}

// rfc3339Schema is how every timestamp of this contract is written: an RFC
// 3339 string, or null.
//
// Null is in it because metav1.Time says so. Its MarshalJSON writes null for
// the zero value, and omitempty does not drop a struct, so every embedded
// apimachinery timestamp that nothing set is a null on the wire: the
// creationTimestamp of an object built in memory, and the heartbeat of a node
// condition a cluster reported without one. A schema saying "string" there
// would be a published contract that what kubectl-nv prints does not satisfy,
// and the sample checks in each kind'"'"'s package are what caught it.
func rfc3339Schema() *jsonschema.Schema {
	return &jsonschema.Schema{OneOf: []*jsonschema.Schema{
		{Type: "string", Format: "date-time"},
		{Type: "null"},
	}}
}

// stringMapSchema is how labels and annotations are written.
func stringMapSchema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "object", AdditionalProperties: &jsonschema.Schema{Type: "string"}}
}

// trimmedObjectMeta is ObjectMeta as this contract exposes it. The property set
// is objectMetaPaths, which is also what --fields accepts, so the schema and the
// projection cannot describe two different objects.
func trimmedObjectMeta() *jsonschema.Schema {
	properties := jsonschema.NewProperties()
	for _, name := range objectMetaPaths {
		switch name {
		case "labels", "annotations":
			properties.Set(name, stringMapSchema())
		case "creationTimestamp":
			properties.Set(name, rfc3339Schema())
		default:
			properties.Set(name, &jsonschema.Schema{Type: "string"})
		}
	}

	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           properties,
		AdditionalProperties: jsonschema.FalseSchema,
	}
}

// trimmedListMeta is ListMeta as this contract exposes it: the truncation count
// --limit writes, and the continue token a paging consumer would look for.
// selfLink, resourceVersion and shardInfo describe an API server this tool is
// not, and nothing here ever sets them.
func trimmedListMeta() *jsonschema.Schema {
	properties := jsonschema.NewProperties()
	properties.Set("continue", &jsonschema.Schema{Type: "string"})
	properties.Set("remainingItemCount", &jsonschema.Schema{Type: "integer"})

	return &jsonschema.Schema{
		Type:                 "object",
		Properties:           properties,
		AdditionalProperties: jsonschema.FalseSchema,
	}
}

// stampIdentity pins apiVersion and kind to the one value they can hold and
// makes both required, on the kind and on the items of a list. Left as free
// strings they tell a consumer nothing: the two fields are what it switches on.
func stampIdentity(schema *jsonschema.Schema, kind string) {
	if schema == nil || schema.Properties == nil {
		return
	}

	// Only what the kind actually has is pinned and required. A payload type
	// nested inside a kind carries no TypeMeta, and requiring fields it has no
	// property for would be a schema nothing can satisfy.
	var identity []string
	if _, ok := schema.Properties.Get("apiVersion"); ok {
		schema.Properties.Set("apiVersion", &jsonschema.Schema{Type: "string", Const: APIVersion})
		identity = append(identity, "apiVersion")
	}
	if _, ok := schema.Properties.Get("kind"); ok {
		schema.Properties.Set("kind", &jsonschema.Schema{Type: "string", Const: kind})
		identity = append(identity, "kind")
	}
	schema.Required = requireFirst(schema.Required, identity...)

	// The items of a list carry their own identity, and it is the item kind:
	// the items of a GPUNodeList are GPUNodes.
	if items, ok := schema.Properties.Get("items"); ok && items != nil && items.Items != nil {
		stampIdentity(items.Items, strings.TrimSuffix(kind, "List"))
	}
}

// requireFirst puts names at the front of a required list, without repeating
// any that are already there.
func requireFirst(required []string, names ...string) []string {
	out := make([]string, 0, len(required)+len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, name := range required {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}

	return out
}

// ValidateAgainstSchema checks that a sample of a kind validates against the
// schema this registry generates for it.
//
// It is what a package that owns a kind calls in its own test. A schema is only
// worth publishing if what the command actually prints satisfies it, and the
// two drift apart silently: a field renamed in Go regenerates the schema, but a
// field that marshals as null where the schema says array does not.
func (r *SchemaRegistry) ValidateAgainstSchema(kind string, sample any) error {
	raw, err := r.Schema(kind)
	if err != nil {
		return err
	}

	// Generated with invopop, validated with the draft 2020-12 validator the
	// MCP SDK already brings in. One library writes schemas and the other
	// checks them, which is the point: a schema that only its own generator
	// understands is not a published contract.
	var schema validator.Schema
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("reading the generated schema of %s: %w", kind, err)
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return fmt.Errorf("resolving the schema of %s: %w", kind, err)
	}

	// The sample goes through JSON first, so that what is validated is what a
	// consumer receives rather than the Go value behind it.
	encoded, err := json.Marshal(sample)
	if err != nil {
		return fmt.Errorf("marshalling the %s sample: %w", kind, err)
	}
	var instance any
	if err := json.Unmarshal(encoded, &instance); err != nil {
		return fmt.Errorf("reading the %s sample back: %w", kind, err)
	}

	if err := resolved.Validate(instance); err != nil {
		return fmt.Errorf("a %s sample does not satisfy its own schema: %w", kind, err)
	}

	return nil
}

// ValidateAgainstSchema is ValidateAgainstSchema on the default registry.
func ValidateAgainstSchema(kind string, sample any) error {
	return defaultSchemaRegistry.ValidateAgainstSchema(kind, sample)
}

// requireAlwaysMarshalled adds to required every property backed by a Go field
// that marshals whatever its value is.
//
// A slice without omitempty renders as [] rather than disappearing, so a
// consumer can rely on the key being there; leaving it optional in the schema
// would tell it the opposite. The Go type is the source of that fact, not the
// generated schema, so it is read here rather than guessed.
func requireAlwaysMarshalled(schema *jsonschema.Schema, t reflect.Type) {
	if schema == nil || schema.Properties == nil {
		return
	}

	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return
	}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		name, inline, skip := jsonName(field)
		if skip {
			continue
		}
		if inline {
			requireAlwaysMarshalled(schema, field.Type)
			continue
		}

		property, ok := schema.Properties.Get(name)
		if !ok {
			continue
		}
		if alwaysMarshalled(field) {
			schema.Required = appendUnique(schema.Required, name)
		}
		requireAlwaysMarshalled(property, field.Type)
		if property.Items != nil {
			requireAlwaysMarshalled(property.Items, elemType(field.Type))
		}
	}
}

// alwaysMarshalled reports whether a field is written out whatever its value
// is: a slice or a map with no omitempty and no omitzero.
func alwaysMarshalled(field reflect.StructField) bool {
	tag := field.Tag.Get("json")
	if strings.Contains(tag, ",omitempty") || strings.Contains(tag, ",omitzero") {
		return false
	}

	switch field.Type.Kind() {
	case reflect.Slice, reflect.Map:
		return true
	}

	return false
}

// elemType returns the element type of a slice, or the type itself.
func elemType(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() == reflect.Slice {
		return t.Elem()
	}

	return t
}

// appendUnique appends a name that is not already in the list.
func appendUnique(names []string, name string) []string {
	for _, existing := range names {
		if existing == name {
			return names
		}
	}

	return append(names, name)
}
