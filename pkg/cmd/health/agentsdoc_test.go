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

package health_test

import (
	"bytes"
	"encoding/json"
	stderrors "errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	cmdhealth "github.com/NVIDIA/kubectl-nv/pkg/cmd/health"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// agentsDoc is the contract page a program is written against.
const agentsDoc = "agents.md"

// docPath matches a JSON path docs/agents.md names in backticks, dotted or
// not. A flag such as `--fail-on`, a kind such as `GPUNodeList` and a command
// line such as `kubectl nv health` are none of them a path and do not match.
var docPath = regexp.MustCompile("`([a-z][A-Za-z0-9]*(?:\\.[a-z][A-Za-z0-9]*)*)`")

// partialReport is a real health report over a cluster that refused a read, so
// that it carries the field this test is about.
func partialReport(t *testing.T) (map[string]any, string) {
	t.Helper()

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: out, ErrOut: errOut}

	clientset := fake.NewSimpleClientset(
		gpuNode("worker-1", 4, 4), hintedNode("no-gpus-1"), hintedNode("no-gpus-2"))
	clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "", stderrors.New("nope"))
	})

	command := cmdhealth.NewCommand(&cmd.Options{
		Streams: io,
		Factory: &fakeFactory{clientset: clientset, dynamic: newDynamic()},
		Logger:  output.NewLogger(io, output.LoggerOptions{NoColor: true}),
	})
	command.SetOut(out)
	command.SetErr(errOut)
	// Partial and truncated, so that the report carries every field the page
	// attributes to this kind: a fixture missing one of them would let the
	// page name it wrongly and still pass.
	command.SetArgs([]string{"-o", "json", "--limit", "1"})
	command.SilenceUsage = true
	command.SilenceErrors = true
	_ = command.Execute()

	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parsing the report: %v\n%s", err, out.String())
	}
	for _, want := range []string{"unreachable", "partial", "findings", "remainingItemCount"} {
		if _, ok := report[want]; !ok {
			t.Fatalf("the fixture produced no %s, so the page could name it wrongly and pass:\n%s",
				want, out.String())
		}
	}

	return report, out.String()
}

// jsonPaths collects every dotted path of an object, so that a path named in
// the documentation can be looked for without this test knowing the shape.
func jsonPaths(value any, prefix string, out map[string]bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return
	}
	for key, child := range object {
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		out[path] = true
		jsonPaths(child, path, out)
	}
}

// docs/agents.md said that what a partial run could not see is listed in
// status.unreachable. A health report carries it at the top level, because its
// status is embedded inline: a program following the documented path reads
// nothing and concludes that nothing was out of reach, which is the opposite
// of what happened.
//
// The assertion is made against a real report rather than against the Go type.
// The Go type is not what a consumer holds: the inlining happens in
// MarshalJSON, so a field that moved between the struct and the wire would pass
// a test written against the struct.
func TestTheAgentsDocNamesPathsTheReportActuallyHas(t *testing.T) {
	t.Parallel()

	report, raw := partialReport(t)
	actual := map[string]bool{}
	jsonPaths(report, "", actual)

	// Only the sentences that are about this kind. The page documents several,
	// and each one puts its status where its own shape puts it: status.findings
	// is right for a TopologyReport and wrong here, and a test that could not
	// tell them apart could only be satisfied by a page that said neither.
	checked := 0
	for _, sentence := range sentencesAbout(readAgentsDoc(t), "`HealthReport`") {
		for _, match := range docPath.FindAllStringSubmatch(sentence, -1) {
			path := match[1]
			last := path[strings.LastIndex(path, ".")+1:]
			candidates := pathsEndingIn(actual, last)
			if len(candidates) == 0 {
				continue
			}
			checked++
			if !actual[path] {
				t.Errorf("docs/%s says a health report carries %q; it carries %v\n%s",
					agentsDoc, path, candidates, raw)
			}
		}
	}

	if checked == 0 {
		t.Fatalf("docs/%s names no path of a health report, so this compared nothing", agentsDoc)
	}
}

// readAgentsDoc returns the contract page.
func readAgentsDoc(t *testing.T) string {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", agentsDoc))
	if err != nil {
		t.Fatalf("reading docs/%s: %v", agentsDoc, err)
	}

	return string(raw)
}

// sentencesAbout returns the parts of the page that name a subject.
//
// A paragraph is prose with hard wrapped lines, so its newlines are flattened
// before it is split into sentences, or a sentence spanning two lines would be
// two. A table is not prose: each row is about one kind, and flattening a
// table would make every row a statement about every kind in it.
func sentencesAbout(doc, subject string) []string {
	var out []string
	for _, block := range strings.Split(doc, "\n\n") {
		if strings.HasPrefix(strings.TrimSpace(block), "|") {
			for _, row := range strings.Split(block, "\n") {
				if strings.Contains(row, subject) {
					out = append(out, row)
				}
			}

			continue
		}

		flat := strings.ReplaceAll(block, "\n", " ")
		for _, sentence := range strings.Split(flat, ". ") {
			if strings.Contains(sentence, subject) {
				out = append(out, sentence)
			}
		}
	}

	return out
}

// pathsEndingIn lists the paths ending in a segment, sorted, for the message.
func pathsEndingIn(paths map[string]bool, leaf string) []string {
	var out []string
	for path := range paths {
		if path == leaf || strings.HasSuffix(path, "."+leaf) {
			out = append(out, path)
		}
	}
	sort.Strings(out)

	return out
}
