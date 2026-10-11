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
}

// NewSchemaRegistry returns an empty registry. Tests use their own rather than
// the package level one, so that what one test registers cannot be what another
// test sees.
func NewSchemaRegistry() *SchemaRegistry {
	return &SchemaRegistry{kinds: map[string]any{}, commands: map[string]string{}}
}

// defaultSchemaRegistry is the registry the commands share.
var defaultSchemaRegistry = NewSchemaRegistry()

// DefaultSchemaRegistry returns the registry RegisterSchema writes to and the
// schema command reads from.
func DefaultSchemaRegistry() *SchemaRegistry { return defaultSchemaRegistry }

// RegisterSchema records the prototype the JSON Schema of a kind is generated
// from. It panics when the kind is already registered.
func RegisterSchema(kind string, prototype any) { defaultSchemaRegistry.Register(kind, prototype) }

// RegisterCommandKind records which kind a command prints, so that a caller can
// ask for a schema by the command they were going to run.
func RegisterCommandKind(command, kind string) {
	defaultSchemaRegistry.RegisterCommand(command, kind)
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
	r.commands[command] = kind
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
	r.mu.RUnlock()

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

// rfc3339Schema is how every timestamp of this contract is written.
func rfc3339Schema() *jsonschema.Schema {
	return &jsonschema.Schema{Type: "string", Format: "date-time"}
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

	if _, ok := schema.Properties.Get("apiVersion"); ok {
		schema.Properties.Set("apiVersion", &jsonschema.Schema{Type: "string", Const: APIVersion})
	}
	if _, ok := schema.Properties.Get("kind"); ok {
		schema.Properties.Set("kind", &jsonschema.Schema{Type: "string", Const: kind})
	}
	schema.Required = requireFirst(schema.Required, "apiVersion", "kind")

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
