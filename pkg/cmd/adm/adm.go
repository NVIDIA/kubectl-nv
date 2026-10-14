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

// Package adm carries the admin-level operations on the NVIDIA resources of a
// cluster.
package adm

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
)

// NewCommand builds the adm command and its subcommands. run is the seam the
// must-gather is executed through.
func NewCommand(o *cmd.Options, run mustgather.RunFunc) *cobra.Command {
	adm := &cobra.Command{
		Use:   "adm",
		Short: "Perform admin level operations on cluster NV resources",
		// Without this a mistyped subcommand prints the help and exits 0, so a
		// script that gathered nothing reports that it did.
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}

	adm.AddCommand(newMustGatherCommand(o, run))

	return adm
}

func newMustGatherCommand(o *cmd.Options, run mustgather.RunFunc) *cobra.Command {
	opts := &mustgather.Options{}

	mg := &cobra.Command{
		Use:   "must-gather",
		Short: "collects the information from your cluster that is most likely needed for debugging issues",
		Args:  cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			opts.Debug = o.Debug
			if opts.ArtifactDir == "" {
				opts.ArtifactDir = mustgather.DefaultArtifactDir(time.Now())
			}
			// The version is read off the root rather than threaded through
			// this constructor: the root is where the linker stamps it, and a
			// second copy of it is a second thing to forget to update.
			opts.ToolVersion = c.Root().Version
			opts.Context = kubeconfigContext(o)

			if err := opts.ValidateWindows(); err != nil {
				return err
			}

			log := o.Log()
			log.Infof("Using kubeconfig: %s", o.KubeconfigDisplay())
			log.Infof("Collecting cluster information into %s", opts.ArtifactDir)

			if err := run(opts); err != nil {
				return err
			}

			log.Infof("Please send the contents of %s to NVIDIA support", opts.ArtifactDir)
			return nil
		},
	}

	// The flags of a gather are registered in one place, pkg/mustgather, for
	// both commands that run one. A copy here would be a second spelling of
	// --since and --no-exec for a support engineer to get wrong, and the
	// tests that compared the two copies could only report the drift after it
	// had happened.
	mustgather.AddGatherFlags(mg.Flags(), opts)
	// --node is not among them, and is registered here. debug takes the node
	// as its positional argument, and a command offering both would let the
	// two disagree about which node the bundle is of.
	mg.Flags().StringVar(&opts.Node, "node", "",
		"collect only this node: its node object, the operand pods on it, their logs, "+
			"its bug report and its device usage. The cluster-scoped resources are "+
			"collected either way")

	return mg
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
