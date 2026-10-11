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

package topology_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	cmdtopology "github.com/NVIDIA/kubectl-nv/pkg/cmd/topology"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// kueueGroup is the API group of Kueue's own resources.
const kueueGroup = "kueue.x-k8s.io"

// fakeFactory hands the command fake clients instead of real ones.
type fakeFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

func (f *fakeFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f *fakeFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f *fakeFactory) RESTConfig() (*rest.Config, error)               { return &rest.Config{}, nil }

// gpuNode builds a node advertising whole GPUs, in the given clique and
// fabric tier.
func gpuNode(name, clique string, allocatable int64) *corev1.Node {
	quantity := *resource.NewQuantity(allocatable, resource.DecimalSI)

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			"nvidia.com/gpu.clique":                       clique,
			"fabric.topograph.run/tier-1":                 "fabric-1",
			"feature.node.kubernetes.io/pci-10de.present": "true",
		}},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": quantity},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": quantity},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// gpuPod builds a running pod holding GPUs on a node.
func gpuPod(name, nodeName string, count int64) *corev1.Pod {
	request := corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(count, resource.DecimalSI)}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "research"},
		Spec: corev1.PodSpec{
			NodeName: nodeName,
			Containers: []corev1.Container{{
				Name:      "trainer",
				Resources: corev1.ResourceRequirements{Requests: request, Limits: request},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// gpuJob builds a Job whose pods each want count GPUs.
func gpuJob(name string, parallelism int32, count int64) *batchv1.Job {
	request := corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(count, resource.DecimalSI)}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "research"},
		Spec: batchv1.JobSpec{
			Parallelism: &parallelism,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{
					Name:      "trainer",
					Resources: corev1.ResourceRequirements{Requests: request, Limits: request},
				}},
			}},
		},
	}
}

// run executes the topology command over a cluster of the given objects and
// returns what it printed on stdout.
func run(t *testing.T, args []string, objects ...runtime.Object) (string, error) {
	t.Helper()

	return runWith(t, args, nil, objects...)
}

// runWith is the same with a hook that can install reactors or discovery on
// the fakes before the command runs.
func runWith(
	t *testing.T,
	args []string,
	setup func(*fake.Clientset, *dynamicfake.FakeDynamicClient),
	objects ...runtime.Object,
) (string, error) {
	t.Helper()

	out := &bytes.Buffer{}
	streams := genericiooptions.IOStreams{In: &bytes.Buffer{}, Out: out, ErrOut: &bytes.Buffer{}}

	namespace := "research"
	configFlags := genericclioptions.NewConfigFlags(true)
	configFlags.Namespace = &namespace

	kinds := map[schema.GroupVersionResource]string{}
	for _, version := range []string{"v1beta2", "v1beta1", "v1alpha1"} {
		kinds[schema.GroupVersionResource{Group: kueueGroup, Version: version, Resource: "topologies"}] = "TopologyList"
		kinds[schema.GroupVersionResource{Group: kueueGroup, Version: version, Resource: "resourceflavors"}] = "ResourceFlavorList"
	}

	clientset := fake.NewSimpleClientset(objects...)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), kinds)
	if setup != nil {
		setup(clientset, dyn)
	}

	o := &cmd.Options{
		ConfigFlags: configFlags,
		Streams:     streams,
		Factory:     &fakeFactory{clientset: clientset, dynamic: dyn},
	}

	c := cmdtopology.NewCommand(o)
	c.SetArgs(args)
	c.SetOut(out)
	c.SetErr(streams.ErrOut)
	c.SilenceUsage = true
	c.SilenceErrors = true

	// The command is executed before the buffer is read: the two are written
	// in one return statement at the reader's peril, because Go evaluates the
	// arguments left to right and would hand back an empty string.
	err := c.Execute()

	return out.String(), err
}

// decode reads the JSON report the command printed.
func decode(t *testing.T, raw string) map[string]any {
	t.Helper()

	var got map[string]any
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatalf("decoding %q: %v", raw, err)
	}

	return got
}

// TestTopologyGroupsTheClusterByClique is the command end to end: two nodes
// in one clique, one of them holding a GPU already.
func TestTopologyGroupsTheClusterByClique(t *testing.T) {
	raw, err := run(t, []string{"-o", "json"},
		gpuNode("node-a", "clique-a", 4),
		gpuNode("node-b", "clique-a", 4),
		gpuPod("busy", "node-a", 3),
	)
	if err != nil {
		t.Fatalf("topology -o json: %v", err)
	}

	got := decode(t, raw)
	if got["kind"] != "TopologyReport" {
		t.Fatalf("kind is %v, want TopologyReport: %s", got["kind"], raw)
	}

	levels, _ := got["levels"].([]any)
	if len(levels) == 0 {
		t.Fatalf("the report carries no levels: %s", raw)
	}

	first, _ := levels[0].(map[string]any)
	if first["name"] != "node" {
		t.Fatalf("the first level is %v, want the node: %s", first["name"], raw)
	}

	var clique map[string]any
	for _, level := range levels {
		if l, _ := level.(map[string]any); l["name"] == "clique" {
			clique = l
		}
	}
	if clique == nil {
		t.Fatalf("the report has no clique level: %s", raw)
	}

	groups, _ := clique["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("the clique level has %d groups, want 1: %s", len(groups), raw)
	}

	group, _ := groups[0].(map[string]any)
	for field, want := range map[string]float64{
		"nodeCount":       2,
		"allocatable":     8,
		"requested":       3,
		"free":            5,
		"largestFreeNode": 4,
	} {
		if got, _ := group[field].(float64); got != want {
			t.Errorf("%s is %v, want %v: %s", field, group[field], want, raw)
		}
	}
}

// TestTopologyReportsKueueAsMissing: a cluster with no Kueue CRDs is an
// answer, not a failure.
func TestTopologyReportsKueueAsMissing(t *testing.T) {
	raw, err := run(t, []string{"-o", "json"}, gpuNode("node-a", "clique-a", 4))
	if err != nil {
		t.Fatalf("topology -o json: %v", err)
	}

	kueue, _ := decode(t, raw)["kueue"].(map[string]any)
	if kueue == nil {
		t.Fatalf("the report says nothing about Kueue: %s", raw)
	}
	if kueue["installed"] != false {
		t.Errorf("Kueue is reported as installed: %s", raw)
	}
	if reason, _ := kueue["reason"].(string); reason == "" {
		t.Errorf("the report gives no reason why there are no Kueue levels: %s", raw)
	}
}

// TestTopologyAnswersForAJob covers --for end to end: two pods of four GPUs
// against two idle nodes of four in one clique.
func TestTopologyAnswersForAJob(t *testing.T) {
	raw, err := run(t, []string{"-o", "json", "--for", "job/trainer"},
		gpuNode("node-a", "clique-a", 4),
		gpuNode("node-b", "clique-a", 4),
		gpuJob("trainer", 2, 4),
	)
	if err != nil {
		t.Fatalf("topology --for job/trainer: %v", err)
	}

	fit, _ := decode(t, raw)["fit"].(map[string]any)
	if fit == nil {
		t.Fatalf("the report carries no answer about the job: %s", raw)
	}
	if fit["smallest"] != "clique" {
		t.Errorf("the smallest level that fits is %v, want the clique: %s", fit["smallest"], raw)
	}
	if got, _ := fit["gpusPerPod"].(float64); got != 4 {
		t.Errorf("GPUs per pod is %v, want 4: %s", fit["gpusPerPod"], raw)
	}
}

// TestTopologyExitsTwoForAMissingJob: a job that is not there is something
// the user typed wrong, which is exit 2 and not a report of nothing.
func TestTopologyExitsTwoForAMissingJob(t *testing.T) {
	_, err := run(t, []string{"-o", "json", "--for", "job/absent"}, gpuNode("node-a", "clique-a", 4))
	if err == nil {
		t.Fatal("asking about a job that does not exist succeeded")
	}

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError so the command exits 2", err, err)
	}

	want := `no job "absent" in namespace "research"`
	if err.Error() != want {
		t.Errorf("the error is\n  %q\nwant\n  %q", err.Error(), want)
	}
}

// TestTopologyExitsTwoForAJobWithoutGPUs.
func TestTopologyExitsTwoForAJobWithoutGPUs(t *testing.T) {
	_, err := run(t, []string{"-o", "json", "--for", "job/trainer"},
		gpuNode("node-a", "clique-a", 4),
		gpuJob("trainer", 2, 0),
	)

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError so the command exits 2", err, err)
	}
}

// TestTopologyRejectsAForTargetItCannotAnswer.
func TestTopologyRejectsAForTargetItCannotAnswer(t *testing.T) {
	_, err := run(t, []string{"--for", "pod/trainer"}, gpuNode("node-a", "clique-a", 4))

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
}

// TestTopologyPrintsATable is the default output: a reader who ran the
// command with no flags gets columns, not JSON.
func TestTopologyPrintsATable(t *testing.T) {
	raw, err := run(t, nil, gpuNode("node-a", "clique-a", 4))
	if err != nil {
		t.Fatalf("topology: %v", err)
	}

	lines := strings.Split(raw, "\n")
	if len(lines) < 2 {
		t.Fatalf("the table has no rows:\n%s", raw)
	}

	header := strings.Fields(lines[0])
	want := []string{"LEVEL", "GROUP", "NODES", "ALLOCATABLE", "REQUESTED", "FREE", "LARGEST-FREE"}
	if len(header) != len(want) {
		t.Fatalf("the header is %v, want %v", header, want)
	}
	for i := range want {
		if header[i] != want[i] {
			t.Errorf("column %d is %q, want %q", i, header[i], want[i])
		}
	}

	for _, name := range []string{"clique", "clique-a", "node-a", "cluster"} {
		if !strings.Contains(raw, name) {
			t.Errorf("the table does not carry %q:\n%s", name, raw)
		}
	}
}

// TestTopologyMarksTheSmallestLevelInTheTable: the answer to --for is one
// level out of several, and a reader scanning a long table should not have to
// remember the level order to find it.
func TestTopologyMarksTheSmallestLevelInTheTable(t *testing.T) {
	raw, err := run(t, []string{"--for", "job/trainer"},
		gpuNode("node-a", "clique-a", 4),
		gpuNode("node-b", "clique-a", 4),
		gpuJob("trainer", 2, 4),
	)
	if err != nil {
		t.Fatalf("topology --for job/trainer: %v", err)
	}

	if !strings.Contains(raw, "yes (smallest)") {
		t.Errorf("no row is marked as the smallest level that fits:\n%s", raw)
	}

	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "clique" {
			continue
		}
		if !strings.HasSuffix(line, "yes (smallest)") {
			t.Errorf("the clique row is %q, want it marked as the smallest level: two pods of four fit in one clique\n%s",
				line, raw)
		}
	}
}

// TestTopologyRejectsAnUnknownOutputFormat keeps -o a closed set, so a typo
// is exit 2 rather than a silent fall back to the table.
func TestTopologyRejectsAnUnknownOutputFormat(t *testing.T) {
	_, err := run(t, []string{"-o", "toml"}, gpuNode("node-a", "clique-a", 4))

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
}

// kueueDiscovery announces the Kueue resources at v1beta2, which is what a
// cluster with Kueue installed looks like to a discovery read.
func kueueDiscovery(cs *fake.Clientset) {
	cs.Resources = append(cs.Resources, &metav1.APIResourceList{
		GroupVersion: kueueGroup + "/v1beta2",
		APIResources: []metav1.APIResource{
			{Name: "topologies"},
			{Name: "resourceflavors"},
		},
	})
}

// TestTopologyReportsADeniedKueueReadAsPartial is the RBAC case: the cluster
// says it serves the Kueue topology API and then refuses the read. The rest
// of the report is still the answer to most of the question, so it is printed
// and the run exits 3 rather than 4.
func TestTopologyReportsADeniedKueueReadAsPartial(t *testing.T) {
	raw, err := runWith(t, []string{"-o", "json"},
		func(cs *fake.Clientset, dyn *dynamicfake.FakeDynamicClient) {
			kueueDiscovery(cs)
			dyn.PrependReactor("list", "topologies", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(
					schema.GroupResource{Group: kueueGroup, Resource: "topologies"}, "", errors.New("denied"))
			})
		},
		gpuNode("node-a", "clique-a", 4),
	)
	if err == nil {
		t.Fatal("a denied Kueue read exited 0")
	}
	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Fatalf("exit code is %d, want %d (%v)", got, output.ExitPartial, err)
	}

	report := decode(t, raw)

	levels, _ := report["levels"].([]any)
	if len(levels) == 0 {
		t.Errorf("the report carries no levels: the node labels were readable\n%s", raw)
	}

	// Kueue is installed: discovery said so before the read was refused.
	// Reporting installed:false would send the user to install something
	// they already have.
	kueue, _ := report["kueue"].(map[string]any)
	if kueue["installed"] != true {
		t.Errorf("kueue.installed is %v, want true: discovery answered before the read was denied\n%s",
			kueue["installed"], raw)
	}
	if reason, _ := kueue["reason"].(string); reason == "" {
		t.Errorf("kueue gives no reason why its levels are missing\n%s", raw)
	}

	status, _ := report["status"].(map[string]any)
	if status["partial"] != true {
		t.Errorf("status.partial is %v, want true\n%s", status["partial"], raw)
	}

	unreachable, _ := status["unreachable"].([]any)
	if len(unreachable) != 1 {
		t.Fatalf("status.unreachable holds %d entries, want exactly 1\n%s", len(unreachable), raw)
	}

	entry, _ := unreachable[0].(map[string]any)
	if entry["resource"] != "topologies" {
		t.Errorf("the unreachable entry names %v, want topologies\n%s", entry["resource"], raw)
	}
	if reason, _ := entry["reason"].(string); !strings.Contains(reason, "forbidden") {
		t.Errorf("the unreachable reason is %q, want it to say the read was forbidden\n%s", reason, raw)
	}
}

// TestTopologyReportsAMalformedTopology: one broken Topology object must not
// cost the user the whole report, and the reason says which one it was.
func TestTopologyReportsAMalformedTopology(t *testing.T) {
	broken := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kueueGroup + "/v1beta2",
		"kind":       "Topology",
		"metadata":   map[string]any{"name": "broken"},
		"spec":       map[string]any{"levels": "not-a-list"},
	}}
	flavor := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": kueueGroup + "/v1beta2",
		"kind":       "ResourceFlavor",
		"metadata":   map[string]any{"name": "tas"},
		"spec":       map[string]any{"topologyName": "broken"},
	}}

	raw, err := runWith(t, []string{"-o", "json"},
		func(cs *fake.Clientset, dyn *dynamicfake.FakeDynamicClient) {
			kueueDiscovery(cs)
			for _, obj := range []*unstructured.Unstructured{broken, flavor} {
				if createErr := dyn.Tracker().Add(obj); createErr != nil {
					t.Fatalf("seeding %s: %v", obj.GetName(), createErr)
				}
			}
		},
		gpuNode("node-a", "clique-a", 4),
	)
	if err != nil {
		t.Fatalf("topology: %v", err)
	}

	kueue, _ := decode(t, raw)["kueue"].(map[string]any)
	if kueue["installed"] != true {
		t.Errorf("kueue.installed is %v, want true\n%s", kueue["installed"], raw)
	}

	reason, _ := kueue["reason"].(string)
	if !strings.Contains(reason, "broken") {
		t.Errorf("kueue.reason is %q, want it to name the Topology it could not read\n%s", reason, raw)
	}
}

// TestTopologyKeepsTheReportWhenTheJobReadIsDenied: the fit is the part that
// could not be answered, and the levels are the part that could.
func TestTopologyKeepsTheReportWhenTheJobReadIsDenied(t *testing.T) {
	raw, err := runWith(t, []string{"-o", "json", "--for", "job/trainer"},
		func(cs *fake.Clientset, _ *dynamicfake.FakeDynamicClient) {
			cs.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(
					schema.GroupResource{Group: "batch", Resource: "jobs"}, "trainer", errors.New("denied"))
			})
		},
		gpuNode("node-a", "clique-a", 4),
	)
	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Fatalf("exit code is %d, want %d (%v)", got, output.ExitPartial, err)
	}

	report := decode(t, raw)
	if _, found := report["fit"]; found {
		t.Errorf("the report carries a fit for a job it could not read\n%s", raw)
	}
	if levels, _ := report["levels"].([]any); len(levels) == 0 {
		t.Errorf("the report carries no levels\n%s", raw)
	}

	status, _ := report["status"].(map[string]any)
	unreachable, _ := status["unreachable"].([]any)
	if len(unreachable) != 1 {
		t.Fatalf("status.unreachable holds %d entries, want 1\n%s", len(unreachable), raw)
	}
	if entry, _ := unreachable[0].(map[string]any); entry["resource"] != "jobs" {
		t.Errorf("the unreachable entry names %v, want jobs\n%s", entry["resource"], raw)
	}
}

// TestTopologyExitsFourWhenTheClusterCannotBeRead: a transport failure means
// nothing was read, and nothing should be printed as though it were an
// answer.
func TestTopologyExitsFourWhenTheClusterCannotBeRead(t *testing.T) {
	raw, err := runWith(t, []string{"-o", "json"},
		func(cs *fake.Clientset, _ *dynamicfake.FakeDynamicClient) {
			cs.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("connection refused")
			})
		},
	)
	if err == nil {
		t.Fatal("an unreachable cluster exited 0")
	}
	if got := output.ExitCodeFor(err); got != output.ExitError {
		t.Errorf("exit code is %d, want %d (%v)", got, output.ExitError, err)
	}
	if raw != "" {
		t.Errorf("a report was printed for a cluster that was never read:\n%s", raw)
	}
}

// TestTopologyAnswersWithTheNodeLevelForASinglePod covers the implicit levels
// end to end: a one-pod job is placed on one node, and the report says so
// rather than naming the clique that contains it.
func TestTopologyAnswersWithTheNodeLevelForASinglePod(t *testing.T) {
	raw, err := run(t, []string{"-o", "json", "--for", "job/trainer"},
		gpuNode("node-a", "clique-a", 4),
		gpuJob("trainer", 1, 2),
	)
	if err != nil {
		t.Fatalf("topology --for job/trainer: %v", err)
	}

	fit, _ := decode(t, raw)["fit"].(map[string]any)
	if fit == nil {
		t.Fatalf("the report carries no answer about the job\n%s", raw)
	}
	if fit["smallest"] != "node" {
		t.Errorf("the smallest level that fits is %v, want the node\n%s", fit["smallest"], raw)
	}
}

// TestTopologyExitsFourWhenTheKueueReadFails: a Kueue read that fails for any
// reason other than a denial or an absent CRD means the cluster could not be
// talked to. Printing the report anyway and exiting 3 would say "here is your
// topology, some of it is missing", when what happened is that the run hit a
// transport failure part way through and has no idea what else it missed.
func TestTopologyExitsFourWhenTheKueueReadFails(t *testing.T) {
	raw, err := runWith(t, []string{"-o", "json"},
		func(cs *fake.Clientset, dyn *dynamicfake.FakeDynamicClient) {
			kueueDiscovery(cs)
			dyn.PrependReactor("list", "topologies", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("connection refused")
			})
		},
		gpuNode("node-a", "clique-a", 4),
	)
	if err == nil {
		t.Fatal("a failed Kueue read exited 0")
	}
	if got := output.ExitCodeFor(err); got != output.ExitError {
		t.Errorf("exit code is %d, want %d (%v)", got, output.ExitError, err)
	}
	if raw != "" {
		t.Errorf("a report was printed for a run that failed part way through:\n%s", raw)
	}
}

// TestTopologyKeepsTheReportWhenThePodReadIsDenied is the RBAC case on the
// pods: the nodes and their labels were readable, so the levels and the node
// counts are the answer to most of the question. What is not known is what
// already runs there, and the report says so rather than reporting the
// cluster as idle.
func TestTopologyKeepsTheReportWhenThePodReadIsDenied(t *testing.T) {
	raw, err := runWith(t, []string{"-o", "json"},
		func(cs *fake.Clientset, _ *dynamicfake.FakeDynamicClient) {
			cs.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(
					schema.GroupResource{Resource: "pods"}, "", errors.New("denied"))
			})
		},
		gpuNode("node-a", "clique-a", 4),
		gpuNode("node-b", "clique-a", 4),
	)
	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Fatalf("exit code is %d, want %d (%v)", got, output.ExitPartial, err)
	}

	report := decode(t, raw)

	levels, _ := report["levels"].([]any)
	if len(levels) == 0 {
		t.Fatalf("the report carries no levels: the nodes were readable\n%s", raw)
	}

	var clique map[string]any
	for _, level := range levels {
		if l, _ := level.(map[string]any); l["name"] == "clique" {
			clique = l
		}
	}
	if clique == nil {
		t.Fatalf("the report has no clique level\n%s", raw)
	}

	groups, _ := clique["groups"].([]any)
	if len(groups) != 1 {
		t.Fatalf("the clique level has %d groups, want 1\n%s", len(groups), raw)
	}

	group, _ := groups[0].(map[string]any)
	if group["requestedKnown"] != false {
		t.Errorf("requestedKnown is %v, want false: the pods were never listed\n%s", group["requestedKnown"], raw)
	}
	if count, _ := group["nodeCount"].(float64); count != 2 {
		t.Errorf("nodeCount is %v, want 2: both nodes were read\n%s", group["nodeCount"], raw)
	}
	if free, _ := group["free"].(float64); free != 0 {
		t.Errorf("free is %v, want 0 while nothing is known about what runs there\n%s", group["free"], raw)
	}

	status, _ := report["status"].(map[string]any)
	if status["partial"] != true {
		t.Errorf("status.partial is %v, want true\n%s", status["partial"], raw)
	}

	unreachable, _ := status["unreachable"].([]any)
	if len(unreachable) != 1 {
		t.Fatalf("status.unreachable holds %d entries, want 1\n%s", len(unreachable), raw)
	}
	if entry, _ := unreachable[0].(map[string]any); entry["resource"] != "pods" {
		t.Errorf("the unreachable entry names %v, want pods\n%s", entry["resource"], raw)
	}
}

// TestTopologyTakesTheSharedOutputFlags: -o, --fields and --limit come from
// one place, so that the MCP server and every command agree on what the
// format flag is called.
func TestTopologyTakesTheSharedOutputFlags(t *testing.T) {
	raw, err := run(t, []string{"-o", "json", "--fields", "levels"},
		gpuNode("node-a", "clique-a", 4),
	)
	if err != nil {
		t.Fatalf("topology --fields levels: %v", err)
	}

	if !strings.Contains(raw, `"levels"`) {
		t.Errorf("the projection carries no levels:\n%s", raw)
	}
	if strings.Contains(raw, `"kueue"`) || strings.Contains(raw, `"status"`) {
		t.Errorf("the projection carries a section nobody asked for:\n%s", raw)
	}
}

// TestTopologyProjectsTheTableColumns: --fields on a table keeps the columns
// whose field the user named, out of the same vocabulary as -o json. Asking
// for the fit keeps the column that answers about it and drops the rest.
func TestTopologyProjectsTheTableColumns(t *testing.T) {
	raw, err := run(t, []string{"--fields", "fit", "--for", "job/trainer"},
		gpuNode("node-a", "clique-a", 4),
		gpuJob("trainer", 1, 2),
	)
	if err != nil {
		t.Fatalf("topology --fields fit: %v", err)
	}

	header := strings.Fields(strings.Split(raw, "\n")[0])
	if len(header) != 1 || header[0] != "FITS" {
		t.Errorf("the header is %v, want just FITS:\n%s", header, raw)
	}
}

// TestTopologyRefusesAnUnknownField keeps the field vocabulary closed, so a
// typo is exit 2 rather than an empty projection.
func TestTopologyRefusesAnUnknownField(t *testing.T) {
	_, err := run(t, []string{"--fields", "nonesuch"}, gpuNode("node-a", "clique-a", 4))

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
}

// TestTopologyRefusesLimitOnAReportThatIsNotAList: the report is one object,
// not a list of them, so there is nothing for --limit to cut. Answering a
// bounded request with an unbounded one would leave the caller believing it
// asked for less than it got.
func TestTopologyRefusesLimitOnAReportThatIsNotAList(t *testing.T) {
	_, err := run(t, []string{"--limit", "1"}, gpuNode("node-a", "clique-a", 4))

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("error is %T (%v), want an output.UsageError", err, err)
	}
	if !strings.Contains(err.Error(), "TopologyReport") {
		t.Errorf("the error is %q, want it to name the kind", err)
	}
}
