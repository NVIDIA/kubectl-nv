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

package main

import (
	"fmt"
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// version is stamped at build time with -X main.version=<version>. A binary
// built without that, such as a "go build" from a developer's tree, says so.
var version = "dev"

func main() {
	streams := genericiooptions.IOStreams{
		In:     os.Stdin,
		Out:    os.Stdout,
		ErrOut: os.Stderr,
	}

	// The one exit point: everything below returns a code rather than calling
	// os.Exit, so that deferred work, the panic handler included, still runs.
	os.Exit(run(os.Args[1:], streams, root.NewCommand(version, root.Deps{Streams: streams})))
}

// run executes the command tree and returns the process exit code.
func run(args []string, streams genericiooptions.IOStreams, c *cobra.Command) (code int) {
	// A bug in kubectl-nv should read as one line saying so, not as a page of
	// runtime output on top of whatever the user was doing. The stack is kept
	// for --debug, where someone is looking for it.
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(streams.ErrOut, "kubectl-nv: internal error: %v\n", r)
			if debugRequested(args) {
				fmt.Fprintf(streams.ErrOut, "%s", debug.Stack())
			}
			code = output.ExitError
		}
	}()

	c.SetArgs(args)
	c.SetIn(streams.In)
	c.SetOut(streams.Out)
	c.SetErr(streams.ErrOut)

	executed, err := c.ExecuteC()
	if err == nil {
		return output.ExitOK
	}

	// The root silences usage once the flags have parsed and the pre-run hook
	// has had its say. A command that still has usage on at this point failed
	// before it started doing any work, which makes it the user's to fix.
	if executed != nil && !executed.SilenceUsage {
		err = output.WrapUsageError(err, "%s", executed.CommandPath())
	}

	fmt.Fprintf(streams.ErrOut, "Error: %v\n", err)
	return output.ExitCodeFor(err)
}

// debugRequested reports whether the command line asked for debug output. The
// parsed flag is not available when the panic happened before or during
// parsing, so this reads the arguments as given.
func debugRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--debug" || arg == "-d" || arg == "--debug=true" {
			return true
		}
	}
	return os.Getenv("DEBUG") == "1" || os.Getenv("DEBUG") == "true"
}
