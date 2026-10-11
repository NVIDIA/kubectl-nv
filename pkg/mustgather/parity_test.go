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
	"sort"
	"strings"
	"testing"
)

// scriptArtifacts is every file the gpu-operator must-gather collector writes,
// hand-transcribed from hack/must-gather.sh at commit
// f6d32bad44eec471ecbdccbfc4ce8e7fafa62228 (2026-09-30). The line number each
// one comes from is kept so that the next reader can check the transcription
// against the script rather than against this list.
//
// A placeholder stands for the part of a name the script fills in from the
// cluster: <node> for a node name, <pod> for a pod name, <daemonset> for a
// daemonset name.
var scriptArtifacts = map[string]string{
	"must-gather.log":                    "line 30, tee of stdout",
	"must-gather.stderr.log":             "line 31, redirect of stderr",
	"version":                            "line 34, the operator version, plugin image only",
	"openshift_version.yaml":             "line 43, clusterversion/version",
	"hyperconverged.yaml":                "line 56, hyperconvergeds.hco.kubevirt.io -A",
	"kubevirt.yaml":                      "line 84, kubevirts.kubevirt.io -A",
	"cluster_policy.yaml":                "line 98, the ClusterPolicy",
	"cluster_policy.missing":             "line 101, marker when there is no ClusterPolicy",
	"nvidiadrivers.yaml":                 "line 114, nvidiadrivers.nvidia.com -A",
	"gpu_cluster.yaml":                   "line 129, gpuclusters.nvidia.com",
	"deviceclasses.yaml":                 "line 145, deviceclasses.resource.k8s.io",
	"resourceslices-<driver>.yaml":       "line 164, one file per NVIDIA DRA driver",
	"all_machines.list":                  "line 178, machines -A, OpenShift only",
	"gpu_nodes.labels":                   "lines 196-205, the labels of each GPU node",
	"gpu_nodes.status":                   "line 209, get nodes -o wide",
	"gpu_nodes.descr":                    "line 212, describe nodes",
	"gpu_operator_pod.status":            "line 224, get pod -o wide",
	"gpu_operator_pod.yaml":              "line 230, get pod -o yaml",
	"gpu_operator_pod.log":               "line 236, the operator log",
	"gpu_operator_pod.previous.log":      "line 242, the operator's previous log",
	"gpu_operand_pods.status":            "line 253, get pods -o wide",
	"gpu_operand_pods.yaml":              "line 258, get pods -o yaml",
	"gpu_operand_pod_images.txt":         "line 263, the image of every operand container",
	"gpu_operand_pod_<pod>.log":          "line 284, the log of each operand pod",
	"gpu_operand_pod_<pod>.previous.log": "line 291, the previous log of each operand pod",
	"gpu_operand_pod_<pod>.descr":        "line 295, describe of each operand pod",
	"gpu_operand_ds.status":              "line 308, get ds",
	"gpu_operand_ds.yaml":                "line 314, get ds -o yaml",
	"gpu_operand_ds_<daemonset>.descr":   "line 326, describe of each operand daemonset",
	"nvidia-bug-report_<node>.log.gz":    "line 355, the bug report of each driver pod's node",
	"gpu_device_usage_<node>.log":        "line 371, nvidia-smi, lsof, ps and fuser",
}

// scriptToOurs maps a name the script writes onto the name kubectl-nv writes
// for the same content. Only the names that differ appear here; the table in
// docs/must-gather.md carries the same mapping for the reader of a bundle.
//
// A mapping to several of our files means the script put in one file what we
// split across more: the script's per-pod log holds every container of that
// pod, which we write one container at a time.
var scriptToOurs = map[string][]string{
	"nvidiadrivers.yaml":                 {"nvidia_driver.yaml", "nvidia_driver.missing"},
	"resourceslices-<driver>.yaml":       {"resourceslices.yaml"},
	"all_machines.list":                  {"machines.yaml"},
	"gpu_operator_pod.log":               {"gpu_operator_logs_<container>.log"},
	"gpu_operator_pod.previous.log":      {"gpu_operator_logs_<container>.previous.log"},
	"gpu_operand_pod_<pod>.log":          {"<pod>_<container>.log"},
	"gpu_operand_pod_<pod>.previous.log": {"<pod>_<container>.previous.log"},
	"gpu_operand_pod_<pod>.descr":        {"<pod>.descr"},
	"gpu_operand_ds.yaml":                {"daemonsets.yaml"},
}

// notProduced lists what kubectl-nv does not write, and why. Every entry is a
// decision rather than an omission, and anything not named here has to be in
// the planned set.
var notProduced = map[string]string{
	// The script is also shipped as an OpenShift must-gather plugin image, and
	// writes "version" only when it runs as /usr/bin/gather, from the VERSION
	// of that image. kubectl-nv reports its own version instead, and it goes in
	// manifest.json where a reader can parse it.
	"version": "the tool version is a field of manifest.json, not a file of its own",
}

// TestPlannedArtifactsCoverTheScript is the parity gate. Every file the
// gpu-operator script writes has to come out of some collector here, under our
// name for it, or be listed as a deliberate omission. A collector dropped or a
// name changed without the mapping being updated fails here rather than when a
// support engineer opens a bundle and finds the file they wanted is gone.
func TestPlannedArtifactsCoverTheScript(t *testing.T) {
	planned := make(map[string]bool)
	for _, name := range PlannedArtifacts() {
		planned[name] = true
	}
	if len(planned) == 0 {
		t.Fatal("the collector registry plans no artifacts at all")
	}

	for script, where := range scriptArtifacts {
		if reason, omitted := notProduced[script]; omitted {
			if planned[script] {
				t.Errorf("%s is listed as not produced (%s) but the registry plans it", script, reason)
			}
			continue
		}

		wanted := scriptToOurs[script]
		if wanted == nil {
			wanted = []string{script}
		}
		for _, name := range wanted {
			if !planned[name] {
				t.Errorf("no collector plans %s, which stands for the script's %s (%s)\nplanned: %v",
					name, script, where, sortedArtifacts())
			}
		}
	}
}

// TestManifestIsPlanned keeps the bundle index in the planned set. It is the
// one artifact that is not in the script at all, and a reader that cannot find
// it has no way to tell a collector that was skipped from one that never ran.
func TestManifestIsPlanned(t *testing.T) {
	for _, name := range PlannedArtifacts() {
		if name == ManifestFileName {
			t.Fatalf("%s is in the planned set, but it indexes the bundle rather than being part of it", name)
		}
	}
}

// TestEveryMappingNamesAScriptArtifact keeps the two tables above honest. A
// mapping or an omission for a file the script does not write is a leftover,
// and a leftover is what lets the parity check pass while the parity is gone.
func TestEveryMappingNamesAScriptArtifact(t *testing.T) {
	for script := range scriptToOurs {
		if _, ok := scriptArtifacts[script]; !ok {
			t.Errorf("scriptToOurs maps %q, which the script does not write", script)
		}
	}
	for script := range notProduced {
		if _, ok := scriptArtifacts[script]; !ok {
			t.Errorf("notProduced names %q, which the script does not write", script)
		}
	}
}

// sortedArtifacts lists the planned set for a failure message.
func sortedArtifacts() string {
	names := PlannedArtifacts()
	sort.Strings(names)
	return strings.Join(names, " ")
}
