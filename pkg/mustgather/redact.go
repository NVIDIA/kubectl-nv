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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	v1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"sigs.k8s.io/yaml"
)

// redactedValue is what takes the place of secret material. It is a constant
// rather than a pseudonym because nothing is gained by being able to tell two
// passwords apart, and a pseudonym is a handle on the original.
const redactedValue = "REDACTED"

// pseudonymDigits is how much of the HMAC a pseudonym carries. Twenty hex
// digits is eighty bits, so two nodes of one cluster sharing a pseudonym is
// not something that happens; a short one would make it a real risk on a large
// fleet, and a bundle where two nodes share a name reads as one node
// contradicting itself.
const pseudonymDigits = 20

// Redactor rewrites the identities and the secret material out of a bundle.
//
// The mapping is keyed by a random per-run key that is never written anywhere.
// That is what makes it one way. The same node name in two files of one bundle
// comes out as the same pseudonym, so a reader can still follow a node from the
// node list to the pods on it; the same node name in two bundles comes out
// differently, so nobody holding both can line them up or confirm a guess at a
// name.
type Redactor struct {
	key []byte
	// names maps a literal found in the bundle onto its pseudonym. It is only
	// a cache: Pseudonym is a pure function of the key and the input, so a
	// missing entry costs an HMAC rather than a different answer.
	names map[string]string
	// extra are identities the caller learned somewhere this package cannot
	// see, such as the cluster's whole node list under --node.
	extra []identity
	// dropped are the files removed because they could not be rewritten
	// without being corrupted.
	dropped []DroppedFile
	// renamed maps a file's old name onto its new one, so that the text naming
	// it can be rewritten to match.
	renamed map[string]string
}

// DroppedFile is a file --redact removed from the bundle rather than ship
// unrewritten. The manifest carries them: a file that silently disappeared is
// one a reader goes looking for and blames on a bad upload.
type DroppedFile struct {
	// Path is the file's name within the bundle.
	Path string `json:"path" yaml:"path"`
	// Reason says why it was removed.
	Reason string `json:"reason" yaml:"reason"`
}

// NewRedactor returns a redactor with a fresh key.
func NewRedactor() (*Redactor, error) {
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		// Without a random key the mapping is reversible by anyone who knows
		// how it is derived, so there is no falling back to a fixed one.
		return nil, fmt.Errorf("drawing the redaction key: %w", err)
	}

	return &Redactor{key: key, names: map[string]string{}, renamed: map[string]string{}}, nil
}

// Pseudonym is the stand-in for one value of one kind. The kind is part of the
// input, so one string meaning a node and meaning an address does not collapse
// into a single pseudonym.
func (r *Redactor) Pseudonym(kind, value string) string {
	cacheKey := kind + "\x00" + value
	if cached, ok := r.names[cacheKey]; ok {
		return cached
	}

	mac := hmac.New(sha256.New, r.key)
	mac.Write([]byte(kind))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	out := kind + "-" + hex.EncodeToString(mac.Sum(nil))[:pseudonymDigits]
	r.names[cacheKey] = out

	return out
}

// keyHexForTest returns the key, so that a test can state that the bundle does
// not hold it. Nothing else may read it: a key that leaves this type is a
// mapping somebody can undo.
func (r *Redactor) keyHexForTest() string { return hex.EncodeToString(r.key) }

// Dropped returns the files the redaction removed, sorted.
func (r *Redactor) Dropped() []DroppedFile {
	out := append([]DroppedFile(nil), r.dropped...)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })

	return out
}

// Learn adds identities the caller knows and the bundle does not name.
//
// The gather uses it for the cluster's whole node list: under --node the
// bundle holds one node's objects and its logs still name the peers that node
// talked to, so an identity set learned from the bundle alone would leave
// every other node of the cluster in the files.
func (r *Redactor) Learn(kind string, values ...string) {
	for _, value := range values {
		if value == "" {
			continue
		}
		r.extra = append(r.extra, identity{literal: value, replacement: r.Pseudonym(kind, value)})
	}
}

// RedactText rewrites one string with the identities already learned. It is
// how the gather rewrites the manifest, which is written after the files and
// is not one of them.
func (r *Redactor) RedactText(text string) string {
	return r.rewrite(text, r.identities())
}

// RedactBundle rewrites every file of a bundle directory in place.
//
// The identities are learned first, from everything in the bundle that names
// one, because they are the values a reader follows between files: a rewrite
// that found them one file at a time would give the same node two pseudonyms
// and break every reference.
//
// Each file is written to a temporary file and renamed over the original, so
// that a failure leaves either the old file or the new one and never half of
// either. A failure stops the whole redaction and the caller removes what is
// left: a bundle the user believes is redacted and is not is one they send on.
func (r *Redactor) RedactBundle(dir string) error {
	names, err := r.learnIdentities(dir)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading the bundle directory to redact it: %w", err)
	}

	// Sorted, so that two runs over one bundle do the same work in the same
	// order and a failure is reproducible.
	files := make([]os.DirEntry, 0, len(entries))
	for _, entry := range entries {
		info, ierr := entry.Info()
		// Regular files only, for the reason indexBundle gives: a symlink
		// points outside the bundle and a fifo never ends. The manifest is
		// written after this and indexes what this leaves behind.
		if ierr != nil || !info.Mode().IsRegular() || entry.Name() == ManifestFileName {
			continue
		}
		files = append(files, entry)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })

	// The renames are decided before anything is rewritten, so that the text
	// inside the files can be rewritten to name the files as they will be.
	for _, entry := range files {
		if renamed := replaceAll(entry.Name(), names); renamed != entry.Name() {
			r.renamed[entry.Name()] = renamed
		}
	}

	for _, entry := range files {
		if err := r.redactFile(dir, entry, names); err != nil {
			return err
		}
	}

	return nil
}

// redactFile rewrites one file of the bundle, or drops it.
func (r *Redactor) redactFile(dir string, entry os.DirEntry, names []identity) error {
	name := entry.Name()
	path := filepath.Join(dir, name)

	info, err := entry.Info()
	if err != nil {
		return fmt.Errorf("reading the mode of %s to redact it: %w", name, err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s to redact it: %w", name, err)
	}

	rewritten, ok, err := r.redactContent(raw, names)
	if err != nil {
		return fmt.Errorf("redacting %s: %w", name, err)
	}
	if !ok {
		// Neither text nor gzip. A byte rewrite would corrupt it and shipping
		// it unrewritten ships whatever is in it, so it goes, and the manifest
		// records that it went.
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing the binary %s: %w", name, err)
		}
		r.dropped = append(r.dropped, DroppedFile{
			Path:   r.renamedName(name),
			Reason: "dropped by --redact: neither text nor gzip, so it cannot be rewritten",
		})

		return nil
	}

	return r.writeThenRename(dir, name, rewritten, info.Mode().Perm())
}

// writeThenRename writes the rewritten bytes beside the original and renames
// them over it, under the file's redacted name.
//
// A rename within one directory is atomic, so an interrupted redaction leaves
// the file it had rather than a truncated one. The temporary file carries the
// original's name so that a crash between the write and the rename leaves
// something a reader can recognise rather than a stray name.
func (r *Redactor) writeThenRename(dir, name string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(dir, "."+name+".redacting-*")
	if err != nil {
		return fmt.Errorf("creating the temporary file for %s: %w", name, err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)

		return fmt.Errorf("writing the redacted %s: %w", name, err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)

		return fmt.Errorf("closing the redacted %s: %w", name, err)
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		os.Remove(tmpName)

		return fmt.Errorf("setting the mode of the redacted %s: %w", name, err)
	}

	final := filepath.Join(dir, r.renamedName(name))
	if err := os.Rename(tmpName, final); err != nil {
		os.Remove(tmpName)

		return fmt.Errorf("renaming the redacted %s: %w", name, err)
	}
	if final != filepath.Join(dir, name) {
		// The file moved, so the original has to go: leaving it would leave
		// the node's name in the directory listing, which is the whole reason
		// for the rename.
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing the unredacted %s: %w", name, err)
		}
	}

	return nil
}

// renamedName is what a file is called after the redaction.
func (r *Redactor) renamedName(name string) string {
	if renamed, ok := r.renamed[name]; ok {
		return renamed
	}

	return name
}

// gzipMagic is the first two bytes of every gzip stream.
var gzipMagic = []byte{0x1f, 0x8b}

// redactContent rewrites the bytes of one file, reporting whether it could.
//
// A gzip stream is gunzipped, rewritten and gzipped again: a byte rewrite of
// the compressed form changes nothing a reader can see, because the identity
// is not in those bytes, and corrupts the file for whoever unpacks it. That is
// how a bug report, which is the one file support reads first, stayed
// unredacted.
//
// Anything else that is not text comes back not ok. Rewriting a binary by
// bytes would corrupt it, and the lengths do not even match: a pseudonym is
// not the length of the name it replaces.
func (r *Redactor) redactContent(raw []byte, names []identity) ([]byte, bool, error) {
	if bytes.HasPrefix(raw, gzipMagic) {
		plain, err := gunzip(raw)
		if err != nil {
			// A truncated gzip is not something to rewrite and not something
			// to ship: the gather that wrote it was cut short.
			return nil, false, nil
		}
		if !isText(plain) {
			return nil, false, nil
		}

		out, err := regzip([]byte(r.rewrite(string(plain), names)))
		if err != nil {
			return nil, false, err
		}

		return out, true, nil
	}

	if !isText(raw) {
		return nil, false, nil
	}

	return []byte(r.rewrite(string(raw), names)), true, nil
}

// gunzip decompresses a gzip stream whole.
func gunzip(raw []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	out, err := io.ReadAll(zr)
	if err != nil {
		return nil, err
	}

	return out, zr.Close()
}

// regzip compresses bytes back into a gzip stream.
func regzip(plain []byte) ([]byte, error) {
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write(plain); err != nil {
		return nil, fmt.Errorf("re-compressing the redacted content: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("finishing the redacted gzip stream: %w", err)
	}

	return out.Bytes(), nil
}

// isText reports whether bytes can be rewritten as text.
//
// A NUL byte settles it: no text file of a bundle holds one and every binary
// format does. Invalid UTF-8 settles the rest, because a rewrite produces a Go
// string and anything that did not survive that round trip was not text.
func isText(raw []byte) bool {
	if bytes.IndexByte(raw, 0) >= 0 {
		return false
	}

	return utf8.Valid(raw)
}

// identity is one literal to replace and what to replace it with.
type identity struct {
	literal     string
	replacement string
}

// rewrite applies the whole redaction to one string, in the order the parts
// have to run in.
//
// Secrets first: a credential whose value happened to hold a node name would
// otherwise come out as a pseudonym rather than as nothing at all, and the
// pseudonym would say that the value was a node name. Then the renamed files,
// as exact strings, so that the manifest and the logs name the files the
// bundle actually holds. Then the identities, on token boundaries. Then the
// addresses.
func (r *Redactor) rewrite(text string, names []identity) string {
	out := r.redactSecrets(text)

	for _, old := range sortedKeysByLengthDesc(r.renamed) {
		out = strings.ReplaceAll(out, old, r.renamed[old])
	}

	out = replaceIdentities(out, names)

	return r.redactAddresses(out)
}

// sortedKeysByLengthDesc orders a rename map longest first, so that a name
// which is a prefix of another is not replaced inside it.
func sortedKeysByLengthDesc(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j])
		}

		return out[i] < out[j]
	})

	return out
}

// byLengthDesc orders identities longest first.
//
// It is the whole trick. "gpu-worker-1" is a prefix of "gpu-worker-10", and
// replacing the short one first leaves the trailing digit behind: one node's
// name becomes another's pseudonym with a stray character after it, and the
// two nodes are no longer distinguishable.
func byLengthDesc(out []identity) {
	sort.Slice(out, func(i, j int) bool {
		if len(out[i].literal) != len(out[j].literal) {
			return len(out[i].literal) > len(out[j].literal)
		}

		return out[i].literal < out[j].literal
	})
}

// learnIdentities reads everything in the bundle that names a node and returns
// every identity in it, longest first.
//
// Every file that names a node is read, not only the node list. A gather whose
// node read was refused still wrote the pods, the events and the slices, and
// each of those names the node it is about; an OpenShift gather also wrote the
// machines, which name the host behind each node.
func (r *Redactor) learnIdentities(dir string) ([]identity, error) {
	seen := map[string]bool{}
	var out []identity
	add := func(kind, value string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, identity{literal: value, replacement: r.Pseudonym(kind, value)})
	}

	for _, known := range r.extra {
		if known.literal == "" || seen[known.literal] {
			continue
		}
		seen[known.literal] = true
		out = append(out, known)
	}

	r.learnFromNodes(dir, add)
	r.learnFromPods(dir, add)
	r.learnFromEvents(dir, add)
	r.learnFromSlices(dir, add)
	r.learnFromMachines(dir, add)

	byLengthDesc(out)

	return out, nil
}

// readBundleYAML decodes one file of a bundle, reporting nothing when it is
// not there or does not parse.
//
// A file that cannot be read is not an error here. Refusing would leave the
// whole bundle unredacted, which is the one outcome worth avoiding, so what
// can be learned is learned and the patterns do the rest.
func readBundleYAML(dir, name string, out any) bool {
	raw, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return false
	}

	return yaml.Unmarshal(raw, out) == nil
}

// learnFromNodes reads the node objects, which carry a node's name, its
// hostname label and the hostname it reports.
//
// Both hostnames are taken. A kubelet given --hostname-override reports one
// string and carries another in its label, and a daemon writing a log line
// knows whichever one it was told.
func (r *Redactor) learnFromNodes(dir string, add func(kind, value string)) {
	for _, name := range []string{"nodes.yaml", "gpu_nodes.yaml"} {
		var list v1.NodeList
		if !readBundleYAML(dir, name, &list) {
			continue
		}
		for i := range list.Items {
			node := &list.Items[i]
			add("node", node.Name)
			add("host", node.Labels["kubernetes.io/hostname"])
			for _, address := range node.Status.Addresses {
				// The IP addresses are left to the address pattern, so that
				// one written in a log the node list never mentions gets the
				// same pseudonym as the one in the node object.
				switch address.Type {
				case v1.NodeHostName, v1.NodeInternalDNS, v1.NodeExternalDNS:
					add("host", address.Address)
				}
			}
		}
	}
}

// learnFromPods reads the node a pod runs on, which is the only place a node
// is named in a bundle whose node read was refused.
func (r *Redactor) learnFromPods(dir string, add func(kind, value string)) {
	for _, name := range []string{gpuPodsFile, "gpu_operand_pods.yaml", "gpu_operator_pod.yaml"} {
		var list v1.PodList
		if !readBundleYAML(dir, name, &list) {
			continue
		}
		for i := range list.Items {
			add("node", list.Items[i].Spec.NodeName)
		}
	}
}

// learnFromEvents reads the node an event is about.
func (r *Redactor) learnFromEvents(dir string, add func(kind, value string)) {
	var list v1.EventList
	if !readBundleYAML(dir, nodeEventsFile, &list) {
		return
	}
	for i := range list.Items {
		if list.Items[i].InvolvedObject.Kind == "Node" {
			add("node", list.Items[i].InvolvedObject.Name)
		}
	}
}

// learnFromSlices reads the node a ResourceSlice was published for.
func (r *Redactor) learnFromSlices(dir string, add func(kind, value string)) {
	var list resourcev1.ResourceSliceList
	if !readBundleYAML(dir, "resourceslices.yaml", &list) {
		return
	}
	for i := range list.Items {
		if name := list.Items[i].Spec.NodeName; name != nil {
			add("node", *name)
		}
	}
}

// learnFromMachines reads the host behind each node on OpenShift.
//
// The file is a list of Machines, which this package has no type for, so it is
// read as the generic shape: every "name" under a "nodeRef", and the machine's
// own name, which carries the cluster's infrastructure id.
func (r *Redactor) learnFromMachines(dir string, add func(kind, value string)) {
	var doc map[string]any
	if !readBundleYAML(dir, "machines.yaml", &doc) {
		return
	}

	items, _ := doc["items"].([]any)
	for _, item := range items {
		machine, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if status, ok := machine["status"].(map[string]any); ok {
			if ref, ok := status["nodeRef"].(map[string]any); ok {
				if name, ok := ref["name"].(string); ok {
					add("node", name)
				}
			}
		}
		if meta, ok := machine["metadata"].(map[string]any); ok {
			if name, ok := meta["name"].(string); ok {
				add("host", name)
			}
		}
	}
}

// identities returns the identities learned outside a bundle read, longest
// first, for RedactText.
func (r *Redactor) identities() []identity {
	out := append([]identity(nil), r.extra...)
	byLengthDesc(out)

	return out
}

// replaceAll replaces every identity in a string without regard to
// boundaries. It is for file names, which are names and not prose: a bug
// report called nvidia-bug-report_<node>.log.gz carries the node's name in the
// directory listing, where no rewrite of the contents reaches it.
func replaceAll(text string, names []identity) string {
	for _, id := range names {
		text = strings.ReplaceAll(text, id.literal, id.replacement)
	}

	return text
}

// replaceIdentities replaces every identity in text, but only where the match
// is a name rather than part of a longer one.
//
// A cluster may have a node called "gpu". Replacing the three letters wherever
// they appear would rewrite nvidia.com/gpu in every file of the bundle: every
// node would stop advertising GPUs, every taint would change key, and the
// whole report would be about a cluster that does not exist. So a match counts
// only when the character before it is not part of a qualified name and the
// character after it does not continue one.
func replaceIdentities(text string, names []identity) string {
	for _, id := range names {
		text = replaceOnBoundaries(text, id.literal, id.replacement)
	}

	return text
}

// replaceOnBoundaries replaces literal with replacement wherever the match is
// not inside a longer qualified name.
func replaceOnBoundaries(text, literal, replacement string) string {
	if literal == "" {
		return text
	}

	var out strings.Builder
	for {
		i := strings.Index(text, literal)
		if i < 0 {
			out.WriteString(text)

			break
		}

		end := i + len(literal)
		if boundedBefore(text, i) && boundedAfter(text, end) {
			out.WriteString(text[:i])
			out.WriteString(replacement)
		} else {
			out.WriteString(text[:end])
		}
		text = text[end:]
	}

	return out.String()
}

// nameRunesBefore are the characters that, just before a match, make it the
// tail of a longer qualified name: the "gpu" of nvidia.com/gpu and the
// "driver" of nvidia.com/gpu.deploy.driver.
//
// "_" is deliberately not among them. That is what separates a prefix from a
// node name in an artifact's file name, and the text of the bundle names those
// files.
const nameRunesBefore = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789.-/"

// nameRunesAfter are the characters that, just after a match, continue the
// name.
//
// A dot is not among them: a node named in a file name is followed by its
// extension, and a short name followed by a domain has already had the long
// form replaced, because the identities run longest first.
//
// "/" is not among them either, because "kubectl describe pod" prints the node
// as "Node: <name>/<address>" and that line is in every .descr file of every
// bundle. It is what would otherwise protect a node named "nvidia.com" from
// having nvidia.com/gpu rewritten around it, which is a name no cluster has
// against a format every bundle holds.
const nameRunesAfter = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-"

// boundedBefore reports whether the character before a match ends a name.
func boundedBefore(text string, i int) bool {
	if i == 0 {
		return true
	}

	return !strings.ContainsRune(nameRunesBefore, rune(text[i-1]))
}

// boundedAfter reports whether the character after a match ends a name.
func boundedAfter(text string, end int) bool {
	if end >= len(text) {
		return true
	}

	return !strings.ContainsRune(nameRunesAfter, rune(text[end]))
}

// credentialName matches a name whose value is a credential.
//
// It matches generously on purpose. A value redacted that need not have been
// costs a reader one line of a log; a value missed is a credential in a file
// that goes to another company.
var credentialName = regexp.MustCompile(
	`(?i)(pass|passwd|password|secret|token|key|credential|cred|auth|licen[cs]e|signature|session)`)

// The shapes a secret arrives in, whatever file it is in.
//
// Each one is here because it survived a redaction that looked finished. A
// line scanner over "key: value" reads YAML and nothing else, and a bundle is
// not one format: it holds kernel logs, container logs, shell transcripts,
// JSON embedded in YAML annotations, kubeconfig fragments and URLs.
var (
	// authHeader is an Authorization header anywhere in a line, with either
	// scheme: a log of an HTTP exchange carries one mid-line.
	authHeader = regexp.MustCompile(`(?i)(authorization\s*:\s*)(bearer|basic|token)(\s+)\S+`)
	// bearerToken is the scheme on its own, as a log writes it without the
	// header name.
	bearerToken = regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	// pemBlock is a private key or a certificate written out whole.
	pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]+-----.*?-----END [A-Z ]+-----`)
	// shellAssignment is NAME=value, as a shell transcript, a process listing
	// and half the log lines in a bundle write it.
	shellAssignment = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_.\-]*)=([^\s"',;]+)`)
	// jsonString is "name": "value" in JSON, and the same escaped inside a
	// YAML annotation, which is where last-applied-configuration keeps a whole
	// pod spec.
	jsonString = regexp.MustCompile(`(\\?")([A-Za-z0-9_.\-]+)(\\?"\s*:\s*\\?")([^"\\]*)(\\?")`)
	// jsonEnvPair is the {"name":"FOO","value":"bar"} shape, where the name
	// that decides is not the key of the value.
	jsonEnvPair = regexp.MustCompile(
		`(\\?"name\\?"\s*:\s*\\?")([A-Za-z0-9_.\-]+)(\\?"\s*,\s*\\?"value\\?"\s*:\s*\\?")([^"\\]*)(\\?")`)
	// commandFlag is --flag=value and --flag value, which is how a credential
	// reaches a container that takes one on its command line.
	commandFlag = regexp.MustCompile(`(--[A-Za-z0-9][A-Za-z0-9-]*)([= ])([^\s"',;]+)`)
	// urlUserinfo is the user and password of a URL.
	urlUserinfo = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)([^/\s:@]+):([^/\s@]+)@`)
	// dataField is a kubeconfig's base64 blobs, whose names say "data" rather
	// than "secret".
	dataField = regexp.MustCompile(`^(\s*-?\s*[A-Za-z0-9_.\-]*-data\s*:\s*)(\S.*)$`)
	// keyedValue is "name: value" as YAML and a kubeconfig write it.
	keyedValue = regexp.MustCompile(`^(\s*-?\s*"?([A-Za-z0-9_.\-/]+)"?\s*:\s*)(\S.*)$`)
	// blockScalar is a key whose value is an indented block rather than a
	// scalar: "password: |" and everything indented under it.
	blockScalar = regexp.MustCompile(`^(\s*-?\s*"?([A-Za-z0-9_.\-/]+)"?\s*:\s*)([|>][-+0-9]*)\s*$`)
	// envName is the "- name: FOO" line of a container's env list, which is
	// what says whether the "value:" line under it is a credential.
	envName = regexp.MustCompile(`^\s*-?\s*"?name"?\s*:\s*"?([A-Za-z0-9_.\-]+)"?,?\s*$`)
	// valueLine is the "value: ..." that follows it.
	valueLine = regexp.MustCompile(`^(\s*"?value"?\s*:\s*)(\S.*)$`)
)

// notACredentialKey is the one name that matches credentialName and never
// holds a credential.
//
// A taint and a toleration are keyed by a field literally called "key", and
// its value is a resource name such as nvidia.com/gpu. Writing REDACTED over
// it changes which pods the operator can place, so the redacted bundle would
// describe a cluster that does not exist.
func notACredentialKey(name string) bool {
	return strings.EqualFold(name, "key")
}

// isLiteralValue reports whether a value is a boolean, a number or a YAML null
// rather than a string.
//
// Only strings are redacted. automountServiceAccountToken is a boolean whose
// name holds "token" and privileged is a boolean next to one: writing REDACTED
// over either makes a document that no longer says what it said, and changes
// the answer a reader takes from it.
func isLiteralValue(value string) bool {
	v := strings.TrimSpace(value)
	v = strings.Trim(v, `"'`)
	switch strings.ToLower(v) {
	case "true", "false", "null", "~", "yes", "no", "on", "off", "":
		return true
	}

	if _, err := strconv.ParseFloat(v, 64); err == nil {
		return true
	}

	return false
}

// redactSecrets takes the secret material out of one file.
//
// It is a line scanner with a set of shape rules rather than a parser, because
// the files of a bundle are not one format and a parser for any one of them
// cannot read the others. What that costs is that the rules are shapes, which
// is why credentialName matches generously and why isLiteralValue is consulted
// before anything is rewritten.
func (r *Redactor) redactSecrets(text string) string {
	text = pemBlock.ReplaceAllString(text, redactedValue)
	text = authHeader.ReplaceAllString(text, "${1}${2}${3}"+redactedValue)
	text = bearerToken.ReplaceAllString(text, "${1}"+redactedValue)
	text = urlUserinfo.ReplaceAllString(text, "${1}"+redactedValue+":"+redactedValue+"@")
	text = redactJSONEnvPairs(text)
	text = redactJSONStrings(text)
	text = redactShellAssignments(text)
	text = redactCommandFlags(text)

	lines := strings.Split(text, "\n")
	// pendingEnv is set when the previous line named a credential in a
	// container's env list, so that the "value:" under it is taken out even
	// though "value" says nothing about what it holds.
	pendingEnv := false
	// dataIndent is the indentation of a "data:" or "stringData:" key, and
	// blockIndent that of a credential whose value is an indented block. Every
	// line indented further than either is part of that value.
	dataIndent, blockIndent := -1, -1

	for i, line := range lines {
		if blockIndent >= 0 {
			if strings.TrimSpace(line) != "" && indentOf(line) > blockIndent {
				lines[i] = ""

				continue
			}
			blockIndent = -1
		}

		if dataIndent >= 0 {
			if strings.TrimSpace(line) != "" && indentOf(line) > dataIndent {
				lines[i] = redactKeyedValue(line)

				continue
			}
			dataIndent = -1
		}

		if indent, ok := dataKey(line); ok {
			dataIndent = indent

			continue
		}

		if m := blockScalar.FindStringSubmatch(line); m != nil &&
			credentialName.MatchString(m[2]) && !notACredentialKey(m[2]) {
			lines[i] = m[1] + redactedValue
			blockIndent = indentOf(line)

			continue
		}

		if pendingEnv {
			pendingEnv = false
			if m := valueLine.FindStringSubmatch(line); m != nil && !isLiteralValue(m[2]) {
				lines[i] = m[1] + redactedValue

				continue
			}
		}

		if m := envName.FindStringSubmatch(line); m != nil {
			pendingEnv = credentialName.MatchString(m[1])

			continue
		}

		if m := dataField.FindStringSubmatch(line); m != nil && !isLiteralValue(m[2]) {
			lines[i] = m[1] + redactedValue

			continue
		}

		if m := keyedValue.FindStringSubmatch(line); m != nil &&
			credentialName.MatchString(m[2]) && !notACredentialKey(m[2]) && !isLiteralValue(m[3]) {
			lines[i] = m[1] + redactedValue
		}
	}

	return strings.Join(lines, "\n")
}

// redactShellAssignments rewrites NAME=value where the name says credential.
func redactShellAssignments(text string) string {
	return shellAssignment.ReplaceAllStringFunc(text, func(match string) string {
		m := shellAssignment.FindStringSubmatch(match)
		if !credentialName.MatchString(m[1]) || notACredentialKey(m[1]) || isLiteralValue(m[2]) {
			return match
		}

		return m[1] + "=" + redactedValue
	})
}

// redactCommandFlags rewrites --flag=value and --flag value where the flag
// says credential.
func redactCommandFlags(text string) string {
	return commandFlag.ReplaceAllStringFunc(text, func(match string) string {
		m := commandFlag.FindStringSubmatch(match)
		name := strings.TrimPrefix(m[1], "--")
		if !credentialName.MatchString(name) || notACredentialKey(name) || isLiteralValue(m[3]) {
			return match
		}

		return m[1] + m[2] + redactedValue
	})
}

// redactJSONStrings rewrites "name": "value" where the name says credential,
// escaped or not. Only a quoted value is touched, so a number and a boolean
// are left alone by construction.
func redactJSONStrings(text string) string {
	return jsonString.ReplaceAllStringFunc(text, func(match string) string {
		m := jsonString.FindStringSubmatch(match)
		if !credentialName.MatchString(m[2]) || notACredentialKey(m[2]) {
			return match
		}

		return m[1] + m[2] + m[3] + redactedValue + m[5]
	})
}

// redactJSONEnvPairs rewrites {"name":"FOO","value":"bar"}, where what decides
// is the value of "name" rather than the key of the value.
func redactJSONEnvPairs(text string) string {
	return jsonEnvPair.ReplaceAllStringFunc(text, func(match string) string {
		m := jsonEnvPair.FindStringSubmatch(match)
		if !credentialName.MatchString(m[2]) || notACredentialKey(m[2]) {
			return match
		}

		return m[1] + m[2] + m[3] + redactedValue + m[5]
	})
}

// dataKey reports whether a line opens the value map of a Secret, and at what
// indentation.
func dataKey(line string) (int, bool) {
	trimmed := strings.TrimSpace(line)
	if trimmed != "data:" && trimmed != "stringData:" {
		return 0, false
	}

	return indentOf(line), true
}

// indentOf counts the leading spaces of a line.
func indentOf(line string) int {
	return len(line) - len(strings.TrimLeft(line, " "))
}

// redactKeyedValue replaces the value of a "key: value" line, keeping the key.
// A boolean or a number is left alone, for the reason isLiteralValue gives.
func redactKeyedValue(line string) string {
	if m := keyedValue.FindStringSubmatch(line); m != nil && !isLiteralValue(m[3]) {
		return m[1] + redactedValue
	}

	return line
}

// Addresses as a bundle writes them. Both patterns are deliberately loose and
// every match is parsed before it is replaced: a tight pattern for either
// family is unreadable, and a loose one that validates is not.
//
// The IPv6 pattern has no trailing word boundary, because an address may end
// in "::" and a boundary after a colon never matches: that is how every
// link-local address and every pod CIDR of a dual-stack cluster stayed in the
// bundle.
var (
	ipv4Pattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	ipv6Pattern = regexp.MustCompile(`\b[0-9A-Fa-f]{0,4}(?::[0-9A-Fa-f]{0,4}){2,7}`)
)

// redactAddresses replaces every IP address with a stable pseudonym.
//
// The pseudonym is not itself an address. A reader following one between files
// needs them to match, which they do; a reader that tried to connect to one
// would be connecting to whatever machine the redaction happened to pick, which
// is worse than a value that obviously is not an address.
//
// A CIDR keeps its prefix length: /64 says how the network was carved up,
// which is a fact about the cluster's shape rather than about where it is.
func (r *Redactor) redactAddresses(text string) string {
	replace := func(match string) string {
		candidate := match
		// A match may have run on past the address into a separator. An
		// address ending in "::" keeps both colons; anything else gives the
		// trailing one back.
		if !strings.HasSuffix(candidate, "::") {
			candidate = strings.TrimRight(candidate, ":")
		}
		if net.ParseIP(candidate) == nil {
			return match
		}

		return r.Pseudonym("ip", candidate) + match[len(candidate):]
	}

	text = ipv4Pattern.ReplaceAllStringFunc(text, replace)

	return ipv6Pattern.ReplaceAllStringFunc(text, replace)
}

// redactBundle rewrites the bundle this gather wrote.
//
// A redaction that could not be done is a failure of the gather rather than a
// note on it, and the bundle goes with it: the alternative is handing somebody
// a bundle they asked to have redacted and that is not, which they then send
// on. Exit 4 says nothing usable came back.
func (g *Gatherer) redactBundle() error {
	r, err := NewRedactor()
	if err != nil {
		return err
	}

	// The cluster's whole node list, not the bundle's. Under --node the bundle
	// holds one node's objects and its logs still name every peer that node
	// talked to; a redactor that learned only what the bundle names would
	// leave those in.
	for i := range g.allNodes {
		node := &g.allNodes[i]
		r.Learn("node", node.Name)
		r.Learn("host", node.Labels["kubernetes.io/hostname"])
		for _, address := range node.Status.Addresses {
			switch address.Type {
			case v1.NodeHostName, v1.NodeInternalDNS, v1.NodeExternalDNS:
				r.Learn("host", address.Address)
			}
		}
	}
	// What was asked for and which cluster it came from are identities too,
	// and both are recorded in the manifest rather than in any file.
	r.Learn("node", g.opts.Node)
	r.Learn("context", g.opts.Context)

	g.redactor = r

	if err := r.RedactBundle(g.opts.ArtifactDir); err != nil {
		g.discardBundle()

		return &Error{Err: fmt.Errorf("redacting the bundle: %w", err)}
	}

	g.step("Redacted the node names, addresses and credential values out of the bundle")

	return nil
}

// discardBundle removes everything left in the artifact directory.
//
// It runs when a redaction could not finish. What is there is a mixture of
// rewritten files and files that were never reached, and nobody can tell which
// is which from the outside: the whole point of --redact is that the directory
// can be sent, and half of one cannot.
func (g *Gatherer) discardBundle() {
	entries, err := os.ReadDir(g.opts.ArtifactDir)
	if err != nil {
		g.note("could not clear the half-redacted bundle in %s: %v", g.opts.ArtifactDir, err)

		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(g.opts.ArtifactDir, entry.Name())
		if rerr := os.Remove(path); rerr != nil {
			g.note("could not remove %s from the half-redacted bundle: %v", entry.Name(), rerr)
		}
	}
}

// redactManifest rewrites the parts of the index that name a node or a
// cluster: what was asked for, which cluster it came from, and every status
// and artifact name a collector recorded.
//
// The manifest is written after the files and is not one of them, so it does
// not go through RedactBundle. It is also the first file anybody opens.
func (g *Gatherer) redactManifest(m *Manifest) {
	if g.redactor == nil {
		return
	}

	m.Flags.Node = g.redactor.RedactText(m.Flags.Node)
	m.Context = g.redactor.RedactText(m.Context)

	for i := range m.Collectors {
		m.Collectors[i].Status = g.redactor.RedactText(m.Collectors[i].Status)
		for j := range m.Collectors[i].Artifacts {
			m.Collectors[i].Artifacts[j] = g.redactor.RedactText(m.Collectors[i].Artifacts[j])
		}
	}

	m.Dropped = g.redactor.Dropped()
}
