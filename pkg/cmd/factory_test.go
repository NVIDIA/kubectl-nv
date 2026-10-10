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
)

func TestResolveKubeconfigPath(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(existing, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// A home directory of our own, so that the developer's own kubeconfig
	// cannot decide whether this test passes.
	home := t.TempDir()
	homeConfig := filepath.Join(home, ".kube", "config")

	cases := []struct {
		name      string
		explicit  string
		env       string
		writeHome bool
		want      string
		wantErr   bool
	}{
		{name: "explicit path that exists", explicit: existing, want: existing},
		{name: "explicit path wins over the environment", explicit: existing, env: "/nowhere/config", want: existing},
		{name: "environment when nothing is given", env: existing, want: existing},
		{name: "environment when the explicit path is missing", explicit: "/nowhere/config", env: existing, want: existing},
		{name: "home directory fallback", writeHome: true, want: homeConfig},
		{name: "nothing to fall back on", wantErr: true},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("KUBECONFIG", c.env)
			if c.writeHome {
				if err := os.MkdirAll(filepath.Dir(homeConfig), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(homeConfig, []byte("apiVersion: v1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.Remove(homeConfig) })
			}

			got, err := ResolveKubeconfigPath(c.explicit)
			if (err != nil) != c.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, c.wantErr)
			}
			if c.wantErr {
				if !strings.Contains(err.Error(), homeConfig) {
					t.Errorf("error = %q, want it to name %q", err.Error(), homeConfig)
				}
				return
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

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
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", "")

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
