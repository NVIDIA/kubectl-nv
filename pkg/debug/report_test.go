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

package debug_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/debug"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// TestNewReportStampsTheEnvelope covers the reason the constructor exists. The
// cli-runtime printers read the GroupVersionKind off the struct before any
// marshalling, so a report built as a literal prints as an error at -o json
// and MarshalJSON never gets the chance to fix it.
func TestNewReportStampsTheEnvelope(t *testing.T) {
	t.Parallel()

	r := debug.NewReport("worker-1")

	if got := r.GetObjectKind().GroupVersionKind().Kind; got != output.KindDebugReport {
		t.Errorf("kind off the struct = %q, want %q", got, output.KindDebugReport)
	}
	if got := r.GetObjectKind().GroupVersionKind().GroupVersion().String(); got != output.APIVersion {
		t.Errorf("apiVersion off the struct = %q, want %q", got, output.APIVersion)
	}
	if r.Name != "worker-1" {
		t.Errorf("metadata.name = %q, want worker-1", r.Name)
	}
}

// TestReportMarshalsNothingAsNull pins the guarantee a consumer is given: every
// slice comes off the wire as an array, so ranging over it needs no null check
// first. The zero value is the one that reaches encoding/json by accident.
func TestReportMarshalsNothingAsNull(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(debug.Report{})
	if err != nil {
		t.Fatalf("marshalling a zero Report: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("the marshalled report does not parse: %v\n%s", err, raw)
	}

	if got["apiVersion"] != output.APIVersion {
		t.Errorf("apiVersion = %v, want %q", got["apiVersion"], output.APIVersion)
	}
	if got["kind"] != output.KindDebugReport {
		t.Errorf("kind = %v, want %q", got["kind"], output.KindDebugReport)
	}

	bundle, ok := got["bundle"].(map[string]any)
	if !ok {
		t.Fatalf("bundle is %T, want an object:\n%s", got["bundle"], raw)
	}
	for _, key := range []string{"skipped", "errored"} {
		if _, isArray := bundle[key].([]any); !isArray {
			t.Errorf("bundle.%s is %#v, want an array", key, bundle[key])
		}
	}

	status, ok := got["status"].(map[string]any)
	if !ok {
		t.Fatalf("status is %T, want an object:\n%s", got["status"], raw)
	}
	if _, isArray := status["findings"].([]any); !isArray {
		t.Errorf("status.findings is %#v, want an array", status["findings"])
	}
}

// manifestWith builds a bundle index holding the given collector statuses, in
// the order a gather would have run them.
func manifestWith(statuses ...[2]string) *mustgather.Manifest {
	m := &mustgather.Manifest{}
	for _, s := range statuses {
		m.Collectors = append(m.Collectors, mustgather.CollectorStatus{Name: s[0], Status: s[1]})
	}

	return m
}

// TestSummariseBundleCountsEachStatusApart is the difference between a bundle
// that holds the driver evidence and one that does not. A summary that counted
// a skipped collector as ok would report a gather that was refused every exec
// as a complete one, which is the single thing a reader of this report needs
// to see.
func TestSummariseBundleCountsEachStatusApart(t *testing.T) {
	t.Parallel()

	m := manifestWith(
		[2]string{"nodes", "ok"},
		[2]string{"gpu-nodes", "ok"},
		[2]string{"bug-report", "skipped:--no-exec"},
		[2]string{"kubevirt", "skipped:KubeVirt is not installed"},
		[2]string{"daemonsets", "error:the API server is having a bad day"},
	)
	m.Files = []mustgather.FileEntry{
		{Path: "nodes.yaml", Size: 120},
		{Path: "gpu_nodes.yaml", Size: 80},
	}

	got := debug.SummariseBundle("/tmp/bundle", m)

	want := debug.Bundle{
		Directory:  "/tmp/bundle",
		Manifest:   "/tmp/bundle/manifest.json",
		Collectors: debug.CollectorCounts{OK: 2, Skipped: 2, Error: 1},
		Skipped: []debug.CollectorOutcome{
			{Name: "bug-report", Reason: "--no-exec"},
			{Name: "kubevirt", Reason: "KubeVirt is not installed"},
		},
		Errored: []debug.CollectorOutcome{
			{Name: "daemonsets", Reason: "the API server is having a bad day"},
		},
		Files: 2,
		Bytes: 200,
	}

	if !reflect.DeepEqual(got, want) {
		t.Errorf("SummariseBundle =\n%#v\nwant\n%#v", got, want)
	}
}

// TestSummariseBundleKeepsTheGatherOrder pins the order the outcomes are
// listed in. A map iteration here would make two runs over the same bundle
// print different reports, and a diff between two gathers useless.
func TestSummariseBundleKeepsTheGatherOrder(t *testing.T) {
	t.Parallel()

	m := manifestWith(
		[2]string{"zeta", "skipped:last in the registry"},
		[2]string{"alpha", "skipped:first in the registry"},
	)

	got := debug.SummariseBundle("/tmp/bundle", m)

	want := []debug.CollectorOutcome{
		{Name: "zeta", Reason: "last in the registry"},
		{Name: "alpha", Reason: "first in the registry"},
	}
	if !reflect.DeepEqual(got.Skipped, want) {
		t.Errorf("skipped = %#v, want the registry order %#v", got.Skipped, want)
	}
}

// TestSummariseBundleWithoutAManifest covers the gather that failed before it
// could index itself. The report still has to say which directory was written,
// because that directory is what the user has to send.
func TestSummariseBundleWithoutAManifest(t *testing.T) {
	t.Parallel()

	got := debug.SummariseBundle("/tmp/bundle", nil)

	if got.Directory != "/tmp/bundle" {
		t.Errorf("directory = %q, want /tmp/bundle", got.Directory)
	}
	if got.Manifest != "" {
		t.Errorf("manifest = %q, want it empty when the bundle has no index", got.Manifest)
	}
	if got.Skipped == nil || got.Errored == nil {
		t.Errorf("skipped = %#v and errored = %#v, want both empty rather than nil",
			got.Skipped, got.Errored)
	}
}
