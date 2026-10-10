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
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
)

// artifactDirEnvVar names the directory the artifacts go to when
// --artifacts-dir is not given.
const artifactDirEnvVar = "ARTIFACT_DIR"

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
				opts.ArtifactDir = defaultArtifactDir(time.Now())
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

	mg.Flags().StringVar(&opts.ArtifactDir, "artifacts-dir", os.Getenv(artifactDirEnvVar),
		"path to the directory where the artifacts will be stored. "+
			"Defaults to /tmp/nvidia-gpu-operator_<timestamp>")

	return mg
}

// defaultArtifactDir names the directory the gpu-operator must-gather script
// also uses, so that a report collected by either tool looks the same.
func defaultArtifactDir(now time.Time) string {
	return fmt.Sprintf("/tmp/nvidia-gpu-operator_%s", now.Format("20060102_1504"))
}
