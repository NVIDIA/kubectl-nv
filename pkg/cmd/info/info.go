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

// Package info implements "kubectl nv info <node>", everything kubectl-nv
// knows about one GPU node.
package info

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
	fromBundle string
	format     string
}

// NewCommand builds the info command.
func NewCommand(o *cmd.Options) *cobra.Command {
	opts := &options{}

	c := &cobra.Command{
		Use:   "info NODE",
		Short: "Show everything known about one GPU node",
		Long: "Show everything kubectl-nv knows about one node: its GPU labels, what it " +
			"advertises, its conditions and taints, the GPU Operator pods running on it with " +
			"their images and the validator's results, and the ResourceSlices published for " +
			"it.\n\nA node that carries an NVIDIA label and advertises no GPUs is shown, marked " +
			"as hinted: that it should have GPUs and does not is the answer.",
		Args:         cobra.ExactArgs(1),
		SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			return run(c, o, opts, args[0])
		},
	}

	flags := c.Flags()
	flags.StringVar(&opts.fromBundle, "from-bundle", "",
		"Read a must-gather directory instead of a cluster")
	flags.StringVarP(&opts.format, "output", "o", "",
		"Output format: table, wide, json, yaml or name")

	return c
}

// run builds the one node and prints it.
func run(c *cobra.Command, o *cmd.Options, opts *options, name string) error {
	format, err := output.ParseFormat(opts.format)
	if err != nil {
		return err
	}

	src, err := newSource(o, opts.fromBundle)
	if err != nil {
		return err
	}

	// The error is held rather than returned: a source.PartialError comes with
	// a usable node, and this command's job is to show it.
	list, buildErr := inventory.Build(c.Context(), src, inventory.BuildOptions{
		Node:          name,
		IncludeHinted: true,
		WithDetails:   true,
	})
	if list == nil {
		return buildErr
	}

	node := nodeNamed(list, name)
	if node == nil {
		// Not an error about the cluster: the node is either not there or not
		// one this tool has anything to say about, and either way the user
		// typed the name. Exit 2.
		return output.NewUsageError("node %s has no GPUs and no GPU labels", name)
	}

	if format == output.FormatTable || format == output.FormatWide {
		if err := describe(o.Streams.Out, node); err != nil {
			return err
		}

		return buildErr
	}

	printer, err := output.NewPrinter(opts.format)
	if err != nil {
		return err
	}

	if err := printer.PrintObj(node, o.Streams.Out); err != nil {
		return fmt.Errorf("printing the GPU node: %w", err)
	}

	return buildErr
}

// nodeNamed picks the node out of the built list. The builder was asked for
// one node, so the list holds at most one; it is looked up by name rather than
// taken by index, so that a change to the filter cannot make this command
// describe the wrong node.
func nodeNamed(list *inventory.GPUNodeList, name string) *inventory.GPUNode {
	for i := range list.Items {
		if list.Items[i].Name == name {
			return &list.Items[i]
		}
	}

	return nil
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
