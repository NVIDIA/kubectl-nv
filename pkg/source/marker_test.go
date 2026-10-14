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

package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// markerBundle writes a bundle whose ClusterPolicy and ComputeDomains are both
// absent, each with the marker the gather writes and the reason it gives.
func markerBundle(t *testing.T, policyReason, domainReason string) *Bundle {
	t.Helper()

	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	write("nodes.yaml", "apiVersion: v1\nkind: NodeList\nitems: []\n")
	if policyReason != "" {
		write("cluster_policy.missing", policyReason+"\n")
	}
	if domainReason != "" {
		write("compute_domains.missing", domainReason+"\n")
	}

	bundle, err := NewBundle(dir)
	if err != nil {
		t.Fatalf("reading the bundle: %v", err)
	}

	return bundle
}

// A marker saying "not installed" is an answer: the cluster served no such
// collection because the CRD was never applied, so nothing was denied and
// nothing is missing. A reader that called it a partial read would report
// every cluster without the GPU Operator as a cluster it could not see.
func TestANotInstalledMarkerIsAnAnswer(t *testing.T) {
	t.Parallel()

	bundle := markerBundle(t, notInstalledReason, notInstalledReason)
	ctx := context.Background()

	_, err := bundle.ClusterPolicies(ctx)
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("the ClusterPolicy marker gave %v, want ErrNotInstalled", err)
	}
	var partial *PartialError
	if errors.As(err, &partial) {
		t.Errorf("the ClusterPolicy marker reads as a partial read: %v", err)
	}

	domains, err := bundle.ComputeDomains(ctx)
	if err != nil {
		t.Errorf("the ComputeDomain marker gave %v, want no error at all", err)
	}
	if len(domains) != 0 {
		t.Errorf("the ComputeDomain marker gave %d domains, want none", len(domains))
	}
}

// A marker saying "forbidden" is the opposite: the cluster has the collection
// and would not let the gather read it, so the bundle is missing something it
// was meant to hold. That is exit 3, and the error has to name the file, or
// the user is told the answer is incomplete and not which part.
func TestAForbiddenMarkerIsAPartialRead(t *testing.T) {
	t.Parallel()

	bundle := markerBundle(t, "forbidden", "forbidden")
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		read func() error
		file string
	}{
		{
			name: "cluster policy",
			read: func() error { _, err := bundle.ClusterPolicies(ctx); return err },
			file: fileClusterPolicy,
		},
		{
			name: "compute domains",
			read: func() error { _, err := bundle.ComputeDomains(ctx); return err },
			file: fileComputeDomains,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.read()
			if err == nil {
				t.Fatal("a refused read came back as an answer")
			}

			var partial *PartialError
			if !errors.As(err, &partial) {
				t.Fatalf("error is %T (%v), want a PartialError", err, err)
			}
			if code := partial.ExitCode(); code != 3 {
				t.Errorf("the error exits %d, want 3", code)
			}
			if !strings.Contains(partial.Resource+" "+err.Error(), tc.file) {
				t.Errorf("the error does not name %s: %v", tc.file, err)
			}
			if errors.Is(err, ErrNotInstalled) {
				t.Error("a refused read matched ErrNotInstalled: it is not an absent CRD")
			}
		})
	}
}

// No marker and no file is a bundle gathered before the collector existed. It
// is still a partial read, because nothing ever looked.
func TestNoMarkerAndNoFileIsStillPartial(t *testing.T) {
	t.Parallel()

	bundle := markerBundle(t, "", "")
	ctx := context.Background()

	if _, err := bundle.ClusterPolicies(ctx); err == nil {
		t.Error("a bundle with no ClusterPolicy and no marker read as complete")
	}
	if _, err := bundle.ComputeDomains(ctx); err == nil {
		t.Error("a bundle with no ComputeDomains and no marker read as complete")
	}
}

// A pod recorded in both files is one pod. The operand list holds every pod of
// the operator namespace and the GPU list holds every pod that asks for GPUs,
// so an operand that asks for one is in both; counting it twice would double
// what a node has in use.
func TestBundlePodsCountAPodInBothFilesOnce(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	pod := func(name, namespace, node string) string {
		return "- apiVersion: v1\n  kind: Pod\n  metadata:\n    name: " + name +
			"\n    namespace: " + namespace + "\n  spec:\n    nodeName: " + node + "\n"
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	write(fileOperandPods, "apiVersion: v1\nkind: PodList\nitems:\n"+
		pod("dcgm-exporter-abc", "gpu-operator", "gpu-node-1")+
		pod("plugin-xyz", "gpu-operator", "gpu-node-1"))
	write(fileGPUPods, "apiVersion: v1\nkind: PodList\nitems:\n"+
		pod("dcgm-exporter-abc", "gpu-operator", "gpu-node-1")+
		pod("trainer", "research", "gpu-node-1"))

	bundle, err := NewBundle(dir)
	if err != nil {
		t.Fatalf("reading the bundle: %v", err)
	}

	pods, err := bundle.Pods(context.Background(), "", "")
	if err != nil {
		t.Fatalf("reading the pods: %v", err)
	}

	seen := map[string]int{}
	for i := range pods {
		seen[pods[i].Namespace+"/"+pods[i].Name]++
	}
	if got := seen["gpu-operator/dcgm-exporter-abc"]; got != 1 {
		t.Errorf("the pod recorded in both files came back %d times, want once", got)
	}
	if len(pods) != 3 {
		t.Errorf("the bundle gave %d pods, want the 3 distinct ones: %v", len(pods), seen)
	}
}
