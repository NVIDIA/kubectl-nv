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
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"sigs.k8s.io/yaml"
)

// threeNodes is a list long enough that a limit of two leaves something behind,
// in the order a command would have sorted it into.
func threeNodes(t *testing.T) *gpuNodeList {
	t.Helper()

	return &gpuNodeList{
		TypeMeta: TypeMetaFor(KindGPUNodeList),
		Items: []gpuNode{
			{
				TypeMeta:   TypeMetaFor(KindGPUNode),
				ObjectMeta: metav1.ObjectMeta{Name: "node-a", Labels: map[string]string{"lane": "one"}},
				Spec:       gpuNodeSpec{Driver: "575.51"},
				Status:     gpuNodeStatus{GPUs: 4},
			},
			{
				TypeMeta:   TypeMetaFor(KindGPUNode),
				ObjectMeta: metav1.ObjectMeta{Name: "node-b"},
				Spec:       gpuNodeSpec{Driver: "580.65"},
				Status:     gpuNodeStatus{GPUs: 8},
			},
			{
				TypeMeta:   TypeMetaFor(KindGPUNode),
				ObjectMeta: metav1.ObjectMeta{Name: "node-c"},
				Spec:       gpuNodeSpec{Driver: "580.65"},
				Status:     gpuNodeStatus{GPUs: 2},
			},
		},
	}
}

// printBounded runs the helper over a pair of buffers and returns what each
// stream received.
func printBounded(t *testing.T, format string, b BoundedOptions, obj *gpuNodeList) (string, string, error) {
	t.Helper()

	var out, errOut bytes.Buffer
	streams := genericiooptions.IOStreams{Out: &out, ErrOut: &errOut}
	err := PrintBounded(streams, format, b, obj)

	return out.String(), errOut.String(), err
}

// decodedItems is the items array of a printed list, as a consumer would read it.
type decodedItems []map[string]any

// decodeItems reads the items of a printed JSON list.
func decodeItems(t *testing.T, out string) decodedItems {
	t.Helper()

	var decoded struct {
		Items decodedItems `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decoding the printed list: %v\n%s", err, out)
	}

	return decoded.Items
}

// names returns the metadata.name of every item, in the printed order. The
// order is part of the contract: --limit keeps the first N after the sort, so a
// test that compared sets would not notice the wrong N being kept.
func (d decodedItems) names(t *testing.T) []string {
	t.Helper()

	out := make([]string, 0, len(d))
	for _, item := range d {
		meta, ok := item["metadata"].(map[string]any)
		if !ok {
			t.Fatalf("item has no metadata object: %v", item)
		}
		name, ok := meta["name"].(string)
		if !ok {
			t.Fatalf("item has no metadata.name: %v", item)
		}
		out = append(out, name)
	}

	return out
}

// byName returns the one item with that name, failing when it is absent.
func (d decodedItems) byName(t *testing.T, name string) map[string]any {
	t.Helper()

	for _, item := range d {
		meta, ok := item["metadata"].(map[string]any)
		if ok && meta["name"] == name {
			return item
		}
	}
	t.Fatalf("no item named %q in %v", name, d.names(t))

	return nil
}

// remainingItemCount reads metadata.remainingItemCount of a printed list.
func remainingItemCount(t *testing.T, out string) *int64 {
	t.Helper()

	var decoded struct {
		Metadata struct {
			RemainingItemCount *int64 `json:"remainingItemCount"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decoding the list metadata: %v\n%s", err, out)
	}

	return decoded.Metadata.RemainingItemCount
}

func TestAddBoundedOutputFlagsRegistersBothFlags(t *testing.T) {
	t.Parallel()

	c := &cobra.Command{Use: "list"}
	b := AddBoundedOutputFlags(c)
	if b == nil {
		t.Fatal("AddBoundedOutputFlags returned nil, want the options the flags write into")
	}

	limit := c.Flags().Lookup("limit")
	if limit == nil {
		t.Fatal("--limit was not registered")
	}
	if limit.DefValue != "0" {
		t.Errorf("--limit defaults to %q, want %q: the default has to be no limit", limit.DefValue, "0")
	}

	fields := c.Flags().Lookup("fields")
	if fields == nil {
		t.Fatal("--fields was not registered")
	}
	if fields.DefValue != "[]" {
		t.Errorf("--fields defaults to %q, want %q: the default has to be every field", fields.DefValue, "[]")
	}

	if err := c.ParseFlags([]string{"--limit", "2", "--fields", "status.gpus,spec.driver"}); err != nil {
		t.Fatalf("parsing the flags: %v", err)
	}
	if b.Limit != 2 {
		t.Errorf("Limit is %d, want 2", b.Limit)
	}
	if want := []string{"status.gpus", "spec.driver"}; !reflect.DeepEqual(b.Fields, want) {
		t.Errorf("Fields is %v, want %v", b.Fields, want)
	}
}

// Every format has to honour --limit. A format that quietly prints everything
// is worse than no flag at all: the caller believes the output is bounded.
func TestLimitIsHonouredByEveryFormat(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		format  string
		want    []string
		printed func(t *testing.T, out string) []string
	}{
		{
			format: "json",
			want:   []string{"node-a", "node-b"},
			printed: func(t *testing.T, out string) []string {
				t.Helper()
				return decodeItems(t, out).names(t)
			},
		},
		{
			format: "yaml",
			want:   []string{"node-a", "node-b"},
			printed: func(t *testing.T, out string) []string {
				t.Helper()
				var decoded struct {
					Items decodedItems `json:"items"`
				}
				if err := yaml.Unmarshal([]byte(out), &decoded); err != nil {
					t.Fatalf("decoding the printed YAML: %v\n%s", err, out)
				}
				return decoded.Items.names(t)
			},
		},
		{
			format: "name",
			want:   []string{"gpunode/node-a", "gpunode/node-b"},
			printed: func(t *testing.T, out string) []string {
				t.Helper()
				return strings.Split(strings.TrimRight(out, "\n"), "\n")
			},
		},
		{
			format: "table",
			want:   []string{"node-a", "node-b"},
			printed: func(t *testing.T, out string) []string {
				t.Helper()
				_, rows := splitTable(t, out)
				return firstColumn(t, rows)
			},
		},
		{
			format: "wide",
			want:   []string{"node-a", "node-b"},
			printed: func(t *testing.T, out string) []string {
				t.Helper()
				_, rows := splitTable(t, out)
				return firstColumn(t, rows)
			},
		},
	} {
		t.Run(tc.format, func(t *testing.T) {
			t.Parallel()

			out, errOut, err := printBounded(t, tc.format, BoundedOptions{Limit: 2}, threeNodes(t))
			if err != nil {
				t.Fatalf("printing as %s: %v", tc.format, err)
			}
			if got := tc.printed(t, out); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("%s printed %v under --limit 2, want %v", tc.format, got, tc.want)
			}
			if want := "note: showing 2 of 3 items (--limit 2)\n"; errOut != want {
				t.Errorf("%s wrote %q to the error stream, want %q: truncation has to be visible",
					tc.format, errOut, want)
			}
		})
	}
}

// firstColumn returns the first cell of every table row.
func firstColumn(t *testing.T, rows []string) []string {
	t.Helper()

	out := make([]string, 0, len(rows))
	for _, row := range rows {
		cells := strings.Fields(row)
		if len(cells) == 0 {
			t.Fatalf("empty table row in %v", rows)
		}
		out = append(out, cells[0])
	}

	return out
}

// Truncation has to be visible to a machine too, not only to whoever is reading
// the error stream.
func TestLimitRecordsWhatWasLeftBehind(t *testing.T) {
	t.Parallel()

	out, _, err := printBounded(t, "json", BoundedOptions{Limit: 2}, threeNodes(t))
	if err != nil {
		t.Fatalf("printing: %v", err)
	}

	remaining := remainingItemCount(t, out)
	if remaining == nil {
		t.Fatal("metadata.remainingItemCount is absent, want it to say how many items were dropped")
	}
	if *remaining != 1 {
		t.Errorf("metadata.remainingItemCount is %d, want 1", *remaining)
	}
}

func TestLimitAtOrAboveTheItemCountChangesNothing(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{0, 3, 9} {
		out, errOut, err := printBounded(t, "json", BoundedOptions{Limit: limit}, threeNodes(t))
		if err != nil {
			t.Fatalf("--limit %d: %v", limit, err)
		}
		if errOut != "" {
			t.Errorf("--limit %d wrote %q to the error stream, want nothing: no truncation happened", limit, errOut)
		}
		want := []string{"node-a", "node-b", "node-c"}
		if got := decodeItems(t, out).names(t); !reflect.DeepEqual(got, want) {
			t.Errorf("--limit %d printed %v, want %v", limit, got, want)
		}
		if remaining := remainingItemCount(t, out); remaining != nil {
			t.Errorf("--limit %d set metadata.remainingItemCount to %d, want it absent", limit, *remaining)
		}
	}
}

func TestNegativeLimitIsAUsageError(t *testing.T) {
	t.Parallel()

	_, _, err := printBounded(t, "json", BoundedOptions{Limit: -1}, threeNodes(t))
	if err == nil {
		t.Fatal("--limit -1 was accepted, want a usage error")
	}
	if want := "--limit must not be negative, got -1"; err.Error() != want {
		t.Errorf("error is %q, want %q", err.Error(), want)
	}
	if got := ExitCodeFor(err); got != ExitUsage {
		t.Errorf("exit code is %d, want %d", got, ExitUsage)
	}
}

// The projection keeps the three fields a consumer needs to know what it is
// looking at, whether or not it asked for them.
func TestFieldsProjectsItemsAndKeepsTheIdentity(t *testing.T) {
	t.Parallel()

	out, _, err := printBounded(t, "json", BoundedOptions{Fields: []string{"status.gpus"}}, threeNodes(t))
	if err != nil {
		t.Fatalf("printing: %v", err)
	}

	var decoded struct {
		APIVersion string       `json:"apiVersion"`
		Kind       string       `json:"kind"`
		Items      decodedItems `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("decoding the projected list: %v\n%s", err, out)
	}
	if decoded.APIVersion != APIVersion || decoded.Kind != KindGPUNodeList {
		t.Errorf("the list envelope is %s/%s, want %s/%s",
			decoded.APIVersion, decoded.Kind, APIVersion, KindGPUNodeList)
	}
	if len(decoded.Items) != 3 {
		t.Fatalf("got %d items, want 3", len(decoded.Items))
	}

	want := map[string]any{
		"apiVersion": APIVersion,
		"kind":       KindGPUNode,
		"metadata":   map[string]any{"name": "node-a"},
		"status":     map[string]any{"gpus": float64(4)},
	}
	if got := decoded.Items.byName(t, "node-a"); !reflect.DeepEqual(got, want) {
		t.Errorf("projected item is %v, want %v: only the requested field and the identity may survive", got, want)
	}
}

func TestFieldsAcceptsSeveralPathsIncludingAlwaysKeptOnes(t *testing.T) {
	t.Parallel()

	b := BoundedOptions{Fields: []string{"spec.driver", "metadata.labels", "metadata.name"}}
	out, _, err := printBounded(t, "json", b, threeNodes(t))
	if err != nil {
		t.Fatalf("printing: %v", err)
	}

	items := decodeItems(t, out)
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	want := map[string]any{
		"apiVersion": APIVersion,
		"kind":       KindGPUNode,
		"metadata":   map[string]any{"name": "node-a", "labels": map[string]any{"lane": "one"}},
		"spec":       map[string]any{"driver": "575.51"},
	}
	if got := items.byName(t, "node-a"); !reflect.DeepEqual(got, want) {
		t.Errorf("projected item is %v, want %v", got, want)
	}

	// node-b carries no labels, so the projection must leave the key out rather
	// than invent a null for it.
	wantMeta := map[string]any{"name": "node-b"}
	if got := items.byName(t, "node-b")["metadata"]; !reflect.DeepEqual(got, wantMeta) {
		t.Errorf("node-b metadata is %v, want %v: an absent field must not become a null", got, wantMeta)
	}
}

func TestFieldsRestrictsTheTableColumns(t *testing.T) {
	t.Parallel()

	out, _, err := printBounded(t, "wide", BoundedOptions{Fields: []string{"spec.driver"}}, threeNodes(t))
	if err != nil {
		t.Fatalf("printing: %v", err)
	}

	header, rows := splitTable(t, out)
	if want := []string{"NAME", "DRIVER"}; !reflect.DeepEqual(strings.Fields(header), want) {
		t.Errorf("header is %v, want %v: the table has to drop the columns nobody asked for, keeping the name",
			strings.Fields(header), want)
	}
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	if want := []string{"node-a", "575.51"}; !reflect.DeepEqual(strings.Fields(rows[0]), want) {
		t.Errorf("first row is %v, want %v", strings.Fields(rows[0]), want)
	}
}

func TestFieldsLeavesNameOutputAlone(t *testing.T) {
	t.Parallel()

	out, _, err := printBounded(t, "name", BoundedOptions{Fields: []string{"spec.driver"}}, threeNodes(t))
	if err != nil {
		t.Fatalf("printing: %v", err)
	}
	if want := "gpunode/node-a\ngpunode/node-b\ngpunode/node-c\n"; out != want {
		t.Errorf("name output is %q, want %q", out, want)
	}
}

func TestNoFieldsPrintsTheWholeObject(t *testing.T) {
	t.Parallel()

	out, _, err := printBounded(t, "json", BoundedOptions{}, threeNodes(t))
	if err != nil {
		t.Fatalf("printing: %v", err)
	}

	items := decodeItems(t, out)
	if len(items) != 3 {
		t.Fatalf("got %d items, want 3", len(items))
	}
	node := items.byName(t, "node-a")
	if want := (map[string]any{"driver": "575.51"}); !reflect.DeepEqual(node["spec"], want) {
		t.Errorf("spec is %v, want %v", node["spec"], want)
	}
	if want := (map[string]any{"gpus": float64(4)}); !reflect.DeepEqual(node["status"], want) {
		t.Errorf("status is %v, want %v", node["status"], want)
	}
}

// A field name nobody can spell right is the common case, so the error has to
// say what the valid names are rather than only that this one was not.
func TestUnknownFieldIsAUsageErrorListingTheValidOnes(t *testing.T) {
	t.Parallel()

	_, _, err := printBounded(t, "json", BoundedOptions{Fields: []string{"status.redy"}}, threeNodes(t))
	if err == nil {
		t.Fatal("an unknown --fields path was accepted")
	}
	want := "unknown --fields path \"status.redy\": want one of " +
		"apiVersion, kind, metadata.annotations, metadata.creationTimestamp, metadata.labels, " +
		"metadata.name, metadata.namespace, spec.driver, status.gpus"
	if err.Error() != want {
		t.Errorf("error is %q,\nwant %q", err.Error(), want)
	}
	if got := ExitCodeFor(err); got != ExitUsage {
		t.Errorf("exit code is %d, want %d", got, ExitUsage)
	}
}

func TestUnknownFieldIsRejectedForEveryFormat(t *testing.T) {
	t.Parallel()

	for _, format := range []string{"json", "yaml", "name", "table", "wide"} {
		_, _, err := printBounded(t, format, BoundedOptions{Fields: []string{"nope"}}, threeNodes(t))
		if err == nil {
			t.Errorf("%s accepted an unknown --fields path", format)
			continue
		}
		if got := ExitCodeFor(err); got != ExitUsage {
			t.Errorf("%s reported exit code %d for an unknown --fields path, want %d", format, got, ExitUsage)
		}
	}
}

func TestFieldPathsOfAListAreTheItemPaths(t *testing.T) {
	t.Parallel()

	want := []string{
		"apiVersion",
		"kind",
		"metadata.annotations",
		"metadata.creationTimestamp",
		"metadata.labels",
		"metadata.name",
		"metadata.namespace",
		"spec.driver",
		"status.gpus",
	}

	paths, err := FieldPaths(&gpuNodeList{})
	if err != nil {
		t.Fatalf("FieldPaths on the list: %v", err)
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("list paths are %v,\nwant %v", paths, want)
	}

	single, err := FieldPaths(&gpuNode{})
	if err != nil {
		t.Fatalf("FieldPaths on the item: %v", err)
	}
	if !reflect.DeepEqual(single, want) {
		t.Errorf("item paths are %v,\nwant %v: a list and its item kind have to offer the same paths", single, want)
	}
}

func TestLimitAndFieldsComposeOnTheSameRun(t *testing.T) {
	t.Parallel()

	b := BoundedOptions{Limit: 1, Fields: []string{"status.gpus"}}
	out, errOut, err := printBounded(t, "json", b, threeNodes(t))
	if err != nil {
		t.Fatalf("printing: %v", err)
	}
	if want := "note: showing 1 of 3 items (--limit 1)\n"; errOut != want {
		t.Errorf("error stream is %q, want %q", errOut, want)
	}

	items := decodeItems(t, out)
	if want := []string{"node-a"}; !reflect.DeepEqual(items.names(t), want) {
		t.Fatalf("printed %v, want %v", items.names(t), want)
	}
	if want := (map[string]any{"gpus": float64(4)}); !reflect.DeepEqual(items.byName(t, "node-a")["status"], want) {
		t.Errorf("status is %v, want %v", items.byName(t, "node-a")["status"], want)
	}
}

// A single object is not a list: there is nothing to limit, and the projection
// still applies.
func TestBoundedOutputOnASingleObject(t *testing.T) {
	t.Parallel()

	node := &gpuNode{
		TypeMeta:   TypeMetaFor(KindGPUNode),
		ObjectMeta: metav1.ObjectMeta{Name: "node-a"},
		Spec:       gpuNodeSpec{Driver: "575.51"},
		Status:     gpuNodeStatus{GPUs: 4},
	}

	var out, errOut bytes.Buffer
	streams := genericiooptions.IOStreams{Out: &out, ErrOut: &errOut}
	if err := PrintBounded(streams, "json", BoundedOptions{Limit: 1, Fields: []string{"spec.driver"}}, node); err != nil {
		t.Fatalf("printing: %v", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("error stream is %q, want nothing: a single object was never truncated", errOut.String())
	}

	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("decoding the projected object: %v\n%s", err, out.String())
	}
	want := map[string]any{
		"apiVersion": APIVersion,
		"kind":       KindGPUNode,
		"metadata":   map[string]any{"name": "node-a"},
		"spec":       map[string]any{"driver": "575.51"},
	}
	if !reflect.DeepEqual(decoded, want) {
		t.Errorf("projected object is %v, want %v", decoded, want)
	}
}

func TestPrintBoundedRejectsAnUnknownFormat(t *testing.T) {
	t.Parallel()

	_, _, err := printBounded(t, "xml", BoundedOptions{}, threeNodes(t))
	if err == nil {
		t.Fatal("an unknown -o value was accepted")
	}
	if got := ExitCodeFor(err); got != ExitUsage {
		t.Errorf("exit code is %d, want %d", got, ExitUsage)
	}
}
