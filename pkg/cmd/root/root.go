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
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
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
}

// NewCommand builds the kubectl-nv command tree. version is what the binary
// reports; the release stamps it through ldflags.
func NewCommand(version string, d Deps) *cobra.Command {
	configFlags := genericclioptions.NewConfigFlags(true)
	o := &cmd.Options{
		ConfigFlags: configFlags,
		Streams:     d.Streams,
		Factory:     d.Factory,
	}

	root := &cobra.Command{
		Use:           "kubectl-nv",
		Short:         "A kubectl plugin for NVIDIA powered Kubernetes clusters",
		Version:       version,
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
			if o.Factory == nil {
				o.Factory = cmd.NewFactory(configFlags)
			}
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

	runMustGather := d.MustGather
	if runMustGather == nil {
		runMustGather = func(opts *mustgather.Options) error {
			return mustgather.Run(opts, o.Factory, o.Streams.ErrOut)
		}
	}

	root.AddCommand(newVersionCommand(version))
	root.AddCommand(adm.NewCommand(o, runMustGather))

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
		return false, fmt.Errorf("could not parse %s=%q as a boolean: %w", debugEnvVar, value, err)
	}
	return debug, nil
}
