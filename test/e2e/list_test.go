//go:build e2e

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

package e2e

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// laneGPUCount is how many simulated GPUs the t4 profile advertises per
// worker. It matches mokka::profile_gpu_count in hack/mokka/lib.sh; the suite
// asserts the number rather than reading it back off the node, because
// reporting whatever the cluster says would pass against a node with none.
const laneGPUCount = 4

// laneProduct is the device name GFD writes on the lane's workers.
const laneProduct = "NVIDIA-T4"

// gpuNodeJSON is the part of a GPUNode this suite asserts on.
type gpuNodeJSON struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Product        string `json:"product"`
	Count          int    `json:"count"`
	Capacity       int64  `json:"capacity"`
	Allocatable    int64  `json:"allocatable"`
	Requested      int64  `json:"requested"`
	RequestedKnown bool   `json:"requestedKnown"`
	DriverVersion  string `json:"driverVersion"`
	CUDAVersion    string `json:"cudaVersion"`
	Mode           string `json:"mode"`
	Hinted         bool   `json:"hinted"`
	Operands       []struct {
		Name      string `json:"name"`
		Component string `json:"component"`
	} `json:"operands"`
}

// gpuNodeListJSON is the envelope kubectl nv list prints.
type gpuNodeListJSON struct {
	APIVersion string        `json:"apiVersion"`
	Kind       string        `json:"kind"`
	Items      []gpuNodeJSON `json:"items"`
}

// listJSON runs kubectl nv list with the given extra arguments and decodes it.
func listJSON(t *testing.T, args ...string) gpuNodeListJSON {
	t.Helper()

	res := run(t, append([]string{"list", "-o", "json"}, args...)...)
	if res.code != 0 {
		t.Fatalf("kubectl nv list exited %d, want 0\nstderr:\n%s", res.code, res.stderr)
	}

	var list gpuNodeListJSON
	if err := json.Unmarshal([]byte(res.stdout), &list); err != nil {
		t.Fatalf("decoding kubectl nv list output: %v\n%s", err, res.stdout)
	}

	return list
}

// nodeNames lists the names in a decoded list, in the order it holds them.
func nodeNames(list gpuNodeListJSON) []string {
	names := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		names = append(names, item.Metadata.Name)
	}

	return names
}

// nodeNamed returns the listed node of that name.
func nodeNamed(t *testing.T, list gpuNodeListJSON, name string) gpuNodeJSON {
	t.Helper()

	for _, item := range list.Items {
		if item.Metadata.Name == name {
			return item
		}
	}

	t.Fatalf("kubectl nv list does not hold %q; it holds %v", name, nodeNames(list))

	return gpuNodeJSON{}
}

func TestListAgainstTheLane(t *testing.T) {
	workers := workerNodeNames(t)
	if len(workers) != 2 {
		t.Fatalf("the lane has %d workers (%v), want 2", len(workers), workers)
	}

	list := listJSON(t)

	t.Run("the GPU nodes are the workers", func(t *testing.T) {
		if got := nodeNames(list); !reflect.DeepEqual(got, workers) {
			t.Errorf("kubectl nv list = %v, want the workers %v, sorted", got, workers)
		}
	})

	t.Run("every worker advertises the profile's GPUs", func(t *testing.T) {
		for _, name := range workers {
			node := nodeNamed(t, list, name)

			if node.Capacity != laneGPUCount {
				t.Errorf("%s capacity = %d, want %d", name, node.Capacity, laneGPUCount)
			}

			if node.Allocatable != laneGPUCount {
				t.Errorf("%s allocatable = %d, want %d", name, node.Allocatable, laneGPUCount)
			}

			if node.Product != laneProduct {
				t.Errorf("%s product = %q, want %q", name, node.Product, laneProduct)
			}

			if node.Mode != "device-plugin" {
				t.Errorf("%s mode = %q, want device-plugin", name, node.Mode)
			}

			if node.Hinted {
				t.Errorf("%s is reported as hinted, want a real GPU node", name)
			}

			// GFD wrote these, under whichever of the two label spellings its
			// version uses. An empty one means kubectl-nv read neither.
			if node.DriverVersion == "" {
				t.Errorf("%s has no driver version; see docs/labels.md", name)
			}

			if node.CUDAVersion == "" {
				t.Errorf("%s has no CUDA version; see docs/labels.md", name)
			}
		}
	})

	t.Run("the control plane is out of the list", func(t *testing.T) {
		// The simulated-GPU DaemonSet tolerates every taint, so NFD labels the
		// control plane. It has no allocatable GPU and never will, and a list
		// that holds it is a list that cannot be used to find a node to run on.
		for _, name := range nodeNames(list) {
			if isControlPlane(t, name) {
				t.Errorf("kubectl nv list holds the control plane node %q", name)
			}
		}
	})

	t.Run("the control plane is counted on stderr", func(t *testing.T) {
		res := run(t, "list")
		if res.code != 0 {
			t.Fatalf("kubectl nv list exited %d, want 0\nstderr:\n%s", res.code, res.stderr)
		}

		// The whole line, including the count. The lane has exactly one
		// hinted node, the control plane, so a note saying anything else is
		// either counting the wrong set or counting it after the wrong step.
		want := "1 node(s) carry an NVIDIA label but advertise no GPUs; pass --include-hinted to list them"
		if got := strings.TrimSpace(res.stderr); got != want {
			t.Errorf("stderr = %q, want %q", got, want)
		}
	})

	t.Run("the control plane is listed on request", func(t *testing.T) {
		withHinted := listJSON(t, "--include-hinted")

		if got := nodeNames(withHinted); !reflect.DeepEqual(got, clusterNodeNames(t)) {
			t.Errorf("kubectl nv list --include-hinted = %v, want every node %v", got, clusterNodeNames(t))
		}

		for _, name := range nodeNames(withHinted) {
			node := nodeNamed(t, withHinted, name)
			if isControlPlane(t, name) && !node.Hinted {
				t.Errorf("%s is listed without hinted set", name)
			}
		}
	})

	t.Run("name output is one gpunode line per node", func(t *testing.T) {
		res := run(t, "list", "-o", "name")
		if res.code != 0 {
			t.Fatalf("kubectl nv list -o name exited %d, want 0\nstderr:\n%s", res.code, res.stderr)
		}

		want := make([]string, 0, len(workers))
		for _, name := range workers {
			want = append(want, "gpunode/"+name)
		}

		if got := splitLines(res.stdout); !reflect.DeepEqual(got, want) {
			t.Errorf("kubectl nv list -o name = %v, want %v", got, want)
		}
	})

	t.Run("the table has the agreed columns", func(t *testing.T) {
		res := run(t, "list")
		if res.code != 0 {
			t.Fatalf("kubectl nv list exited %d, want 0\nstderr:\n%s", res.code, res.stderr)
		}

		header := strings.Fields(splitLines(res.stdout)[0])
		want := []string{"NODE", "PRODUCT", "GPUS", "DRIVER", "CUDA", "MODE", "MIG", "UPGRADE", "STATUS"}

		if !reflect.DeepEqual(header, want) {
			t.Errorf("header = %v, want %v", header, want)
		}
	})

	t.Run("no-pods reports the requested count as unknown", func(t *testing.T) {
		res := run(t, "list", "--no-pods")
		if res.code != 0 {
			t.Fatalf("kubectl nv list --no-pods exited %d, want 0\nstderr:\n%s", res.code, res.stderr)
		}

		if !strings.Contains(res.stdout, "?/") {
			t.Errorf("kubectl nv list --no-pods counted the requested GPUs anyway:\n%s", res.stdout)
		}
	})
}

// TestListFromABundleMatchesTheLane is the round trip: a must-gather of the
// lane, replayed offline, has to describe the same cluster as the live run.
//
// Exactly five JSON fields are removed before the comparison. Every other
// field of every node, including ones added to the model after this test was
// written, has to be equal, which is why the comparison is over generic maps
// rather than over a struct listing what to check.
//
// Removed at the node level:
//
//   - requested, and requestedKnown with it. must-gather records only the
//     operator's namespace, so a bundle cannot sum the GPU demand of the
//     cluster. The bundle says so through requestedKnown=false rather than by
//     reporting the part it saw as the whole, and that is asserted below
//     rather than normalised away.
//
// Removed from each operand:
//
//   - phase, ready and restarts. These three are a moment in time, not a
//     property of the cluster: the gather and the live run are seconds apart,
//     and an operand that restarts between them changes all three. The
//     operand's name, namespace, component, images and init container results
//     are all still compared.
//
// Nothing else is removed. Conditions and resourceSlices need no removal:
// kubectl nv list fills neither, so both are empty on both sides and are compared as
// such.
func TestListFromABundleMatchesTheLane(t *testing.T) {
	b := gather(t)

	live := rawList(t)
	if live.code != 0 {
		t.Fatalf("the live kubectl nv list exited %d, want 0\nstderr:\n%s", live.code, live.stderr)
	}

	fromBundle := rawList(t, "--from-bundle", b.dir)

	// A bundle holds one namespace, so the pod read is partial by
	// construction, and exit 3 is how the command says the answer is
	// incomplete rather than clean.
	if fromBundle.code != 3 {
		t.Fatalf("kubectl nv list --from-bundle exited %d, want 3\nstderr:\n%s", fromBundle.code, fromBundle.stderr)
	}

	liveNodes := decodeNodes(t, live.stdout)
	bundleNodes := decodeNodes(t, fromBundle.stdout)

	// Both sides have to hold the lane's two workers before any comparison
	// means anything: two empty lists are equal, and would pass every
	// assertion below while proving that neither run saw a cluster.
	if len(liveNodes) != 2 {
		t.Fatalf("the live run found %d GPU nodes (%v), want the lane's 2 workers",
			len(liveNodes), listedNames(liveNodes))
	}

	if !reflect.DeepEqual(listedNames(liveNodes), listedNames(bundleNodes)) {
		t.Fatalf("the bundle holds %v, the cluster holds %v",
			listedNames(bundleNodes), listedNames(liveNodes))
	}

	for _, name := range listedNames(liveNodes) {
		fromCluster := normaliseNode(liveNodes[name])
		replayed := normaliseNode(bundleNodes[name])

		if !reflect.DeepEqual(fromCluster, replayed) {
			t.Errorf("%s differs between the cluster and the bundle:\nlive:   %v\nbundle: %v",
				name, fromCluster, replayed)
		}
	}

	t.Run("the bundle admits it did not count the pods", func(t *testing.T) {
		for name, node := range bundleNodes {
			if known, _ := node["requestedKnown"].(bool); known {
				t.Errorf("%s claims a requested count from a bundle that holds one namespace", name)
			}
		}
	})
}

// rawList runs kubectl nv list -o json without failing on a non-zero exit, so that
// the caller can assert the exit code itself.
func rawList(t *testing.T, args ...string) result {
	t.Helper()

	return run(t, append([]string{"list", "-o", "json"}, args...)...)
}

// decodeNodes decodes a GPUNodeList into one generic map per node, keyed by
// name. Generic rather than typed so that a field added to the model later is
// compared by this test without it being edited.
func decodeNodes(t *testing.T, stdout string) map[string]map[string]any {
	t.Helper()

	var envelope struct {
		Items []map[string]any `json:"items"`
	}

	if err := json.Unmarshal([]byte(stdout), &envelope); err != nil {
		t.Fatalf("decoding kubectl nv list output: %v\n%s", err, stdout)
	}

	out := make(map[string]map[string]any, len(envelope.Items))

	for _, item := range envelope.Items {
		meta, _ := item["metadata"].(map[string]any)
		name, _ := meta["name"].(string)

		if name == "" {
			t.Fatalf("a listed node has no name: %v", item)
		}

		out[name] = item
	}

	return out
}

// normaliseNode drops what a bundle cannot know, so that what is left has to
// be equal.
func normaliseNode(node map[string]any) map[string]any {
	out := make(map[string]any, len(node))
	for k, v := range node {
		out[k] = v
	}

	delete(out, "requested")
	delete(out, "requestedKnown")

	operands, _ := out["operands"].([]any)
	trimmed := make([]any, 0, len(operands))

	for _, raw := range operands {
		operand, ok := raw.(map[string]any)
		if !ok {
			continue
		}

		copied := make(map[string]any, len(operand))
		for k, v := range operand {
			copied[k] = v
		}

		delete(copied, "phase")
		delete(copied, "ready")
		delete(copied, "restarts")
		trimmed = append(trimmed, copied)
	}

	out["operands"] = trimmed

	return out
}

// isControlPlane reports whether the named node carries the control plane role.
func isControlPlane(t *testing.T, name string) bool {
	t.Helper()

	for _, node := range clusterNodes(t) {
		if node.Name != name {
			continue
		}

		_, found := node.Labels[controlPlaneRoleLabel]

		return found
	}

	return false
}

// splitLines returns the non-empty lines of some output.
func splitLines(s string) []string {
	out := make([]string, 0, 8)

	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}

	return out
}

// sortedKeys names what a decoded list holds, sorted.
func listedNames(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}

	sort.Strings(out)

	return out
}
