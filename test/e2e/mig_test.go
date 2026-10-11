//go:build e2e && mig

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
	"fmt"
	"sort"
	"strconv"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
)

// This file is behind "e2e && mig" and not "e2e" alone, so the whole-GPU lane
// never compiles it. The two lanes are not interchangeable: this one disables
// the GPU Operator's device plugin, which the must-gather test asserts is
// present, and that one installs a chart that cannot partition a board.
//
// "make e2e-mig" runs this file and only this file. It does so with
// -run '^TestMIGLane$', which is why every assertion below is a subtest of one
// parent: a second top-level test here would silently never run.

const (
	// migCommandGroup is the subcommand the read-only commands live under.
	migCommandGroup = "gpu"

	// migExpectedGPUs is what a worker in this lane advertises: two H100
	// boards, each carved into seven 1g.10gb partitions, every partition
	// published as one nvidia.com/gpu because the device plugin runs with
	// --mig-strategy=single.
	//
	// Written out rather than read from the lane scripts. A test that takes
	// its expected value from the thing under test agrees with it by
	// construction and catches nothing; hack/mokka/mig/up_test.sh holds
	// hack/mokka/mig/lib.sh to the same number from the other side.
	migExpectedGPUs = 14

	// migExpectedStrategy is the MIG strategy the lane runs under, and so the
	// value of the nvidia.com/mig.strategy label GFD publishes.
	migExpectedStrategy = "single"

	// migExpectedWorkers is the worker count in hack/mokka/mig/kind-mig.yaml.
	// Two, because the per-worker assertions exist to catch a lane where only
	// one node got partitioned and a single-worker cluster cannot catch that.
	migExpectedWorkers = 2

	// What GFD published on the lane run of 2026-10-11, written out as the
	// literals they were observed to be rather than as "non-empty".
	//
	// These three are what a partitioned board CHANGES, and that is why they
	// are pinned. Under the single strategy GFD renames the product after the
	// slice, counts partitions rather than boards, and reports the slice's
	// memory rather than the board's: an unpartitioned H100 would publish
	// NVIDIA-H100-80GB-HBM3, a count of 2 and 81559 MiB. A test that only
	// asked for a non-empty product would pass on that node, which is the one
	// node this lane exists to tell apart from its own.
	migExpectedProduct = "NVIDIA-H100-80GB-HBM3-MIG-1g.10gb"
	migExpectedCount   = 14
	migExpectedMemory  = "10240"
)

// The GFD labels this lane reads. They are the node's, not the binary's: every
// field the commands report below is compared against the label it was built
// from, read straight from the API server, as well as against a literal.
const (
	labelMIGStrategy = "nvidia.com/mig.strategy"
	labelMIGCapable  = "nvidia.com/mig.capable"
	labelGPUProduct  = "nvidia.com/gpu.product"
	labelGPUCount    = "nvidia.com/gpu.count"
	labelGPUMemory   = "nvidia.com/gpu.memory"
)

// resourceGPU is the extended resource a partition is published as under the
// single strategy.
const resourceGPU = "nvidia.com/gpu"

// migResourceCount mirrors inventory.ResourceCount on the wire.
type migResourceCount struct {
	Capacity    int64 `json:"capacity"`
	Allocatable int64 `json:"allocatable"`
}

// migConfig mirrors inventory.MIG on the wire.
type migConfig struct {
	Strategy    string                      `json:"strategy"`
	Config      string                      `json:"config"`
	ConfigState string                      `json:"configState"`
	Capable     bool                        `json:"capable"`
	Resources   map[string]migResourceCount `json:"resources"`
}

// migNodeMeta is the part of the node's metadata the commands report.
type migNodeMeta struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
}

// migNode mirrors inventory.GPUNode on the wire, with only the fields this
// file is about. Decoding into a struct of this repository's own types would
// couple a black box test to the packages the binary is built from; these tags
// are the contract, and they are the ones in pkg/inventory/types.go.
type migNode struct {
	Kind        string      `json:"kind"`
	Metadata    migNodeMeta `json:"metadata"`
	Product     string      `json:"product"`
	Count       int         `json:"count"`
	Capacity    int64       `json:"capacity"`
	Allocatable int64       `json:"allocatable"`
	MIG         migConfig   `json:"mig"`
}

// migNodeList mirrors inventory.GPUNodeList on the wire.
type migNodeList struct {
	APIVersion string    `json:"apiVersion"`
	Kind       string    `json:"kind"`
	Items      []migNode `json:"items"`
}

func TestMIGLane(t *testing.T) {
	workers := workerNodeNames(t)
	if len(workers) != migExpectedWorkers {
		t.Fatalf("the lane has %d worker(s) (%v), want %d; this is not a MIG lane",
			len(workers), workers, migExpectedWorkers)
	}
	nodesByName := migNodesByName(t)

	// Before anything is asked of the binary: the cluster itself has to be
	// what this file claims it is. Without this, every assertion below could
	// be satisfied by a binary that reports fourteen partitions on a lane that
	// has none, and the suite would be green against a cluster that never came
	// up partitioned.
	t.Run("the lane really is partitioned", func(t *testing.T) {
		for _, name := range workers {
			node := nodesByName[name]

			allocatable := node.Status.Allocatable[v1.ResourceName(resourceGPU)]
			if got := allocatable.Value(); got != migExpectedGPUs {
				t.Errorf("%s: allocatable %s is %d, want %d",
					name, resourceGPU, got, migExpectedGPUs)
			}
			capacity := node.Status.Capacity[v1.ResourceName(resourceGPU)]
			if got := capacity.Value(); got != migExpectedGPUs {
				t.Errorf("%s: capacity %s is %d, want %d",
					name, resourceGPU, got, migExpectedGPUs)
			}
			if got := node.Labels[labelMIGStrategy]; got != migExpectedStrategy {
				t.Errorf("%s: label %s is %q, want %q",
					name, labelMIGStrategy, got, migExpectedStrategy)
			}
			if got := node.Labels[labelMIGCapable]; got != "true" {
				t.Errorf("%s: label %s is %q, want \"true\"", name, labelMIGCapable, got)
			}
			// The three GFD labels a partitioned board changes, against the
			// values the lane was observed to publish.
			for _, pinned := range []struct {
				label string
				want  string
			}{
				{labelGPUProduct, migExpectedProduct},
				{labelGPUCount, strconv.Itoa(migExpectedCount)},
				{labelGPUMemory, migExpectedMemory},
			} {
				if got := node.Labels[pinned.label]; got != pinned.want {
					t.Errorf("%s: label %s is %q, want %q",
						name, pinned.label, got, pinned.want)
				}
			}
		}
	})

	t.Run("gpu list reports every worker's partitions", func(t *testing.T) {
		res := run(t, migCommandGroup, "list", "-o", "json")
		if res.code != 0 {
			t.Fatalf("%s list exited %d, want 0\nstderr:\n%s",
				migCommandGroup, res.code, res.stderr)
		}

		var list migNodeList
		if err := json.Unmarshal([]byte(res.stdout), &list); err != nil {
			t.Fatalf("%s list -o json does not parse: %v\nstdout:\n%s",
				migCommandGroup, err, res.stdout)
		}
		if list.Kind != "GPUNodeList" {
			t.Errorf("kind is %q, want \"GPUNodeList\"", list.Kind)
		}

		listed := make(map[string]migNode, len(list.Items))
		for _, item := range list.Items {
			listed[item.Metadata.Name] = item
		}
		for _, name := range workers {
			item, ok := listed[name]
			if !ok {
				t.Errorf("%s list does not list worker %s, it lists %v",
					migCommandGroup, name, sortedNames(listed))
				continue
			}

			if item.Allocatable != migExpectedGPUs {
				t.Errorf("%s: list reports allocatable %d, want %d",
					name, item.Allocatable, migExpectedGPUs)
			}
			if item.Capacity != migExpectedGPUs {
				t.Errorf("%s: list reports capacity %d, want %d",
					name, item.Capacity, migExpectedGPUs)
			}
			// Against the node as well as against the literal: a binary that
			// reported a hardcoded 14 would pass the line above.
			node := nodesByName[name]
			nodeAllocatable := node.Status.Allocatable[v1.ResourceName(resourceGPU)]
			if item.Allocatable != nodeAllocatable.Value() {
				t.Errorf("%s: list reports allocatable %d, the node advertises %d",
					name, item.Allocatable, nodeAllocatable.Value())
			}

			if item.MIG.Strategy != migExpectedStrategy {
				t.Errorf("%s: list reports MIG strategy %q, want %q",
					name, item.MIG.Strategy, migExpectedStrategy)
			}
			if item.MIG.Strategy != node.Labels[labelMIGStrategy] {
				t.Errorf("%s: list reports MIG strategy %q, the node is labelled %q",
					name, item.MIG.Strategy, node.Labels[labelMIGStrategy])
			}

			// Under single every partition is published as nvidia.com/gpu, so
			// there is no per-profile resource to report. A non-empty map here
			// means the node is being read as if it were mixed, which is the
			// shape the counts above would then be wrong in.
			if len(item.MIG.Resources) != 0 {
				t.Errorf("%s: list reports %d per-profile MIG resource(s) (%v) under the %s strategy, want none",
					name, len(item.MIG.Resources), sortedKeysOf(item.MIG.Resources), migExpectedStrategy)
			}
		}
	})

	t.Run("gpu info carries the MIG fields", func(t *testing.T) {
		for _, name := range workers {
			node := nodesByName[name]

			res := run(t, migCommandGroup, "info", name, "-o", "json")
			if res.code != 0 {
				t.Errorf("%s info %s exited %d, want 0\nstderr:\n%s",
					migCommandGroup, name, res.code, res.stderr)
				continue
			}

			var got migNode
			if err := json.Unmarshal([]byte(res.stdout), &got); err != nil {
				t.Errorf("%s info %s -o json does not parse: %v\nstdout:\n%s",
					migCommandGroup, name, err, res.stdout)
				continue
			}

			if got.Kind != "GPUNode" {
				t.Errorf("%s: kind is %q, want \"GPUNode\"", name, got.Kind)
			}
			if got.Metadata.Name != name {
				t.Errorf("%s: info reports the node as %q", name, got.Metadata.Name)
			}

			if got.MIG.Strategy != migExpectedStrategy {
				t.Errorf("%s: info reports MIG strategy %q, want %q",
					name, got.MIG.Strategy, migExpectedStrategy)
			}
			if !got.MIG.Capable {
				t.Errorf("%s: info reports the node as not MIG capable, the node is labelled %s=%q",
					name, labelMIGCapable, node.Labels[labelMIGCapable])
			}
			if got.MIG.Capable != (node.Labels[labelMIGCapable] == "true") {
				t.Errorf("%s: info reports MIG capable %t, the node is labelled %s=%q",
					name, got.MIG.Capable, labelMIGCapable, node.Labels[labelMIGCapable])
			}

			// The GFD labels, each against the label it is built from. These
			// are what a partitioned board changes: under single, GFD renames
			// the product after the slice and counts partitions rather than
			// boards, so reading them off a whole-GPU node would give the
			// wrong answer here and the comparison would catch it.
			if got.Product != migExpectedProduct {
				t.Errorf("%s: info reports product %q, want %q",
					name, got.Product, migExpectedProduct)
			}
			if got.Product != node.Labels[labelGPUProduct] {
				t.Errorf("%s: info reports product %q, the node is labelled %s=%q",
					name, got.Product, labelGPUProduct, node.Labels[labelGPUProduct])
			}
			if got.Count != migExpectedCount {
				t.Errorf("%s: info reports count %d, want %d",
					name, got.Count, migExpectedCount)
			}
			wantCount, err := strconv.Atoi(node.Labels[labelGPUCount])
			if err != nil {
				t.Errorf("%s: label %s is %q, which is not a number: %v",
					name, labelGPUCount, node.Labels[labelGPUCount], err)
			} else if got.Count != wantCount {
				t.Errorf("%s: info reports count %d, the node is labelled %s=%q",
					name, got.Count, labelGPUCount, node.Labels[labelGPUCount])
			}

			// The memory label has no field of its own on a GPU node, so it
			// is checked where info does carry it: the node's own labels,
			// which info reports under metadata.
			if got := got.Metadata.Labels[labelGPUMemory]; got != migExpectedMemory {
				t.Errorf("%s: info reports metadata label %s=%q, want %q",
					name, labelGPUMemory, got, migExpectedMemory)
			}

			if got.Allocatable != migExpectedGPUs {
				t.Errorf("%s: info reports allocatable %d, want %d",
					name, got.Allocatable, migExpectedGPUs)
			}
			if len(got.MIG.Resources) != 0 {
				t.Errorf("%s: info reports %d per-profile MIG resource(s) (%v) under the %s strategy, want none",
					name, len(got.MIG.Resources), sortedKeysOf(got.MIG.Resources), migExpectedStrategy)
			}

			// Config and ConfigState are the mig-manager's fields: what
			// partitioning it was asked to apply and how far it got. This
			// lane has no mig-manager, because the layout is decided at
			// install time by the chart and the driver capability surface is
			// staged once. Both must therefore be empty. A non-empty
			// ConfigState here would mean the reader is inventing a value
			// for a label the node does not carry, which is how a
			// mig-manager rule ends up firing on a node that has none.
			if got.MIG.Config != "" {
				t.Errorf("%s: info reports MIG config %q, want \"\": this lane runs no mig-manager",
					name, got.MIG.Config)
			}
			if got.MIG.ConfigState != "" {
				t.Errorf("%s: info reports MIG config state %q, want \"\": this lane runs no mig-manager",
					name, got.MIG.ConfigState)
			}
		}
	})

	// The healthy-partitioned-node gate. A correctly partitioned lane is the
	// one shape a MIG rule must NOT fire on: NV-MIG-001 is about a node whose
	// partitioning did not take, NV-NODE-002 about a node that advertises GPU
	// hints and no GPUs, NV-NODE-004 about an operand that is not running. All
	// three are false here, so a finding at warning or above means a rule is
	// firing on a healthy node.
	t.Run("gpu health finds nothing at warning or above", func(t *testing.T) {
		res := run(t, migCommandGroup, "health", "--fail-on", "warning", "-o", "json")

		// Checked before the report is walked: a command that printed no
		// report at all has an exit code and a stderr that say why, and
		// "unexpected end of JSON input" does not.
		if !json.Valid([]byte(res.stdout)) {
			t.Fatalf("%s health --fail-on warning exited %d and did not print a JSON report\nstdout:\n%s\nstderr:\n%s",
				migCommandGroup, res.code, res.stdout, res.stderr)
		}

		raised := findingsAtOrAbove(t, res.stdout, "warning")
		if len(raised) != 0 {
			t.Errorf("%s health raised %d finding(s) at warning or above on a healthy partitioned lane:\n  %s",
				migCommandGroup, len(raised), strings.Join(raised, "\n  "))
		}

		// Checked after the findings, so a failure reports which rule fired
		// rather than only that the exit code was not zero.
		if res.code != 0 {
			t.Errorf("%s health --fail-on warning exited %d, want 0\nstdout:\n%s\nstderr:\n%s",
				migCommandGroup, res.code, res.stdout, res.stderr)
		}
	})
}

// migNodesByName indexes the cluster's nodes, so that what the binary reports
// can be compared against the node it was built from rather than against
// itself.
func migNodesByName(t *testing.T) map[string]v1.Node {
	t.Helper()

	byName := make(map[string]v1.Node)
	for _, node := range clusterNodes(t) {
		byName[node.Name] = node
	}

	return byName
}

// findingsAtOrAbove returns a readable line per finding in the report whose
// severity is at or above the threshold.
//
// The report is walked rather than decoded into a struct. The HealthReport
// envelope is Track E's and is still settling; what is fixed is the shape of a
// finding, which carries both a "code" and a "severity". Walking for that pair
// finds every finding wherever the envelope puts it, and a finding that moves
// to a different field keeps being counted instead of silently vanishing from
// a decode that no longer matches.
func findingsAtOrAbove(t *testing.T, stdout, threshold string) []string {
	t.Helper()

	var doc any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("health -o json does not parse: %v\nstdout:\n%s", err, stdout)
	}

	// info < warning < error, as pkg/output ranks them.
	rank := map[string]int{"info": 0, "warning": 1, "error": 2}
	want, ok := rank[threshold]
	if !ok {
		t.Fatalf("findingsAtOrAbove: %q is not a severity", threshold)
	}

	var raised []string
	var walk func(any)
	walk = func(node any) {
		switch value := node.(type) {
		case map[string]any:
			code, hasCode := value["code"].(string)
			severity, hasSeverity := value["severity"].(string)
			if hasCode && hasSeverity {
				got, known := rank[severity]
				if !known {
					t.Errorf("finding %s carries severity %q, which is not one of info, warning, error",
						code, severity)
				} else if got >= want {
					message, _ := value["message"].(string)
					node, _ := value["node"].(string)
					raised = append(raised, fmt.Sprintf("%s (%s) on %q: %s",
						code, severity, node, message))
				}
				// A finding has no findings inside it; no need to descend.
				return
			}
			for _, child := range value {
				walk(child)
			}
		case []any:
			for _, child := range value {
				walk(child)
			}
		}
	}
	walk(doc)
	sort.Strings(raised)

	return raised
}

// sortedNames lists the keys of a node map, for a failure message.
func sortedNames(nodes map[string]migNode) []string {
	names := make([]string, 0, len(nodes))
	for name := range nodes {
		names = append(names, name)
	}
	sort.Strings(names)

	return names
}

// sortedKeysOf lists the keys of a MIG resource map, for a failure message.
func sortedKeysOf(resources map[string]migResourceCount) []string {
	keys := make([]string, 0, len(resources))
	for key := range resources {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}
