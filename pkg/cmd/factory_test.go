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

package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

func TestSetKubernetesDefaults(t *testing.T) {
	cfg := &rest.Config{}
	err := setKubernetesDefaults(cfg)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if cfg.APIPath != "/api" {
		t.Error("APIPath not set")
	}
	if cfg.GroupVersion == nil || cfg.GroupVersion.Version != "v1" {
		t.Error("GroupVersion not set")
	}
}

// TestExplicitKubeconfigMustExist pins the one resolver. A path given on the
// command line is a statement of intent: falling back to KUBECONFIG because
// that path is missing would gather from a cluster the user did not name.
func TestExplicitKubeconfigMustExist(t *testing.T) {
	isolateKubeconfig(t)

	good := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(good, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", good)

	missing := filepath.Join(t.TempDir(), "nonexistent")
	flags := genericclioptions.NewConfigFlags(false)
	flags.KubeConfig = &missing

	_, err := NewFactory(flags).RESTConfig()
	if err == nil {
		t.Fatal("expected an error for a kubeconfig that does not exist")
	}
	if !strings.Contains(err.Error(), missing) {
		t.Errorf("error = %q, want it to name %q", err.Error(), missing)
	}
}

// TestServerAndTokenWithoutAKubeconfig covers the other half: with --server and
// --token there is nothing to read from disk, and demanding a kubeconfig file
// would turn a working invocation into an error.
func TestServerAndTokenWithoutAKubeconfig(t *testing.T) {
	isolateKubeconfig(t)

	server := "https://127.0.0.1:1"
	token := "not-a-real-token"
	flags := genericclioptions.NewConfigFlags(false)
	flags.APIServer = &server
	flags.BearerToken = &token

	f := NewFactory(flags)
	config, err := f.RESTConfig()
	if err != nil {
		t.Fatalf("RESTConfig returned %v", err)
	}
	if config.Host != server {
		t.Errorf("host = %q, want %q", config.Host, server)
	}
	if _, err := f.KubernetesClient(); err != nil {
		t.Errorf("KubernetesClient returned %v", err)
	}
}

const testKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`

// TestKubeconfigDisplay checks the string the gather reports against the files
// the client configuration would actually read. It is a report, not a second
// resolver: an explicit path is named even when it does not exist, because
// that is the path the clients will fail on.
func TestKubeconfigDisplay(t *testing.T) {
	home := t.TempDir()
	onDisk := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(onDisk, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	missing := "/nonexistent/config"

	cases := []struct {
		name     string
		explicit string
		env      string
		want     string
	}{
		{name: "explicit path", explicit: onDisk, want: onDisk},
		{name: "explicit path that does not exist", explicit: missing, env: onDisk, want: missing},
		{name: "environment", env: onDisk, want: onDisk},
		// client-go fixes the home path at package load, so the default is its
		// own constant rather than anything HOME says now.
		{name: "default location", want: clientcmd.RecommendedHomeFile},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("KUBECONFIG", c.env)

			flags := genericclioptions.NewConfigFlags(false)
			flags.KubeConfig = &c.explicit
			o := &Options{ConfigFlags: flags}

			if got := o.KubeconfigDisplay(); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
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
