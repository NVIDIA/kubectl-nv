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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
	"github.com/NVIDIA/kubectl-nv/pkg/topology"
)

// reader is everything this command reads. It is an interface rather than
// *source.Live so that the tests drive the command over fakes, and so that a
// must-gather bundle can be read through the same code once the Kueue
// objects are collected into one.
type reader interface {
	topology.NodeReader

	Topologies(ctx context.Context) ([]unstructured.Unstructured, error)
	ResourceFlavors(ctx context.Context) ([]unstructured.Unstructured, error)
	Jobs(ctx context.Context, namespace string) ([]batchv1.Job, error)
}

// options is the state one run of the command carries.
type options struct {
	format string
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

	c.Flags().StringVarP(&opts.format, "output", "o", "",
		"Output format: table, wide, json, yaml or name")
	c.Flags().StringVar(&opts.forRef, "for", "",
		"Answer where one workload would fit, as job/<name> in the namespace -n names")

	return c
}

// run reads the cluster, builds the report and prints it.
func run(ctx context.Context, o *cmd.Options, opts *options) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// The format is parsed before anything is read, so that a typo in -o
	// costs nothing and is reported as the usage error it is.
	printer, err := output.NewPrinter(opts.format)
	if err != nil {
		return err
	}

	// The same for --for: a target this command cannot answer about is the
	// user's to fix, and finding that out after a cluster-wide read would
	// only be slower.
	var jobName string
	if opts.forRef != "" {
		if jobName, err = topology.ParseForTarget(opts.forRef); err != nil {
			return err
		}
	}

	src, err := newSource(o)
	if err != nil {
		return err
	}

	report, partial, err := build(ctx, src, o, jobName)
	if err != nil {
		return err
	}

	if err := printer.PrintObj(report, o.Streams.Out); err != nil {
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

	nodes, err := topology.BuildNodes(ctx, src)
	if err != nil {
		if !isPartial(err) {
			return nil, nil, err
		}
		partial = append(partial, err)
	}

	in := topology.Input{Nodes: nodes}
	in.Kueue, in.Topologies, err = readKueue(ctx, src)
	if err != nil {
		partial = append(partial, err)
	}

	report := topology.Build(in)

	if jobName != "" {
		if err := answerForJob(ctx, src, o, report, nodes, jobName); err != nil {
			return nil, nil, err
		}
	}

	report.Status.Unreachable = unreachableOf(partial)
	report.Status.Partial = len(report.Status.Unreachable) > 0

	return report, errors.Join(partial...), nil
}

// answerForJob reads the job and fills in the fit.
func answerForJob(
	ctx context.Context,
	src reader,
	o *cmd.Options,
	report *topology.TopologyReport,
	nodes []inventory.GPUNode,
	jobName string,
) error {
	namespace := namespaceOf(o)

	jobs, err := src.Jobs(ctx, namespace)
	if err != nil && !isPartial(err) {
		return err
	}

	for i := range jobs {
		if jobs[i].Name != jobName {
			continue
		}

		req, err := topology.JobRequestFrom(&jobs[i])
		if err != nil {
			return err
		}

		report.Fit = report.FitJob(req, nodes)

		return nil
	}

	// A job that is not there is a question about something that does not
	// exist, which is exit 2 and not a report of nothing.
	return output.NewUsageError("no job %q in namespace %q", jobName, namespace)
}

// readKueue reads the Kueue topology levels, reporting what it found rather
// than failing when there is nothing to find.
func readKueue(ctx context.Context, src reader) (topology.KueueStatus, []topology.KueueTopology, error) {
	topologies, err := src.Topologies(ctx)
	switch {
	case errors.Is(err, source.ErrKueueNotInstalled):
		return topology.KueueStatus{Reason: "the cluster serves no kueue.x-k8s.io topologies"}, nil, nil
	case isPartial(err):
		return topology.KueueStatus{Reason: "reading the Kueue topologies was not allowed"}, nil, err
	case err != nil:
		return topology.KueueStatus{}, nil, err
	}

	flavors, err := src.ResourceFlavors(ctx)
	switch {
	case errors.Is(err, source.ErrKueueNotInstalled):
		return topology.KueueStatus{
			Installed: true,
			Reason:    "the cluster serves no kueue.x-k8s.io resourceflavors",
		}, nil, nil
	case isPartial(err):
		return topology.KueueStatus{
			Installed: true,
			Reason:    "reading the Kueue resource flavors was not allowed",
		}, nil, err
	case err != nil:
		return topology.KueueStatus{Installed: true}, nil, err
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
