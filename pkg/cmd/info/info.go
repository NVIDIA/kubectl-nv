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
	// out is -o, --fields and --limit, registered together by pkg/output so
	// that every command and the MCP server agree on what they are called.
	//
	// --limit is registered and does nothing here: this command prints one
	// object, never a list. It is taken anyway because a program driving
	// kubectl-nv should be able to pass the same bounds to every read-only
	// command without first learning which of them are list shaped.
	out *output.OutputOptions
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
		Args: cmd.UsageArgs(cobra.ExactArgs(1)),
		RunE: func(c *cobra.Command, args []string) error {
			return run(c, o, opts, args[0])
		},
	}

	flags := c.Flags()
	flags.StringVar(&opts.fromBundle, "from-bundle", "",
		"Read a must-gather directory instead of a cluster")
	opts.out = output.AddOutputFlags(c)

	return c
}

// run builds the one node and prints it.
func run(c *cobra.Command, o *cmd.Options, opts *options, name string) error {
	// Validated against an empty node of the kind, before anything is read: a
	// misspelled --fields found after the scan has cost the user the wait.
	if err := opts.out.Validate(inventory.NewGPUNode("")); err != nil {
		return err
	}

	format, err := output.ParseFormat(opts.out.Format)
	if err != nil {
		return err
	}

	// The table this command prints is a description of one node rather than a
	// row of columns, so there is nothing for --fields to keep or drop. Saying
	// so is the honest answer: applying it silently to nothing would hand back
	// the whole node to a caller that asked for one field of it.
	if err := refuseFieldsOnADescription(format, opts.out.Bounded.Fields, output.KindGPUNode); err != nil {
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
		// A node missing from an incomplete read is not a node with no GPUs.
		// A DRA node is a GPU node only because a ResourceSlice names it, so
		// with that list denied the node falls out of the inventory entirely;
		// saying it has none would state a fact about the cluster that nobody
		// established. The partial error says what was not read, and exit 3
		// says the answer is incomplete.
		if buildErr != nil {
			return buildErr
		}

		// The read was complete and the node still is not in it: either the
		// cluster does not have it or it has nothing to do with GPUs, and
		// either way the user typed the name. Exit 2.
		return NotAGPUNodeError(name)
	}

	if format == output.FormatTable || format == output.FormatWide {
		if err := describe(o.Streams.Out, node); err != nil {
			return err
		}

		return buildErr
	}

	if err := opts.out.Print(o.Streams, node); err != nil {
		return fmt.Errorf("printing the GPU node: %w", err)
	}

	return buildErr
}

// refuseFieldsOnADescription rejects --fields where the output is a described
// object rather than a table of columns.
//
// It is exported through info because kubectl nv debug prints the same shape
// and has to give the same answer: a caller learning one command must not find
// the other quietly ignoring the flag.
func refuseFieldsOnADescription(format output.Format, fields []string, kind string) error {
	if len(fields) == 0 || (format != output.FormatTable && format != output.FormatWide) {
		return nil
	}

	return output.NewUsageError(
		"--fields cannot be used with -o %s on %s, which is described rather than tabulated: "+
			"ask for -o json or -o yaml", format, kind)
}

// RefuseFieldsOnADescription is refuseFieldsOnADescription for the commands
// that print the same shape from another package.
func RefuseFieldsOnADescription(format output.Format, fields []string, kind string) error {
	return refuseFieldsOnADescription(format, fields, kind)
}

// NotAGPUNodeError is what every command says about a node that a complete
// read did not find among the GPU nodes: the cluster does not have it, or it
// has nothing to do with GPUs, and either way the user typed the name.
//
// It is exported because kubectl nv debug answers the same question, and a
// support engineer reading one answer has to recognise the other. Two copies
// of the sentence drift the moment either is improved.
//
// It is a usage error, so the process exits 2. That is the difference between
// "you typed something wrong" and "the cluster is broken".
func NotAGPUNodeError(name string) error {
	return output.NewUsageError("node %s has no GPUs and no GPU labels", name)
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
