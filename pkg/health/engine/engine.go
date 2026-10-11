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
	"errors"
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
	in, err = narrow(in, opts.Node)
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

	report.Summary = summarize(in, report.Findings)

	return report, nil
}

// keep reports whether a finding survives the --node filter. A finding with no
// node is about the cluster, and a cluster that is broken is why the node the
// user asked about is broken, so it stays.
func keep(finding output.Finding, nodeFilter string) bool {
	return nodeFilter == "" || finding.Node == "" || finding.Node == nodeFilter
}

// narrow returns a copy of in restricted to nodeFilter, or a copy of in when
// no filter was given. The copy is shallow per node, which is enough: a rule
// that writes through a node's maps is writing into the caller's input, and
// the deep copy below is what stops it.
func narrow(in health.Input, nodeFilter string) (health.Input, error) {
	out := in
	out.Nodes = copyNodes(in.Nodes)
	out.Hinted = copyNodes(in.Hinted)

	if nodeFilter == "" {
		return out, nil
	}

	nodes := selectNode(out.Nodes, nodeFilter)
	hinted := selectNode(out.Hinted, nodeFilter)

	if len(nodes)+len(hinted) == 0 {
		// The node list is a primary read: a failure to list nodes is a plain
		// error rather than a partial one, so a name missing from the input is
		// a name missing from the cluster.
		return out, output.NewUsageError(
			"no node named %q: --node names a node of this cluster, and %s",
			nodeFilter, knownNodes(in))
	}

	out.Nodes = nodes
	out.Hinted = hinted

	return out, nil
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

// selectNode returns the nodes named name, as a fresh slice.
func selectNode(nodes []inventory.GPUNode, name string) []inventory.GPUNode {
	var out []inventory.GPUNode

	for _, n := range nodes {
		if n.Name == name {
			out = append(out, n)
		}
	}

	return out
}

// knownNodes renders what the user could have asked for instead.
func knownNodes(in health.Input) string {
	names := make([]string, 0, len(in.Nodes)+len(in.Hinted))
	for _, n := range in.Nodes {
		names = append(names, n.Name)
	}

	for _, n := range in.Hinted {
		names = append(names, n.Name)
	}

	if len(names) == 0 {
		return "this run found none"
	}

	sort.Strings(names)

	return "this run found " + join(names)
}

// summarize counts what was found over what was looked at.
func summarize(in health.Input, findings []output.Finding) health.Summary {
	summary := health.Summary{NodesChecked: len(in.Nodes) + len(in.Hinted)}

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
	requestedKnown := summary.NodesChecked > 0

	for _, set := range [][]inventory.GPUNode{in.Nodes, in.Hinted} {
		for _, n := range set {
			summary.GPUCapacity += n.Capacity
			summary.GPUAllocatable += n.Allocatable
			summary.GPURequested += n.Requested

			if !n.RequestedKnown {
				requestedKnown = false
			}
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

// collectPartial walks an errors.Join tree and returns every PartialError in
// it. ok is false as soon as one leaf is not a PartialError.
func collectPartial(err error) ([]*source.PartialError, bool) {
	var partial *source.PartialError
	if errors.As(err, &partial) {
		// errors.As reaches through a wrapper, so a single PartialError, and a
		// PartialError somebody wrapped with context, both land here.
		if joined, ok := err.(interface{ Unwrap() []error }); !ok || len(joined.Unwrap()) == 0 {
			return []*source.PartialError{partial}, true
		}
	}

	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return nil, false
	}

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
