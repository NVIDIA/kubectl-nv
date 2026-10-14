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

package inventory

import (
	"strings"
	"testing"
)

// The component constants are the verbatim values the GPU Operator puts in the
// "app" label on its operand pods. A rule that matches on a component compares
// against one of these, so a typo here is a rule that silently never fires.
// Spelling them out is the whole point of the constants, so the test spells
// them out again rather than deriving them.
func TestComponentConstants(t *testing.T) {
	t.Parallel()

	for name, pair := range map[string][2]string{
		"ComponentDriver":             {ComponentDriver, "nvidia-driver-daemonset"},
		"ComponentVGPUManager":        {ComponentVGPUManager, "nvidia-vgpu-manager-daemonset"},
		"ComponentContainerToolkit":   {ComponentContainerToolkit, "nvidia-container-toolkit-daemonset"},
		"ComponentDevicePlugin":       {ComponentDevicePlugin, "nvidia-device-plugin-daemonset"},
		"ComponentGFD":                {ComponentGFD, "gpu-feature-discovery"},
		"ComponentDCGM":               {ComponentDCGM, "nvidia-dcgm"},
		"ComponentDCGMExporter":       {ComponentDCGMExporter, "nvidia-dcgm-exporter"},
		"ComponentOperatorValidator":  {ComponentOperatorValidator, "nvidia-operator-validator"},
		"ComponentMIGManager":         {ComponentMIGManager, "nvidia-mig-manager"},
		"ComponentNodeStatusExporter": {ComponentNodeStatusExporter, "nvidia-node-status-exporter"},
		"ComponentOperator":           {ComponentOperator, "gpu-operator"},
		// The operator's verbatim "app" label value; the standalone charts
		// label the same pod by component and are mapped onto this.
		"ComponentDRADriver": {ComponentDRADriver, "nvidia-dra-driver-kubelet-plugin"},
		// Not an "app" value: that pod has none, so this is the upstream
		// template's own name for the workload.
		"ComponentIMEXDaemon": {ComponentIMEXDaemon, "compute-domain-daemon"},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %q, want %q", name, pair[0], pair[1])
		}
	}
}

// Components lists every constant above, which is what a command uses to build
// a "component not found on this node" check without naming each one.
func TestComponentsListsEveryConstant(t *testing.T) {
	t.Parallel()

	got := Components()

	if len(got) < 13 {
		t.Errorf("Components() has %d entries, want at least the 13 named constants", len(got))
	}

	seen := map[string]bool{}
	for _, component := range got {
		if seen[component] {
			t.Errorf("Components() lists %q twice", component)
		}

		seen[component] = true

		if component != strings.ToLower(component) || strings.TrimSpace(component) != component {
			t.Errorf("Components() entry %q is not a bare lowercase label value", component)
		}
	}

	for _, want := range []string{ComponentDriver, ComponentGFD, ComponentDRADriver, ComponentIMEXDaemon} {
		if !seen[want] {
			t.Errorf("Components() does not list %q", want)
		}
	}
}

// Components returns a copy: a caller that sorts or truncates it must not be
// editing the package's own list.
func TestComponentsReturnsACopy(t *testing.T) {
	t.Parallel()

	got := Components()
	got[0] = "mutated"

	if Components()[0] == "mutated" {
		t.Error("mutating the returned slice changed the package list")
	}
}

// The deploy labels are nvidia.com/gpu.deploy.<key>, and the key is not the
// component name: "gpu-feature-discovery" is deployed under "gpu-feature-
// discovery" but the driver is under "driver", not "nvidia-driver-daemonset".
// A command that reported the raw key as the component would be reporting two
// different vocabularies as one.
func TestComponentForDeployKey(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		key  string
		want string
		ok   bool
	}{
		{"driver", ComponentDriver, true},
		{"vgpu-manager", ComponentVGPUManager, true},
		{"container-toolkit", ComponentContainerToolkit, true},
		{"device-plugin", ComponentDevicePlugin, true},
		{"gpu-feature-discovery", ComponentGFD, true},
		{"dcgm", ComponentDCGM, true},
		{"dcgm-exporter", ComponentDCGMExporter, true},
		{"operator-validator", ComponentOperatorValidator, true},
		{"mig-manager", ComponentMIGManager, true},
		{"node-status-exporter", ComponentNodeStatusExporter, true},
		{"unknown-operand", "", false},
		{"", "", false},
		{"nvidia.com/gpu.deploy.driver", "", false},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Parallel()

			got, ok := ComponentForDeployKey(tc.key)
			if ok != tc.ok || got != tc.want {
				t.Errorf("ComponentForDeployKey(%q) = %q, %v, want %q, %v", tc.key, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// DeployLabel builds the full label name for a key, which is what a builder
// reads off a node. Keeping it here means the prefix is written once.
func TestDeployLabel(t *testing.T) {
	t.Parallel()

	if got := DeployLabel("driver"); got != "nvidia.com/gpu.deploy.driver" {
		t.Errorf("DeployLabel(%q) = %q", "driver", got)
	}

	if got := DeployLabelPrefix; got != "nvidia.com/gpu.deploy." {
		t.Errorf("DeployLabelPrefix = %q", got)
	}
}

// The deploy map must not grow a key that maps to a component no constant
// names: that is how the two vocabularies drift apart.
func TestEveryDeployKeyMapsToAKnownComponent(t *testing.T) {
	t.Parallel()

	known := map[string]bool{}
	for _, component := range Components() {
		known[component] = true
	}

	for _, key := range DeployKeys() {
		component, ok := ComponentForDeployKey(key)
		if !ok {
			t.Errorf("DeployKeys lists %q but ComponentForDeployKey does not resolve it", key)

			continue
		}

		if !known[component] {
			t.Errorf("deploy key %q maps to %q, which Components() does not list", key, component)
		}
	}
}
