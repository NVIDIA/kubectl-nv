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

package rules_test

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// imexNode is a DRA node carrying the IMEX daemon pod of a ComputeDomain.
func imexNode(name, clique string, daemonReady bool, withDaemon bool) inventory.GPUNode {
	opts := []nodeOption{
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withEveryOperandDeployed(),
		withCount(4),
		withMode(inventory.ModeDRA),
		withResourceSlices(name + "-gpu.nvidia.com"),
		withOperand(inventory.ComponentDRADriver, true),
		withClique(clique),
	}

	if withDaemon {
		opts = append(opts, withOperand(inventory.ComponentIMEXDaemon, daemonReady))
	}

	return buildNode(name, opts...)
}

func cdInput(node inventory.GPUNode, domains ...unstructured.Unstructured) health.Input {
	return health.Input{
		Nodes:             []inventory.GPUNode{node},
		ComputeDomains:    domains,
		OperatorInstalled: true,
	}
}

// The ComputeDomain's own status is what says whether the IMEX daemon on a
// node is ready: status.nodes[].status, Ready or NotReady
// (api/nvidia.com/resource/v1beta1/computedomain.go, k8s-dra-driver-gpu
// fb8b9674).
func TestComputeDomainNotReadyReportsANotReadyNode(t *testing.T) {
	t.Parallel()

	rule := rules.NewComputeDomainNotReady()
	domain := computeDomain("workload", "cd-a", "NotReady", cdNode("worker-1", "uuid.1", "NotReady"))

	finding := only(t, evaluate(rule, cdInput(imexNode("worker-1", "uuid.1", true, true), domain)))

	checkFinding(t, rule, finding, inventory.ComponentIMEXDaemon,
		"the IMEX daemon of a ComputeDomain covering the node is not ready",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityError {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityError)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"computeDomain":       "workload/cd-a",
		"computeDomainStatus": "NotReady",
		"nodeStatus":          "NotReady",
		"cliqueID":            "uuid.1",
		"pod":                 "compute-domain-daemon-abcde",
		"ready":               "true",
	})
}

// The daemon pod on the node is the second half of the evidence: a
// ComputeDomain whose status has not caught up still has a pod that is not
// reporting ready, and that is the same fault.
func TestComputeDomainNotReadyReportsAnUnreadyDaemonPod(t *testing.T) {
	t.Parallel()

	rule := rules.NewComputeDomainNotReady()
	domain := computeDomain("workload", "cd-a", "Ready", cdNode("worker-1", "uuid.1", "Ready"))

	finding := only(t, evaluate(rule, cdInput(imexNode("worker-1", "uuid.1", false, true), domain)))

	if got := finding.Evidence["ready"]; got != "false" {
		t.Errorf("evidence ready is %q, want \"false\"", got)
	}

	if got := finding.Evidence["nodeStatus"]; got != "Ready" {
		t.Errorf("evidence nodeStatus is %q, want \"Ready\": the ComputeDomain itself is content", got)
	}
}

// A ready domain on a ready node with a ready daemon pod is the whole point of
// the thing working.
func TestComputeDomainNotReadyStaysQuietOnAHealthyDomain(t *testing.T) {
	t.Parallel()

	domain := computeDomain("workload", "cd-a", "Ready", cdNode("worker-1", "uuid.1", "Ready"))

	if findings := evaluate(rules.NewComputeDomainNotReady(), cdInput(imexNode("worker-1", "uuid.1", true, true), domain)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A node without nvidia.com/gpu.clique cannot be placed in an IMEX domain,
// which is a different fault from the daemon not being ready and reads as a
// node problem rather than a daemon problem.
func TestComputeDomainNotReadyReportsAMissingClique(t *testing.T) {
	t.Parallel()

	rule := rules.NewComputeDomainNotReady()
	domain := computeDomain("workload", "cd-a", "Ready", cdNode("worker-1", "", "Ready"))

	finding := only(t, evaluate(rule, cdInput(imexNode("worker-1", "", true, true), domain)))

	checkFinding(t, rule, finding, rules.ComponentNode,
		"the node is in a ComputeDomain and carries no nvidia.com/gpu.clique",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityWarning {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityWarning)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"computeDomains": "workload/cd-a",
	})
}

// A node the ComputeDomain does not name is a node outside it. Reporting one
// would make every GPU node in the cluster a member of every domain.
func TestComputeDomainNotReadyIgnoresANodeOutsideTheDomain(t *testing.T) {
	t.Parallel()

	domain := computeDomain("workload", "cd-a", "NotReady", cdNode("worker-2", "uuid.1", "NotReady"))

	if findings := evaluate(rules.NewComputeDomainNotReady(), cdInput(imexNode("worker-1", "uuid.1", true, true), domain)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A ComputeDomain naming a node this run never saw says nothing about the
// inventory, and inventing a finding for a node nobody can look up gives a
// reader nothing to run next.
func TestComputeDomainNotReadyIgnoresANodeOutsideTheInventory(t *testing.T) {
	t.Parallel()

	domain := computeDomain("workload", "cd-a", "NotReady", cdNode("ghost", "uuid.1", "NotReady"))

	if findings := evaluate(rules.NewComputeDomainNotReady(), cdInput(imexNode("worker-1", "uuid.1", true, true), domain)); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A cluster with no ComputeDomains, which is every cluster not running a
// multi-node IMEX workload, gets nothing at all.
func TestComputeDomainNotReadyStaysQuietWithoutAnyDomain(t *testing.T) {
	t.Parallel()

	if findings := evaluate(rules.NewComputeDomainNotReady(), healthyLane()); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A node covered by two domains gets one clique finding, not two: the missing
// label is a fact about the node, and both domains are named in the evidence.
func TestComputeDomainNotReadyNamesEveryDomainOnOneCliqueFinding(t *testing.T) {
	t.Parallel()

	in := cdInput(imexNode("worker-1", "", true, true),
		computeDomain("workload", "cd-b", "Ready", cdNode("worker-1", "", "Ready")),
		computeDomain("other", "cd-a", "Ready", cdNode("worker-1", "", "Ready")))

	finding := only(t, evaluate(rules.NewComputeDomainNotReady(), in))

	if got := finding.Evidence["computeDomains"]; got != "other/cd-a,workload/cd-b" {
		t.Errorf("computeDomains is %q, want the sorted, comma joined pair", got)
	}
}

// A covered node with no IMEX daemon pod yet is still covered: the domain's
// own node status is the authority, and the pod keys are left out.
func TestComputeDomainNotReadyReportsANodeWithNoDaemonPod(t *testing.T) {
	t.Parallel()

	domain := computeDomain("workload", "cd-a", "NotReady", cdNode("worker-1", "uuid.1", "NotReady"))

	finding := only(t, evaluate(rules.NewComputeDomainNotReady(), cdInput(imexNode("worker-1", "uuid.1", false, false), domain)))

	if _, present := finding.Evidence["pod"]; present {
		t.Errorf("there is no daemon pod, so the key is left out: %v", finding.Evidence)
	}
}

func TestComputeDomainNotReadySkipsHintedNodes(t *testing.T) {
	t.Parallel()

	node := mokkaControlPlane()
	domain := computeDomain("workload", "cd-a", "NotReady", cdNode(node.Name, "uuid.1", "NotReady"))

	in := health.Input{
		Hinted:            []inventory.GPUNode{node},
		ComputeDomains:    []unstructured.Unstructured{domain},
		OperatorInstalled: true,
	}

	if findings := evaluate(rules.NewComputeDomainNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The ComputeDomain CRD is served by the DRA driver the GPU Operator ships
// from v25.3.0, so the rule declares that floor. An unknown version still
// runs it, which is what VersionInRange answers for an empty string.
func TestComputeDomainNotReadyIsGatedAtTheDRADriverRelease(t *testing.T) {
	t.Parallel()

	rule := rules.NewComputeDomainNotReady()

	if rule.Applies("v24.9.0") {
		t.Error("the rule must not apply below v25.3.0, where no ComputeDomain CRD is served")
	}

	if !rule.Applies("v25.3.0") || !rule.Applies("v25.10.0") || !rule.Applies("") {
		t.Error("the rule has to apply from v25.3.0 upwards and to an unknown version")
	}
}
