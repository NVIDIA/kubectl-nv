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

// Package topology carries the "kubectl nv topology" command: how the GPU
// nodes of a cluster are wired together, and where a job would fit.
package topology

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// reader is everything this command reads: the inventory's own source, plus
// the three reads Track G added to it. It is an interface rather than
// *source.Live so that the tests drive the command over fakes, and so that a
// must-gather bundle can be read through the same code once the Kueue objects
// are collected into one.
type reader interface {
	source.Source

	Topologies(ctx context.Context) ([]unstructured.Unstructured, error)
	ResourceFlavors(ctx context.Context) ([]unstructured.Unstructured, error)
	Job(ctx context.Context, namespace, name string) (*batchv1.Job, error)
}

// options is the state one run of the command carries.
type options struct {
	out    *output.OutputOptions
	forRef string
}

// NewCommand builds the topology command over the shared options.
func NewCommand(o *cmd.Options) *cobra.Command {
	opts := &options{}

	c := &cobra.Command{
		Use:   "topology",
		Short: "Show how the GPU nodes are wired together, and where a job fits",
		Long: "Group the GPU nodes by NVLink clique, fabric tier, accelerator domain and, " +
			"where topology aware scheduling is in use, by the levels a Kueue Topology declares. " +
			"Each group reports the GPUs it has, the GPUs already spoken for and the GPUs left, " +
			"and --for job/<name> answers where every pod of that job would fit.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(c *cobra.Command, _ []string) error {
			return run(c.Context(), o, opts)
		},
	}

	// -o, --fields and --limit come from one place, so that this command, the
	// schema and the MCP server cannot disagree about what the format flag is
	// called.
	opts.out = output.AddOutputFlags(c)
	c.Flags().StringVar(&opts.forRef, "for", "",
		"Answer where one workload would fit, as job/<name> in the namespace -n names")

	return c
}

// run reads the cluster, builds the report and prints it.
func run(ctx context.Context, o *cmd.Options, opts *options) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// The output flags are checked before anything is read, so that a typo in
	// -o or --fields costs nothing and is reported as the usage error it is,
	// rather than after a cluster-wide scan nobody needed.
	if err := opts.out.Validate(topology.NewTopologyReport()); err != nil {
		return err
	}

	// The same for --for: a target this command cannot answer about is the
	// user's to fix, and finding that out after a cluster-wide read would
	// only be slower.
	var jobName string
	if opts.forRef != "" {
		name, err := topology.ParseForTarget(opts.forRef)
		if err != nil {
			return err
		}
		jobName = name
	}

	src, err := newSource(o)
	if err != nil {
		return err
	}

	report, partial, err := build(ctx, src, o, jobName)
	if err != nil {
		return err
	}

	if err := opts.out.Print(o.Streams, report); err != nil {
		return fmt.Errorf("printing the topology report: %w", err)
	}

	// The report was printed first: a partial read is reported through the
	// exit code, and the part of the cluster that was readable is still the
	// answer to most of the question.
	return partial
}

// build reads the cluster and assembles the report, returning it alongside
// the partial read to exit with, if any.
func build(ctx context.Context, src reader, o *cmd.Options, jobName string) (*topology.TopologyReport, error, error) {
	var partial []error

	// The whole inventory, not a model of its own: the grouping reads the
	// clique, the Topograph labels, the allocatable and the requested GPUs,
	// and every one of those is a field pkg/inventory already fills from the
	// same three reads. NoPods is deliberately left off: without the pod list
	// there is no requested count, and a report that cannot say what is free
	// cannot answer where anything fits.
	list, err := inventory.Build(ctx, src, inventory.BuildOptions{})
	if err != nil {
		if !isPartial(err) {
			return nil, nil, err
		}
		partial = append(partial, err)
	}

	var nodes []inventory.GPUNode
	if list != nil {
		nodes = list.Items
	}

	in := topology.Input{Nodes: nodes}

	var kueueErr error
	in.Kueue, in.Topologies, kueueErr = readKueue(ctx, src)
	switch {
	case kueueErr == nil:
	case isPartial(kueueErr):
		partial = append(partial, kueueErr)
	default:
		// The cluster could not be talked to. Nothing was read, so nothing is
		// printed: a report that came back from a failed run reads as an
		// answer, and exit 4 says there is no answer here.
		return nil, nil, kueueErr
	}

	report := topology.Build(in)

	if jobName != "" {
		jobErr := answerForJob(ctx, src, o, report, nodes, jobName)
		switch {
		case jobErr == nil:
		case isPartial(jobErr):
			partial = append(partial, jobErr)
		default:
			return nil, nil, jobErr
		}
	}

	report.Status.Unreachable = unreachableOf(partial)
	report.Status.Partial = len(report.Status.Unreachable) > 0

	return report, errors.Join(partial...), nil
}

// answerForJob reads the job and fills in the fit.
//
// A job that is not there is a question about something that does not exist,
// which is the user's to fix and exits 2. A job that could not be read is a
// different thing: the levels were still read, so the report is printed
// without a fit and the run exits 3, with the denied read named in the
// status so that a reader can see why the answer is missing.
func answerForJob(
	ctx context.Context,
	src reader,
	o *cmd.Options,
	report *topology.TopologyReport,
	nodes []inventory.GPUNode,
	jobName string,
) error {
	namespace := namespaceOf(o)

	job, err := src.Job(ctx, namespace, jobName)
	switch {
	case apierrors.IsNotFound(err):
		return output.NewUsageError("no job %q in namespace %q", jobName, namespace)
	case err != nil:
		if isPartial(err) {
			o.Log().Warnf("Cannot answer about job %s/%s: %v", namespace, jobName, err)
		}

		return err
	}

	req, err := topology.JobRequestFrom(job)
	if err != nil {
		return err
	}

	report.Fit = report.FitJob(req, nodes)

	return nil
}

// readKueue reads the Kueue topology levels, reporting what it found rather
// than failing when there is nothing to find.
//
// Whether Kueue is installed is discovery's answer, not the list's. A cluster
// that serves the topology API and then refuses the read is a cluster with
// Kueue on it and an RBAC rule in the way, and reporting it as not installed
// would send the user to install something they already have.
func readKueue(ctx context.Context, src reader) (topology.KueueStatus, []topology.KueueTopology, error) {
	topologies, err := src.Topologies(ctx)
	if status, stop := kueueProblem(err, "topologies"); stop {
		return status, nil, passPartial(err)
	}

	flavors, err := src.ResourceFlavors(ctx)
	if status, stop := kueueProblem(err, "resource flavors"); stop {
		// The topologies were readable, so Kueue is installed whatever the
		// flavors did.
		status.Installed = true

		return status, nil, passPartial(err)
	}

	referenced, parseErr := topology.ReferencedTopologies(topologies, flavors)

	status := topology.KueueStatus{Installed: true}
	switch {
	case parseErr != nil:
		status.Reason = parseErr.Error()
	case len(referenced) == 0:
		status.Reason = "no ResourceFlavor references a Topology"
	}

	return status, referenced, nil
}

// kueueProblem turns a failed Kueue read into the status to report, and says
// whether the read can go on. A nil error is no problem and no status.
func kueueProblem(err error, what string) (topology.KueueStatus, bool) {
	switch {
	case err == nil:
		return topology.KueueStatus{}, false

	case errors.Is(err, source.ErrKueueNotInstalled):
		return topology.KueueStatus{
			Reason: "the cluster serves no kueue.x-k8s.io " + what,
		}, true

	case errors.Is(err, source.ErrKueueDiscovery):
		// The cluster would not say what it serves, so whether Kueue is there
		// is simply not known, and claiming either way would be a guess.
		return topology.KueueStatus{
			Reason: "the cluster would not say whether it serves the Kueue API: " + err.Error(),
		}, true

	case isPartial(err):
		return topology.KueueStatus{
			Installed: true,
			Reason:    "reading the Kueue " + what + " was not allowed: " + err.Error(),
		}, true

	default:
		return topology.KueueStatus{}, true
	}
}

// passPartial returns err when it is one the run can carry on from, and the
// error itself otherwise. A read that failed for any other reason means the
// cluster was not read at all.
func passPartial(err error) error {
	if err == nil || isPartial(err) {
		return err
	}
	if errors.Is(err, source.ErrKueueNotInstalled) {
		// Not an error: a cluster without Kueue is an answer.
		return nil
	}

	return err
}

// isPartial reports whether err is a partial read, that is, whether the run
// saw enough to be worth printing.
func isPartial(err error) bool {
	var pe *source.PartialError

	return errors.As(err, &pe)
}

// unreachableOf names what a run could not see, for the report status.
func unreachableOf(errs []error) []output.Unreachable {
	out := []output.Unreachable{}
	for _, err := range errs {
		var pe *source.PartialError
		for unwrapped := err; unwrapped != nil; unwrapped = errors.Unwrap(unwrapped) {
			if errors.As(unwrapped, &pe) {
				out = append(out, output.Unreachable{Resource: pe.Resource, Reason: pe.Err.Error()})
				break
			}
		}
	}

	return out
}

// namespaceOf is the namespace -n named, falling back to the one the
// kubeconfig's current context carries, exactly as kubectl does.
func namespaceOf(o *cmd.Options) string {
	if o.ConfigFlags == nil {
		return "default"
	}
	if o.ConfigFlags.Namespace != nil && *o.ConfigFlags.Namespace != "" {
		return *o.ConfigFlags.Namespace
	}

	namespace, _, err := o.ConfigFlags.ToRawKubeConfigLoader().Namespace()
	if err != nil || namespace == "" {
		return "default"
	}

	return namespace
}

// newSource builds the read path against the cluster the configuration flags
// point at.
func newSource(o *cmd.Options) (reader, error) {
	clientset, err := o.Factory.KubernetesClient()
	if err != nil {
		return nil, err
	}

	dyn, err := o.Factory.DynamicClient()
	if err != nil {
		return nil, err
	}

	return source.NewLive(clientset, dyn), nil
}
