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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd/mcp"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/root"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"
)

// The process stdout is the JSON-RPC transport. A command that printed to it
// rather than to the stream it was given would put bytes inside a frame and
// break the session for every later call.
//
// It is asserted here, over the whole command tree, rather than enforced in
// pkg/cmd/mcp: cobra reads os.Stdout on every execution, so a library that
// reassigned it would be racing every command in the process. The guarantee
// belongs to the commands, and this is where all of them are linked.
//
// Not parallel: it replaces os.Stdout for its duration.
func TestNoToolWritesToTheProcessStdout(t *testing.T) {
	isolateKubeconfigForMCP(t)

	leaked := captureStdout(t, func() {
		session := mcpSession(t)
		listed, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatalf("listing the tools: %v", err)
		}
		if len(listed.Tools) == 0 {
			t.Fatal("the server offered no tools, so nothing was exercised")
		}
		for _, tool := range listed.Tools {
			if _, err := session.CallTool(context.Background(),
				&sdk.CallToolParams{Name: tool.Name, Arguments: map[string]any{}}); err != nil {
				t.Fatalf("calling %s: %v", tool.Name, err)
			}
		}
	})

	if len(leaked) != 0 {
		t.Errorf("a tool call wrote %q to the process stdout, which is the transport", leaked)
	}
}

// The check above is only worth running if it would notice. Not parallel, for
// the same reason.
func TestTheStdoutCheckNoticesAWrite(t *testing.T) {
	leaked := captureStdout(t, func() {
		fmt.Fprintln(os.Stdout, "a command that printed to the wrong stream")
	})

	if !strings.Contains(string(leaked), "the wrong stream") {
		t.Fatalf("the capture did not notice a write to the process stdout, it saw %q", leaked)
	}
}

// captureStdout runs body with the process stdout replaced by a pipe and
// returns everything written to it.
func captureStdout(t *testing.T, body func()) []byte {
	t.Helper()

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("making a pipe: %v", err)
	}

	// The pipe is drained on its own goroutine: a body that wrote more than the
	// pipe buffer holds would otherwise block forever.
	done := make(chan []byte, 1)
	go func() {
		contents, _ := io.ReadAll(read)
		done <- contents
	}()

	realStdout := os.Stdout
	os.Stdout = write
	defer func() {
		os.Stdout = realStdout
		_ = write.Close()
	}()

	body()

	os.Stdout = realStdout
	if err := write.Close(); err != nil {
		t.Fatalf("closing the pipe: %v", err)
	}

	return <-done
}

// mcpSession connects to a server over the real command tree, with the gather
// and the clients stubbed so that nothing reaches a cluster.
func mcpSession(t *testing.T) *sdk.ClientSession {
	t.Helper()

	newRoot := func(deps mcp.CallDeps) *cobra.Command {
		return root.NewCommand("test", root.Deps{
			Streams:     deps.Streams,
			ConfigFlags: deps.ConfigFlags,
			MustGather:  func(*mustgather.Options) error { return nil },
		})
	}

	server, err := mcp.NewServer(newRoot, "test")
	if err != nil {
		t.Fatalf("building the server: %v", err)
	}

	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connecting the server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "test"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("connecting the client: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })

	return session
}

// isolateKubeconfigForMCP keeps these tests away from the kubeconfig of
// whoever runs them. KUBECONFIG has to name a path that does not exist rather
// than be emptied: client-go reads an empty value as unset and falls back to
// the default location, which is the file this protects.
func isolateKubeconfigForMCP(t *testing.T) {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig-here"))
}
