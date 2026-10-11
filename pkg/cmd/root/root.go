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

// Package root builds the kubectl-nv command tree.
package root

import (
	"fmt"
	"os"
	"strconv"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/adm"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/mcp"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/schema"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// debugEnvVar is read when --debug is not given, so that the environment
// variable the urfave/cli tree honoured keeps working.
const debugEnvVar = "DEBUG"

// Deps are the seams of the command tree. The zero value talks to a real
// cluster and runs a real must-gather; tests replace the parts they need to.
type Deps struct {
	Streams    genericiooptions.IOStreams
	Factory    cmd.Factory
	MustGather mustgather.RunFunc

	// Options, when non-nil, is the state the root fills in and every
	// subcommand reads. A caller that needs to see what parsing produced passes
	// its own; the root makes one otherwise.
	Options *cmd.Options
}

// NewCommand builds the kubectl-nv command tree. version is what the binary
// reports; the release stamps it through ldflags.
func NewCommand(version string, d Deps) *cobra.Command {
	configFlags := genericclioptions.NewConfigFlags(true)
	o := d.Options
	if o == nil {
		o = &cmd.Options{}
	}
	o.ConfigFlags = configFlags
	o.Streams = d.Streams
	o.Factory = d.Factory
	if o.Factory == nil {
		// Built here rather than in the pre-run hook, so that no subcommand can
		// leave it nil whatever it does with hooks of its own. The factory reads
		// nothing until a client is asked for.
		o.Factory = cmd.NewFactory(configFlags)
	}

	// Cobra runs only the closest PersistentPreRunE in a chain by default, so a
	// subcommand with one of its own would silently skip the debug resolution
	// below.
	cobra.EnableTraverseRunHooks = true

	root := &cobra.Command{
		Use:           "kubectl-nv",
		Short:         "A kubectl plugin for NVIDIA powered Kubernetes clusters",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		PersistentPreRunE: func(c *cobra.Command, _ []string) error {
			// The flags parsed, so whatever fails from here on is not a usage
			// mistake and should not be answered with the flag list.
			c.SilenceUsage = true

			debug, err := resolveDebug(c)
			if err != nil {
				return err
			}
			o.Debug = debug

			noColor, err := c.Flags().GetBool("no-color")
			if err != nil {
				return err
			}
			o.Logger = output.NewLogger(o.Streams, output.LoggerOptions{NoColor: noColor})
			return nil
		},
		// Without a subcommand there is nothing to do but show what is on offer.
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}

	// kubectl owns completion for its plugins, so the tree carries only the
	// commands kubectl-nv implements.
	root.CompletionOptions.DisableDefaultCmd = true

	cmd.AddConfigFlags(root.PersistentFlags(), configFlags)
	root.PersistentFlags().BoolP("debug", "d", false, "Enable debug-level logging")
	root.PersistentFlags().Bool("no-color", false, "Never colorize the progress output")

	// Flags the user got wrong are the user's to fix, and exit 2 says so.
	root.SetFlagErrorFunc(func(c *cobra.Command, err error) error {
		return output.WrapUsageError(err, "%s", c.CommandPath())
	})

	runMustGather := d.MustGather
	if runMustGather == nil {
		runMustGather = func(opts *mustgather.Options) error {
			return mustgather.Run(opts, o.Factory, o.Logger)
		}
	}

	root.AddCommand(newVersionCommand(version))
	root.AddCommand(adm.NewCommand(o, runMustGather))
	root.AddCommand(schema.NewCommand(o))
	// The server runs one fresh tree per tool call: cobra keeps the flags a run
	// set, so a reused tree would carry one call's --limit into the next.
	root.AddCommand(mcp.NewCommand(o, func(s genericiooptions.IOStreams) *cobra.Command {
		call := d
		call.Streams = s
		call.Options = nil
		return NewCommand(version, call)
	}))

	return root
}

// newVersionCommand prints the same string as --version, for the users who
// reach for the subcommand.
func newVersionCommand(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the kubectl-nv version",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(c.OutOrStdout(), "kubectl-nv version %s\n", version)
			return err
		},
	}
}

// resolveDebug reads --debug, falling back to the DEBUG environment variable
// when the flag was not given. An unparseable DEBUG is an error rather than a
// silent "true": a typo in a support engineer's shell should be visible.
func resolveDebug(c *cobra.Command) (bool, error) {
	flags := c.Flags()
	if flags.Changed("debug") {
		return flags.GetBool("debug")
	}

	value := os.Getenv(debugEnvVar)
	if value == "" {
		return false, nil
	}
	debug, err := strconv.ParseBool(value)
	if err != nil {
		// The environment is part of the command line as far as the user is
		// concerned, so a value that cannot be read is a usage error.
		return false, output.WrapUsageError(err, "could not parse %s=%q as a boolean", debugEnvVar, value)
	}
	return debug, nil
}
