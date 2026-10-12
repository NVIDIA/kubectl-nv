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
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// splitNode is a node whose hostname label and whose reported hostname address
// disagree, which is ordinary on a cluster where the kubelet was given a
// --hostname-override. A redactor that learned one of the two would leave the
// other in every log.
func splitNode(name, label, address string) *v1.Node {
	four := *resource.NewQuantity(4, resource.DecimalSI)

	return &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"kubernetes.io/hostname":                      label,
				"nvidia.com/gpu.present":                      "true",
				"feature.node.kubernetes.io/pci-10de.present": "true",
			},
		},
		Status: v1.NodeStatus{
			Capacity:    v1.ResourceList{"nvidia.com/gpu": four},
			Allocatable: v1.ResourceList{"nvidia.com/gpu": four},
			Conditions:  []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
			Addresses: []v1.NodeAddress{
				{Type: v1.NodeHostName, Address: address},
				{Type: v1.NodeInternalIP, Address: "10.42.7.21"},
			},
		},
	}
}

// A node's hostname label and the hostname it reports are two different
// strings on a cluster whose kubelet was given --hostname-override, and a log
// line carries whichever one the daemon on that node knows.
func TestRedactLearnsTheHostnameFromBothTheLabelAndTheAddress(t *testing.T) {
	const label = "worker-21.lab.example.com"
	const address = "worker21"

	node := splitNode("gpu-node-21", label, address)
	seam := writingSeam([]byte(
		"kubelet on " + label + " started\nfabricmanager greeted " + address + "\n"))

	dir, names := redactedGather(t, &Options{FetchPodFile: seam}, node)

	for _, name := range names {
		text := readArtifact(t, dir, name)
		for _, planted := range []string{label, address, "gpu-node-21"} {
			if strings.Contains(text, planted) {
				t.Errorf("%s still holds %q", name, planted)
			}
		}
	}
}

// machines.yaml is OpenShift's own record of the machine behind a node, and it
// names the host. A redactor that read only nodes.yaml would rewrite the node
// and leave the machine naming it.
func TestRedactLearnsTheIdentitiesOpenShiftRecords(t *testing.T) {
	dir, _ := redactedGather(t, &Options{}, twoNodeCluster()...)

	// The fixture cluster serves no machines, so the file is planted into the
	// bundle directly, which is what an OpenShift gather leaves behind.
	const machineHost = "worker-ocp-7f2b1.ec2.internal"
	planted := filepath.Join(dir, "machines.yaml")
	body := "apiVersion: v1\nkind: List\nitems:\n" +
		"- apiVersion: machine.openshift.io/v1beta1\n" +
		"  kind: Machine\n" +
		"  metadata:\n    name: cluster-worker-abcde\n" +
		"  status:\n    nodeRef:\n      name: " + machineHost + "\n"
	if err := os.WriteFile(planted, []byte(body), 0o600); err != nil {
		t.Fatalf("writing machines.yaml: %v", err)
	}
	logPath := filepath.Join(dir, "extra.log")
	if err := os.WriteFile(logPath, []byte("drain requested for "+machineHost+"\n"), 0o600); err != nil {
		t.Fatalf("writing extra.log: %v", err)
	}

	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting: %v", err)
	}

	for _, name := range []string{"machines.yaml", "extra.log"} {
		raw, rerr := os.ReadFile(filepath.Join(dir, name))
		if rerr != nil {
			t.Fatalf("reading %s: %v", name, rerr)
		}
		if strings.Contains(string(raw), machineHost) {
			t.Errorf("%s still names the machine's node %q", name, machineHost)
		}
	}
}

// A gather narrowed to one node still writes about the others: a driver log
// names the peers it talked to, and a ComputeDomain names every node in it.
// The identities are learned from the cluster rather than from what the bundle
// happens to carry, so a node left out of the bundle is still taken out of it.
func TestRedactLearnsEveryNodeEvenUnderTheNodeFlag(t *testing.T) {
	seam := writingSeam([]byte(
		"imex domain peers: gpu-node-1, gpu-node-2\nnvlink partner gpu-node-2 is up\n"))

	dir, names := redactedGather(t,
		&Options{Node: "gpu-node-1", FetchPodFile: seam}, twoNodeCluster()...)

	for _, name := range names {
		text := readArtifact(t, dir, name)
		if strings.Contains(text, "gpu-node-2") {
			t.Errorf("%s names gpu-node-2, which --node left out of the bundle and not out of the logs", name)
		}
	}
}

// The identities a bundle's own files name have to be learned from those files
// too: a pod says which node it runs on, an event says which node it is about,
// and a ResourceSlice says which node published it.
func TestRedactLearnsTheIdentitiesTheBundlesOwnFilesName(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("writing %s: %v", name, err)
		}
	}

	// No nodes.yaml at all: a gather whose node read was refused still wrote
	// these, and each one names a node.
	write("gpu_pods.yaml", "apiVersion: v1\nkind: PodList\nitems:\n"+
		"- apiVersion: v1\n  kind: Pod\n  metadata:\n    name: trainer\n    namespace: research\n"+
		"  spec:\n    nodeName: pod-named-node\n")
	write("node_events.yaml", "apiVersion: v1\nkind: EventList\nitems:\n"+
		"- apiVersion: v1\n  kind: Event\n  metadata:\n    name: cordoned\n    namespace: default\n"+
		"  involvedObject:\n    kind: Node\n    name: event-named-node\n")
	write("resourceslices.yaml", "apiVersion: resource.k8s.io/v1\nkind: ResourceSliceList\nitems:\n"+
		"- apiVersion: resource.k8s.io/v1\n  kind: ResourceSlice\n  metadata:\n    name: slice-1\n"+
		"  spec:\n    driver: gpu.nvidia.com\n    nodeName: slice-named-node\n    pool:\n      name: p\n")
	write("extra.log", "saw pod-named-node, event-named-node and slice-named-node\n")

	r, err := NewRedactor()
	if err != nil {
		t.Fatalf("building the redactor: %v", err)
	}
	if err := r.RedactBundle(dir); err != nil {
		t.Fatalf("redacting: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "extra.log"))
	if err != nil {
		t.Fatalf("reading extra.log: %v", err)
	}
	for _, planted := range []string{"pod-named-node", "event-named-node", "slice-named-node"} {
		if strings.Contains(string(raw), planted) {
			t.Errorf("extra.log still holds %q", planted)
		}
	}
}

// unusedRuntime keeps the runtime import load bearing for the fixtures above.
var _ = []runtime.Object(nil)
