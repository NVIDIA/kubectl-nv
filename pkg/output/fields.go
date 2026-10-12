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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"sigs.k8s.io/yaml"
)

// alwaysKept are the paths a projection never drops. Without the first two a
// consumer cannot tell what it is holding, and without the third it cannot say
// which thing the values belong to; a projection that could hide any of them
// would be a projection nothing could safely consume.
var alwaysKept = []string{"apiVersion", "kind", "metadata.name"}

// objectMetaPaths is ObjectMeta as this contract exposes it. The apimachinery
// type carries two dozen fields that mean nothing for a diagnostic report, so
// only these are offered to --fields, documented, and put in the JSON Schema.
var objectMetaPaths = []string{"annotations", "creationTimestamp", "labels", "name", "namespace"}

// BoundedOptions is what --fields and --limit parsed into. Its zero value
// prints everything, which is what a command that never calls
// AddBoundedOutputFlags gets.
type BoundedOptions struct {
	// Fields are the dotted JSON paths to keep on each item, for instance
	// "status.ready". Empty keeps every field.
	Fields []string
	// Limit is how many items of a list to print, after the command's own sort.
	// Zero means every item.
	Limit int
}

// AddBoundedOutputFlags registers --fields and --limit on a command and returns
// the options they write into. Every list-like command calls it, so that the
// two flags mean the same thing everywhere rather than once per command.
func AddBoundedOutputFlags(c *cobra.Command) *BoundedOptions {
	b := &BoundedOptions{}
	BindBoundedOutputFlags(c, b)

	return b
}

// BindBoundedOutputFlags registers the two flags against options the caller
// already holds. AddOutputFlags needs it: pflag binds to an address, so a
// BoundedOptions copied out of a constructor would be a different struct from
// the one the flags write into, and every call would read its zero value.
func BindBoundedOutputFlags(c *cobra.Command, b *BoundedOptions) {
	c.Flags().StringSliceVar(&b.Fields, "fields", nil,
		"Keep only these dotted JSON paths on each item, for instance status.ready. "+
			"apiVersion, kind and metadata.name are always kept")
	c.Flags().IntVar(&b.Limit, "limit", 0,
		"Print at most this many items, after sorting. 0 prints every item")
}

// OutputOptions is the whole output interface of a list-like command: the
// format behind -o and the two flags that bound what it prints.
type OutputOptions struct {
	// Format is the raw -o value, as ParseFormat reads it.
	Format string
	// Bounded is --fields and --limit.
	Bounded BoundedOptions
}

// AddOutputFlags registers -o, --fields and --limit on a command and returns
// the options they write into.
//
// The three are registered together because the MCP server and every command
// have to agree on what the format flag is called: the server forces -o json,
// and a command that spelled it differently would be served in whatever format
// it defaults to and its output would not parse.
func AddOutputFlags(c *cobra.Command) *OutputOptions {
	o := &OutputOptions{}
	c.Flags().StringVarP(&o.Format, "output", "o", string(FormatTable),
		"Output format: one of "+strings.Join(formatNames(), ", "))
	BindBoundedOutputFlags(c, &o.Bounded)

	return o
}

// Print applies the bounds and prints obj in the chosen format.
func (o *OutputOptions) Print(streams genericiooptions.IOStreams, obj runtime.Object) error {
	return PrintBounded(streams, o.Format, o.Bounded, obj)
}

// Validate reports whether these bounds can be applied to a kind, without
// printing anything.
//
// A command calls it before it reads from the cluster. A misspelled --fields
// found afterwards has cost a scan nobody needed, and the caller waited for it
// only to be told it typed the wrong thing.
func (b BoundedOptions) Validate(prototype runtime.Object) error {
	if b.Limit < 0 {
		return NewUsageError("--limit must not be negative, got %d", b.Limit)
	}

	valid, err := FieldPaths(prototype)
	if err != nil {
		return err
	}

	return validateFields(b.Fields, valid)
}

// Validate is Validate on the bounds, for a command holding the whole set.
func (o *OutputOptions) Validate(prototype runtime.Object) error {
	if _, err := ParseFormat(o.Format); err != nil {
		return err
	}

	return o.Bounded.Validate(prototype)
}

// formatNames lists the accepted -o values.
func formatNames() []string {
	names := make([]string, 0, len(formats))
	for _, f := range formats {
		names = append(names, string(f))
	}

	return names
}

// PrintBounded applies --limit and --fields to obj and prints it in the format
// behind -o. Truncation is reported on the error stream and recorded in
// metadata.remainingItemCount, so that neither a reader nor a program can
// mistake a bounded answer for a complete one.
func PrintBounded(streams genericiooptions.IOStreams, format string, b BoundedOptions, obj runtime.Object) error {
	f, err := ParseFormat(format)
	if err != nil {
		return err
	}

	if b.Limit < 0 {
		return NewUsageError("--limit must not be negative, got %d", b.Limit)
	}

	valid, err := FieldPaths(obj)
	if err != nil {
		return err
	}
	if err := validateFields(b.Fields, valid); err != nil {
		return err
	}

	bounded, total, truncated, err := applyLimit(obj, b.Limit)
	if err != nil {
		return err
	}
	if truncated {
		fmt.Fprintf(streams.ErrOut, "note: showing %d of %d items (--limit %d)\n", b.Limit, total, b.Limit)
	}

	if len(b.Fields) == 0 {
		printer, err := NewPrinter(format)
		if err != nil {
			return err
		}

		return printer.PrintObj(bounded, streams.Out)
	}

	switch f {
	case FormatName:
		// A name line is a name: there is nothing left to project away.
		return (&namePrinter{}).PrintObj(bounded, streams.Out)
	case FormatTable, FormatWide:
		return (&TablePrinter{Wide: f == FormatWide, Fields: b.Fields}).PrintObj(bounded, streams.Out)
	case FormatJSON, FormatYAML:
		return printProjected(streams.Out, f, bounded, b.Fields)
	}

	return NewUsageError("unknown output format %q", format)
}

// printProjected renders the projection of obj as JSON or YAML. The projection
// is a plain map rather than a typed object, because a type cannot describe a
// shape the caller chose at run time.
func printProjected(w io.Writer, f Format, obj runtime.Object, fields []string) error {
	projected, err := projectObject(obj, fields)
	if err != nil {
		return err
	}

	var raw []byte
	if f == FormatYAML {
		raw, err = yaml.Marshal(projected)
	} else {
		raw, err = json.MarshalIndent(projected, "", "    ")
		raw = append(raw, '\n')
	}
	if err != nil {
		return fmt.Errorf("rendering the projected output: %w", err)
	}

	if _, err := w.Write(raw); err != nil {
		return fmt.Errorf("writing the projected output: %w", err)
	}

	return nil
}

// FieldPaths returns every dotted JSON path --fields accepts for obj, sorted.
// For a list it is the paths of one item: a caller projects items, never the
// list envelope around them.
func FieldPaths(obj runtime.Object) ([]string, error) {
	t := itemType(reflect.TypeOf(obj))
	if t == nil {
		return nil, fmt.Errorf("%T has no fields to project", obj)
	}

	var paths []string
	collectFieldPaths(t, "", map[reflect.Type]bool{}, &paths)
	sort.Strings(paths)

	return paths, nil
}

// itemType returns the type whose fields --fields names: the element type of
// the Items slice for a list, and the type itself for anything else.
func itemType(t reflect.Type) reflect.Type {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return t
	}

	items, ok := t.FieldByName("items")
	if !ok {
		items, ok = t.FieldByName("Items")
	}
	if !ok || items.Type.Kind() != reflect.Slice {
		return t
	}

	elem := items.Type.Elem()
	for elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}

	return elem
}

// collectFieldPaths walks a struct and appends the JSON path of every leaf.
// Recursion stops at a slice, a map and anything that marshals itself, because
// a path into one of those is a query language rather than a field name.
func collectFieldPaths(t reflect.Type, prefix string, seen map[reflect.Type]bool, out *[]string) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}

	if t == reflect.TypeOf(metav1.ObjectMeta{}) {
		for _, name := range objectMetaPaths {
			*out = append(*out, join(prefix, name))
		}

		return
	}

	// marshalsItself is not consulted at the root. A list kind commonly has a
	// MarshalJSON of its own, to stamp its envelope or to render a nil items as
	// an empty array, and stopping there would leave the kind with no paths at
	// all: every --fields a caller named would come back as unknown.
	if t.Kind() != reflect.Struct || (prefix != "" && marshalsItself(t)) || seen[t] {
		if prefix != "" {
			*out = append(*out, prefix)
		}

		return
	}

	seen[t] = true
	defer delete(seen, t)

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
			collectFieldPaths(field.Type, prefix, seen, out)
			continue
		}
		collectFieldPaths(field.Type, join(prefix, name), seen, out)
	}
}

// jsonName reads the JSON name of a struct field. A field embedded without a
// name of its own is inlined, which is how TypeMeta contributes apiVersion and
// kind at the top level.
func jsonName(field reflect.StructField) (name string, inline bool, skip bool) {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return "", false, true
	}

	name = strings.Split(tag, ",")[0]
	if name == "" {
		if field.Anonymous {
			return "", true, false
		}

		return field.Name, false, false
	}

	return name, false, false
}

// marshalsItself reports whether a type renders as something other than its
// fields, in which case its fields are not paths.
func marshalsItself(t reflect.Type) bool {
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()

	return t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler)
}

// join appends one segment to a dotted path.
func join(prefix, name string) string {
	if prefix == "" {
		return name
	}

	return prefix + "." + name
}

// validateFields rejects a path the kind does not have. A typo in --fields
// would otherwise silently return an item missing the very field the caller
// branches on, which is worse than no answer.
func validateFields(fields, valid []string) error {
	known := make(map[string]bool, len(valid))
	for _, path := range valid {
		known[path] = true
	}

	for _, field := range fields {
		if !known[field] {
			return NewUsageError("unknown --fields path %q: want one of %s", field, strings.Join(valid, ", "))
		}
	}

	return nil
}

// Limitable is implemented by a kind that --limit bounds but that is not a
// list: a report whose repeated part is a named field rather than Items.
//
// A health report is a list of findings and a topology report is a list of
// groups, and both carry a summary and a status around them that a limit must
// not touch. The kind is the only place that knows which of its fields is the
// repeated one, and the only place that can record the remainder where its own
// shape puts it, so it does the keeping and this package does the rest: one
// --limit, one truncation note, one meaning.
type Limitable interface {
	// LimitItems keeps the first n of whatever this kind's --limit bounds,
	// modifying the receiver, and records the remainder in the output. It
	// returns how many there were before.
	//
	// It is called on a deep copy, never on the object the command built, so
	// that printing cannot change what the caller still holds.
	LimitItems(n int) int
}

// applyLimit keeps the first limit items of a list, on a copy, and records how
// many were left behind. It reports the original item count and whether
// anything was dropped.
//
// A kind with no items and no LimitItems cannot honour a limit, and answering
// a bounded request with an unbounded one is worse than refusing it: the
// caller believes it asked for less than it got.
func applyLimit(obj runtime.Object, limit int) (runtime.Object, int, bool, error) {
	if limit <= 0 {
		return obj, 0, false, nil
	}

	if _, ok := obj.(Limitable); ok {
		return limitReport(obj, limit)
	}

	items := itemsField(obj)
	if !items.IsValid() {
		return nil, 0, false, NewUsageError("--limit cannot be used on %s, which is not a list", kindOf(obj))
	}

	total := items.Len()
	if total <= limit {
		return obj, total, false, nil
	}

	clone := obj.DeepCopyObject()
	cloned := itemsField(clone)
	if !cloned.IsValid() || !cloned.CanSet() {
		return nil, 0, false, fmt.Errorf("%T cannot be limited: its copy has no settable Items", obj)
	}
	cloned.Set(cloned.Slice(0, limit))

	// The count belongs on the object as well as on the error stream: a program
	// reading the JSON never sees stderr.
	accessor, err := meta.ListAccessor(clone)
	if err != nil {
		return nil, 0, false, fmt.Errorf("reading the list metadata of %T: %w", obj, err)
	}
	remaining := int64(total - limit)
	accessor.SetRemainingItemCount(&remaining)

	return clone, total, true, nil
}

// limitReport applies a limit through the kind's own LimitItems.
func limitReport(obj runtime.Object, limit int) (runtime.Object, int, bool, error) {
	clone, ok := obj.DeepCopyObject().(Limitable)
	if !ok {
		return nil, 0, false, fmt.Errorf("%T cannot be limited: its copy is not limitable", obj)
	}

	total := clone.LimitItems(limit)
	if total <= limit {
		// Nothing was dropped, so the original is printed rather than the copy:
		// a report that says it was truncated when it was not is worse than one
		// that says nothing.
		return obj, total, false, nil
	}

	printable, ok := clone.(runtime.Object)
	if !ok {
		return nil, 0, false, fmt.Errorf("%T cannot be limited: its copy is not printable", obj)
	}

	return printable, total, true, nil
}

// kindOf names a kind for an error message, falling back to the Go type when
// the object was never stamped.
func kindOf(obj runtime.Object) string {
	if kind := obj.GetObjectKind().GroupVersionKind().Kind; kind != "" {
		return kind
	}

	return fmt.Sprintf("%T", obj)
}

// itemsField returns the settable Items slice of a list kind, or the zero Value
// when obj is not one.
func itemsField(obj runtime.Object) reflect.Value {
	v := reflect.ValueOf(obj)
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return reflect.Value{}
	}

	items := v.Elem().FieldByName("Items")
	if !items.IsValid() || items.Kind() != reflect.Slice {
		return reflect.Value{}
	}

	return items
}

// projectObject renders obj as JSON and keeps only the requested paths of each
// item, plus the paths no projection may drop. The list envelope itself is kept
// whole, so that metadata.remainingItemCount survives the projection.
func projectObject(obj runtime.Object, fields []string) (any, error) {
	raw, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("marshalling %T before projecting it: %w", obj, err)
	}

	// UseNumber keeps an int64 that cannot be held exactly by a float64 from
	// being rewritten on its way through the projection.
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()

	var top map[string]any
	if err := decoder.Decode(&top); err != nil {
		return nil, fmt.Errorf("reading %T before projecting it: %w", obj, err)
	}

	items, ok := top["items"].([]any)
	if !ok {
		return projectMap(top, fields), nil
	}

	projected := make([]any, 0, len(items))
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			projected = append(projected, item)
			continue
		}
		projected = append(projected, projectMap(m, fields))
	}
	top["items"] = projected

	return top, nil
}

// projectMap copies the requested paths, and the ones that are always kept,
// from src into a fresh map. A path that src does not carry is left out rather
// than written as a null: an absent field and a null field mean different
// things to whoever reads the result.
func projectMap(src map[string]any, fields []string) map[string]any {
	out := map[string]any{}
	for _, path := range alwaysKept {
		copyPath(src, out, strings.Split(path, "."))
	}
	for _, path := range fields {
		copyPath(src, out, strings.Split(path, "."))
	}

	return out
}

// copyPath copies one dotted path from src to dst, creating the objects on the
// way down.
func copyPath(src, dst map[string]any, segments []string) {
	head := segments[0]
	value, ok := src[head]
	if !ok {
		return
	}

	if len(segments) == 1 {
		dst[head] = value

		return
	}

	child, ok := value.(map[string]any)
	if !ok {
		return
	}

	nested, ok := dst[head].(map[string]any)
	if !ok {
		nested = map[string]any{}
		dst[head] = nested
	}
	copyPath(child, nested, segments[1:])
}

// keptColumns reports which of the paths a table column may render survive
// --fields.
func keptColumns(fields []string) map[string]bool {
	kept := make(map[string]bool, len(fields)+len(alwaysKept))
	for _, path := range alwaysKept {
		kept[path] = true
	}
	for _, path := range fields {
		kept[path] = true
	}

	return kept
}
