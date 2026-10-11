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

package rules

import (
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// computeDomainReady is the one status value that means an IMEX daemon is
// brokering GPU memory exchanges. The CRD enumerates Ready and NotReady and
// defaults to NotReady, so anything else is a domain this code cannot read
// (api/nvidia.com/resource/v1beta1/computedomain.go, k8s-dra-driver-gpu
// fb8b9674).
const computeDomainReady = "Ready"

// draDriverMinVersion is the first GPU Operator release that ships the NVIDIA
// DRA driver, and so the first where a cluster can serve a ComputeDomain at
// all.
const draDriverMinVersion = "v25.3.0"

// ComputeDomainNotReady is NV-CD-001: a ComputeDomain covers the node and the
// IMEX daemon there is not ready, or the node carries no clique.
//
// A ComputeDomain is what a multi-node NVLink workload asks for, and it is the
// one object whose failure mode is silent: the pods stay in ContainerCreating
// and nothing in the namespace says why. One of two things is wrong when they
// do.
//
// The daemon is not ready. The ComputeDomain's own status is the authority
// here: status.nodes names each node it covers, with that node's IMEX daemon
// status, Ready or NotReady. The daemon pod on the node is read as well,
// because a domain whose status has not caught up still has a pod that is not
// reporting ready, and that is the same fault a few seconds earlier.
//
// The node has no nvidia.com/gpu.clique. A node outside any clique cannot be
// placed in an IMEX domain however healthy its daemon looks, and that is a
// fact about the node rather than about one domain: a node covered by two
// domains gets one finding naming both.
//
// Only nodes the run actually saw are reported. A ComputeDomain naming a node
// outside the inventory is a domain describing a node this run has nothing to
// say about, and a finding nobody can look up gives a reader no command to run
// next.
type ComputeDomainNotReady struct{ versionRange }

// NewComputeDomainNotReady returns the rule, gated at the operator release
// that first ships the DRA driver. An unknown version still runs it, which is
// what VersionInRange answers for a version it cannot read: a rule skipped on
// a cluster whose version has an unusual shape reports clean for the wrong
// reason.
func NewComputeDomainNotReady() *ComputeDomainNotReady {
	return &ComputeDomainNotReady{versionRange: versionRange{MinVersion: draDriverMinVersion}}
}

// Code implements health.Rule.
func (r *ComputeDomainNotReady) Code() string { return "NV-CD-001" }

// Severity implements health.Rule. An error: a workload waiting on an IMEX
// domain that never goes ready is a job that never starts.
func (r *ComputeDomainNotReady) Severity() output.Severity { return output.SeverityError }

// Evaluate implements health.Rule.
func (r *ComputeDomainNotReady) Evaluate(in health.Input) []output.Finding {
	if len(in.ComputeDomains) == 0 {
		return nil
	}

	byName := make(map[string]inventory.GPUNode, len(in.Nodes))
	for _, node := range in.Nodes {
		byName[node.Name] = node
	}

	var findings []output.Finding

	// cliqueless collects the domains covering each node that has no clique,
	// so that the node gets one finding rather than one per domain.
	cliqueless := map[string][]string{}

	for i := range in.ComputeDomains {
		domain := &in.ComputeDomains[i]
		name := domainName(domain)

		for _, entry := range domainNodes(domain) {
			node, known := byName[entry.name]
			if !known {
				continue
			}

			if node.Clique == "" {
				cliqueless[node.Name] = append(cliqueless[node.Name], name)
			}

			pod, hasPod := operandFor(node, inventory.ComponentIMEXDaemon)
			if entry.status == computeDomainReady && (!hasPod || pod.Ready) {
				continue
			}

			evidence := map[string]string{
				"computeDomain":       name,
				"computeDomainStatus": domainStatus(domain),
				"nodeStatus":          entry.status,
			}

			setIf(evidence, "cliqueID", entry.cliqueID)

			if hasPod {
				evidence["pod"] = pod.Name
				evidence["ready"] = btoa(pod.Ready)
			}

			findings = append(findings, output.Finding{
				Code:      r.Code(),
				Severity:  r.Severity(),
				Node:      node.Name,
				Component: inventory.ComponentIMEXDaemon,
				Message:   "the IMEX daemon of a ComputeDomain covering the node is not ready",
				Evidence:  evidence,
				Next:      infoCommand(node.Name),
			})
		}
	}

	return append(findings, cliqueFindings(r, cliqueless)...)
}

// cliqueFindings is one finding per node with no clique, in node order so
// that two runs over the same cluster produce the same bytes.
func cliqueFindings(r *ComputeDomainNotReady, cliqueless map[string][]string) []output.Finding {
	nodes := make([]string, 0, len(cliqueless))
	for node := range cliqueless {
		nodes = append(nodes, node)
	}

	sort.Strings(nodes)

	out := make([]output.Finding, 0, len(nodes))

	for _, node := range nodes {
		domains := cliqueless[node]
		sort.Strings(domains)

		out = append(out, output.Finding{
			Code:      r.Code(),
			Severity:  output.SeverityWarning,
			Node:      node,
			Component: ComponentNode,
			Message:   "the node is in a ComputeDomain and carries no " + inventory.LabelClique,
			Evidence:  map[string]string{"computeDomains": strings.Join(domains, ",")},
			Next:      infoCommand(node),
		})
	}

	return out
}

// domainNode is one entry of a ComputeDomain's status.nodes.
type domainNode struct {
	name     string
	cliqueID string
	status   string
}

// domainName is how a finding names a ComputeDomain. They are namespaced, so
// the name alone is ambiguous across namespaces.
func domainName(domain *unstructured.Unstructured) string {
	return domain.GetNamespace() + "/" + domain.GetName()
}

// domainStatus reads status.status, the domain's global verdict.
func domainStatus(domain *unstructured.Unstructured) string {
	status, found, err := unstructured.NestedString(domain.Object, "status", "status")
	if err != nil || !found {
		return ""
	}

	return status
}

// domainNodes reads status.nodes, which is how a ComputeDomain names the nodes
// it covers and reports each one's IMEX daemon.
//
// The CRD is not vendored and its schema can move, so an entry of an
// unexpected shape is skipped rather than guessed at: an entry this code
// cannot read is one fewer node reported, never one more.
//
// An entry with no status at all reads as not ready, which is the CRD's own
// default for the field (+kubebuilder:default:=NotReady). Reading an absent
// status as ready would report a domain nobody has joined as working.
func domainNodes(domain *unstructured.Unstructured) []domainNode {
	items, found, err := unstructured.NestedSlice(domain.Object, "status", "nodes")
	if err != nil || !found {
		return nil
	}

	out := make([]domainNode, 0, len(items))

	for _, item := range items {
		fields, ok := item.(map[string]any)
		if !ok {
			continue
		}

		name := stringField(fields, "name")
		if name == "" {
			continue
		}

		status := stringField(fields, "status")
		if status == "" {
			status = "NotReady"
		}

		out = append(out, domainNode{
			name:     name,
			cliqueID: stringField(fields, "cliqueID"),
			status:   status,
		})
	}

	return out
}
