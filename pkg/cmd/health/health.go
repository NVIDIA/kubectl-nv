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

// Package health implements "kubectl nv health", the GPU checks over a
// cluster or a must-gather bundle.
package health

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/engine"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// operatorSelector picks the GPU Operator's own controller pod out of the
// cluster. It is a label selector rather than a namespace, because the
// operator is installed wherever its chart was told to put it and a command
// that assumed gpu-operator would report "not installed" on every cluster that
// chose another name.
const operatorSelector = "app=gpu-operator"

// options is what the flags fill in.
type options struct {
	node       string
	failOn     string
	summary    bool
	fromBundle string
	// upgradeStuckAfter is how long a node may sit in one step of a driver
	// upgrade before NV-UPG-001 calls it stuck. It is a flag rather than a
	// constant because the right answer is a cluster's own: a fleet whose
	// drain takes an hour would be reported wedged on every upgrade at any
	// fixed default short enough to be useful elsewhere.
	upgradeStuckAfter time.Duration
	// out is -o, --fields and --limit, registered together by pkg/output so
	// that every command and the MCP server agree on what they are called.
	out *output.OutputOptions
}

// RuleCount is how many checks this command runs. It is exported for the test
// that holds it to the catalog: a command wired to a short list would report a
// cluster clean because it never asked the missing questions, and nothing else
// in the tree would notice.
func RuleCount() int { return len(rules.All()) }

// NewCommand builds the health command.
func NewCommand(o *cmd.Options) *cobra.Command {
	opts := &options{}

	c := &cobra.Command{
		Use:   "health",
		Short: "Check the GPU stack on every node and report what is wrong",
		Long: "Run every GPU health check over the cluster and report what it found: the " +
			"node labels and capacity, the GPU Operator's ClusterPolicy, and the operand " +
			"pods on each node.\n\nEach finding carries the evidence it was concluded from, " +
			"a link to its catalog entry and the command to run next. " +
			"`kubectl nv explain <code>` prints the entry.\n\nThe exit code is 0 when " +
			"nothing was found at or above --fail-on, 1 when something was, and 3 when part " +
			"of the cluster could not be read.",
		Example: "  # Check the whole cluster\n" +
			"  kubectl nv health\n\n" +
			"  # Fail a pipeline on anything at all\n" +
			"  kubectl nv health --fail-on info\n\n" +
			"  # One node, and the machine readable report\n" +
			"  kubectl nv health --node worker-1 -o json",
		Args: cmd.UsageArgs(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error {
			return run(c, o, opts)
		},
	}

	flags := c.Flags()
	flags.StringVar(&opts.node, "node", "", "Report on one node only")
	flags.StringVar(&opts.failOn, "fail-on", string(output.SeverityError),
		"Exit 1 when a finding is at or above this severity: error, warning or info")
	flags.BoolVar(&opts.summary, "summary", false,
		"Print the summary line only, without the findings")
	flags.StringVar(&opts.fromBundle, "from-bundle", "",
		"Read a must-gather directory instead of a cluster")
	flags.DurationVar(&opts.upgradeStuckAfter, "upgrade-stuck-after", rules.DefaultUpgradeStuckAfter,
		"Report a node stuck in a driver upgrade step once it has been there this long")
	opts.out = output.AddOutputFlags(c)

	return c
}

// run reads the cluster, evaluates the rules and prints the report.
func run(c *cobra.Command, o *cmd.Options, opts *options) error {
	// Both validated before anything is read. A misspelled --fail-on is the
	// difference between a gate and no gate, and finding either mistake after
	// the scan has cost the user the wait for it.
	failOn, err := output.ParseSeverity(opts.failOn)
	if err != nil {
		return err
	}

	// A window of zero would report every node the instant it entered an
	// upgrade step, and a negative one every node always. Neither is a window
	// anybody means, and finding that out after the scan has cost the user
	// the wait for it.
	if opts.upgradeStuckAfter <= 0 {
		return output.NewUsageError(
			"--upgrade-stuck-after must be positive, got %s", opts.upgradeStuckAfter)
	}

	if err := opts.out.Validate(health.NewHealthReport()); err != nil {
		return err
	}

	format, err := output.ParseFormat(opts.out.Format)
	if err != nil {
		return err
	}

	src, err := newSource(o, opts.fromBundle)
	if err != nil {
		return err
	}

	in, inputErr, err := buildInput(c.Context(), src)
	if err != nil {
		return err
	}

	report, err := engine.Run(rules.AllWith(rules.Options{UpgradeStuckAfter: opts.upgradeStuckAfter}), in, engine.Options{
		Node:     opts.node,
		InputErr: inputErr,
	})
	if err != nil {
		return err
	}

	if err := print(o, opts, format, report); err != nil {
		return err
	}

	return exitFor(report, failOn, inputErr)
}

// buildInput reads everything the rules are allowed to see.
//
// The second return is the partial half: the reads that were denied or absent,
// joined, which the engine turns into the report's Partial and Unreachable.
// The third is a failure that means the run did not happen at all.
func buildInput(ctx context.Context, src source.Source) (health.Input, error, error) {
	// IncludeHinted because a node that should have GPUs and advertises none
	// is the single most common thing this command is run to find, and
	// WithDetails because the node rules read conditions and ResourceSlices.
	list, _, buildErr := inventory.BuildWithHinted(ctx, src, inventory.BuildOptions{
		IncludeHinted: true,
		WithDetails:   true,
	})
	if list == nil {
		return health.Input{}, nil, buildErr
	}

	var partials []error
	if buildErr != nil {
		partials = append(partials, buildErr)
	}

	in := health.Input{Now: time.Now()}

	for i := range list.Items {
		if list.Items[i].Hinted {
			in.Hinted = append(in.Hinted, list.Items[i])

			continue
		}

		in.Nodes = append(in.Nodes, list.Items[i])
	}

	policies, err := src.ClusterPolicies(ctx)
	switch {
	case err == nil:
		in.ClusterPolicies = policies
	case errors.Is(err, source.ErrNotInstalled):
		// Not a partial read. The cluster serves no such collection because
		// the CRD was never applied, which is an answer: nothing was denied
		// and nothing is missing.
	default:
		partials = append(partials, err)
	}

	slices, err := src.ResourceSlices(ctx)
	if err != nil {
		partials = append(partials, err)
	} else {
		in.ResourceSlices = slices
	}

	// The operator's own pod is where "installed" and the version come from.
	// It is a second, selector-scoped pod read on top of the inventory's
	// unfiltered one, which is one small list against one large one: the
	// builder does not hand its pods back, and the controller runs on a node
	// that need not be in the GPU inventory at all.
	operator, err := src.Pods(ctx, "", operatorSelector)
	if err != nil {
		partials = append(partials, err)
	}

	in.OperatorInstalled = len(operator) > 0
	in.OperatorVersion = operatorVersion(operator)

	events, err := source.ReadNodeEvents(ctx, src)
	if err != nil {
		partials = append(partials, err)
	} else {
		in.Events = events
	}

	// The ComputeDomains are NV-CD-001's input. A cluster without the DRA
	// driver serves no such collection, and pkg/source answers that with an
	// empty list and no error: nothing was denied and nothing is missing, so
	// it is not a partial read and the report must not say it is.
	domains, err := source.ReadComputeDomains(ctx, src)
	if err != nil {
		partials = append(partials, err)
	} else {
		in.ComputeDomains = domains
	}

	return in, errors.Join(partials...), nil
}

// print renders the report in the chosen format.
func print(o *cmd.Options, opts *options, format output.Format, report *health.HealthReport) error {
	if format != output.FormatTable && format != output.FormatWide {
		// --summary is a flag about the human table. A consumer that wants
		// the summary alone out of the machine readable report asks for it by
		// name, with --fields summary.
		if err := opts.out.Print(o.Streams, report); err != nil {
			return fmt.Errorf("printing the health report: %w", err)
		}

		return nil
	}

	if !opts.summary && len(report.Findings) > 0 {
		if err := opts.out.Print(o.Streams, report); err != nil {
			return fmt.Errorf("printing the health report: %w", err)
		}
	}

	if _, err := fmt.Fprintln(o.Streams.Out, report.Summary.Line()); err != nil {
		return fmt.Errorf("printing the summary: %w", err)
	}

	return nil
}

// exitFor turns the report into the process exit status.
//
// Nothing found at or above the threshold is a nil error and exit 0. A partial
// read with nothing found returns the read's own error, which carries exit 3
// and names what was missed. Findings win over both, because a caller needs to
// hear what was found before it hears what was not read.
func exitFor(report *health.HealthReport, failOn output.Severity, inputErr error) error {
	switch code := report.ExitCode(failOn); code {
	case output.ExitOK:
		return nil
	case output.ExitPartial:
		return inputErr
	default:
		return &findingsError{report: report, failOn: failOn}
	}
}

// findingsError is the error a run with findings ends on. It exists for its
// exit code: the findings are already on stdout, and what this adds is the one
// line saying why the command exited non-zero.
type findingsError struct {
	report *health.HealthReport
	failOn output.Severity
}

func (e *findingsError) Error() string {
	return fmt.Sprintf("found %d errors, %d warnings and %d infos; --fail-on is %s",
		e.report.Summary.Errors, e.report.Summary.Warnings, e.report.Summary.Infos, e.failOn)
}

// ExitCode is what output.ExitCodeFor reads.
func (e *findingsError) ExitCode() int { return output.ExitFindings }

// operatorVersion reads the GPU Operator version off its controller pod.
func operatorVersion(pods []corev1.Pod) string {
	for i := range pods {
		for _, container := range pods[i].Spec.Containers {
			if v := OperatorVersionFromImage(container.Image); v != "" {
				return v
			}
		}
	}

	return ""
}

// OperatorVersionFromImage reads the version out of a container image
// reference, or returns an empty string when there is none to read.
//
// Empty is the honest answer rather than a guess, and the rules are built for
// it: health.VersionInRange treats an unknown version as "no constraint", so
// every rule runs. A rule skipped because this function invented a version
// would report a cluster clean for the wrong reason.
//
// Two shapes have to be got right. The registry may carry a port, so the tag
// is whatever follows the last colon after the last slash, not after the first
// colon. And a digest is not a version: an image pinned by @sha256: has no tag
// at all, and reading "sha256" out of one would gate every rule in the catalog
// on a string that is not a version.
func OperatorVersionFromImage(image string) string {
	if image == "" {
		return ""
	}

	// Everything after the last slash is name[:tag] or name@digest; the
	// registry, with or without a port, is in front of it.
	name := image
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}

	// A digest pins the image instead of a tag, and may follow one. Cutting it
	// first leaves the tag when there is one and nothing when there is not.
	if i := strings.Index(name, "@"); i >= 0 {
		name = name[:i]
	}

	// Exactly one colon, or none. A repository name cannot contain one, so
	// anything with two is a reference this code cannot read, and an
	// unreadable reference means an unknown version rather than a guess at
	// which colon was the right one: health.VersionInRange treats unknown as
	// no constraint, so every rule runs, where a wrong guess would gate the
	// catalog on a string that is not a version.
	_, tag, found := strings.Cut(name, ":")
	if !found || strings.Contains(tag, ":") {
		return ""
	}

	return tag
}

// newSource returns the read path: a must-gather directory when one was named,
// and the cluster the kubectl flags point at otherwise.
func newSource(o *cmd.Options, bundle string) (source.Source, error) {
	if bundle != "" {
		b, err := source.NewBundle(bundle)
		if err != nil {
			return nil, output.WrapUsageError(err, "reading the must-gather bundle")
		}

		return b, nil
	}

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
