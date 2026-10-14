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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretClass is one shape a credential arrives in, the line it arrives on,
// and the exact string that must not survive.
//
// They are written out one per shape rather than folded into a pattern,
// because every one of them is a shape that survived a redaction that looked
// finished: a reader of this list can see what is covered without reading the
// regular expressions.
type secretClass struct {
	name    string
	line    string
	planted string
	// keep is what the line must still carry afterwards, so that a redaction
	// which deleted the line would not pass.
	keep string
}

// secretClasses is every shape the redactor has to recognise.
func secretClasses() []secretClass {
	return []secretClass{
		{
			name:    "shell assignment",
			line:    "DB_PASSWORD=p-shell-9f21a",
			planted: "p-shell-9f21a",
			keep:    "DB_PASSWORD",
		},
		{
			name:    "lowercase assignment in a log line",
			line:    "connecting with password=p-logline-7c04b to the registry",
			planted: "p-logline-7c04b",
			keep:    "password",
		},
		{
			name:    "JSON string value",
			line:    `{"registryToken":"p-json-5de31","replicas":2}`,
			planted: "p-json-5de31",
			keep:    `"replicas":2`,
		},
		{
			name:    "one line JSON env object",
			line:    `{"name":"HF_TOKEN","value":"p-envjson-11aa2"}`,
			planted: "p-envjson-11aa2",
			keep:    "HF_TOKEN",
		},
		{
			name:    "env inside last-applied-configuration",
			line:    `    kubectl.kubernetes.io/last-applied-configuration: '{"env":[{"name":"NGC_API_KEY","value":"p-lastapplied-6b7c8"}]}'`,
			planted: "p-lastapplied-6b7c8",
			keep:    "NGC_API_KEY",
		},
		{
			name:    "flag with an equals sign",
			line:    "    - --hf-token=p-flageq-2c9d0",
			planted: "p-flageq-2c9d0",
			keep:    "--hf-token",
		},
		{
			name:    "flag with a space",
			line:    "running: trainer --password p-flagsp-8e1f4 --epochs 3",
			planted: "p-flagsp-8e1f4",
			keep:    "--epochs 3",
		},
		{
			name:    "URL userinfo",
			line:    "pulling from https://robot:p-userinfo-4a5b6@registry.example.com/team/image",
			planted: "p-userinfo-4a5b6",
			keep:    "registry.example.com",
		},
		{
			name:    "Authorization Basic mid line",
			line:    `curl -v -H "Authorization: Basic cDpiYXNpYy0zZDJlMQ==" https://api.example.com`,
			planted: "cDpiYXNpYy0zZDJlMQ==",
			keep:    "Authorization",
		},
		{
			name:    "kubeconfig data field",
			line:    "    client-key-data: p-kubeconfig-9d8e7",
			planted: "p-kubeconfig-9d8e7",
			keep:    "client-key-data",
		},
		{
			name:    "YAML block scalar",
			line:    "password: |\n  p-blockscalar-1f2e3\n  second-line-of-the-secret\nreplicas: 2",
			planted: "p-blockscalar-1f2e3",
			keep:    "replicas: 2",
		},
		{
			name: "PEM block",
			line: "-----BEGIN RSA PRIVATE KEY-----\np-pem-7h8i9\n-----END RSA PRIVATE KEY-----",

			planted: "p-pem-7h8i9",
			keep:    "",
		},
		{
			name:    "IPv6 ending in a double colon",
			line:    "imex peer reachable at fd00:10:244:1::",
			planted: "fd00:10:244:1::",
			keep:    "imex peer",
		},
		{
			name:    "IPv6 CIDR",
			line:    "pod network is fd00:10:244:1::/64",
			planted: "fd00:10:244:1::",
			keep:    "/64",
		},
		{
			name:    "IPv4 CIDR",
			line:    "service network is 10.96.0.0/12",
			planted: "10.96.0.0",
			keep:    "/12",
		},
	}
}

// classFixture writes one file holding every class, in a bundle directory.
func classFixture(t *testing.T) (string, *Redactor) {
	t.Helper()

	dir := t.TempDir()
	var body strings.Builder
	for _, class := range secretClasses() {
		body.WriteString(class.line)
		body.WriteString("\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "must-gather.log"), []byte(body.String()), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting: %v", err)
	}

	return dir, r
}

// Every shape a credential or an address arrives in, one subtest each, so that
// a failure names the shape rather than saying the file still holds something.
func TestRedactCoversEverySecretClass(t *testing.T) {
	dir, _ := classFixture(t)

	raw, err := os.ReadFile(filepath.Join(dir, "must-gather.log"))
	if err != nil {
		t.Fatalf("reading the redacted fixture: %v", err)
	}
	text := string(raw)

	for _, class := range secretClasses() {
		t.Run(class.name, func(t *testing.T) {
			if strings.Contains(text, class.planted) {
				t.Errorf("the file still holds %q\n%s", class.planted, text)
			}
			if class.keep != "" && !strings.Contains(text, class.keep) {
				t.Errorf("the redaction took %q with it, which is not a secret\n%s", class.keep, text)
			}
		})
	}
}

// An address means the same thing wherever it appears, so it has to come out
// as the same pseudonym wherever it appears: a reader following a connection
// between two log files is following the pseudonym.
func TestRedactGivesOneAddressOnePseudonymInEveryFile(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"must-gather.log", "fabricmanager.log", "dmesg.log"} {
		body := "peer 10.42.7.11 answered; see also 10.42.7.11\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting: %v", err)
	}

	want := r.Pseudonym("ip", "10.42.7.11")
	if want == "" {
		t.Fatal("the redactor produced no pseudonym for the address")
	}
	for _, name := range []string{"must-gather.log", "fabricmanager.log", "dmesg.log"} {
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("reading %s: %v", name, rerr)
		}
		if strings.Count(string(raw), want) != 2 {
			t.Errorf("%s is %q, want both addresses rewritten to %q", name, raw, want)
		}
	}
}

// A dotted quad that is not an address is not rewritten. Every candidate goes
// through net.ParseIP, so a version number keeps its digits: a reader matching
// a driver version against a release note needs it to be the version.
func TestRedactLeavesSomethingThatIsNotAnAddress(t *testing.T) {
	dir := t.TempDir()
	body := "NVRM: driver 999.888.777.666 loaded at 10:30:45 on bus 0000:3b:00.0\n"
	if err := os.WriteFile(filepath.Join(dir, "must-gather.log"), []byte(body), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "must-gather.log"))
	if err != nil {
		t.Fatalf("reading the redacted fixture: %v", err)
	}
	for _, keep := range []string{"999.888.777.666", "10:30:45", "0000:3b:00.0"} {
		if !strings.Contains(string(raw), keep) {
			t.Errorf("the redaction rewrote %q, which is not an address: %q", keep, raw)
		}
	}
}
