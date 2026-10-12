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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// The kubeconfig context a gather records. It names the cluster, so it is an
// identity like any other.
const plantedContext = "kind-acme-prod-east"

// redactedGather runs a gather with --redact over the given cluster and
// returns the bundle directory and the names it holds.
func redactedGather(t *testing.T, opts *Options, objects ...runtime.Object) (string, []string) {
	t.Helper()

	if opts.Context == "" {
		opts.Context = plantedContext
	}
	opts.Redact = true
	opts.ToolVersion = "v1.2.3"

	dir, names, err := runGather(t, opts, objects...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	return dir, names
}

// writingSeam returns a pod file fetcher that writes the given bytes, which is
// how a test puts a realistic bug report into the bundle.
func writingSeam(data []byte) PodFileFetcher {
	return func(_ context.Context, _ *v1.Pod, destPath string) error {
		return os.WriteFile(destPath, data, 0o600)
	}
}

// A file named after a node carries that node's name in the directory listing,
// where no rewrite of the contents reaches it. Somebody unpacking the archive
// reads the listing before they open anything.
func TestRedactRenamesTheFilesNamedAfterANode(t *testing.T) {
	_, names := redactedGather(t, &Options{Node: "gpu-node-1"}, twoNodeCluster()...)

	for _, name := range names {
		if strings.Contains(name, "gpu-node-1") || strings.Contains(name, "gpu-node-2") {
			t.Errorf("the bundle holds a file called %q", name)
		}
	}

	// The per-node files are still there, under a pseudonym: renaming them out
	// of existence would be redaction by deletion.
	var found bool
	for _, name := range names {
		if strings.HasPrefix(name, "nvidia-bug-report_") {
			found = true
			if !strings.Contains(name, "node-") {
				t.Errorf("the bug report is called %q, which carries no pseudonym", name)
			}
		}
	}
	if !found {
		t.Fatalf("the bundle holds no bug report at all, so this compared nothing: %v", names)
	}
}

// manifest.json is the first file a reader opens, and every node name the
// gather ever wrote down is in it: what was asked for, which cluster it came
// from, what each collector did and what each file is called.
func TestRedactRewritesTheManifest(t *testing.T) {
	dir, _ := redactedGather(t, &Options{Node: "gpu-node-1"}, twoNodeCluster()...)

	raw := readArtifact(t, dir, ManifestFileName)
	for _, planted := range []string{"gpu-node-1", "gpu-node-2", plantedContext} {
		if strings.Contains(raw, planted) {
			t.Errorf("%s holds %q", ManifestFileName, planted)
		}
	}

	m := readManifest(t, dir)
	if m.Flags.Node == "" || m.Flags.Node == "gpu-node-1" {
		t.Errorf("flags.node is %q: it has to be the node's pseudonym", m.Flags.Node)
	}
	if m.Context == "" || m.Context == plantedContext {
		t.Errorf("context is %q: it has to be a pseudonym", m.Context)
	}

	// Every indexed path has to be a file that is actually there, under the
	// name it is actually under: an index naming a file as it was before the
	// rename is an index whose digests nobody can check.
	if len(m.Files) == 0 {
		t.Fatal("the manifest indexes nothing, so this compared nothing")
	}
	for _, f := range m.Files {
		if _, err := os.Stat(filepath.Join(dir, f.Path)); err != nil {
			t.Errorf("the manifest indexes %s, which the bundle does not hold: %v", f.Path, err)
		}
	}
}

// gzippedBugReport is what a real nvidia-bug-report.sh produces: thousands of
// lines of kernel log, with the node's identity scattered through them.
func gzippedBugReport(t *testing.T, host, ip, token string) []byte {
	t.Helper()

	var plain bytes.Buffer
	plain.WriteString("nvidia-bug-report.sh output\n")
	for i := range 3000 {
		switch i % 4 {
		case 0:
			fmt.Fprintf(&plain, "[%6d.123456] nvidia-nvlink: probe on %s\n", i, host)
		case 1:
			fmt.Fprintf(&plain, "[%6d.123456] eth0: link up, address %s\n", i, ip)
		case 2:
			fmt.Fprintf(&plain, "[%6d.123456] fabricmanager: GET /v1 Authorization: Bearer %s\n", i, token)
		default:
			fmt.Fprintf(&plain, "[%6d.123456] NVRM: loading driver 550.54.15\n", i)
		}
	}

	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := w.Write(plain.Bytes()); err != nil {
		t.Fatalf("gzipping the bug report: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the gzip writer: %v", err)
	}

	return out.Bytes()
}

// A bug report arrives gzipped, so a byte rewrite of it changes nothing a
// reader can see and breaks the file for whoever unpacks it. It has to be
// gunzipped, rewritten and gzipped again, and still gunzip at the far end.
func TestRedactRewritesInsideAGzippedBugReport(t *testing.T) {
	const host = "gpu-node-1"
	const ip = "10.42.7.11"
	const token = "eyJhbGciOiJIUzI1NiJ9.plantedbugreporttoken"

	dir, names := redactedGather(t,
		&Options{FetchPodFile: writingSeam(gzippedBugReport(t, host, ip, token))},
		twoNodeCluster()...)

	var reports []string
	for _, name := range names {
		if strings.HasPrefix(name, "nvidia-bug-report_") {
			reports = append(reports, name)
		}
	}
	if len(reports) == 0 {
		t.Fatalf("the bundle holds no bug report, so this compared nothing: %v", names)
	}

	for _, name := range reports {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		if len(raw) < 2 || raw[0] != 0x1f || raw[1] != 0x8b {
			t.Fatalf("%s is no longer gzipped", name)
		}

		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s does not gunzip: %v", name, err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("%s does not gunzip cleanly: %v", name, err)
		}
		if err := zr.Close(); err != nil {
			t.Errorf("%s has a bad gzip trailer: %v", name, err)
		}

		text := string(plain)
		for _, planted := range []string{host, ip, token} {
			if strings.Contains(text, planted) {
				t.Errorf("the gunzipped %s still holds %q", name, planted)
			}
		}
		// And it is still the file it was: a redaction that emptied it would
		// pass every assertion above.
		if !strings.Contains(text, "NVRM: loading driver 550.54.15") {
			t.Errorf("the gunzipped %s lost its content", name)
		}
		if lines := strings.Count(text, "\n"); lines < 3000 {
			t.Errorf("the gunzipped %s has %d lines, want the 3001 it was written with", name, lines)
		}
	}
}

// A small gzip has to survive the round trip too. The bug report above is
// large enough to span several deflate blocks; this one is not, and a rewrite
// that only ever worked on large input would pass that test and corrupt this.
func TestRedactKeepsASmallGzipValid(t *testing.T) {
	var out bytes.Buffer
	w := gzip.NewWriter(&out)
	if _, err := io.WriteString(w, "probe on gpu-node-1\n"); err != nil {
		t.Fatalf("gzipping: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("closing the gzip writer: %v", err)
	}

	dir, names := redactedGather(t, &Options{FetchPodFile: writingSeam(out.Bytes())}, twoNodeCluster()...)

	for _, name := range names {
		if !strings.HasPrefix(name, "nvidia-bug-report_") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("%s does not gunzip: %v", name, err)
		}
		plain, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("%s does not gunzip cleanly: %v", name, err)
		}
		if strings.Contains(string(plain), "gpu-node-1") {
			t.Errorf("%s still holds the node name", name)
		}
		if !strings.Contains(string(plain), "probe on ") {
			t.Errorf("%s lost its content: %q", name, plain)
		}
	}
}

// dropRecord is how the manifest records a file --redact removed.
type dropRecord struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// manifestDrops reads the dropped list out of the index.
func manifestDrops(t *testing.T, dir string) []dropRecord {
	t.Helper()

	var m struct {
		Dropped []dropRecord `json:"dropped"`
	}
	if err := json.Unmarshal([]byte(readArtifact(t, dir, ManifestFileName)), &m); err != nil {
		t.Fatalf("%s does not parse: %v", ManifestFileName, err)
	}

	return m.Dropped
}

// A file that is neither text nor gzip cannot be rewritten without corrupting
// it, and shipping it unrewritten ships whatever is in it. It is dropped, and
// the manifest says so: a file that silently disappeared is one a reader goes
// looking for and blames on a bad upload.
func TestRedactDropsABinaryFileAndRecordsIt(t *testing.T) {
	// A core dump shaped file: not gzip, not text.
	binary := []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00, 0x00, 0x00, 0xff, 0xfe}

	dir, names := redactedGather(t, &Options{FetchPodFile: writingSeam(binary)}, twoNodeCluster()...)

	for _, name := range names {
		if !strings.HasPrefix(name, "nvidia-bug-report_") {
			continue
		}
		t.Errorf("the bundle still holds the binary %s", name)
	}

	drops := manifestDrops(t, dir)
	if len(drops) == 0 {
		t.Fatalf("the manifest records no dropped file: %s", readArtifact(t, dir, ManifestFileName))
	}
	var named bool
	for _, drop := range drops {
		if strings.HasPrefix(drop.Path, "nvidia-bug-report_") {
			named = true
			if !strings.Contains(drop.Reason, "redact") {
				t.Errorf("the drop reason is %q, which does not say --redact removed it", drop.Reason)
			}
		}
	}
	if !named {
		t.Errorf("the manifest does not name the dropped bug report: %+v", drops)
	}
}

// A gather whose redaction could not finish must not leave the bundle behind.
// Half a redaction is a bundle the user believes is redacted and is not, and
// they send it on.
func TestAFailedRedactionLeavesNoUnredactedBundle(t *testing.T) {
	// A file the redactor cannot read: it is there, it is a regular file, and
	// its mode denies the read.
	seam := func(_ context.Context, _ *v1.Pod, destPath string) error {
		if err := os.WriteFile(destPath, []byte("probe on gpu-node-1\n"), 0o600); err != nil {
			return err
		}

		return os.Chmod(destPath, 0o000)
	}

	opts := &Options{Redact: true, ToolVersion: "v1.2.3", Context: plantedContext, FetchPodFile: seam}
	dir, names, err := runGather(t, opts, twoNodeCluster()...)
	if err == nil {
		t.Fatal("a redaction that could not read a file reported no error")
	}

	// No manifest may claim a redaction that did not happen, and nothing
	// unredacted may be left for somebody to send on.
	for _, name := range names {
		if name == ManifestFileName {
			t.Errorf("the bundle still holds %s after a failed redaction", name)
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			continue
		}
		if strings.Contains(string(raw), "gpu-node-1") {
			t.Errorf("%s survived a failed redaction holding a node name", name)
		}
	}
}
