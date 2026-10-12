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

// docPath matches every dotted JSON path docs/agents.md names in backticks. A
// flag such as `--fail-on` and a kind such as `GPUNodeList` are not paths and
// do not match.
var docPath = regexp.MustCompile("`([a-z][A-Za-z0-9]*(?:\\.[a-z][A-Za-z0-9]*)+)`")

// partialReport is a real health report over a cluster that refused a read, so
// that it carries the field this test is about.
func partialReport(t *testing.T) (map[string]any, string) {
	t.Helper()

	out, errOut := &bytes.Buffer{}, &bytes.Buffer{}
	io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: out, ErrOut: errOut}

	clientset := fake.NewSimpleClientset(gpuNode("worker-1", 4, 4))
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
	command.SetArgs([]string{"-o", "json"})
	command.SilenceUsage = true
	command.SilenceErrors = true
	_ = command.Execute()

	var report map[string]any
	if err := json.Unmarshal(out.Bytes(), &report); err != nil {
		t.Fatalf("parsing the report: %v\n%s", err, out.String())
	}
	if _, ok := report["unreachable"]; !ok {
		t.Fatalf("the fixture produced no unreachable list, so this would compare nothing:\n%s", out.String())
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

	checked := 0
	for _, match := range docPath.FindAllStringSubmatch(readAgentsDoc(t), -1) {
		path := match[1]
		// Only the paths whose last segment this report has somewhere. A path
		// belonging to another kind, such as metadata.labels on a GPUNode, is
		// not this test's to check.
		last := path[strings.LastIndex(path, ".")+1:]
		candidates := pathsEndingIn(actual, last)
		if len(candidates) == 0 {
			continue
		}
		checked++
		if !actual[path] {
			t.Errorf("docs/%s names %q, which a health report does not carry; it has %v\n%s",
				agentsDoc, path, candidates, raw)
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
