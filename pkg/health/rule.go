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

// Package health holds the rule contract behind gpu health: what a rule may
// read, what it returns, and the registry that collects them.
//
// A rule is a pure function of an Input. It reads no cluster, no clock and no
// file: everything it is allowed to know is a field of Input, the clock
// included. That is what makes a rule testable from a table of inputs rather
// than from a cluster, and it is why the reading of the cluster lives entirely
// outside this package.
//
// A rule never decides the exit code. It returns findings with a severity, and
// pkg/output's ExitCode maps those onto the process exit code against the
// --fail-on threshold.
package health

import (
	"fmt"
	"regexp"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// CodePattern is the form every rule code takes: NV-<AREA>-<NNN>, where AREA
// names the part of the stack the rule is about. The areas are closed, because
// a code is what a user suppresses and what a script greps for: a rule that
// invented its own prefix would be invisible to both.
//
//	NODE  the node itself       OP    the GPU operator
//	DRV   the driver            CTK   the container toolkit
//	DP    the device plugin     DRA   dynamic resource allocation
//	MIG   multi-instance GPU    DCGM  DCGM and the exporter
//	CD    cluster policy and custom resources
//	UPG   driver upgrades       TOPO  fabric topology
const CodePattern = `^NV-(NODE|OP|DRV|CTK|DP|DRA|MIG|DCGM|CD|UPG|TOPO)-[0-9]{3}$`

// codeRE is CodePattern compiled once. It is a package level value rather than
// a field so that the two cannot drift apart.
var codeRE = regexp.MustCompile(CodePattern)

// Input is everything a rule may read. Rules are pure functions of it.
type Input struct {
	// Nodes are the GPU nodes: those with nvidia.com/gpu capacity or a
	// gpu.nvidia.com ResourceSlice.
	Nodes []inventory.GPUNode
	// Hinted are the label-only nodes, each with Hinted set. A node that
	// carries a GPU hint label and advertises no GPUs is the most common
	// thing a user runs gpu health to find, so it is a set of its own rather
	// than a flag to filter Nodes by.
	Hinted []inventory.GPUNode
	// ClusterPolicies are the operator's ClusterPolicy objects, unstructured
	// because the CRD is not vendored and its schema moves between operator
	// versions.
	ClusterPolicies []unstructured.Unstructured
	// OperatorVersion is the GPU Operator version, empty when unknown. A rule
	// gates on it through Applies rather than reading it here, so that the
	// gate is visible without running the rule.
	OperatorVersion string
	// OperatorInstalled is whether the operator was found at all. It is
	// separate from OperatorVersion because "installed, version unreadable"
	// and "not installed" call for different findings.
	OperatorInstalled bool
	// Events are cluster events, possibly empty. They are what upgrade timing
	// is read from: a node stuck mid-upgrade looks fine in its labels and
	// tells the truth in its events.
	Events []corev1.Event
	// Now is the injected clock. A rule that called time.Now would be a rule
	// whose findings change between two runs over the same input.
	Now time.Time
}

// Rule is one check. Implementations hold no state: Evaluate is called with
// everything it may read and returns everything it concluded.
type Rule interface {
	// Code is the stable identifier, matching CodePattern.
	Code() string
	// Severity is the default severity of the findings this rule emits. A
	// rule may emit a finding at another severity when the evidence calls for
	// it; this is what the rule is listed as.
	Severity() output.Severity
	// Applies reports whether the rule means anything against this GPU
	// Operator version. A rule about a field that only exists from v25.3.0
	// has to say so, or it reports a false finding on every older cluster.
	// The empty version means unknown, and a rule decides for itself whether
	// to run then.
	Applies(operatorVersion string) bool
	// Evaluate returns the findings, or nothing. It must not mutate in.
	Evaluate(in Input) []output.Finding
}

// Registry collects rules under their codes. It is safe for concurrent use:
// rules register from their own files during package initialisation, and the
// order those run in is not something a rule file can see.
type Registry struct {
	mu     sync.RWMutex
	byCode map[string]Rule
}

// NewRegistry returns an empty registry. Tests build their own rather than
// reaching for the package level one, so that registering a rule in one test
// cannot change what another test sees.
func NewRegistry() *Registry {
	return &Registry{byCode: map[string]Rule{}}
}

// Register adds a rule. It panics on a code that does not match CodePattern
// and on a code that is already registered.
//
// Panicking is the point. Both mistakes are made once, at startup, by a
// developer adding a rule, and both are silent at runtime: a duplicate code
// means one of the two rules never runs and the cluster is still reported
// clean, and a malformed code means a finding nobody can suppress or grep for.
// A run that reports clean because a rule was shadowed is worse than a run
// that refused to start.
func (r *Registry) Register(rule Rule) {
	code := rule.Code()
	if !codeRE.MatchString(code) {
		panic(fmt.Sprintf("health: rule code %q does not match %s", code, CodePattern))
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byCode[code]; exists {
		panic(fmt.Sprintf("health: rule code %q is registered twice", code))
	}

	r.byCode[code] = rule
}

// Rules returns the registered rules sorted by code. The slice is freshly
// built, so a caller may sort or truncate it without reordering the registry
// for everyone else. Sorting by code is what makes two runs over the same
// cluster produce findings in the same order.
func (r *Registry) Rules() []Rule {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Rule, 0, len(r.byCode))
	for _, rule := range r.byCode {
		out = append(out, rule)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Code() < out[j].Code() })

	return out
}

// defaultRegistry is the registry the package level Register and Rules reach.
// Rule files across the package register into it from their own files, which
// is the one thing a per-call registry cannot do.
var defaultRegistry = NewRegistry()

// Register adds a rule to the default registry, panicking on a malformed or
// duplicate code. See Registry.Register.
func Register(rule Rule) { defaultRegistry.Register(rule) }

// Rules returns the default registry's rules, sorted by code.
func Rules() []Rule { return defaultRegistry.Rules() }
