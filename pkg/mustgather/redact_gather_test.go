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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

// --redact is a flag of the gather itself, so both commands that run one offer
// it: a support engineer who learned it on must-gather must find it on debug.
func TestRedactIsAGatherFlag(t *testing.T) {
	t.Parallel()

	fs := pflag.NewFlagSet("gather", pflag.ContinueOnError)
	opts := &Options{}
	AddGatherFlags(fs, opts)

	flag := fs.Lookup("redact")
	if flag == nil {
		t.Fatal("the gather declares no --redact")
	}
	if flag.Value.Type() != "bool" {
		t.Errorf("--redact is a %s, want a bool", flag.Value.Type())
	}
	if flag.DefValue != "false" {
		t.Errorf("--redact defaults to %q: a bundle is only redacted when it was asked for", flag.DefValue)
	}

	if err := fs.Parse([]string{"--redact"}); err != nil {
		t.Fatalf("parsing --redact: %v", err)
	}
	if !opts.Redact {
		t.Error("--redact did not reach the options")
	}
}

// A gather that was asked to redact produces a bundle holding none of the
// node's identities, and says so in its index.
func TestAGatherWithRedactWritesARedactedBundle(t *testing.T) {
	dir, names, err := runGather(t, &Options{Redact: true, ToolVersion: "v1.2.3"}, twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := readManifest(t, dir)
	if !m.Flags.Redact {
		t.Error("the manifest does not record that the bundle is redacted")
	}

	for _, name := range names {
		if name == ManifestFileName {
			continue
		}
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("reading %s: %v", name, rerr)
		}
		for _, node := range []string{"gpu-node-1", "gpu-node-2"} {
			if strings.Contains(string(raw), node) {
				t.Errorf("%s still names %s", name, node)
			}
		}
	}
}

// The checksums have to be the redacted files'. An index taken before the
// rewrite would make every file of the bundle fail its own integrity check,
// which is the one thing the index exists for.
func TestARedactedBundleIsIndexedAfterTheRewrite(t *testing.T) {
	dir, _, err := runGather(t, &Options{Redact: true, ToolVersion: "v1.2.3"}, twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	m := readManifest(t, dir)
	if len(m.Files) == 0 {
		t.Fatal("the manifest indexes nothing, so this compared nothing")
	}
	for _, f := range m.Files {
		raw, rerr := os.ReadFile(filepath.Join(dir, f.Path))
		if rerr != nil {
			t.Fatalf("reading %s: %v", f.Path, rerr)
		}
		if f.Size != int64(len(raw)) {
			t.Errorf("%s: the manifest says %d bytes, the redacted file is %d", f.Path, f.Size, len(raw))
		}
		sum := sha256.Sum256(raw)
		if want := hex.EncodeToString(sum[:]); f.SHA256 != want {
			t.Errorf("%s: the manifest says %s, the redacted file hashes to %s", f.Path, f.SHA256, want)
		}
	}
}

// Without the flag nothing is rewritten. A gather that redacted by default
// would quietly change what every existing reader of a bundle sees.
func TestAGatherWithoutRedactLeavesTheBundleAlone(t *testing.T) {
	dir, _, err := runGather(t, &Options{ToolVersion: "v1.2.3"}, twoNodeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if m := readManifest(t, dir); m.Flags.Redact {
		t.Error("the manifest says the bundle is redacted and nothing asked for it")
	}
	raw, rerr := os.ReadFile(filepath.Join(dir, "nodes.yaml"))
	if rerr != nil {
		t.Fatalf("reading nodes.yaml: %v", rerr)
	}
	if !strings.Contains(string(raw), "gpu-node-1") {
		t.Error("nodes.yaml no longer names gpu-node-1 and nothing asked for a redaction")
	}
}
