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

// Package explain prints the catalog entry behind a finding code.
package explain

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/kubectl-nv/docs/findings"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// NewCommand builds `kubectl nv explain`.
//
// It touches no cluster. The catalog is compiled into the binary, so the
// command answers on a laptop with no network and a kubeconfig that points
// nowhere, which is the situation a support engineer reading a finding code is
// usually in.
func NewCommand(_ *cmd.Options) *cobra.Command {
	return &cobra.Command{
		Use:   "explain CODE",
		Short: "Explain a finding code reported by kubectl nv health",
		Long: "Print the catalog entry for a finding code: what it means, what\n" +
			"the evidence is, which GPU Operator versions it applies to, what to\n" +
			"run next, and how to reproduce it.\n\n" +
			"The catalog is built into the binary, so this needs no cluster and no\n" +
			"network.",
		Example: "  # Explain the most common finding\n" +
			"  kubectl nv explain NV-NODE-002\n\n" +
			"  # Codes are not case sensitive\n" +
			"  kubectl nv explain nv-dp-001",
		Args: cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			entry, ok := findings.Entry(args[0])
			if !ok {
				return unknownCode(args[0])
			}

			// Written to the command's own stream rather than to the options'
			// streams, so that cobra's SetOut and a kubectl pipeline both land
			// in the same place. Nothing goes to stderr on success: piping
			// this into a pager should get the document and only the document.
			_, err := fmt.Fprint(c.OutOrStdout(), entry)
			if err != nil {
				return fmt.Errorf("writing the catalog entry for %s: %w", args[0], err)
			}

			return nil
		},
	}
}

// unknownCode is the error for a code the catalog does not hold. It lists what
// the user could have asked for instead: a code is easy to mistype and a bare
// "unknown code" leaves them guessing at the one character they got wrong.
//
// It is a usage error, so the process exits 2. That is the difference between
// "you typed something wrong" and "the cluster is broken", and a script that
// branches on the exit code needs to be able to tell them apart.
func unknownCode(code string) error {
	return output.NewUsageError(
		"no finding catalogued as %q: the codes are %s",
		code, strings.Join(findings.Codes(), ", "))
}
