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
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
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
}

// NewRedactor returns a redactor with a fresh key.
func NewRedactor() (*Redactor, error) {
	key := make([]byte, sha256.Size)
	if _, err := rand.Read(key); err != nil {
		// Without a random key the mapping is reversible by anyone who knows
		// how it is derived, so there is no falling back to a fixed one.
		return nil, fmt.Errorf("drawing the redaction key: %w", err)
	}

	return &Redactor{key: key, names: map[string]string{}}, nil
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

// RedactBundle rewrites every file of a bundle directory in place.
//
// The identities are learned first, from the node list, because they are the
// values a reader follows between files: a rewrite that found them one file at
// a time would give the same node two pseudonyms and break every reference.
// What the node list does not name is still caught by the patterns below,
// which need to know nothing about what they are looking at.
func (r *Redactor) RedactBundle(dir string) error {
	names, err := r.learnIdentities(dir)
	if err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading the bundle directory to redact it: %w", err)
	}

	for _, entry := range entries {
		info, err := entry.Info()
		// Regular files only, for the reason indexBundle gives: a symlink
		// points outside the bundle and a fifo never ends. The manifest is
		// written after this and indexes what this leaves behind.
		if err != nil || !info.Mode().IsRegular() || entry.Name() == ManifestFileName {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s to redact it: %w", entry.Name(), err)
		}

		if err := os.WriteFile(path, r.redactBytes(raw, names), info.Mode().Perm()); err != nil {
			return fmt.Errorf("writing the redacted %s: %w", entry.Name(), err)
		}
	}

	return nil
}

// identity is one literal to replace and what to replace it with.
type identity struct {
	literal     string
	replacement string
}

// learnIdentities reads the node list the gather wrote and returns every
// identity in it, longest first.
//
// Longest first is the whole trick. "gpu-worker-1" is a prefix of
// "gpu-worker-10", and replacing the short one first leaves the trailing digit
// behind: one node's name becomes another's pseudonym with a stray character
// after it, and the two nodes are no longer distinguishable.
//
// A bundle with no node list is not an error. A gather that was refused the
// nodes still wrote logs, and those still go through the patterns below.
func (r *Redactor) learnIdentities(dir string) ([]identity, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "nodes.yaml"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("reading the node list to redact it: %w", err)
	}

	var list corev1.NodeList
	if err := yaml.Unmarshal(raw, &list); err != nil {
		// A node list that does not parse is still a file full of node names.
		// Refusing here would leave the whole bundle unredacted, which is the
		// one outcome worth avoiding, so the identities go unlearned and the
		// patterns do what they can.
		return nil, nil
	}

	seen := map[string]bool{}
	var out []identity
	add := func(kind, value string) {
		if value == "" || seen[value] {
			return
		}
		seen[value] = true
		out = append(out, identity{literal: value, replacement: r.Pseudonym(kind, value)})
	}

	for i := range list.Items {
		node := &list.Items[i]
		add("node", node.Name)
		add("host", node.Labels["kubernetes.io/hostname"])
		for _, address := range node.Status.Addresses {
			// The IP addresses are deliberately left to the address pattern,
			// so that one written in a log the node list never mentions gets
			// the same pseudonym as the one in the node object.
			switch address.Type {
			case corev1.NodeHostName, corev1.NodeInternalDNS, corev1.NodeExternalDNS:
				add("host", address.Address)
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if len(out[i].literal) != len(out[j].literal) {
			return len(out[i].literal) > len(out[j].literal)
		}

		return out[i].literal < out[j].literal
	})

	return out, nil
}

// redactBytes rewrites one file.
//
// The order is secrets, then identities, then addresses. Secrets go first
// because a credential whose value happened to hold a node name would
// otherwise come out as a pseudonym rather than as nothing at all, and the
// pseudonym would say that the value was a node name.
func (r *Redactor) redactBytes(raw []byte, names []identity) []byte {
	text := r.redactSecrets(string(raw))

	for _, id := range names {
		text = strings.ReplaceAll(text, id.literal, id.replacement)
	}

	return []byte(r.redactAddresses(text))
}

// credentialName matches an environment variable, a key of a Secret or a field
// of a kubeconfig whose name says its value is a credential.
//
// It matches generously on purpose. A value redacted that need not have been
// costs a reader one line of a log; a value missed is a credential in a file
// that goes to another company.
var credentialName = regexp.MustCompile(
	`(?i)(pass|passwd|password|secret|token|key|credential|cred|auth|licen[cs]e|signature|session)`)

// The shapes a secret arrives in, whatever file it is in.
var (
	// bearerToken is an Authorization header, in a log or in a captured request.
	bearerToken = regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{8,}`)
	// pemBlock is a private key or a certificate written out whole.
	pemBlock = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]+-----.*?-----END [A-Z ]+-----`)
	// keyedValue is "name: value" as YAML, JSON and a kubeconfig all write it.
	// The key is captured so that it can be tested against credentialName and
	// the value replaced without the key itself being touched.
	keyedValue = regexp.MustCompile(`^(\s*-?\s*"?[A-Za-z0-9_.\-/]+"?\s*:\s*)(\S.*)$`)
	// envName is the "- name: FOO" line of a container's env list, which is
	// what says whether the "value:" line under it is a credential.
	envName = regexp.MustCompile(`^\s*-?\s*"?name"?\s*:\s*"?([A-Za-z0-9_.\-]+)"?,?\s*$`)
	// valueLine is the "value: ..." that follows it.
	valueLine = regexp.MustCompile(`^(\s*"?value"?\s*:\s*)(\S.*)$`)
)

// redactSecrets takes the secret material out of one file.
//
// It is a line scanner rather than a parser because the files of a bundle are
// not one format: YAML objects, a kubeconfig fragment, JSON and plain logs all
// carry credentials, and a parser for any one of them cannot read the others.
// What that costs is that the rules are shapes rather than schemas, which is
// why credentialName matches generously.
func (r *Redactor) redactSecrets(text string) string {
	text = pemBlock.ReplaceAllString(text, redactedValue)
	text = bearerToken.ReplaceAllString(text, "${1}"+redactedValue)

	lines := strings.Split(text, "\n")
	// pendingEnv is set when the previous line named a credential in a
	// container's env list, so that the "value:" under it is taken out even
	// though "value" says nothing about what it holds.
	pendingEnv := false
	// dataIndent is the indentation of a "data:" or "stringData:" key. Every
	// line indented further than it is one of a Secret's values.
	dataIndent := -1

	for i, line := range lines {
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

		if pendingEnv {
			pendingEnv = false
			if m := valueLine.FindStringSubmatch(line); m != nil {
				lines[i] = m[1] + redactedValue

				continue
			}
		}

		if m := envName.FindStringSubmatch(line); m != nil {
			pendingEnv = credentialName.MatchString(m[1])

			continue
		}

		if m := keyedValue.FindStringSubmatch(line); m != nil && credentialName.MatchString(m[1]) {
			lines[i] = m[1] + redactedValue
		}
	}

	return strings.Join(lines, "\n")
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
func redactKeyedValue(line string) string {
	if m := keyedValue.FindStringSubmatch(line); m != nil {
		return m[1] + redactedValue
	}

	return line
}

// Addresses as a bundle writes them. Both patterns are deliberately loose and
// every match is parsed before it is replaced: a tight pattern for either
// family is unreadable, and a loose one that validates is not.
var (
	ipv4Pattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	ipv6Pattern = regexp.MustCompile(`\b[0-9A-Fa-f]{0,4}(:[0-9A-Fa-f]{0,4}){2,7}\b`)
)

// redactAddresses replaces every IP address with a stable pseudonym.
//
// The pseudonym is not itself an address. A reader following one between files
// needs them to match, which they do; a reader that tried to connect to one
// would be connecting to whatever machine the redaction happened to pick, which
// is worse than a value that obviously is not an address.
func (r *Redactor) redactAddresses(text string) string {
	replace := func(match string) string {
		if net.ParseIP(match) == nil {
			return match
		}

		return r.Pseudonym("ip", match)
	}

	text = ipv4Pattern.ReplaceAllStringFunc(text, replace)

	return ipv6Pattern.ReplaceAllStringFunc(text, replace)
}

// redactBundle rewrites the bundle this gather wrote.
//
// A redaction that could not be done is a failure of the gather rather than a
// note on it. The alternative is handing somebody a bundle they asked to have
// redacted and that is not, which they would then send on.
func (g *Gatherer) redactBundle() error {
	r, err := NewRedactor()
	if err != nil {
		return err
	}
	if err := r.RedactBundle(g.opts.ArtifactDir); err != nil {
		return fmt.Errorf("redacting the bundle: %w", err)
	}
	g.step("Redacted the node names, addresses and credential values out of the bundle")

	return nil
}
