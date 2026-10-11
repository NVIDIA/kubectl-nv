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

package mcp

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
)

// NewCommand builds the mcp command. newRoot returns a fresh command tree, one
// per tool call, so that no flag a call set is still set on the next.
func NewCommand(o *cmd.Options, newRoot NewRootFunc) *cobra.Command {
	var listTools bool

	c := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the read-only commands as an MCP server over stdio",
		Long: "Serve kubectl-nv as a Model Context Protocol server on stdin and stdout, with\n" +
			"one tool per read-only command. The tools are derived from the command tree, so\n" +
			"a command added to kubectl-nv becomes a tool without the server changing.\n\n" +
			"Nothing served here writes to the cluster, and the one command that can run\n" +
			"something inside a pod is served with --no-exec forced on.",
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			if listTools {
				return printTools(c, newRoot)
			}

			return Serve(c.Context(), newRoot, c.Root().Version)
		},
	}

	c.Flags().BoolVar(&listTools, "list-tools", false,
		"Print the tools this server would offer and exit, instead of serving")

	return c
}

// printTools writes one tool name and description per line. It is how a reader,
// or a test, sees what the server offers without speaking the protocol.
func printTools(c *cobra.Command, newRoot NewRootFunc) error {
	for _, tool := range Tools(newRoot(discardStreams())) {
		gated := ""
		if tool.ForceNoExec {
			gated = " (--no-exec forced)"
		}
		if _, err := fmt.Fprintf(c.OutOrStdout(), "%s\t%s%s\n", tool.Name, tool.Description, gated); err != nil {
			return fmt.Errorf("writing the tool list: %w", err)
		}
	}

	return nil
}
