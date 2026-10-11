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

// Package list implements "kubectl nv list", the inventory of a cluster's GPU
// nodes.
package list

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// options is what the flags fill in.
type options struct {
	selector      string
	node          string
	noPods        bool
	includeHinted bool
	fromBundle    string
	format        string
}

// NewCommand builds the list command.
func NewCommand(o *cmd.Options) *cobra.Command {
	opts := &options{}

	c := &cobra.Command{
		Use:   "list",
		Short: "List the GPU nodes of a cluster",
		Long: "List the GPU nodes of a cluster: what they advertise, which driver and CUDA " +
			"version they run, whether the device plugin or DRA is serving them, and what is " +
			"already asking for their GPUs.\n\n" +
			"A node counts as a GPU node when the scheduler can place GPU work on it: it " +
			"advertises nvidia.com/gpu or a MIG profile, or a gpu.nvidia.com ResourceSlice names " +
			"it. A node that carries an NVIDIA label and advertises nothing is reported with " +
			"--include-hinted, because the label alone is not a GPU.",
		Args: cmd.UsageArgs(cobra.NoArgs),
		RunE: func(c *cobra.Command, _ []string) error {
			return run(c, o, opts)
		},
	}

	flags := c.Flags()
	flags.StringVarP(&opts.selector, "selector", "l", "",
		"Selector (label query) the nodes must match")
	flags.StringVar(&opts.node, "node", "",
		"Report on this node alone")
	flags.BoolVar(&opts.noPods, "no-pods", false,
		"Do not list the pods. The requested GPU count is then reported as unknown")
	flags.BoolVar(&opts.includeHinted, "include-hinted", false,
		"Also list the nodes that carry an NVIDIA label but advertise no GPUs")
	flags.StringVar(&opts.fromBundle, "from-bundle", "",
		"Read a must-gather directory instead of a cluster")
	flags.StringVarP(&opts.format, "output", "o", "",
		"Output format: table, wide, json, yaml or name")

	return c
}

// run builds the inventory and prints it.
func run(c *cobra.Command, o *cmd.Options, opts *options) error {
	printer, err := output.NewPrinter(opts.format)
	if err != nil {
		return err
	}

	src, err := newSource(o, opts.fromBundle)
	if err != nil {
		return err
	}

	// The build error is held rather than returned: a source.PartialError
	// comes with a usable list, and dropping it on the floor would turn an
	// incomplete answer into no answer at all.
	list, hinted, buildErr := inventory.BuildWithHinted(c.Context(), src, inventory.BuildOptions{
		Node:          opts.node,
		Selector:      opts.selector,
		NoPods:        opts.noPods,
		IncludeHinted: opts.includeHinted,
	})
	if list == nil {
		return buildErr
	}

	if err := printer.PrintObj(list, o.Streams.Out); err != nil {
		return fmt.Errorf("printing the GPU nodes: %w", err)
	}

	// A node that should have GPUs and advertises none is the single most
	// common thing a user is looking for, and it is not in the list above. The
	// count goes on stderr so that a pipeline reading stdout is unaffected.
	if hinted > 0 {
		o.Log().Infof("%d node(s) carry an NVIDIA label but advertise no GPUs; pass --include-hinted to list them", hinted)
	}

	return buildErr
}

// newSource returns the read path: a must-gather directory when one was named,
// and the cluster the kubectl flags point at otherwise.
func newSource(o *cmd.Options, bundle string) (source.Source, error) {
	if bundle != "" {
		b, err := source.NewBundle(bundle)
		if err != nil {
			// The path is something the user typed, so exit 2 rather than 4:
			// nothing is wrong with the cluster or with kubectl-nv.
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
