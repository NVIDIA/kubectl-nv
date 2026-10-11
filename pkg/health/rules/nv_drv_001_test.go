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
	"os"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// driverPod builds a driver pod of the named component with its readiness and
// restart count spelled out.
func driverPod(component string, ready bool, restarts int32) inventory.OperandPod {
	phase := string(corev1.PodRunning)
	if !ready {
		phase = string(corev1.PodPending)
	}

	return inventory.OperandPod{
		Name:      component + "-abcde",
		Namespace: "gpu-operator",
		Component: component,
		Phase:     phase,
		Ready:     ready,
		Restarts:  restarts,
	}
}

// driverNode is a worker whose driver pod is in the state the case is about.
func driverNode(name string, pod inventory.OperandPod) inventory.GPUNode {
	return buildNode(name,
		withLabels(map[string]string{rules.LabelGPUPresent: "true", rules.LabelPCIVendor: "true"}),
		withEveryOperandDeployed(),
		withCount(4),
		withGPUs(4, 4),
		withOperandPod(pod),
		withOperand(inventory.ComponentDevicePlugin, true),
	)
}

func TestDriverPodNotReadyReportsAnUnreadyDriverPod(t *testing.T) {
	t.Parallel()

	rule := rules.NewDriverPodNotReady()
	in := health.Input{
		Nodes:             []inventory.GPUNode{driverNode("worker-1", driverPod(inventory.ComponentDriver, false, 7))},
		OperatorInstalled: true,
	}

	finding := only(t, evaluate(rule, in))

	checkFinding(t, rule, finding, inventory.ComponentDriver,
		"the driver pod on the node is not ready",
		"kubectl nv info worker-1")

	if finding.Severity != output.SeverityError {
		t.Errorf("Severity is %q, want %q", finding.Severity, output.SeverityError)
	}

	checkEvidence(t, finding.Evidence, map[string]string{
		"nvidia.com/gpu.deploy.driver": "true",
		"operands":                     "nvidia-device-plugin-daemonset,nvidia-driver-daemonset",
		"pod":                          "nvidia-driver-daemonset-abcde",
		"phase":                        "Pending",
		"ready":                        "false",
		"restarts":                     "7",
	})
}

func TestDriverPodNotReadyStaysQuietOnAReadyDriverPod(t *testing.T) {
	t.Parallel()

	in := health.Input{
		Nodes:             []inventory.GPUNode{driverNode("worker-1", driverPod(inventory.ComponentDriver, true, 0))},
		OperatorInstalled: true,
	}

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// Mokka runs simulated GPUs and no driver pod at all. A node with no driver
// pod is not this finding: reporting one would make every lane run red, and
// the absence of a driver on a node that never had one is not a fault.
func TestDriverPodNotReadyStaysQuietWhenTheNodeRunsNoDriverPod(t *testing.T) {
	t.Parallel()

	if findings := evaluate(rules.NewDriverPodNotReady(), healthyLane()); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// A ready pod that has restarted is not reported. The operand model carries a
// restart count and no termination time, so "restarted an hour ago" and
// "restarted in March" are the same input, and a rule that fired on the count
// alone would report every long lived node in the cluster for ever.
func TestDriverPodNotReadyStaysQuietOnAReadyPodThatHasRestarted(t *testing.T) {
	t.Parallel()

	in := health.Input{
		Nodes:             []inventory.GPUNode{driverNode("worker-1", driverPod(inventory.ComponentDriver, true, 12))},
		OperatorInstalled: true,
	}

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The vGPU manager takes the driver's place on a vGPU host, and it is one of
// the selectors pkg/mustgather collects a bug report through.
func TestDriverPodNotReadyReportsTheVGPUManager(t *testing.T) {
	t.Parallel()

	rule := rules.NewDriverPodNotReady()
	in := health.Input{
		Nodes:             []inventory.GPUNode{driverNode("worker-1", driverPod(inventory.ComponentVGPUManager, false, 0))},
		OperatorInstalled: true,
	}

	finding := only(t, evaluate(rule, in))
	if finding.Component != inventory.ComponentVGPUManager {
		t.Errorf("Component is %q, want %q", finding.Component, inventory.ComponentVGPUManager)
	}
}

// OpenShift names each driver DaemonSet after the RHCOS version it was built
// for, so the "app" label is nvidia-driver-daemonset-<version>-<rhcos> rather
// than the bare name, and app.kubernetes.io/component=nvidia-driver is the
// other spelling pkg/mustgather collects through.
func TestDriverPodNotReadyReportsTheOpenShiftSpellings(t *testing.T) {
	t.Parallel()

	for _, component := range []string{
		"nvidia-driver-daemonset-410.84.202203290245-0",
		"nvidia-driver",
	} {
		t.Run(component, func(t *testing.T) {
			t.Parallel()

			in := health.Input{
				Nodes:             []inventory.GPUNode{driverNode("worker-1", driverPod(component, false, 0))},
				OperatorInstalled: true,
			}

			finding := only(t, evaluate(rules.NewDriverPodNotReady(), in))
			if finding.Component != component {
				t.Errorf("Component is %q, want %q", finding.Component, component)
			}
		})
	}
}

// A component whose name merely starts the same way is not a driver. Matching
// on a prefix without the separator would claim any operand a future chart
// names nvidia-driver-daemonsetfoo.
func TestDriverPodNotReadyIgnoresAnUnrelatedComponent(t *testing.T) {
	t.Parallel()

	in := health.Input{
		Nodes:             []inventory.GPUNode{driverNode("worker-1", driverPod("nvidia-driver-daemonsetfoo", false, 0))},
		OperatorInstalled: true,
	}

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The deploy label is the operator saying it parked the operand on purpose,
// which is NV-OP-002's finding rather than this one's.
func TestDriverPodNotReadyStaysQuietWhileTheDriverIsPaused(t *testing.T) {
	t.Parallel()

	node := driverNode("worker-1", driverPod(inventory.ComponentDriver, false, 0))
	node.Deploy["driver"] = "paused-for-mig-change"

	in := health.Input{Nodes: []inventory.GPUNode{node}, OperatorInstalled: true}

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// No GPU Operator means no deploy labels and no operand it promised to run.
func TestDriverPodNotReadyStaysQuietWithoutTheOperator(t *testing.T) {
	t.Parallel()

	in := health.Input{
		Nodes: []inventory.GPUNode{driverNode("worker-1", driverPod(inventory.ComponentDriver, false, 0))},
	}

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

// The hinted nodes are not walked: a control plane that carries the GPU hint
// labels is NV-NODE-002's business, and on this repository's own lane it
// carries every deploy label while no operand will ever land on it.
func TestDriverPodNotReadySkipsHintedNodes(t *testing.T) {
	t.Parallel()

	node := mokkaControlPlane()
	node.Operands = append(node.Operands, driverPod(inventory.ComponentDriver, false, 3))

	in := health.Input{Hinted: []inventory.GPUNode{node}, OperatorInstalled: true}

	if findings := evaluate(rules.NewDriverPodNotReady(), in); len(findings) != 0 {
		t.Fatalf("got %d findings, want none: %+v", len(findings), findings)
	}
}

func TestDriverPodNotReadyAppliesToEveryOperatorVersion(t *testing.T) {
	t.Parallel()

	rule := rules.NewDriverPodNotReady()
	if !rule.Applies("v25.10.0") || !rule.Applies("") {
		t.Error("the rule has to apply to every operator version and to an unknown one")
	}

	bounded := rules.NewDriverPodNotReady()
	bounded.MinVersion = "v99.0.0"

	if bounded.Applies("v25.10.0") {
		t.Error("the version gate is not wired: a bounded rule still applies below its minimum")
	}
}

// The selectors a driver pod is found through are pkg/mustgather's, because a
// driver this rule does not recognise is a driver nobody collects a bug report
// from either. The two lists are in different packages and only one of them is
// exported, so this is what keeps them the same list: a selector added to
// pkg/mustgather and not here fails this test by name.
func TestDriverComponentsCoverEveryMustGatherSelector(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile("../../mustgather/driverpods.go")
	if err != nil {
		t.Fatalf("reading pkg/mustgather/driverpods.go: %v", err)
	}

	block := regexp.MustCompile(`(?s)var driverLabels = \[\]string\{(.*?)\n\}`).FindStringSubmatch(string(raw))
	if block == nil {
		t.Fatal("pkg/mustgather/driverpods.go no longer declares driverLabels as a slice literal")
	}

	selectors := regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(block[1], -1)
	if len(selectors) == 0 {
		t.Fatal("driverLabels holds no selectors, so this test proves nothing")
	}

	for _, match := range selectors {
		selector := match[1]

		// A bare key with no value is a presence selector. OpenShift's
		// driver-toolkit is the one of those, and the pod it marks carries a
		// driver "app" label as well, which the value selectors below cover.
		_, value, found := strings.Cut(selector, "=")
		if !found {
			continue
		}

		if !rules.IsDriverComponent(value) {
			t.Errorf("pkg/mustgather selects driver pods by %q and IsDriverComponent(%q) is false",
				selector, value)
		}
	}
}
