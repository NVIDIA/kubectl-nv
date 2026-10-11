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

// Package debug implements "kubectl nv debug <node>": a must-gather narrowed
// to one node, with a summary of that node on one screen.
//
// It is the two commands a support engineer runs together, run together. The
// gather is pkg/mustgather itself with its node scope set, so the bundle this
// writes is the bundle "adm must-gather --node" writes, file for file; the
// summary is built from the same inventory kubectl nv info prints.
package debug

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/mcp"
	"github.com/NVIDIA/kubectl-nv/pkg/debug"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// options is what the flags fill in.
type options struct {
	// gather is the must-gather this command runs. Its flags are registered by
	// pkg/mustgather, so they are the same flags, with the same names and the
	// same defaults, as the ones adm must-gather takes.
	gather mustgather.Options
	// gatherFunc is how the gather is executed. It is the seam adm must-gather
	// is built over, so a test drives this command without a cluster.
	gatherFunc mustgather.RunFunc
	// out is -o, --fields and --limit, registered together by pkg/output so
	// that every command and the MCP server agree on what they are called.
	//
	// --limit is registered and does nothing here: this command prints one
	// report, never a list. It is taken anyway because a program driving
	// kubectl-nv should be able to pass the same bounds to every read-only
	// command without first learning which of them are list shaped.
	out *output.OutputOptions
}

// NewCommand builds the debug command. run is the seam the must-gather is
// executed through, the same one adm must-gather is built over.
func NewCommand(o *cmd.Options, run mustgather.RunFunc) *cobra.Command {
	opts := &options{gatherFunc: run}

	c := &cobra.Command{
		Use:   "debug NODE",
		Short: "Collect a must-gather for one node and summarise it",
		Long: "Collect a must-gather narrowed to one node and print what kubectl-nv knows " +
			"about that node on one screen: its GPUs, its driver, its operands, and what " +
			"the gather could and could not collect.\n\nThe bundle is the one " +
			"\"kubectl nv adm must-gather --node\" writes, and the summary is the one " +
			"\"kubectl nv info\" prints. Nothing here says whether the node is healthy; " +
			"\"kubectl nv health\" answers that, over this bundle or over the cluster.",
		Args: cmd.UsageArgs(cobra.ExactArgs(1)),
		// The gather reaches into a driver pod, so the MCP server has to force
		// --no-exec on this command. The annotation is what tells it; a list
		// of command names there would be a thing to forget.
		Annotations: map[string]string{mcp.ExecAnnotation: "true"},
		RunE: func(c *cobra.Command, args []string) error {
			return runDebug(c, o, opts, args[0])
		},
	}

	mustgather.AddGatherFlags(c.Flags(), &opts.gather)
	// The server pins the directory a command writes into rather than letting
	// a model choose it.
	if err := c.Flags().SetAnnotation("artifacts-dir", mcp.HostDirAnnotation, []string{"true"}); err != nil {
		panic(fmt.Sprintf("debug: marking --artifacts-dir as a host directory: %v", err))
	}

	opts.out = output.AddOutputFlags(c)

	return c
}

// runDebug reads the node, gathers for it and prints the report.
func runDebug(c *cobra.Command, o *cmd.Options, opts *options, name string) error {
	// Validated against an empty report, before anything is read or written: a
	// misspelled --fields or an unusable --since found after the gather has
	// cost the user the wait and left a bundle behind.
	if err := opts.out.Validate(debug.NewReport("")); err != nil {
		return err
	}

	format, err := output.ParseFormat(opts.out.Format)
	if err != nil {
		return err
	}

	if err := opts.gather.ValidateWindows(); err != nil {
		return err
	}

	// The node, before the gather. A name that is not a GPU node is the user's
	// to fix, and finding that out after writing a bundle would leave one
	// behind for a question that was never asked.
	node, inventoryErr, err := readNode(c.Context(), o, name)
	if err != nil {
		return err
	}

	prepare(c, o, opts, name)

	log := o.Log()
	log.Infof("Using kubeconfig: %s", o.KubeconfigDisplay())
	log.Infof("Collecting %s into %s", name, opts.gather.ArtifactDir)

	gatherErr := opts.gatherFunc(&opts.gather)

	// A usage error means the gather never started: a directory that already
	// holds a bundle, or a node name the cluster does not have. There is
	// nothing to report and the user has something to type differently.
	var usage *output.UsageError
	if errors.As(gatherErr, &usage) {
		return gatherErr
	}

	report := buildReport(name, node, opts.gather.ArtifactDir, inventoryErr, gatherErr)

	if err := printReport(o, opts, format, report); err != nil {
		return err
	}

	log.Infof("Please send the contents of %s to NVIDIA support", opts.gather.ArtifactDir)

	// The gather's error wins: it carries the exit code that says whether the
	// bundle is worth sending at all, which a partial inventory does not.
	if gatherErr != nil {
		return gatherErr
	}

	return inventoryErr
}

// prepare fills in the parts of the gather that do not come from a flag.
func prepare(c *cobra.Command, o *cmd.Options, opts *options, name string) {
	opts.gather.Node = name
	opts.gather.Debug = o.Debug
	if opts.gather.ArtifactDir == "" {
		opts.gather.ArtifactDir = mustgather.DefaultArtifactDir(time.Now())
	}
	// The version is read off the root rather than threaded through this
	// constructor: the root is where the linker stamps it, and a second copy
	// of it is a second thing to forget to update.
	opts.gather.ToolVersion = c.Root().Version
	opts.gather.Context = kubeconfigContext(o)
}

// readNode builds the one node this command is about, exactly as kubectl nv
// info builds it. It returns the node, the partial error the read came back
// with, and the error that means there is no node to report on.
func readNode(ctx context.Context, o *cmd.Options, name string) (*inventory.GPUNode, error, error) {
	src, err := newSource(o)
	if err != nil {
		return nil, nil, err
	}

	// The error is held rather than returned: a source.PartialError comes with
	// a usable node, and a gather over an incomplete read is still worth
	// collecting.
	list, buildErr := inventory.Build(ctx, src, inventory.BuildOptions{
		Node:          name,
		IncludeHinted: true,
		WithDetails:   true,
	})
	if list == nil {
		return nil, nil, buildErr
	}

	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i], buildErr, nil
		}
	}

	// A node missing from an incomplete read is not a node with no GPUs. A DRA
	// node is a GPU node only because a ResourceSlice names it, so with that
	// list denied the node falls out of the inventory entirely; saying it has
	// none would state a fact about the cluster that nobody established.
	if buildErr != nil {
		return nil, nil, buildErr
	}

	// The same message kubectl nv info gives, word for word. A user who typed
	// the same name into both commands must not be told two different things.
	return nil, nil, output.NewUsageError("node %s has no GPUs and no GPU labels", name)
}

// newSource returns the read path: the cluster the kubectl flags point at.
// There is no --from-bundle here, because this command's job is to make a
// bundle rather than to read one.
func newSource(o *cmd.Options) (source.Source, error) {
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

// buildReport assembles what the command prints from the node, the bundle the
// gather left behind, and what neither of them could see.
func buildReport(
	name string,
	node *inventory.GPUNode,
	dir string,
	inventoryErr, gatherErr error,
) *debug.Report {
	report := debug.NewReport(name)
	report.Node = *node
	report.Bundle = debug.SummariseBundle(dir, readManifest(dir))

	// Partial is the report's own claim that it is incomplete, so it is set
	// from what happened rather than from what was asked for. --no-exec leaves
	// collectors skipped and the gather succeeding, and that is a complete
	// answer to the question the user asked.
	if inventoryErr != nil {
		report.Status.Partial = true

		var partial *source.PartialError
		if errors.As(inventoryErr, &partial) {
			report.Status.Unreachable = append(report.Status.Unreachable, output.Unreachable{
				Resource: partial.Resource,
				Reason:   partial.Err.Error(),
			})
		} else {
			report.Status.Unreachable = append(report.Status.Unreachable, output.Unreachable{
				Resource: "inventory",
				Node:     name,
				Reason:   inventoryErr.Error(),
			})
		}
	}

	if gatherErr != nil {
		report.Status.Partial = true
		report.Status.Unreachable = append(report.Status.Unreachable, output.Unreachable{
			Resource: "must-gather",
			Node:     name,
			Reason:   gatherErr.Error(),
		})
	}

	return report
}

// readManifest parses the bundle's own index, or returns nil when the gather
// did not get as far as writing one.
func readManifest(dir string) *mustgather.Manifest {
	raw, err := os.ReadFile(filepath.Join(dir, mustgather.ManifestFileName))
	if err != nil {
		return nil
	}

	var m mustgather.Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}

	return &m
}

// printReport renders the report in the chosen format.
func printReport(o *cmd.Options, opts *options, format output.Format, report *debug.Report) error {
	if format == output.FormatTable || format == output.FormatWide {
		return summarise(o.Streams.Out, report)
	}

	if err := opts.out.Print(o.Streams, report); err != nil {
		return fmt.Errorf("printing the debug report: %w", err)
	}

	return nil
}

// kubeconfigContext names the context the bundle is collected through, for the
// manifest. The raw configuration is asked rather than the flag alone, so that
// a run without --context records the current context rather than an empty
// field, and a bundle always says which cluster it came from.
func kubeconfigContext(o *cmd.Options) string {
	if o.ConfigFlags == nil {
		return ""
	}

	if o.ConfigFlags.Context != nil && *o.ConfigFlags.Context != "" {
		return *o.ConfigFlags.Context
	}

	raw, err := o.ConfigFlags.ToRawKubeConfigLoader().RawConfig()
	if err != nil {
		return ""
	}

	return raw.CurrentContext
}
