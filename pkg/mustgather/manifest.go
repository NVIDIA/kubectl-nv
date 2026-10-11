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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// ManifestFileName is the index of a bundle. It is written last, so that it
// can hash everything else.
const ManifestFileName = "manifest.json"

// BundleSchemaVersion is the version of the manifest's own shape. A reader
// checks it before trusting any other field, and it moves when a field changes
// meaning rather than when a collector is added.
const BundleSchemaVersion = "1"

// Manifest is the index of a must-gather bundle: what produced it, what was
// asked of it, which collectors ran, and what every file in it hashes to.
//
// A bundle travels to NVIDIA support as an archive. Without this, a file that
// arrived truncated and a collector that was never allowed to run look the
// same from the outside: a file that is not there.
type Manifest struct {
	metav1.TypeMeta `json:",inline" yaml:",inline"`

	// BundleSchemaVersion is the version of this document's shape.
	BundleSchemaVersion string `json:"bundleSchemaVersion" yaml:"bundleSchemaVersion"`
	// ToolVersion is the kubectl-nv that collected the bundle.
	ToolVersion string `json:"toolVersion" yaml:"toolVersion"`
	// Context is the kubeconfig context the bundle was collected through.
	Context string `json:"context" yaml:"context"`
	// Timestamp is when the bundle was written, in UTC.
	Timestamp string `json:"timestamp" yaml:"timestamp"`
	// Flags records what was asked of the gather, so that a bundle missing a
	// node's evidence can be read as narrowed rather than as incomplete.
	Flags ManifestFlags `json:"flags" yaml:"flags"`
	// Collectors is one entry per collector of the registry, in the order they
	// ran.
	Collectors []CollectorStatus `json:"collectors" yaml:"collectors"`
	// Files is every file of the bundle except this one.
	Files []FileEntry `json:"files" yaml:"files"`
}

// ManifestFlags is what the user asked of the gather.
type ManifestFlags struct {
	// Node is the --node the gather was narrowed to, empty for the whole
	// cluster.
	Node string `json:"node" yaml:"node"`
	// Since is the --since log window, empty for the whole log.
	Since string `json:"since" yaml:"since"`
	// NoExec reports whether --no-exec kept the gather out of the driver pods.
	NoExec bool `json:"noExec" yaml:"noExec"`
}

// CollectorStatus is what one collector did.
type CollectorStatus struct {
	// Name is the collector's name in the registry.
	Name string `json:"name" yaml:"name"`
	// Status is "ok", "skipped:<reason>" or "error:<text>".
	Status string `json:"status" yaml:"status"`
	// Artifacts are the files this collector wrote, by name.
	Artifacts []string `json:"artifacts" yaml:"artifacts"`
}

// FileEntry is one file of the bundle, with what it should hash to.
type FileEntry struct {
	// Path is the file's name within the bundle.
	Path string `json:"path" yaml:"path"`
	// Size is its length in bytes.
	Size int64 `json:"size" yaml:"size"`
	// SHA256 is its digest, hex encoded.
	SHA256 string `json:"sha256" yaml:"sha256"`
}

// writeManifest indexes the bundle. It runs last, after every other file is on
// disk and closed, because it hashes what it finds rather than what the
// collectors meant to write.
func (g *Gatherer) writeManifest() error {
	// The log files are still open and are written to right up to this point,
	// so they are flushed before being hashed. A digest taken over a file that
	// is about to grow is a digest that will not check out.
	g.syncLogs()

	files, err := g.indexBundle()
	if err != nil {
		return err
	}

	manifest := Manifest{
		TypeMeta:            output.TypeMetaFor(output.KindBundleManifest),
		BundleSchemaVersion: BundleSchemaVersion,
		ToolVersion:         g.opts.ToolVersion,
		Context:             g.opts.Context,
		Timestamp:           time.Now().UTC().Format(time.RFC3339),
		Flags: ManifestFlags{
			Node:   g.opts.Node,
			NoExec: g.opts.NoExec,
		},
		Collectors: g.results,
		Files:      files,
	}
	if g.opts.Since > 0 {
		manifest.Flags.Since = g.opts.Since.String()
	}
	if manifest.Collectors == nil {
		manifest.Collectors = []CollectorStatus{}
	}

	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("marshalling the bundle manifest: %w", err)
	}
	data = append(data, '\n')

	// Written straight rather than through createArtifact: the manifest is the
	// index of the bundle, not part of what it indexes, and counting it as a
	// collected artifact would turn a gather that collected nothing else into
	// one that looks partly successful.
	path := filepath.Join(g.opts.ArtifactDir, ManifestFileName)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("writing %s: %w", ManifestFileName, err)
	}
	return nil
}

// syncLogs flushes the gather's own log files, which are open until Close.
func (g *Gatherer) syncLogs() {
	for _, file := range []*os.File{g.logFile, g.errLogFile} {
		if file != nil {
			_ = file.Sync()
		}
	}
}

// indexBundle hashes every file of the bundle except the manifest itself, in a
// stable order so that two runs over the same bundle produce the same index.
func (g *Gatherer) indexBundle() ([]FileEntry, error) {
	entries, err := os.ReadDir(g.opts.ArtifactDir)
	if err != nil {
		return nil, fmt.Errorf("reading the bundle directory: %w", err)
	}

	files := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == ManifestFileName {
			continue
		}
		size, digest, err := hashFile(filepath.Join(g.opts.ArtifactDir, entry.Name()))
		if err != nil {
			return nil, err
		}
		files = append(files, FileEntry{Path: entry.Name(), Size: size, SHA256: digest})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	return files, nil
}

// hashFile returns a file's length and its SHA-256, read in one pass so that
// the two cannot describe different versions of it.
func hashFile(path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("opening %s to index it: %w", filepath.Base(path), err)
	}
	defer file.Close()

	hash := sha256.New()
	size, err := io.Copy(hash, file)
	if err != nil {
		return 0, "", fmt.Errorf("reading %s to index it: %w", filepath.Base(path), err)
	}

	return size, hex.EncodeToString(hash.Sum(nil)), nil
}
