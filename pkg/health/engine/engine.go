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

// Package engine runs health rules over one input and turns what they found
// into a report.
//
// It is the only place that knows the order of operations, and the order is
// the contract: narrow the input to what the user asked about, run every rule
// whose declared version range covers this cluster, fill in the catalog path
// for each finding, drop what the --node filter excludes, sort, and count.
//
// The engine reads no cluster and no clock. Everything it works from is the
// health.Input it is handed, which is what lets a cluster's worth of findings
// be reproduced from a table in a test.
package engine

import (
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// Options is what the command line asked for.
type Options struct {
	// Node, when set, restricts the run to one node: the rules see that node
	// alone, and the report keeps that node's findings plus the cluster scoped
	// ones. A name that is in neither node set is a usage error rather than an
	// empty report, because an empty report for a typo reads as "clean".
	Node string

	// InputErr is whatever building the Input returned. A source.PartialError,
	// or a join of them, makes the report partial and names what was missed;
	// anything else means the run did not happen and comes back as an error.
	//
	// It is an option rather than a second argument because a caller that
	// built its input successfully should not have to pass a nil for it.
	InputErr error
}

// Run evaluates rules over in and returns the report.
//
// The rules are taken as a slice rather than read off the package level
// registry, so that a caller, a test above all, says exactly which checks ran.
func Run(rules []health.Rule, in health.Input, opts Options) (*health.HealthReport, error) {
	unreachable, err := classifyInputError(opts.InputErr)
	if err != nil {
		return nil, err
	}

	// Copied before anything else touches it: the caller's input is the
	// caller's, and a rule that sorted a slice in place would otherwise change
	// what every later rule, and the caller, sees.
	in.Nodes = copyNodes(in.Nodes)
	in.Hinted = copyNodes(in.Hinted)

	checked, err := nodesChecked(in, opts.Node, unreachable, opts.InputErr)
	if err != nil {
		return nil, err
	}

	report := health.NewHealthReport()
	report.Partial = len(unreachable) > 0
	report.Unreachable = unreachable

	var found []output.Finding

	for _, rule := range rules {
		if rule == nil || !rule.Applies(in.OperatorVersion) {
			continue
		}

		// Over the whole input, never over the --node subset. A rule that
		// answers a question about the cluster rather than about one node at a
		// time, such as Track E2's topology check, has to see the peers to
		// have anything to say: handed one node it finds that node consistent
		// with itself and reports clean. The filter belongs on the findings.
		for _, finding := range rule.Evaluate(in) {
			// A finding with no link is a worse version of the problem the
			// user already had. A rule that set its own is left alone: it
			// meant the link it chose.
			if finding.Docs == "" {
				finding.Docs = health.CatalogPath(finding.Code)
			}

			if !keep(finding, opts.Node) {
				continue
			}

			found = append(found, finding)
		}
	}

	output.SortFindings(found)

	if found != nil {
		report.Findings = found
	}

	report.Summary = summarize(checked, report.Findings)

	return report, nil
}

// keep reports whether a finding survives the --node filter. A finding with no
// node is about the cluster, and a cluster that is broken is why the node the
// user asked about is broken, so it stays.
func keep(finding output.Finding, nodeFilter string) bool {
	return nodeFilter == "" || finding.Node == "" || finding.Node == nodeFilter
}

// nodesChecked returns the nodes the summary is counted over: every node in
// the input, or the one --node named.
//
// A name that matches nothing is an error rather than an empty report, because
// an empty report reads as "clean". Which error it is depends on whether the
// run saw the whole cluster: with a resource denied, the named node may be in
// the half that was not read, and telling the user to check their spelling
// would be the wrong advice and a claim this command never checked.
func nodesChecked(
	in health.Input,
	nodeFilter string,
	unreachable []output.Unreachable,
	inputErr error,
) ([]inventory.GPUNode, error) {
	all := make([]inventory.GPUNode, 0, len(in.Nodes)+len(in.Hinted))
	all = append(all, in.Nodes...)
	all = append(all, in.Hinted...)

	if nodeFilter == "" {
		return all, nil
	}

	var checked []inventory.GPUNode

	for _, node := range all {
		if node.Name == nodeFilter {
			checked = append(checked, node)
		}
	}

	if len(checked) > 0 {
		return checked, nil
	}

	// Said as what this command knows rather than as what the cluster holds.
	// gpu health only ever looks at GPU nodes and at nodes labelled as having
	// GPUs, so a name it cannot match is either a typo or an ordinary node
	// with no GPUs, and it cannot tell which: "no node named X" would be a
	// claim about the cluster that nothing here checked.
	usage := output.NewUsageError(
		"gpu health looked at no node named %q: it checks GPU nodes and nodes labelled as having GPUs, and %s",
		nodeFilter, knownNodes(all))

	if len(unreachable) > 0 {
		return nil, &partialNodeError{usage: usage, missed: unreachable, cause: inputErr}
	}

	return nil, usage
}

// partialNodeError is an unmatched --node on a run that could not read
// everything. It exits 3 rather than 2: the node may be in the part of the
// cluster that was denied, and sending the user off to check their spelling
// would be the wrong advice.
type partialNodeError struct {
	usage  *output.UsageError
	missed []output.Unreachable
	// cause is the builder's own error, kept so that errors.Is reaches the
	// PartialError underneath. A caller told its node is missing and not told
	// why the run could not see it has been handed half an answer.
	cause error
}

func (e *partialNodeError) Error() string {
	names := make([]string, 0, len(e.missed))
	for _, u := range e.missed {
		names = append(names, u.Resource)
	}

	sort.Strings(names)

	return e.usage.Error() + "; this run could not read " + join(names)
}

// Unwrap exposes both halves: the usage error a caller may be testing for, and
// the partial read that is the reason this is exit 3 rather than exit 2.
func (e *partialNodeError) Unwrap() []error { return []error{e.usage, e.cause} }

// ExitCode is what output.ExitCodeFor reads. It overrides the usage error's
// own 2, which is why this type exists at all.
func (e *partialNodeError) ExitCode() int { return output.ExitPartial }

// knownNodes renders what the user could have asked for instead.
func knownNodes(nodes []inventory.GPUNode) string {
	if len(nodes) == 0 {
		return "this run found none"
	}

	names := make([]string, 0, len(nodes))
	for _, n := range nodes {
		names = append(names, n.Name)
	}

	sort.Strings(names)

	return "this run found " + join(names)
}

// copyNodes deep copies a node set, so that a rule writing through a node's
// labels, deploy labels or operand slice writes into the engine's copy and
// never into the caller's input. GPUNode carries maps and slices, and a rule
// is only a pure function of its input if nothing it does survives the call.
func copyNodes(nodes []inventory.GPUNode) []inventory.GPUNode {
	if nodes == nil {
		return nil
	}

	out := make([]inventory.GPUNode, len(nodes))
	for i := range nodes {
		nodes[i].DeepCopyInto(&out[i])
	}

	return out
}

// summarize counts what was found over what was looked at.
func summarize(checked []inventory.GPUNode, findings []output.Finding) health.Summary {
	summary := health.Summary{NodesChecked: len(checked)}

	for _, f := range findings {
		switch f.Severity {
		case output.SeverityError:
			summary.Errors++
		case output.SeverityWarning:
			summary.Warnings++
		case output.SeverityInfo:
			summary.Infos++
		}
	}

	// Vacuously true over no nodes would print a requested count nobody
	// computed, so an empty run is reported as unknown rather than as zero.
	requestedKnown := len(checked) > 0

	for _, n := range checked {
		summary.GPUCapacity += n.Capacity
		summary.GPUAllocatable += n.Allocatable
		summary.GPURequested += n.Requested

		if !n.RequestedKnown {
			requestedKnown = false
		}
	}

	summary.GPURequestedKnown = requestedKnown

	return summary
}

// classifyInputError splits what building the input returned into the part the
// report can carry and the part that means the run did not happen.
//
// Every error in the tree has to be a source.PartialError. One that is not
// means something the findings depend on was never read, and reporting over
// what did arrive would be reporting on a cluster nobody looked at.
func classifyInputError(err error) ([]output.Unreachable, error) {
	if err == nil {
		return nil, nil
	}

	partials, ok := collectPartial(err)
	if !ok {
		return nil, &runError{err: err}
	}

	out := make([]output.Unreachable, 0, len(partials))
	for _, p := range partials {
		out = append(out, output.Unreachable{Resource: p.Resource, Reason: reasonFor(p.Err)})
	}

	// Sorted so that two runs against the same denied cluster produce the same
	// bytes: errors.Join keeps its own order, and a caller is free to change
	// the order it reads resources in.
	sort.Slice(out, func(i, j int) bool { return out[i].Resource < out[j].Resource })

	return out, nil
}

// runError says the run did not happen. It carries its own exit code because a
// join that mixes a partial read with a real failure still has a PartialError
// somewhere in it, and output.ExitCodeFor would otherwise find that one and
// report 3, "here are incomplete findings", for a run that produced none.
type runError struct{ err error }

func (e *runError) Error() string { return "reading the cluster: " + e.err.Error() }

// Unwrap keeps errors.Is and errors.As reaching the cause.
func (e *runError) Unwrap() error { return e.err }

// ExitCode is what output.ExitCodeFor reads: nothing was checked, so nothing
// can be concluded from this run.
func (e *runError) ExitCode() int { return output.ExitError }

// collectPartial walks an error tree and returns every PartialError in it. ok
// is false as soon as one leaf is something else.
//
// The walk is by type, and that is the whole point of it. The shape of the
// tree is not something this package controls: the builder wraps its reads
// with context, so a denied read arrives as fmt.Errorf("...: %w", partial),
// and several of them arrive as a wrapped errors.Join. A walk that only knew
// about Unwrap() []error stopped at the wrapper, found a PartialError
// underneath it with errors.As, and called the whole tree partial. That turns
// "the API server is unreachable, and by the way pods were denied" into exit
// 3, "here are some incomplete findings", over a run that found nothing at
// all.
//
// So each node is classified before anything is concluded from it:
//
//	*source.PartialError      a leaf, and the thing being collected
//	Unwrap() []error          recurse into every child, all must be partial
//	Unwrap() error            recurse into the one child
//	anything else             a hard failure, and the run did not happen
func collectPartial(err error) ([]*source.PartialError, bool) {
	if err == nil {
		return nil, true
	}

	// A direct type assertion rather than errors.As, which walks the chain:
	// the question here is what this one node is, not what is somewhere
	// underneath it. Checked first because a PartialError is itself
	// unwrappable, carrying the API error it was built from, and recursing
	// into that would find an ordinary forbidden error and call the tree hard.
	if partial, ok := err.(*source.PartialError); ok {
		return []*source.PartialError{partial}, true
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var out []*source.PartialError

		for _, child := range joined.Unwrap() {
			children, ok := collectPartial(child)
			if !ok {
				return nil, false
			}

			out = append(out, children...)
		}

		return out, true
	}

	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return collectPartial(wrapped.Unwrap())
	}

	return nil, false
}

// reasonFor turns the cause of a partial read into the short reason the report
// carries. A reader scanning a list of what was missed wants "forbidden", not
// an API server's sentence about it.
func reasonFor(err error) string {
	switch {
	case err == nil:
		return ""
	case apierrors.IsForbidden(err):
		return "forbidden"
	case apierrors.IsUnauthorized(err):
		return "unauthorized"
	case apierrors.IsNotFound(err):
		return "not found"
	default:
		return err.Error()
	}
}

// join renders a name list for a usage message.
func join(names []string) string {
	out := ""
	for i, name := range names {
		switch {
		case i == 0:
			out = name
		case i == len(names)-1:
			out += " and " + name
		default:
			out += ", " + name
		}
	}

	return out
}
