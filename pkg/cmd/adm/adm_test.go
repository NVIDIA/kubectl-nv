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

package adm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// TestDefaultArtifactDirIsUsedWhenNoneIsGiven pins where a gather lands when
// the user chose nothing. The name itself is the gatherer's, so that the two
// places that build it cannot drift; what matters here is that the command
// uses it rather than inventing a second one.
func TestDefaultArtifactDirIsUsedWhenNoneIsGiven(t *testing.T) {
	isolateKubeconfig(t)

	var got *mustgather.Options
	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	adm := NewCommand(o, func(opts *mustgather.Options) error { got = opts; return nil })

	parent := &cobra.Command{Use: "kubectl-nv", Version: "v9.9.9"}
	parent.AddCommand(adm)
	parent.SetArgs([]string{"adm", "must-gather"})
	parent.SetOut(io.Discard)
	parent.SetErr(io.Discard)
	parent.SilenceUsage = true
	parent.SilenceErrors = true

	if err := parent.Execute(); err != nil {
		t.Fatalf("adm must-gather: %v", err)
	}
	if !strings.HasPrefix(got.ArtifactDir, "/tmp/nvidia-gpu-operator_") {
		t.Errorf("ArtifactDir = %q, want the gpu-operator script's prefix", got.ArtifactDir)
	}
	if got.ArtifactDir == mustgather.DefaultArtifactDir(time.Now()) {
		t.Error("two calls produced the same directory, so two runs would share one bundle")
	}
}

// TestAdmRejectsAnUnknownSubcommand covers the typo directly on the adm
// command, where the missing Args check lived.
func TestAdmRejectsAnUnknownSubcommand(t *testing.T) {
	isolateKubeconfig(t)

	reached := false
	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	adm := NewCommand(o, func(*mustgather.Options) error { reached = true; return nil })

	// Under a parent, which is where adm actually lives. Cobra rejects an
	// unknown first argument to a ROOT command on its own, so an adm executed
	// as its own root would hide the gap.
	parent := &cobra.Command{Use: "kubectl-nv"}
	parent.AddCommand(adm)
	parent.SetArgs([]string{"adm", "must-gaher"})
	parent.SetOut(io.Discard)
	parent.SetErr(io.Discard)

	err := parent.Execute()
	if err == nil {
		t.Fatal("adm must-gaher returned nil, want an error")
	}
	if !strings.Contains(err.Error(), "must-gaher") {
		t.Errorf("error = %q, want it to name the mistyped subcommand", err.Error())
	}
	if reached {
		t.Error("the must-gather ran for a subcommand that does not exist")
	}
}

// isolateKubeconfig keeps a test away from the kubeconfig of whoever is
// running it. KUBECONFIG has to name a path that does not exist rather than be
// emptied: client-go treats an empty KUBECONFIG as unset and falls back to the
// default location, which is the file this is protecting.
func isolateKubeconfig(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, "no-kubeconfig-here"))
}

// runMustGather executes "adm must-gather" with the given extra arguments and
// returns the options the gather was handed, so that a flag that is declared
// but never read is caught.
func runMustGather(t *testing.T, args ...string) (*mustgather.Options, error) {
	t.Helper()
	isolateKubeconfig(t)

	var got *mustgather.Options
	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	adm := NewCommand(o, func(opts *mustgather.Options) error { got = opts; return nil })

	parent := &cobra.Command{Use: "kubectl-nv", Version: "v9.9.9"}
	parent.AddCommand(adm)
	parent.SetArgs(append([]string{"adm", "must-gather", "--artifacts-dir", t.TempDir()}, args...))
	parent.SetOut(io.Discard)
	parent.SetErr(io.Discard)
	parent.SilenceUsage = true
	parent.SilenceErrors = true

	return got, parent.Execute()
}

// TestMustGatherFlagsReachTheGatherer covers the three flags the command gained
// for Track C. A flag that is declared and never copied into the options parses
// cleanly, prints in the help, and changes nothing about the bundle.
func TestMustGatherFlagsReachTheGatherer(t *testing.T) {
	got, err := runMustGather(t, "--node", "gpu-node-7", "--since", "45m", "--no-exec")
	if err != nil {
		t.Fatalf("adm must-gather: %v", err)
	}
	if got == nil {
		t.Fatal("the must-gather did not run")
	}

	if got.Node != "gpu-node-7" {
		t.Errorf("Node = %q, want gpu-node-7", got.Node)
	}
	if got.Since != 45*time.Minute {
		t.Errorf("Since = %v, want 45m", got.Since)
	}
	if !got.NoExec {
		t.Error("NoExec is false, but --no-exec was given")
	}
}

// TestMustGatherDefaultsLeaveTheGatherUnnarrowed keeps the flags out of the way
// of the run a support engineer does without them.
func TestMustGatherDefaultsLeaveTheGatherUnnarrowed(t *testing.T) {
	got, err := runMustGather(t)
	if err != nil {
		t.Fatalf("adm must-gather: %v", err)
	}
	if got.Node != "" || got.Since != 0 || got.NoExec {
		t.Errorf("defaults are Node=%q Since=%v NoExec=%v, want the whole cluster and the whole log",
			got.Node, got.Since, got.NoExec)
	}
}

// TestMustGatherRecordsTheToolVersion pins what the bundle manifest says
// produced it. A bundle that names no version, or names "dev" because the
// command read the wrong field, sends support looking at the wrong source.
func TestMustGatherRecordsTheToolVersion(t *testing.T) {
	got, err := runMustGather(t)
	if err != nil {
		t.Fatalf("adm must-gather: %v", err)
	}
	if got.ToolVersion != "v9.9.9" {
		t.Errorf("ToolVersion = %q, want the version the root command carries", got.ToolVersion)
	}
}

// TestMustGatherRejectsAnUnparseableSince keeps a typo from being read as "no
// window at all", which would quietly collect the whole log of a cluster that
// has been unhappy for weeks.
func TestMustGatherRejectsAnUnparseableSince(t *testing.T) {
	_, err := runMustGather(t, "--since", "45minutes")
	if err == nil {
		t.Fatal("adm must-gather --since 45minutes returned nil, want a usage error")
	}
	if !strings.Contains(err.Error(), "since") {
		t.Errorf("error = %q, want it to name the flag", err.Error())
	}
}

// TestExecTimeoutFlagReachesTheGatherer covers the bound on one exec. Without
// it a driver pod that accepts the connection and never answers costs the user
// the whole bundle rather than one file.
func TestExecTimeoutFlagReachesTheGatherer(t *testing.T) {
	got, err := runMustGather(t, "--exec-timeout", "30s")
	if err != nil {
		t.Fatalf("adm must-gather: %v", err)
	}
	if got.ExecTimeout != 30*time.Second {
		t.Errorf("ExecTimeout = %v, want 30s", got.ExecTimeout)
	}
}

// TestExecTimeoutDefaults pins the default. A zero reaching the gatherer would
// be read as "no deadline", which is the behaviour this flag exists to end.
func TestExecTimeoutDefaults(t *testing.T) {
	got, err := runMustGather(t)
	if err != nil {
		t.Fatalf("adm must-gather: %v", err)
	}
	if got.ExecTimeout != mustgather.DefaultExecTimeout {
		t.Errorf("ExecTimeout = %v, want the default %v", got.ExecTimeout, mustgather.DefaultExecTimeout)
	}
}

// TestRejectsAnUnusableWindow covers the flags whose value only makes sense
// above a floor. A sub-second log window collects nothing and a sub-second exec
// timeout cannot finish a bug report, so both are a mistake worth naming rather
// than a gather that quietly produces an empty bundle.
func TestRejectsAnUnusableWindow(t *testing.T) {
	cases := map[string][]string{
		"since below a second":        {"--since", "500ms"},
		"negative since":              {"--since", "-1h"},
		"exec timeout below a second": {"--exec-timeout", "10ms"},
	}

	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := runMustGather(t, args...)
			if err == nil {
				t.Fatalf("adm must-gather %v returned nil, want a usage error", args)
			}
			if got := output.ExitCodeFor(err); got != output.ExitUsage {
				t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitUsage, err)
			}
			if !strings.Contains(err.Error(), strings.TrimPrefix(args[0], "--")) {
				t.Errorf("error = %q, want it to name the flag", err.Error())
			}
		})
	}
}

// TestContextReachesTheManifest covers what a bundle says about where it came
// from. A bundle that names the wrong cluster, or none, gets filed against the
// wrong one, and the field is filled in by the command rather than by the
// gatherer so nothing but a run through the real wiring proves it.
func TestContextReachesTheManifest(t *testing.T) {
	kubeconfig := writeKubeconfigWithContexts(t)

	t.Run("the current context when none is named", func(t *testing.T) {
		got, err := runMustGatherWithKubeconfig(t, kubeconfig)
		if err != nil {
			t.Fatalf("adm must-gather: %v", err)
		}
		if got.Context != "lane-a" {
			t.Errorf("Context = %q, want the kubeconfig's current context lane-a", got.Context)
		}
	})

	t.Run("the named context wins", func(t *testing.T) {
		got, err := runMustGatherWithKubeconfig(t, kubeconfig, "--context", "lane-b")
		if err != nil {
			t.Fatalf("adm must-gather: %v", err)
		}
		if got.Context != "lane-b" {
			t.Errorf("Context = %q, want lane-b", got.Context)
		}
	})
}

// writeKubeconfigWithContexts writes a kubeconfig holding two contexts, so that
// "the current one" and "the one that was asked for" can be told apart.
func writeKubeconfigWithContexts(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	contents := `apiVersion: v1
kind: Config
clusters:
- name: lane
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: lane-a
  context:
    cluster: lane
    user: lane
- name: lane-b
  context:
    cluster: lane
    user: lane
current-context: lane-a
users:
- name: lane
  user: {}
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing kubeconfig: %v", err)
	}
	return path
}

// runMustGatherWithKubeconfig runs the command against a named kubeconfig,
// through the real configuration flags rather than a stub.
func runMustGatherWithKubeconfig(t *testing.T, kubeconfig string, args ...string) (*mustgather.Options, error) {
	t.Helper()
	isolateKubeconfig(t)

	var got *mustgather.Options
	o := &cmd.Options{
		Streams:     genericiooptions.NewTestIOStreamsDiscard(),
		ConfigFlags: genericclioptions.NewConfigFlags(true),
	}
	adm := NewCommand(o, func(opts *mustgather.Options) error { got = opts; return nil })

	parent := &cobra.Command{Use: "kubectl-nv", Version: "v9.9.9"}
	cmd.AddConfigFlags(parent.PersistentFlags(), o.ConfigFlags)
	parent.AddCommand(adm)
	parent.SetArgs(append([]string{
		"adm", "must-gather",
		"--artifacts-dir", filepath.Join(t.TempDir(), "artifacts"),
		"--kubeconfig", kubeconfig,
	}, args...))
	parent.SetOut(io.Discard)
	parent.SetErr(io.Discard)
	parent.SilenceUsage = true
	parent.SilenceErrors = true

	return got, parent.Execute()
}

// TestNoExecIsForcedTheWayTheMCPServerSpellsIt pins the contract the MCP
// server's exec gate rests on.
//
// That server serves must-gather only because it has a --no-exec flag, and it
// forces the flag by appending "--no-exec=true" to the end of the command line,
// after anything else, so that the last value wins whatever came before. Both
// halves of that are assumptions about this command: that it accepts the
// explicit =true spelling, and that a later value beats an earlier one. If
// either stopped holding, the server would still serve the tool and the gather
// would exec into driver pods on behalf of a model.
func TestNoExecIsForcedTheWayTheMCPServerSpellsIt(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{name: "the explicit spelling the server uses", args: []string{"--no-exec=true"}, want: true},
		{name: "explicitly off", args: []string{"--no-exec=false"}, want: false},
		{
			// The server appends its own last for exactly this reason.
			name: "forced last over an earlier false",
			args: []string{"--no-exec=false", "--no-exec=true"},
			want: true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := runMustGather(t, c.args...)
			if err != nil {
				t.Fatalf("adm must-gather %v: %v", c.args, err)
			}
			if got.NoExec != c.want {
				t.Errorf("NoExec = %v, want %v", got.NoExec, c.want)
			}
		})
	}
}

// TestAServerPinnedArtifactDirIsAccepted covers the directory the MCP server
// makes for each call.
//
// The server will not let a model choose where files go: it creates a fresh
// empty directory with os.MkdirTemp and pins --artifacts-dir to it. This
// command refuses a directory that already holds a bundle, and that refusal has
// to let the server's directory through, or every must-gather over MCP fails on
// a directory the server itself just made.
func TestAServerPinnedArtifactDirIsAccepted(t *testing.T) {
	// Made the same way the server makes it, including the mode it sets.
	dir, err := os.MkdirTemp("", "kubectl-nv-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	isolateKubeconfig(t)

	var got *mustgather.Options
	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	adm := NewCommand(o, func(opts *mustgather.Options) error {
		got = opts

		return mustgather.Run(opts, offlineClients(), discardLogger())
	})

	parent := &cobra.Command{Use: "kubectl-nv", Version: "v9.9.9"}
	parent.AddCommand(adm)
	parent.SetArgs([]string{"adm", "must-gather", "--artifacts-dir=" + dir, "--no-exec=true"})
	parent.SetOut(io.Discard)
	parent.SetErr(io.Discard)
	parent.SilenceUsage = true
	parent.SilenceErrors = true

	err = parent.Execute()
	if err != nil && strings.Contains(err.Error(), "is not empty") {
		t.Fatalf("the gather refused the directory the server pinned for it: %v", err)
	}
	if got == nil {
		t.Fatal("the gather never ran")
	}
	if got.ArtifactDir != dir {
		t.Errorf("ArtifactDir = %q, want the pinned %q", got.ArtifactDir, dir)
	}
	if !got.NoExec {
		t.Error("NoExec is false on a call the server forced it on")
	}
}

// offlineClients hands the gatherer clients that reach no cluster, so that the
// directory check above is exercised by a real Run without one.
func offlineClients() mustgather.Clients {
	return offlineFactory{
		clientset: fake.NewSimpleClientset(),
		// The dynamic fake panics on a List of a resource whose list kind it
		// was not told about, so every custom resource the gather reads is
		// registered even though this cluster holds none of them.
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(), mustgather.CustomResourceListKinds()),
		config: &rest.Config{
			Host: "https://kubectl-nv.invalid",
			Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				http.NotFound(w, r)
				resp := w.Result()
				resp.Request = r

				return resp, nil
			}),
		},
	}
}

type offlineFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
	config    *rest.Config
}

func (f offlineFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f offlineFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f offlineFactory) RESTConfig() (*rest.Config, error)               { return f.config, nil }

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// discardLogger is somewhere for the gather's progress to go.
func discardLogger() *output.Logger {
	return output.NewLogger(genericiooptions.NewTestIOStreamsDiscard(), output.LoggerOptions{})
}
