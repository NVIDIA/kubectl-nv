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

package mustgather

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// customResourceListKinds names the list kind of every custom resource the
// gather reads, which the dynamic fake needs before it will serve a List.
func customResourceListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}:           "ClusterPolicyList",
		{Group: "nvidia.com", Version: "v1alpha1", Resource: "nvidiadrivers"}:       "NVIDIADriverList",
		{Group: "nvidia.com", Version: "v1alpha1", Resource: "gpuclusters"}:         "GPUClusterList",
		{Group: "hco.kubevirt.io", Version: "v1beta1", Resource: "hyperconvergeds"}: "HyperConvergedList",
		{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}:                "KubeVirtList",
		{Group: "resource.k8s.io", Version: "v1", Resource: "deviceclasses"}:        "DeviceClassList",
	}
}

// readManifest parses the bundle index.
func readManifest(t *testing.T, dir string) *Manifest {
	t.Helper()

	data := readArtifact(t, dir, ManifestFileName)
	var m Manifest
	if err := json.Unmarshal([]byte(data), &m); err != nil {
		t.Fatalf("%s does not parse as JSON: %v\n%s", ManifestFileName, err, data)
	}
	return &m
}

// statusOf returns the status the manifest recorded for one collector.
func (m *Manifest) statusOf(t *testing.T, name string) string {
	t.Helper()

	for _, c := range m.Collectors {
		if c.Name == name {
			return c.Status
		}
	}
	names := make([]string, 0, len(m.Collectors))
	for _, c := range m.Collectors {
		names = append(names, c.Name)
	}
	t.Fatalf("the manifest has no entry for collector %q, it has %v", name, names)
	return ""
}

// TestManifestIndexesEveryFile is what makes a bundle something a support
// engineer can trust: a file that arrived truncated, or one that was edited
// between collection and the support ticket, differs from its recorded size and
// digest. The expected values are recomputed here from the files on disk rather
// than taken from the gatherer, so a gatherer that records what it meant to
// write instead of what it wrote fails.
func TestManifestIndexesEveryFile(t *testing.T) {
	dir, names, err := runGather(t, &Options{ToolVersion: "v1.2.3", Context: "kind-kubectl-nv-mokka"},
		twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := readManifest(t, dir)

	indexed := make(map[string]FileEntry, len(m.Files))
	for _, f := range m.Files {
		if _, dup := indexed[f.Path]; dup {
			t.Errorf("the manifest indexes %s twice", f.Path)
		}
		indexed[f.Path] = f
	}

	for _, name := range names {
		if name == ManifestFileName {
			if _, listed := indexed[name]; listed {
				t.Errorf("the manifest indexes itself: its own digest cannot be in it")
			}
			continue
		}

		entry, listed := indexed[name]
		if !listed {
			t.Errorf("the manifest does not index %s", name)
			continue
		}

		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("reading %s: %v", name, rerr)
		}
		if entry.Size != int64(len(raw)) {
			t.Errorf("%s: manifest size = %d, the file is %d bytes", name, entry.Size, len(raw))
		}
		sum := sha256.Sum256(raw)
		if want := hex.EncodeToString(sum[:]); entry.SHA256 != want {
			t.Errorf("%s: manifest sha256 = %q, the file hashes to %q", name, entry.SHA256, want)
		}
	}

	for path := range indexed {
		if !contains(names, path) {
			t.Errorf("the manifest indexes %s, which the bundle does not hold", path)
		}
	}
}

// TestManifestEnvelopeAndFields pins the contract a reader parses the bundle
// through: the kind it switches on, the schema version it checks before
// trusting the rest, and the record of which cluster and which flags produced
// the bundle.
func TestManifestEnvelopeAndFields(t *testing.T) {
	before := time.Now().UTC().Add(-time.Second)

	dir, _, err := runGather(t, &Options{
		ToolVersion: "v1.2.3",
		Context:     "kind-kubectl-nv-mokka",
		Node:        "gpu-node-1",
		Since:       90 * time.Minute,
	}, twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := readManifest(t, dir)

	if m.APIVersion != output.APIVersion {
		t.Errorf("apiVersion = %q, want %q", m.APIVersion, output.APIVersion)
	}
	if m.Kind != output.KindBundleManifest {
		t.Errorf("kind = %q, want %q", m.Kind, output.KindBundleManifest)
	}
	if m.BundleSchemaVersion != BundleSchemaVersion {
		t.Errorf("bundleSchemaVersion = %q, want %q", m.BundleSchemaVersion, BundleSchemaVersion)
	}
	if m.ToolVersion != "v1.2.3" {
		t.Errorf("toolVersion = %q, want %q", m.ToolVersion, "v1.2.3")
	}
	if m.Context != "kind-kubectl-nv-mokka" {
		t.Errorf("context = %q, want %q", m.Context, "kind-kubectl-nv-mokka")
	}

	stamp, perr := time.Parse(time.RFC3339, m.Timestamp)
	if perr != nil {
		t.Fatalf("timestamp %q does not parse as RFC3339: %v", m.Timestamp, perr)
	}
	if stamp.Before(before) || stamp.After(time.Now().UTC().Add(time.Minute)) {
		t.Errorf("timestamp = %q, want the time of this run", m.Timestamp)
	}
	if _, offset := stamp.Zone(); offset != 0 {
		t.Errorf("timestamp %q is not UTC", m.Timestamp)
	}

	if m.Flags.Node != "gpu-node-1" {
		t.Errorf("flags.node = %q, want gpu-node-1", m.Flags.Node)
	}
	if m.Flags.Since != "1h30m0s" {
		t.Errorf("flags.since = %q, want %q", m.Flags.Since, "1h30m0s")
	}
	if m.Flags.NoExec {
		t.Error("flags.noExec is true, but --no-exec was not given")
	}
}

// TestManifestCoversEveryCollector keeps the index of the gather complete. A
// collector that is missing from the manifest is one a reader cannot tell apart
// from a collector that does not exist, which is the whole point of writing the
// file.
func TestManifestCoversEveryCollector(t *testing.T) {
	dir, _, err := runGather(t, &Options{}, twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := readManifest(t, dir)

	recorded := make(map[string]CollectorStatus, len(m.Collectors))
	for _, c := range m.Collectors {
		recorded[c.Name] = c
	}
	for _, c := range Collectors() {
		got, listed := recorded[c.Name]
		if !listed {
			t.Errorf("the manifest has no entry for collector %q", c.Name)
			continue
		}
		if got.Status == "" {
			t.Errorf("collector %q has an empty status", c.Name)
		}
		if got.Status != StatusOK &&
			!strings.HasPrefix(got.Status, "skipped:") &&
			!strings.HasPrefix(got.Status, "error:") {
			t.Errorf("collector %q status = %q, want ok, skipped:<reason> or error:<text>", c.Name, got.Status)
		}
	}
}

// TestManifestArtifactsAreTheFilesThatCollectorWrote keeps the per-collector
// artifact lists tied to the bundle. The list is what a reader uses to find the
// evidence one collector produced, and a name in it that is not in the bundle
// sends them looking for a file that is not there.
func TestManifestArtifactsAreTheFilesThatCollectorWrote(t *testing.T) {
	dir, names, err := runGather(t, &Options{}, twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := readManifest(t, dir)

	claimed := make(map[string]string)
	for _, c := range m.Collectors {
		for _, artifact := range c.Artifacts {
			if !contains(names, artifact) {
				t.Errorf("collector %q claims %s, which the bundle does not hold", c.Name, artifact)
			}
			if other, dup := claimed[artifact]; dup {
				t.Errorf("%s is claimed by both %q and %q", artifact, other, c.Name)
			}
			claimed[artifact] = c.Name
		}
		if c.Status == StatusOK && len(c.Artifacts) == 0 {
			t.Errorf("collector %q succeeded but claims no artifact", c.Name)
		}
	}

	// A bug report that was collected has to be traceable to the collector that
	// collected it; it is the artifact support asks about first.
	var reports []string
	for _, name := range names {
		if strings.HasPrefix(name, "nvidia-bug-report_") {
			reports = append(reports, name)
		}
	}
	sort.Strings(reports)
	if len(reports) == 0 {
		t.Fatalf("the gather collected no bug report at all: %v", names)
	}
	for _, report := range reports {
		if claimed[report] != "bug-report" {
			t.Errorf("%s is claimed by %q, want the bug-report collector", report, claimed[report])
		}
	}
}

// TestManifestIsNotWorldReadable keeps the index under the same mode as the
// rest of the bundle. It names every file and carries the context the bundle
// came from, so it is no less sensitive than what it indexes.
func TestManifestIsNotWorldReadable(t *testing.T) {
	dir, _, err := runGather(t, &Options{}, twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	info, serr := os.Stat(filepath.Join(dir, ManifestFileName))
	if serr != nil {
		t.Fatal(serr)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("%s mode = %04o, want 0600", ManifestFileName, got)
	}
}
