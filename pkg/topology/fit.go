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

package topology

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
)

// LevelFit is the answer at one level: the groups of that level where every
// pod of the job fits.
type LevelFit struct {
	// Level is the level name, as Level.Name prints it.
	Level string `json:"level" yaml:"level"`
	// Label is the node label the level reads.
	Label string `json:"label" yaml:"label"`
	// Groups are the values of the groups the job fits in, sorted. It is
	// empty when it fits in none of them.
	Groups []string `json:"groups" yaml:"groups"`
}

// levelFitJSON breaks the method set so that MarshalJSON does not recurse.
type levelFitJSON LevelFit

// MarshalJSON renders a nil group list as an empty array.
func (f LevelFit) MarshalJSON() ([]byte, error) {
	out := levelFitJSON(f)
	if out.Groups == nil {
		out.Groups = []string{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling the fit at level %q: %w", f.Level, err)
	}

	return raw, nil
}

// JobFit is the whole answer to --for job/<name>: what was asked, where it
// fits at every level, and the narrowest level that takes it.
type JobFit struct {
	// Namespace and Name are the job the question was about.
	Namespace string `json:"namespace" yaml:"namespace"`
	Name      string `json:"name" yaml:"name"`
	// Pods is how many pods it runs at once: spec.parallelism, or 1.
	Pods int64 `json:"pods" yaml:"pods"`
	// GPUsPerPod is what one pod asks for, by the effective request rule.
	GPUsPerPod int64 `json:"gpusPerPod" yaml:"gpusPerPod"`
	// Levels is one entry per level of the report, narrowest first.
	Levels []LevelFit `json:"levels" yaml:"levels"`
	// Smallest is the name of the narrowest level with a group that fits, and
	// is empty when the job fits nowhere.
	Smallest string `json:"smallest" yaml:"smallest"`
}

// jobFitJSON breaks the method set so that MarshalJSON does not recurse.
type jobFitJSON JobFit

// MarshalJSON renders a nil level list as an empty array.
func (f JobFit) MarshalJSON() ([]byte, error) {
	out := jobFitJSON(f)
	if out.Levels == nil {
		out.Levels = []LevelFit{}
	}

	raw, err := json.Marshal(out)
	if err != nil {
		return nil, fmt.Errorf("marshalling the answer about job %s/%s: %w", f.Namespace, f.Name, err)
	}

	return raw, nil
}

// JobRequest is the question --for asks: this many pods, each wanting this
// many GPUs.
type JobRequest struct {
	// Namespace and Name are the job it came from.
	Namespace string
	Name      string
	// Pods is how many pods run at once.
	Pods int64
	// GPUsPerPod is the effective nvidia.com/gpu request of one pod.
	GPUsPerPod int64
}

// FitJob answers the request against every level of the report, narrowest
// first, and names the narrowest level a group of which takes the whole job.
//
// nodes is the same slice the report was built from; it is taken again rather
// than read off the groups because the answer turns on the free GPUs of each
// node separately, which a group total cannot express.
func (r *TopologyReport) FitJob(req JobRequest, nodes []inventory.GPUNode) *JobFit {
	free := freeByNode(nodes)

	fit := &JobFit{
		Namespace:  req.Namespace,
		Name:       req.Name,
		Pods:       req.Pods,
		GPUsPerPod: req.GPUsPerPod,
		Levels:     make([]LevelFit, 0, len(r.Levels)),
	}

	for _, level := range r.Levels {
		at := LevelFit{Level: level.Name, Label: level.Label, Groups: []string{}}
		for _, group := range level.Groups {
			if !group.RequestedKnown {
				// Nothing is known about what runs on these nodes, so
				// nothing can be promised about what would fit beside it.
				continue
			}
			if fits(freeOfGroup(group, free), req.Pods, req.GPUsPerPod) {
				at.Groups = append(at.Groups, group.Value)
			}
		}
		sort.Strings(at.Groups)

		if fit.Smallest == "" && len(at.Groups) > 0 {
			fit.Smallest = level.Name
		}
		fit.Levels = append(fit.Levels, at)
	}

	return fit
}

// freeByNode is the free GPUs of every node, by name, clamped at zero.
func freeByNode(nodes []inventory.GPUNode) map[string]int64 {
	out := make(map[string]int64, len(nodes))
	for i := range nodes {
		out[nodes[i].Name] = freeOn(&nodes[i])
	}

	return out
}

// freeOfGroup is the free GPUs of a group, one entry per node.
func freeOfGroup(g Group, free map[string]int64) []int64 {
	out := make([]int64, 0, len(g.Nodes))
	for _, name := range g.Nodes {
		out = append(out, free[name])
	}

	return out
}

// fits reports whether pods of gpusPerPod GPUs each can be placed on nodes
// with these free GPUs.
//
// A pod runs on one node, so what a node can take is its free GPUs divided by
// the pod size and rounded down, and the question is whether those capacities
// add up to the pod count. The sum of the free GPUs is not the same number
// and is the wrong one: three nodes with three free GPUs each hold three pods
// of two, not the four that nine free GPUs would suggest.
//
// Equal pods make this exact rather than a heuristic. Every pod is the same
// size, so no order of placement does better than filling each node as far as
// it goes, and a greedy pass is therefore an answer and not a guess.
func fits(free []int64, pods, gpusPerPod int64) bool {
	if pods <= 0 || gpusPerPod <= 0 {
		return false
	}

	// Largest first: it changes nothing about the total, and it makes the
	// early exit below land on the first nodes rather than the last.
	sorted := append([]int64(nil), free...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] > sorted[j] })

	placed := int64(0)
	for _, available := range sorted {
		placed += available / gpusPerPod
		if placed >= pods {
			return true
		}
	}

	return false
}
