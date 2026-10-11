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

package inventory

import (
	"context"
	"errors"
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/labels"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// BuildOptions is what a command asks the builder for.
type BuildOptions struct {
	// Node narrows the result to one node, by name. It reaches the hinted
	// nodes too: the node a user names is most often the one that went quiet.
	Node string
	// Selector is a label selector the nodes have to match.
	Selector string
	// NoPods skips the pod listing. Requested is then not computed and every
	// node says so through RequestedKnown, and no operands are reported,
	// because both come out of that one read.
	NoPods bool
	// IncludeHinted keeps the nodes that carry a GPU hint label and advertise
	// no GPUs. They are out of the list by default, because on most clusters
	// they are a control plane that node-feature-discovery labelled.
	IncludeHinted bool
	// WithDetails fills the fields gpu info shows and gpu list does not: the
	// node conditions and the names of its ResourceSlices.
	WithDetails bool
}

// Build reads the cluster through src and returns the GPU nodes it holds.
//
// The whole inventory is three reads, whatever the cluster's size: one node
// classification, one pod list over every namespace, one ResourceSlice list.
// There is no per-node read at all, because on a two thousand node cluster
// that is two thousand round trips for data the list already carried.
//
// A source.PartialError comes back unchanged, alongside the nodes that were
// read. That is the exit code 3 contract: the answer is incomplete and usable,
// and the caller must not treat it as "no result". Anything else means the run
// did not happen and no list is returned with it.
func Build(ctx context.Context, src source.Source, opts BuildOptions) (*GPUNodeList, error) {
	list, _, err := BuildWithHinted(ctx, src, opts)

	return list, err
}

// BuildWithHinted is Build, and also reports how many hinted nodes were left
// out because IncludeHinted was not set.
//
// The count is what gpu list says on stderr, and it has to come from the same
// pass: asking the cluster a second time to count them would be a second read
// of a second moment, and the number printed would not be the number of nodes
// missing from the list beside it.
func BuildWithHinted(
	ctx context.Context,
	src source.Source,
	opts BuildOptions,
) (*GPUNodeList, int, error) {
	selector, err := parseSelector(opts.Selector)
	if err != nil {
		return nil, 0, err
	}

	// Every read below may come back partial. They are collected rather than
	// returned at the first one, so that a run denied two resources names both.
	var partials []error

	gpu, hinted, err := src.ClassifyNodes(ctx)
	if err != nil {
		if !isPartial(err) {
			return nil, 0, fmt.Errorf("classifying the cluster's nodes: %w", err)
		}

		partials = append(partials, err)
	}

	slices, err := src.ResourceSlices(ctx)
	if err != nil {
		if !isPartial(err) {
			return nil, 0, fmt.Errorf("reading the GPU resource slices: %w", err)
		}

		partials = append(partials, err)
	}

	var (
		pods           []corev1.Pod
		requestedKnown bool
	)

	if !opts.NoPods {
		// One list over every namespace, unfiltered: the requested GPUs come
		// from every pod in the cluster and the operands from the operator's
		// namespace, and both are read out of the same pass.
		pods, err = src.Pods(ctx, "", "")
		if err != nil {
			if !isPartial(err) {
				return nil, 0, fmt.Errorf("listing the cluster's pods: %w", err)
			}

			partials = append(partials, err)
		}

		// A partial pod list is not a pod list. Summing half of one and
		// presenting the total as the node's GPU demand would be a wrong
		// answer dressed as a right one, so the number is withheld while the
		// operands, which are per pod rather than a total, are kept.
		requestedKnown = err == nil
	}

	gpuNodes := filterNodes(gpu, opts.Node, selector)

	hintedNodes := filterNodes(hinted, opts.Node, selector)
	excluded := 0

	if !opts.IncludeHinted {
		excluded = len(hintedNodes)
		hintedNodes = nil
	}

	out := NewGPUNodeList()
	out.Items = make([]GPUNode, 0, len(gpuNodes)+len(hintedNodes))

	shared := clusterFacts{
		slices:         slicesByNode(slices),
		requested:      requestedByNode(pods),
		requestedKnown: requestedKnown,
		operands:       operandsByNode(pods, operatorNamespace(pods)),
		withDetails:    opts.WithDetails,
	}

	for i := range gpuNodes {
		out.Items = append(out.Items, *buildNode(&gpuNodes[i], shared.forNode(gpuNodes[i].Name, false)))
	}

	for i := range hintedNodes {
		out.Items = append(out.Items, *buildNode(&hintedNodes[i], shared.forNode(hintedNodes[i].Name, true)))
	}

	// One order for the whole list, not one per set: a hinted node belongs
	// between its neighbours, not in an appendix.
	sort.Slice(out.Items, func(i, j int) bool { return out.Items[i].Name < out.Items[j].Name })

	return out, excluded, joinPartials(partials)
}

// joinPartials combines the partial reads into one error, naming each lost
// resource once.
//
// Two reads can lose the same resource: the node classification needs the
// ResourceSlices to decide membership and the builder needs them again by
// node, so a denial on that collection arrives twice. The user reads this
// string, and "could not read resourceslices: forbidden; could not read
// resourceslices: forbidden" reads as two problems and invites a search for a
// second cause that does not exist.
//
// Only the first cause per resource is kept, and it is kept whole, so
// errors.As and errors.Is still reach a real *PartialError.
func joinPartials(partials []error) error {
	if len(partials) == 0 {
		return nil
	}

	seen := make(map[string]struct{}, len(partials))
	kept := make([]error, 0, len(partials))

	for _, err := range partials {
		var partial *source.PartialError
		if !errors.As(err, &partial) {
			kept = append(kept, err)

			continue
		}

		if _, dup := seen[partial.Resource]; dup {
			continue
		}

		seen[partial.Resource] = struct{}{}
		kept = append(kept, err)
	}

	return errors.Join(kept...)
}

// clusterFacts is what the three reads said, indexed by node.
type clusterFacts struct {
	slices         map[string][]string
	requested      map[string]int64
	requestedKnown bool
	operands       map[string][]OperandPod
	withDetails    bool
}

// forNode narrows the cluster's facts to one node.
func (f clusterFacts) forNode(name string, hinted bool) nodeContext {
	return nodeContext{
		hinted:         hinted,
		sliceNames:     f.slices[name],
		requested:      f.requested[name],
		requestedKnown: f.requestedKnown,
		operands:       f.operands[name],
		withDetails:    f.withDetails,
	}
}

// parseSelector turns a -l value into a selector. One that does not parse is
// the user's to fix, and exit 2 says so: silently matching everything would
// hand back a whole cluster where a filtered one was asked for.
func parseSelector(value string) (labels.Selector, error) {
	if value == "" {
		return labels.Everything(), nil
	}

	selector, err := labels.Parse(value)
	if err != nil {
		return nil, output.WrapUsageError(err, "could not parse the label selector %q", value)
	}

	return selector, nil
}

// filterNodes applies --node and -l. Both are client side, because the node
// list is read once for the whole command and neither filter is worth a second
// pass over the cluster.
func filterNodes(nodes []corev1.Node, name string, selector labels.Selector) []corev1.Node {
	out := make([]corev1.Node, 0, len(nodes))

	for i := range nodes {
		node := nodes[i]
		if name != "" && node.Name != name {
			continue
		}

		if !selector.Matches(labels.Set(node.Labels)) {
			continue
		}

		out = append(out, node)
	}

	return out
}

// slicesByNode groups the names of the gpu.nvidia.com ResourceSlices by the
// node each one is published for, sorted.
//
// A slice that names no node is skipped: it describes a pool spanning nodes by
// selector, and attributing it to one of them would be a guess.
func slicesByNode(slices []resourcev1.ResourceSlice) map[string][]string {
	var out map[string][]string

	for i := range slices {
		spec := &slices[i].Spec
		if spec.Driver != source.DRADriverName || spec.NodeName == nil || *spec.NodeName == "" {
			continue
		}

		if out == nil {
			out = map[string][]string{}
		}

		out[*spec.NodeName] = append(out[*spec.NodeName], slices[i].Name)
	}

	for node := range out {
		sort.Strings(out[node])
	}

	return out
}

// isPartial reports whether err is a read that lost one resource and kept the
// rest, as opposed to a run that did not happen.
func isPartial(err error) bool {
	var partial *source.PartialError

	return errors.As(err, &partial)
}
