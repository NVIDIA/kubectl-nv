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
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
)

// readsPartially reports whether reading the bundle the way kubectl nv list
// and kubectl nv health read it comes back incomplete, and names what was
// missing.
//
// The two reads are the ones behind the two commands: the inventory, which
// needs every pod that asks for GPUs, and the node events, which is where
// NV-UPG-001 dates an upgrade step. Both have to be complete for either
// command to exit 0.
func readsPartially(t *testing.T, dir string) error {
	t.Helper()

	bundle, err := source.NewBundle(dir)
	if err != nil {
		t.Fatalf("reading the bundle at %s: %v", dir, err)
	}

	ctx := context.Background()
	list, _, buildErr := inventory.BuildWithHinted(ctx, bundle,
		inventory.BuildOptions{IncludeHinted: true, WithDetails: true})
	if list == nil {
		t.Fatalf("the bundle produced no inventory: %v", buildErr)
	}

	_, eventsErr := source.ReadNodeEvents(ctx, bundle)
	_, operatorErr := bundle.Pods(ctx, "", "app=gpu-operator")

	return errors.Join(buildErr, eventsErr, operatorErr)
}

// A bundle holding everything the readers need reads as complete. Until the
// gather collected the GPU pods of every namespace and the node events, it
// could not: list --from-bundle and health --from-bundle exited 3 on every
// bundle there has ever been, which makes exit 3 mean nothing.
func TestACompleteBundleReadsAsComplete(t *testing.T) {
	dir, _, err := runGather(t, &Options{ToolVersion: "v1.2.3"}, completeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if err := readsPartially(t, dir); err != nil {
		t.Errorf("a bundle gathered from a cluster that answered everything still reads as partial: %v", err)
	}
}

// The requested count is the whole reason the GPU pods are collected. A bundle
// that carries them has to produce the same number a live read would.
func TestACompleteBundleKnowsWhatIsRequested(t *testing.T) {
	dir, _, err := runGather(t, &Options{ToolVersion: "v1.2.3"}, completeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	bundle, err := source.NewBundle(dir)
	if err != nil {
		t.Fatalf("reading the bundle: %v", err)
	}
	list, _, _ := inventory.BuildWithHinted(context.Background(), bundle,
		inventory.BuildOptions{IncludeHinted: true, WithDetails: true})
	if list == nil {
		t.Fatal("the bundle produced no inventory")
	}

	requested := map[string]int64{}
	known := map[string]bool{}
	for i := range list.Items {
		requested[list.Items[i].Name] = list.Items[i].Requested
		known[list.Items[i].Name] = list.Items[i].RequestedKnown
	}

	for node, want := range map[string]int64{"gpu-node-1": 2, "gpu-node-2": 1} {
		if !known[node] {
			t.Errorf("%s reports its requested GPUs as unknown", node)
		}
		if requested[node] != want {
			t.Errorf("%s has %d GPUs requested, want %d", node, requested[node], want)
		}
	}
}

// A bundle from before these collectors existed still reads as partial, and
// says which part is missing. Reading it as complete would be worse than the
// bug this fixes: a count of zero requested GPUs would read as an idle
// cluster.
func TestAnOldBundleStillReadsAsPartial(t *testing.T) {
	dir, _, err := runGather(t, &Options{ToolVersion: "v1.2.3"}, completeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// An old bundle is this one without the two files, which is exactly what
	// kubectl-nv wrote before they existed.
	for _, name := range []string{gpuPodsFile, nodeEventsFile} {
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			t.Fatalf("removing %s: %v", name, err)
		}
	}

	err = readsPartially(t, dir)
	if err == nil {
		t.Fatal("a bundle holding neither the GPU pods nor the node events reads as complete")
	}

	var partial *source.PartialError
	if !errors.As(err, &partial) {
		t.Errorf("the error is %T (%v), want a source.PartialError", err, err)
	}
}

// Half a bundle is still partial. A gather whose event read was refused writes
// the rest, and a reader that called that complete would report a cluster
// clean on evidence it never saw.
func TestABundleMissingOnlyTheEventsIsStillPartial(t *testing.T) {
	dir, _, err := runGather(t, &Options{ToolVersion: "v1.2.3"}, completeCluster()...)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, nodeEventsFile)); err != nil {
		t.Fatalf("removing %s: %v", nodeEventsFile, err)
	}

	if err := readsPartially(t, dir); err == nil {
		t.Error("a bundle with no node events reads as complete")
	}
}
