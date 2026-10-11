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

package mustgather

import (
	"os"
	"time"

	"github.com/spf13/pflag"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// ArtifactDirEnvVar names the directory the artifacts go to when
// --artifacts-dir is not given.
const ArtifactDirEnvVar = "ARTIFACT_DIR"

// AddGatherFlags registers the flags that belong to a gather itself on fs,
// writing into opts.
//
// It lives here rather than in a command package because more than one command
// runs this gather: kubectl nv adm must-gather over a cluster and kubectl nv
// debug over one node. A second registration would be a second set of defaults
// and a second help text to drift, and a support engineer who learned one
// command would find the other spelling the same flag differently.
//
// --node is deliberately not among them. must-gather takes it as a flag and
// debug takes the node as its argument, and a command that offered both would
// let the two disagree about which node the bundle is of.
func AddGatherFlags(fs *pflag.FlagSet, opts *Options) {
	fs.StringVar(&opts.ArtifactDir, "artifacts-dir", os.Getenv(ArtifactDirEnvVar),
		"path to the directory where the artifacts will be stored. "+
			"Defaults to /tmp/nvidia-gpu-operator_<timestamp>")
	fs.DurationVar(&opts.Since, "since", 0,
		"only collect container logs newer than this, for instance 2h or 30m. "+
			"The default collects the whole log")
	fs.BoolVar(&opts.NoExec, "no-exec", false,
		"never exec into a driver pod: no nvidia-bug-report and no GPU device usage")
	fs.DurationVar(&opts.ExecTimeout, "exec-timeout", DefaultExecTimeout,
		"how long one command inside a driver pod, or one copy out of it, may take "+
			"before that pod is recorded as having timed out and the gather moves on")
}

// MinimumWindow is the floor under the two duration flags. A window below it
// collects nothing and an exec deadline below it cannot finish a bug report, so
// either is a mistake worth naming rather than a gather that quietly produces
// an empty bundle.
const MinimumWindow = time.Second

// ValidateWindows rejects a duration the gather cannot do anything useful
// with. It is a usage error, which exits 2: the user can fix it by typing
// something else.
func (o *Options) ValidateWindows() error {
	if o.Since != 0 && o.Since < MinimumWindow {
		return output.NewUsageError(
			"--since is %s: a log window shorter than %s collects nothing", o.Since, MinimumWindow)
	}

	if o.ExecTimeout < MinimumWindow {
		return output.NewUsageError(
			"--exec-timeout is %s: a deadline shorter than %s cannot finish a bug report",
			o.ExecTimeout, MinimumWindow)
	}

	return nil
}
