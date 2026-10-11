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

package engine_test

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/engine"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// fakeRule is a rule whose whole behaviour is in its fields, so that an engine
// test says what it is testing instead of standing up a real check.
type fakeRule struct {
	code     string
	severity output.Severity
	// minVersion and maxVersion are the declared range, both empty meaning
	// "every version".
	minVersion string
	maxVersion string
	// findings is what Evaluate returns, regardless of the input.
	findings []output.Finding
	// evaluate, when set, takes precedence over findings.
	evaluate func(health.Input) []output.Finding
	// calls counts the evaluations, so that a test can prove a rule did not
	// run rather than only that it produced nothing.
	calls *int
}

func (r fakeRule) Code() string              { return r.code }
func (r fakeRule) Severity() output.Severity { return r.severity }

func (r fakeRule) Applies(operatorVersion string) bool {
	return health.VersionInRange(operatorVersion, r.minVersion, r.maxVersion)
}

func (r fakeRule) Evaluate(in health.Input) []output.Finding {
	if r.calls != nil {
		*r.calls++
	}

	if r.evaluate != nil {
		return r.evaluate(in)
	}

	return r.findings
}

func node(name string) inventory.GPUNode {
	n := inventory.NewGPUNode(name)

	return *n
}

// Two runs over the same cluster have to produce the same bytes, so the engine
// sorts rather than handing back whatever order the rules happened to run in.
func TestRunSortsFindingsByNodeThenCode(t *testing.T) {
	t.Parallel()

	rules := []health.Rule{
		fakeRule{code: "NV-OP-001", findings: []output.Finding{
			{Code: "NV-OP-001", Node: "worker-2", Severity: output.SeverityWarning},
			{Code: "NV-OP-001", Severity: output.SeverityWarning},
		}},
		fakeRule{code: "NV-NODE-002", findings: []output.Finding{
			{Code: "NV-NODE-002", Node: "worker-2", Severity: output.SeverityError},
			{Code: "NV-NODE-002", Node: "worker-1", Severity: output.SeverityError},
		}},
	}

	report, err := engine.Run(rules, health.Input{Nodes: []inventory.GPUNode{node("worker-1"), node("worker-2")}}, engine.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := [][2]string{
		// The cluster scoped finding has no node, and the empty string sorts
		// first, so it leads the report.
		{"", "NV-OP-001"},
		{"worker-1", "NV-NODE-002"},
		{"worker-2", "NV-NODE-002"},
		{"worker-2", "NV-OP-001"},
	}

	if got := nodeCodes(report.Findings); !reflect.DeepEqual(got, want) {
		t.Errorf("findings are ordered\n%v\nwant\n%v", got, want)
	}
}

// A finding with no link and no next command is a worse version of the problem
// the user already had, so the engine fills the catalog path for every code it
// emits and leaves the rule's own next command alone.
func TestRunFillsDocsAndKeepsNext(t *testing.T) {
	t.Parallel()

	rules := []health.Rule{fakeRule{code: "NV-NODE-002", findings: []output.Finding{
		{Code: "NV-NODE-002", Node: "worker-1", Severity: output.SeverityError, Next: "kubectl nv info worker-1"},
	}}}

	report, err := engine.Run(rules, health.Input{Nodes: []inventory.GPUNode{node("worker-1")}}, engine.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(report.Findings) != 1 {
		t.Fatalf("got %d findings, want 1", len(report.Findings))
	}

	if got, want := report.Findings[0].Docs, "docs/findings/NV-NODE-002.md"; got != want {
		t.Errorf("Docs is %q, want %q", got, want)
	}

	if got, want := report.Findings[0].Next, "kubectl nv info worker-1"; got != want {
		t.Errorf("Next is %q, want the rule's own %q", got, want)
	}
}

// A rule that set its own Docs meant it: the engine fills the field, it does
// not overwrite it.
func TestRunDoesNotOverwriteDocsARuleSet(t *testing.T) {
	t.Parallel()

	rules := []health.Rule{fakeRule{code: "NV-OP-001", findings: []output.Finding{
		{Code: "NV-OP-001", Severity: output.SeverityError, Docs: "https://docs.nvidia.com/datacenter/cloud-native/"},
	}}}

	report, err := engine.Run(rules, health.Input{}, engine.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got, want := report.Findings[0].Docs, "https://docs.nvidia.com/datacenter/cloud-native/"; got != want {
		t.Errorf("Docs is %q, want the rule's own %q", got, want)
	}
}

// A rule whose declared range excludes the cluster's operator version must not
// run at all. Producing no findings is not the same thing: a rule that reads a
// field which does not exist on this version can produce a false one.
func TestRunSkipsRulesOutsideTheirVersionRange(t *testing.T) {
	t.Parallel()

	var applicable, excluded int

	rules := []health.Rule{
		fakeRule{code: "NV-NODE-002", minVersion: "v25.3.0", calls: &applicable},
		fakeRule{code: "NV-NODE-003", minVersion: "v99.0.0", calls: &excluded, findings: []output.Finding{
			{Code: "NV-NODE-003", Node: "worker-1", Severity: output.SeverityWarning},
		}},
	}

	report, err := engine.Run(rules, health.Input{OperatorVersion: "v25.10.0"}, engine.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if applicable != 1 {
		t.Errorf("the in-range rule ran %d times, want 1", applicable)
	}

	if excluded != 0 {
		t.Errorf("the out-of-range rule ran %d times, want 0", excluded)
	}

	if len(report.Findings) != 0 {
		t.Errorf("got %v, want no findings from an excluded rule", report.Findings)
	}
}

// --node is the flag a user reaches for when one machine is misbehaving. It
// keeps that node's findings and the cluster scoped ones: a node reported
// clean while the ClusterPolicy it depends on is broken is a false clean, and
// a false clean is the worst answer this tool can give.
func TestRunFiltersByNode(t *testing.T) {
	t.Parallel()

	rules := []health.Rule{fakeRule{code: "NV-NODE-002", findings: []output.Finding{
		{Code: "NV-NODE-002", Node: "worker-1", Severity: output.SeverityError},
		{Code: "NV-NODE-002", Node: "worker-2", Severity: output.SeverityError},
		{Code: "NV-NODE-002", Severity: output.SeverityError},
	}}}

	in := health.Input{Nodes: []inventory.GPUNode{node("worker-1"), node("worker-2")}}

	report, err := engine.Run(rules, in, engine.Options{Node: "worker-1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := [][2]string{{"", "NV-NODE-002"}, {"worker-1", "NV-NODE-002"}}
	if got := nodeCodes(report.Findings); !reflect.DeepEqual(got, want) {
		t.Errorf("findings are %v, want %v", got, want)
	}

	if report.Summary.NodesChecked != 1 {
		t.Errorf("NodesChecked is %d, want 1: --node narrows what was looked at", report.Summary.NodesChecked)
	}
}

// --node filters the findings and not the input the rules see.
//
// A rule that answers a question about the cluster, rather than about one node
// at a time, has to see the whole cluster to answer it at all: Track E2's
// topology rule compares a node against its peers, and a node handed that rule
// on its own looks consistent with itself. So the rules run over everything
// and the filter is applied afterwards, to the findings.
func TestRunShowsRulesTheWholeClusterUnderANodeFilter(t *testing.T) {
	t.Parallel()

	var seen []string

	rules := []health.Rule{fakeRule{code: "NV-NODE-002", evaluate: func(in health.Input) []output.Finding {
		for _, n := range in.Nodes {
			seen = append(seen, n.Name)
		}

		for _, n := range in.Hinted {
			seen = append(seen, n.Name)
		}

		return nil
	}}}

	in := health.Input{
		Nodes:  []inventory.GPUNode{node("worker-1"), node("worker-2")},
		Hinted: []inventory.GPUNode{node("control-plane")},
	}

	if _, err := engine.Run(rules, in, engine.Options{Node: "worker-2"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []string{"worker-1", "worker-2", "control-plane"}
	if !reflect.DeepEqual(seen, want) {
		t.Errorf("the rule saw %v, want the whole cluster %v", seen, want)
	}
}

// The rule that needs the whole cluster still has to report through the
// filter: a cross-node rule that concluded something about worker-2 by looking
// at worker-1 is answering the question the user asked.
func TestRunKeepsACrossNodeFindingUnderANodeFilter(t *testing.T) {
	t.Parallel()

	rules := []health.Rule{fakeRule{code: "NV-TOPO-001", evaluate: func(in health.Input) []output.Finding {
		// The shape of a cross-node rule: it concludes nothing until it has
		// seen more than one node.
		if len(in.Nodes) < 2 {
			return nil
		}

		var out []output.Finding
		for _, n := range in.Nodes {
			out = append(out, output.Finding{
				Code: "NV-TOPO-001", Node: n.Name, Severity: output.SeverityWarning,
			})
		}

		return out
	}}}

	in := health.Input{Nodes: []inventory.GPUNode{node("worker-1"), node("worker-2")}}

	report, err := engine.Run(rules, in, engine.Options{Node: "worker-2"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := [][2]string{{"worker-2", "NV-TOPO-001"}}
	if got := nodeCodes(report.Findings); !reflect.DeepEqual(got, want) {
		t.Errorf("findings are %v, want %v", got, want)
	}
}

// A hinted node is still a node a user can ask about: it is the most common
// thing gpu health is run to find.
func TestRunAcceptsAHintedNodeFilter(t *testing.T) {
	t.Parallel()

	in := health.Input{Hinted: []inventory.GPUNode{node("control-plane")}}

	report, err := engine.Run(nil, in, engine.Options{Node: "control-plane"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report.Summary.NodesChecked != 1 {
		t.Errorf("NodesChecked is %d, want 1", report.Summary.NodesChecked)
	}
}

// A name --node does not match must not report clean.
//
// The message matters as much as the exit code. gpu health only ever looks at
// GPU nodes and the nodes hinted as ones, so a name it cannot match is either
// a typo or a node of the cluster that has no GPUs at all, and it cannot tell
// which. Saying "no node named X" would be a claim about the cluster that this
// command never checked, so it says what it does know and lists what it did
// look at.
func TestRunRejectsANodeItDidNotLookAt(t *testing.T) {
	t.Parallel()

	in := health.Input{
		Nodes:  []inventory.GPUNode{node("worker-1")},
		Hinted: []inventory.GPUNode{node("control-plane")},
	}

	report, err := engine.Run(nil, in, engine.Options{Node: "wroker-1"})
	if err == nil {
		t.Fatalf("Run returned %+v and no error, want a usage error", report)
	}

	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("the error exits %d, want %d (usage): %v", got, output.ExitUsage, err)
	}

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("Run returned %T, want an *output.UsageError", err)
	}

	want := `gpu health looked at no node named "wroker-1": it checks GPU nodes and nodes ` +
		`labelled as having GPUs, and this run found control-plane and worker-1`
	if err.Error() != want {
		t.Errorf("the message is\n  %q\nwant\n  %q", err.Error(), want)
	}
}

// The same with nothing to list at all. "This run found none" is a different
// thing to tell a user from a list of names, and it is the case they hit when
// their kubeconfig points at the wrong cluster.
func TestRunRejectsANodeWhenItFoundNoneAtAll(t *testing.T) {
	t.Parallel()

	_, err := engine.Run(nil, health.Input{}, engine.Options{Node: "worker-1"})
	if err == nil {
		t.Fatal("Run returned no error over an empty cluster")
	}

	want := `gpu health looked at no node named "worker-1": it checks GPU nodes and nodes ` +
		`labelled as having GPUs, and this run found none`
	if err.Error() != want {
		t.Errorf("the message is\n  %q\nwant\n  %q", err.Error(), want)
	}
}

// An unmatched --node on a run that could not read everything is not a usage
// mistake to report as one: the node may be in the half of the cluster that
// was denied. Exit 3 says "conclude nothing from this", which is true, where
// exit 2 would send the user off to check their spelling.
func TestRunRejectsAnUnknownNodeAsPartialWhenTheReadWasPartial(t *testing.T) {
	t.Parallel()

	inputErr := &source.PartialError{Resource: "nodes/worker-9/proxy", Err: errors.New("forbidden")}

	in := health.Input{Nodes: []inventory.GPUNode{node("worker-1")}}

	_, err := engine.Run(nil, in, engine.Options{Node: "worker-9", InputErr: inputErr})
	if err == nil {
		t.Fatal("Run returned no error")
	}

	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Errorf("the error exits %d, want %d (partial): %v", got, output.ExitPartial, err)
	}

	// The cause has to travel with it, or the user is told their node is
	// missing and never told why the run could not see it.
	if !errors.Is(err, inputErr) {
		t.Errorf("the error does not wrap the partial cause: %v", err)
	}

	if !strings.Contains(err.Error(), "nodes/worker-9/proxy") {
		t.Errorf("the message does not name what could not be read: %v", err)
	}
}

// The summary is the arithmetic a reader trusts when the list is too long to
// read, so it is counted rather than estimated.
func TestRunSummaryArithmetic(t *testing.T) {
	t.Parallel()

	worker1 := node("worker-1")
	worker1.Capacity, worker1.Allocatable, worker1.Requested, worker1.RequestedKnown = 8, 8, 3, true

	worker2 := node("worker-2")
	worker2.Capacity, worker2.Allocatable, worker2.Requested, worker2.RequestedKnown = 4, 2, 1, true

	rules := []health.Rule{fakeRule{code: "NV-NODE-002", findings: []output.Finding{
		{Code: "NV-NODE-002", Node: "worker-1", Severity: output.SeverityError},
		{Code: "NV-NODE-002", Node: "worker-2", Severity: output.SeverityError},
		{Code: "NV-NODE-002", Node: "worker-2", Severity: output.SeverityWarning},
		{Code: "NV-NODE-002", Severity: output.SeverityInfo},
	}}}

	in := health.Input{
		Nodes:  []inventory.GPUNode{worker1, worker2},
		Hinted: []inventory.GPUNode{node("control-plane")},
	}

	report, err := engine.Run(rules, in, engine.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := health.Summary{
		Errors: 2, Warnings: 1, Infos: 1,
		NodesChecked:      3,
		GPUCapacity:       12,
		GPUAllocatable:    10,
		GPURequested:      4,
		GPURequestedKnown: false,
	}

	if report.Summary != want {
		t.Errorf("Summary is\n%+v\nwant\n%+v", report.Summary, want)
	}
}

// Requested is only reported as known when every node checked had its pods
// counted. Reporting four requested GPUs across a cluster where one node was
// never listed is a number a capacity planner would act on and should not.
func TestRunReportsRequestedAsKnownOnlyWhenEveryNodeWasCounted(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		nodes []inventory.GPUNode
		want  bool
	}{
		{
			name: "every node counted",
			nodes: []inventory.GPUNode{
				withRequested(node("worker-1"), 2, true),
				withRequested(node("worker-2"), 1, true),
			},
			want: true,
		},
		{
			name: "one node not counted",
			nodes: []inventory.GPUNode{
				withRequested(node("worker-1"), 2, true),
				withRequested(node("worker-2"), 0, false),
			},
			want: false,
		},
		{
			name:  "no nodes at all",
			nodes: nil,
			want:  false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report, err := engine.Run(nil, health.Input{Nodes: tc.nodes}, engine.Options{})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if report.Summary.GPURequestedKnown != tc.want {
				t.Errorf("GPURequestedKnown is %v, want %v", report.Summary.GPURequestedKnown, tc.want)
			}
		})
	}
}

// The exit code is what a script branches on, and it comes off the report
// against the --fail-on threshold rather than off a count the engine kept.
func TestReportExitCodeAgainstFailOn(t *testing.T) {
	t.Parallel()

	rules := []health.Rule{fakeRule{code: "NV-NODE-003", findings: []output.Finding{
		{Code: "NV-NODE-003", Node: "worker-1", Severity: output.SeverityWarning},
	}}}

	report, err := engine.Run(rules, health.Input{Nodes: []inventory.GPUNode{node("worker-1")}}, engine.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, tc := range []struct {
		failOn string
		want   int
	}{
		{"", output.ExitOK},
		{"error", output.ExitOK},
		{"warning", output.ExitFindings},
		{"info", output.ExitFindings},
	} {
		t.Run("fail-on="+tc.failOn, func(t *testing.T) {
			severity, err := output.ParseSeverity(tc.failOn)
			if err != nil {
				t.Fatalf("ParseSeverity(%q): %v", tc.failOn, err)
			}

			if got := report.ExitCode(severity); got != tc.want {
				t.Errorf("ExitCode(%q) = %d, want %d", severity, got, tc.want)
			}
		})
	}
}

// A resource the run was not allowed to read makes the result partial, not
// wrong: the findings stand, and the report says what is missing from them so
// that nobody reads "clean" out of a scan that did not happen.
func TestRunReportsPartialInput(t *testing.T) {
	t.Parallel()

	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Resource: "resourceslices"}, "", errors.New("no"))
	notFound := &source.PartialError{Resource: "clusterpolicies", Err: errors.New("no such file")}

	inputErr := errors.Join(
		&source.PartialError{Resource: "resourceslices", Err: forbidden},
		notFound,
	)

	rules := []health.Rule{fakeRule{code: "NV-NODE-002", findings: []output.Finding{
		{Code: "NV-NODE-002", Node: "worker-1", Severity: output.SeverityError},
	}}}

	report, err := engine.Run(rules, health.Input{Nodes: []inventory.GPUNode{node("worker-1")}}, engine.Options{InputErr: inputErr})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !report.Partial {
		t.Error("Partial is false, want true for a run that could not read everything")
	}

	// Both names survive the join: a report that lost one of them would be
	// telling the reader to guess which half of the cluster is missing.
	want := []output.Unreachable{
		{Resource: "clusterpolicies", Reason: "no such file"},
		{Resource: "resourceslices", Reason: "forbidden"},
	}
	if !reflect.DeepEqual(report.Unreachable, want) {
		t.Errorf("Unreachable is\n%+v\nwant\n%+v", report.Unreachable, want)
	}

	if len(report.Findings) != 1 {
		t.Errorf("got %d findings, want the rules to have run anyway", len(report.Findings))
	}

	if got := report.ExitCode(output.SeverityError); got != output.ExitFindings {
		t.Errorf("ExitCode = %d, want %d: findings win over partial", got, output.ExitFindings)
	}
}

// A partial run that found nothing still must not exit zero. "Clean" is a
// claim about a cluster that was read, and this one was not.
func TestPartialAndCleanExitsThree(t *testing.T) {
	t.Parallel()

	inputErr := &source.PartialError{Resource: "pods", Err: errors.New("forbidden")}

	report, err := engine.Run(nil, health.Input{}, engine.Options{InputErr: inputErr})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := report.ExitCode(""); got != output.ExitPartial {
		t.Errorf("ExitCode = %d, want %d", got, output.ExitPartial)
	}
}

// An error that is not a PartialError means the run did not happen. Carrying
// on and reporting findings over whatever made it into the input would be
// reporting on a cluster nobody read.
func TestRunRefusesANonPartialInputError(t *testing.T) {
	t.Parallel()

	boom := errors.New("the API server is unreachable")

	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "on its own", err: boom},
		{
			name: "joined with a partial one",
			err:  errors.Join(&source.PartialError{Resource: "pods", Err: errors.New("forbidden")}, boom),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report, err := engine.Run(nil, health.Input{}, engine.Options{InputErr: tc.err})
			if err == nil {
				t.Fatalf("Run returned %+v and no error", report)
			}

			if !errors.Is(err, boom) {
				t.Errorf("Run returned %v, want it to wrap the cause", err)
			}

			if got := output.ExitCodeFor(err); got != output.ExitError {
				t.Errorf("the error exits %d, want %d", got, output.ExitError)
			}
		})
	}
}

// The error the builder returns is a tree, and the walk over it has to be by
// type rather than by shape. fmt.Errorf wraps a join in something whose own
// Unwrap returns one error, so a walk that only knew about Unwrap() []error
// stopped at the wrapper, found a PartialError underneath it through
// errors.As, and called the whole thing partial. That turns "the API server is
// unreachable" into "here are some incomplete findings", which is exit 3 over
// a run that checked nothing.
func TestRunWalksAWrappedErrorTree(t *testing.T) {
	t.Parallel()

	hard := errors.New("the API server is unreachable")
	partial := &source.PartialError{Resource: "pods", Err: errors.New("forbidden")}

	_, err := engine.Run(nil, health.Input{}, engine.Options{
		InputErr: fmt.Errorf("building the inventory: %w", errors.Join(partial, hard)),
	})
	if err == nil {
		t.Fatal("Run returned no error for a wrapped join holding a hard failure")
	}

	if got := output.ExitCodeFor(err); got != output.ExitError {
		t.Errorf("the error exits %d, want %d: %v", got, output.ExitError, err)
	}

	if !errors.Is(err, hard) {
		t.Errorf("the error does not wrap the cause: %v", err)
	}
}

// The same tree with nothing hard in it is partial, and every resource in it
// has to come out: a report that lost one tells the reader to guess which part
// of the cluster is missing from the findings.
func TestRunWalksAWrappedJoinOfPartials(t *testing.T) {
	t.Parallel()

	first := &source.PartialError{Resource: "resourceslices", Err: errors.New("forbidden")}
	second := &source.PartialError{Resource: "clusterpolicies", Err: errors.New("not found")}

	report, err := engine.Run(nil, health.Input{}, engine.Options{
		InputErr: fmt.Errorf("building the inventory: %w", errors.Join(first, second)),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []output.Unreachable{
		{Resource: "clusterpolicies", Reason: "not found"},
		{Resource: "resourceslices", Reason: "forbidden"},
	}
	if !reflect.DeepEqual(report.Unreachable, want) {
		t.Errorf("Unreachable is\n%+v\nwant\n%+v", report.Unreachable, want)
	}
}

// A PartialError reached through a single-error wrapper is still partial: the
// builder wraps its reads with context, and a walk that only recognised a bare
// one would call every wrapped denial a hard failure.
func TestRunWalksASinglyWrappedPartial(t *testing.T) {
	t.Parallel()

	partial := &source.PartialError{Resource: "pods", Err: errors.New("forbidden")}

	report, err := engine.Run(nil, health.Input{}, engine.Options{
		InputErr: fmt.Errorf("listing the operand pods: %w", partial),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []output.Unreachable{{Resource: "pods", Reason: "forbidden"}}
	if !reflect.DeepEqual(report.Unreachable, want) {
		t.Errorf("Unreachable is %+v, want %+v", report.Unreachable, want)
	}
}

// Rules are pure functions of their input and the engine is what guarantees
// it: one rule that sorted the node slice in place would silently change what
// every later rule sees.
func TestRunDoesNotMutateTheInput(t *testing.T) {
	t.Parallel()

	worker := node("worker-1")
	worker.ObjectMeta = metav1.ObjectMeta{Name: "worker-1", Labels: map[string]string{"nvidia.com/gpu.present": "true"}}
	worker.Deploy = map[string]string{"device-plugin": "true"}

	in := health.Input{Nodes: []inventory.GPUNode{worker}, Hinted: []inventory.GPUNode{node("control-plane")}}
	before := fmt.Sprintf("%+v", in)

	rules := []health.Rule{fakeRule{code: "NV-NODE-002", evaluate: func(got health.Input) []output.Finding {
		if len(got.Nodes) > 0 {
			got.Nodes[0].Labels["nvidia.com/gpu.present"] = "false"
			got.Nodes[0].Deploy["device-plugin"] = "false"
		}

		return nil
	}}}

	if _, err := engine.Run(rules, in, engine.Options{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if after := fmt.Sprintf("%+v", in); after != before {
		t.Errorf("Run let a rule change the caller's input:\n got %s\nwant %s", after, before)
	}
}

// An empty run is a legitimate answer, and it has to come back as a report
// rather than as a nil the caller dereferences.
func TestRunOverNoRulesAndNoNodes(t *testing.T) {
	t.Parallel()

	report, err := engine.Run(nil, health.Input{}, engine.Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if report == nil {
		t.Fatal("Run returned a nil report")
	}

	if report.Findings == nil {
		t.Error("Findings is nil, want an empty slice")
	}

	if report.Kind != output.KindHealthReport {
		t.Errorf("Kind is %q, want %q: the report has to be printable", report.Kind, output.KindHealthReport)
	}

	if got := report.ExitCode(""); got != output.ExitOK {
		t.Errorf("ExitCode = %d, want %d", got, output.ExitOK)
	}
}

// withRequested is the one-liner the requested-known table would otherwise
// repeat three times.
func withRequested(n inventory.GPUNode, requested int64, known bool) inventory.GPUNode {
	n.Requested, n.RequestedKnown = requested, known

	return n
}

// nodeCodes reduces findings to the pair the ordering assertions are about.
func nodeCodes(findings []output.Finding) [][2]string {
	out := make([][2]string, 0, len(findings))
	for _, f := range findings {
		out = append(out, [2]string{f.Node, f.Code})
	}

	return out
}
