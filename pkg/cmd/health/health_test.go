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

package health_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/kubectl-nv/docs/findings"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/health"
	healthpkg "github.com/NVIDIA/kubectl-nv/pkg/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// fakeFactory hands the command fake clients, so that no test reaches a
// cluster and none of them needs a kubeconfig.
type fakeFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
}

func (f *fakeFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f *fakeFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f *fakeFactory) RESTConfig() (*rest.Config, error)               { return &rest.Config{}, nil }

// clusterPolicyGVR is what the dynamic fake has to be told about, or it
// invents a list kind and the read comes back empty.
var clusterPolicyGVR = schema.GroupVersionResource{
	Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies",
}

// newDynamic builds the dynamic fake with the ClusterPolicy list kind
// registered. Without it the fake panics on the first list rather than
// returning nothing, which is a crash in the test and not a cluster this
// command could ever meet.
func newDynamic(policies ...runtime.Object) dynamic.Interface {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		clusterPolicyGVR.GroupVersion().WithKind("ClusterPolicyList"),
		&unstructured.UnstructuredList{},
	)

	return dynamicfake.NewSimpleDynamicClient(scheme, policies...)
}

type streams struct {
	out *bytes.Buffer
	err *bytes.Buffer
}

// cluster is everything a test puts in front of the command.
type cluster struct {
	objects  []runtime.Object
	policies []runtime.Object
}

// run executes kubectl nv health over the cluster and returns what it printed.
func run(t *testing.T, c cluster, args ...string) (*streams, error) {
	t.Helper()

	s := &streams{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: s.out, ErrOut: s.err}

	o := &cmd.Options{
		Streams: io,
		Factory: &fakeFactory{
			clientset: fake.NewSimpleClientset(c.objects...),
			dynamic:   newDynamic(c.policies...),
		},
		Logger: output.NewLogger(io, output.LoggerOptions{NoColor: true}),
	}

	command := health.NewCommand(o)
	command.SetOut(s.out)
	command.SetErr(s.err)
	command.SetArgs(args)
	command.SilenceUsage = true
	command.SilenceErrors = true

	err := command.Execute()

	return s, err
}

// gpuNode is a healthy GPU node of a cluster the operator manages.
func gpuNode(name string, capacity, allocatable int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"nvidia.com/gpu.present":                      "true",
				"feature.node.kubernetes.io/pci-10de.present": "true",
				"nvidia.com/gpu.count":                        "4",
				"nvidia.com/gpu.deploy.device-plugin":         "true",
				"nvidia.com/gpu.deploy.dcgm-exporter":         "true",
			},
		},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(capacity, resource.DecimalSI)},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(allocatable, resource.DecimalSI)},
			Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

// operandPod is one GPU Operator pod on a node.
func operandPod(name, node, app string, ready bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "gpu-operator",
			Labels:    map[string]string{"app": app},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Ready: ready}},
		},
	}
}

// operatorPod is the controller itself, which is where the command reads
// whether the operator is installed and which version it is.
func operatorPod(image string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gpu-operator-5d8f9c7b4-abcde",
			Namespace: "gpu-operator",
			Labels:    map[string]string{"app": "gpu-operator"},
		},
		Spec: corev1.PodSpec{
			NodeName:   "control-plane",
			Containers: []corev1.Container{{Name: "gpu-operator", Image: image}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// hintedNode is a node that claims GPUs through its labels and advertises
// none: the single most common thing this command is run to find, and the one
// kind of node the inventory leaves out unless it is asked for.
func hintedNode(name string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"nvidia.com/gpu.present":                      "true",
				"feature.node.kubernetes.io/pci-10de.present": "true",
				"nvidia.com/gpu.deploy.device-plugin":         "true",
			},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	}
}

// clusterPolicy is a ClusterPolicy in the state given.
func clusterPolicy(name, state string) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "nvidia.com/v1",
		"kind":       "ClusterPolicy",
		"metadata":   map[string]any{"name": name},
	}

	if state != "" {
		obj["status"] = map[string]any{"state": state}
	}

	return &unstructured.Unstructured{Object: obj}
}

// healthyCluster is a cluster nothing should be reported about: one GPU node
// with its operands ready, a ready ClusterPolicy, and the operator running.
func healthyCluster() cluster {
	return cluster{
		objects: []runtime.Object{
			gpuNode("worker-1", 4, 4),
			operatorPod("nvcr.io/nvidia/gpu-operator:v26.3.3"),
			operandPod("nvidia-device-plugin-daemonset-aaaaa", "worker-1",
				"nvidia-device-plugin-daemonset", true),
			operandPod("nvidia-dcgm-exporter-bbbbb", "worker-1", "nvidia-dcgm-exporter", true),
			operandPod("gpu-feature-discovery-ccccc", "worker-1", "gpu-feature-discovery", true),
		},
		policies: []runtime.Object{clusterPolicy("cluster-policy", "ready")},
	}
}

// A clean cluster exits zero and says what it looked at. "No findings" with no
// count beside it is not an answer a reader can act on.
func TestHealthOnAHealthyClusterExitsZero(t *testing.T) {
	t.Parallel()

	s, err := run(t, healthyCluster())
	if err != nil {
		t.Fatalf("health: %v\nstdout:\n%s\nstderr:\n%s", err, s.out, s.err)
	}

	if !strings.Contains(s.out.String(), "1 node checked") {
		t.Errorf("the summary does not say what was checked:\n%s", s.out)
	}

	if !strings.Contains(s.out.String(), "0 errors, 0 warnings, 0 infos") {
		t.Errorf("the summary does not count the findings:\n%s", s.out)
	}
}

// A node that claims GPUs and advertises none has to reach the rules at all.
//
// The inventory leaves such nodes out unless it is asked for them, and they
// arrive marked Hinted rather than in a set of their own, so two separate
// things have to be right for NV-NODE-002 to fire: the builder has to be asked
// to include them, and the command has to sort them into Input.Hinted. Neither
// shows up in a fixture made only of GPU nodes.
func TestHealthReportsOnHintedNodes(t *testing.T) {
	t.Parallel()

	c := healthyCluster()
	c.objects = append(c.objects, hintedNode("worker-hinted"))

	s, err := run(t, c)
	if err == nil {
		t.Fatalf("health found nothing on a node that claims GPUs and has none:\n%s", s.out)
	}

	for _, want := range []string{"worker-hinted", "NV-NODE-002"} {
		if !strings.Contains(s.out.String(), want) {
			t.Errorf("the table does not contain %q:\n%s", want, s.out)
		}
	}

	// Two nodes checked: the GPU node and the hinted one. A run that left the
	// hinted node out of the inventory would report one.
	if !strings.Contains(s.out.String(), "2 nodes checked") {
		t.Errorf("the summary does not count the hinted node:\n%s", s.out)
	}

	// Which set it lands in decides which rules see it, and that does not show
	// in the row count: NV-NODE-002 walks both sets, so a hinted node filed
	// under Nodes still produces this finding. What changes is everything that
	// walks the GPU nodes alone. NV-DP-001 would then report a missing device
	// plugin on a node that advertises no GPUs for it to have advertised, and
	// NV-NODE-002's own evidence would call the node not hinted.
	s, err = run(t, c, "-o", "json")
	if err == nil {
		t.Fatal("health found nothing on the hinted node")
	}

	var report struct {
		Findings []struct {
			Code     string            `json:"code"`
			Node     string            `json:"node"`
			Evidence map[string]string `json:"evidence"`
		} `json:"findings"`
	}
	if jsonErr := json.Unmarshal(s.out.Bytes(), &report); jsonErr != nil {
		t.Fatalf("parsing the report: %v\n%s", jsonErr, s.out)
	}

	var sawNodeFinding bool

	for _, f := range report.Findings {
		if f.Node != "worker-hinted" {
			continue
		}

		if f.Code == "NV-DP-001" {
			t.Errorf("a hinted node was checked for a device plugin: %+v", f)
		}

		if f.Code == "NV-NODE-002" {
			sawNodeFinding = true

			if f.Evidence["hinted"] != "true" {
				t.Errorf("evidence[hinted] is %q, want true: the node was filed as a GPU node",
					f.Evidence["hinted"])
			}
		}
	}

	if !sawNodeFinding {
		t.Errorf("no NV-NODE-002 for the hinted node: %+v", report.Findings)
	}
}

// A cluster the operator was never installed on is an answer, not a failure.
// The API server serves no clusterpolicies collection at all there, and
// reading that as a denied read would exit 3 over a cluster that is complete.
func TestHealthOnAClusterWithNoOperator(t *testing.T) {
	t.Parallel()

	s := &streams{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: s.out, ErrOut: s.err}

	dyn := newDynamic()
	dyn.(*dynamicfake.FakeDynamicClient).PrependReactor("list", "clusterpolicies",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewNotFound(
				schema.GroupResource{Group: "nvidia.com", Resource: "clusterpolicies"}, "")
		})

	o := &cmd.Options{
		Streams: io,
		Factory: &fakeFactory{
			clientset: fake.NewSimpleClientset(gpuNode("worker-1", 4, 4)),
			dynamic:   dyn,
		},
		Logger: output.NewLogger(io, output.LoggerOptions{NoColor: true}),
	}

	command := health.NewCommand(o)
	command.SetOut(s.out)
	command.SetErr(s.err)
	command.SetArgs([]string{"-o", "json"})
	command.SilenceUsage = true
	command.SilenceErrors = true

	err := command.Execute()
	if got := output.ExitCodeFor(err); got != output.ExitOK {
		t.Errorf("exit code is %d, want %d: %v\n%s", got, output.ExitOK, err, s.out)
	}

	var report struct {
		Partial bool `json:"partial"`
	}
	if err := json.Unmarshal(s.out.Bytes(), &report); err != nil {
		t.Fatalf("parsing the report: %v\n%s", err, s.out)
	}

	if report.Partial {
		t.Errorf("a cluster with no GPU Operator was reported as a partial read:\n%s", s.out)
	}
}

// A broken node is one table row, in the columns the handoff named, with the
// summary under it.
func TestHealthPrintsOneRowPerFinding(t *testing.T) {
	t.Parallel()

	c := healthyCluster()
	// The device plugin pod goes, so the node keeps its labels and its
	// capacity drops out of allocatable.
	c.objects = []runtime.Object{
		gpuNode("worker-1", 4, 0),
		operatorPod("nvcr.io/nvidia/gpu-operator:v26.3.3"),
		operandPod("gpu-feature-discovery-ccccc", "worker-1", "gpu-feature-discovery", true),
	}

	s, err := run(t, c, "--fail-on", "info")
	if err == nil {
		t.Fatalf("health found nothing on a broken node:\n%s", s.out)
	}

	if got := output.ExitCodeFor(err); got != output.ExitFindings {
		t.Errorf("exit code is %d, want %d", got, output.ExitFindings)
	}

	for _, want := range []string{
		"NODE", "CODE", "SEVERITY", "COMPONENT", "MESSAGE",
		"worker-1", "NV-NODE-002", "error", "node",
		"NV-DP-001", "nvidia-device-plugin-daemonset",
	} {
		if !strings.Contains(s.out.String(), want) {
			t.Errorf("the table does not contain %q:\n%s", want, s.out)
		}
	}
}

// --summary prints the one line and nothing else, which is what a dashboard or
// a shell prompt wants out of this command.
func TestHealthSummaryOnly(t *testing.T) {
	t.Parallel()

	c := healthyCluster()
	c.objects = []runtime.Object{
		gpuNode("worker-1", 4, 0),
		operatorPod("nvcr.io/nvidia/gpu-operator:v26.3.3"),
	}

	s, _ := run(t, c, "--summary")

	if strings.Contains(s.out.String(), "NV-NODE-002") {
		t.Errorf("--summary printed a finding:\n%s", s.out)
	}

	if !strings.Contains(s.out.String(), "1 node checked") {
		t.Errorf("--summary printed no summary:\n%s", s.out)
	}

	if lines := strings.Count(strings.TrimSpace(s.out.String()), "\n"); lines != 0 {
		t.Errorf("--summary printed %d extra lines:\n%s", lines, s.out)
	}
}

// The exit code is what a script branches on, and --fail-on is what moves it.
func TestHealthFailOn(t *testing.T) {
	t.Parallel()

	c := healthyCluster()
	// Allocatable below capacity is NV-NODE-003, a warning and nothing worse,
	// so this cluster is the one that tells the thresholds apart.
	c.objects = []runtime.Object{
		gpuNode("worker-1", 4, 2),
		operatorPod("nvcr.io/nvidia/gpu-operator:v26.3.3"),
		operandPod("nvidia-device-plugin-daemonset-aaaaa", "worker-1",
			"nvidia-device-plugin-daemonset", true),
		operandPod("nvidia-dcgm-exporter-bbbbb", "worker-1", "nvidia-dcgm-exporter", true),
	}

	for _, tc := range []struct {
		failOn string
		want   int
	}{
		{failOn: "", want: output.ExitOK},
		{failOn: "error", want: output.ExitOK},
		{failOn: "warning", want: output.ExitFindings},
		{failOn: "info", want: output.ExitFindings},
	} {
		t.Run("fail-on="+tc.failOn, func(t *testing.T) {
			t.Parallel()

			args := []string{}
			if tc.failOn != "" {
				args = append(args, "--fail-on", tc.failOn)
			}

			_, err := run(t, c, args...)
			if got := output.ExitCodeFor(err); got != tc.want {
				t.Errorf("--fail-on %q exits %d, want %d (%v)", tc.failOn, got, tc.want, err)
			}
		})
	}
}

// A misspelled --fail-on is a usage error, and it is found before the cluster
// is read: a user who waited through a scan to be told they typed the wrong
// thing has waited for nothing.
func TestHealthRejectsAnUnknownFailOnBeforeReading(t *testing.T) {
	t.Parallel()

	var listed bool

	s := &streams{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: s.out, ErrOut: s.err}

	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("list", "*", func(k8stesting.Action) (bool, runtime.Object, error) {
		listed = true

		return false, nil, nil
	})

	o := &cmd.Options{
		Streams: io,
		Factory: &fakeFactory{
			clientset: clientset,
			dynamic:   newDynamic(),
		},
		Logger: output.NewLogger(io, output.LoggerOptions{NoColor: true}),
	}

	command := health.NewCommand(o)
	command.SetOut(s.out)
	command.SetErr(s.err)
	command.SetArgs([]string{"--fail-on", "catastrophic"})
	command.SilenceUsage = true
	command.SilenceErrors = true

	err := command.Execute()
	if err == nil {
		t.Fatal("health accepted --fail-on catastrophic")
	}

	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code is %d, want %d: %v", got, output.ExitUsage, err)
	}

	if listed {
		t.Error("health read the cluster before validating --fail-on")
	}
}

// --node narrows the report to one node. It goes to the engine rather than to
// the inventory builder, because a rule that compares a node against its peers
// needs the peers: narrowing the read would silence it.
func TestHealthNodeFilter(t *testing.T) {
	t.Parallel()

	c := healthyCluster()
	c.objects = append(c.objects, gpuNode("worker-2", 4, 0))

	s, err := run(t, c, "--node", "worker-1")
	if err != nil {
		t.Fatalf("health --node worker-1: %v\n%s", err, s.out)
	}

	if strings.Contains(s.out.String(), "worker-2") {
		t.Errorf("--node worker-1 reported worker-2:\n%s", s.out)
	}

	if !strings.Contains(s.out.String(), "1 node checked") {
		t.Errorf("--node did not narrow what was checked:\n%s", s.out)
	}
}

// A --node nobody can match exits 2 and says what the run did look at.
func TestHealthRejectsAnUnknownNode(t *testing.T) {
	t.Parallel()

	_, err := run(t, healthyCluster(), "--node", "wroker-1")
	if err == nil {
		t.Fatal("health accepted a node it never looked at")
	}

	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code is %d, want %d: %v", got, output.ExitUsage, err)
	}

	if !strings.Contains(err.Error(), "worker-1") {
		t.Errorf("the message does not list what was found: %v", err)
	}
}

// The machine readable output is the contract every consumer parses.
func TestHealthJSONOutput(t *testing.T) {
	t.Parallel()

	c := healthyCluster()
	c.objects = []runtime.Object{
		gpuNode("worker-1", 4, 0),
		operatorPod("nvcr.io/nvidia/gpu-operator:v26.3.3"),
	}

	s, _ := run(t, c, "-o", "json")

	var report struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Findings   []struct {
			Code string `json:"code"`
			Docs string `json:"docs"`
			Next string `json:"next"`
		} `json:"findings"`
		Summary struct {
			NodesChecked int `json:"nodesChecked"`
		} `json:"summary"`
	}
	if err := json.Unmarshal(s.out.Bytes(), &report); err != nil {
		t.Fatalf("parsing the report: %v\n%s", err, s.out)
	}

	if report.Kind != output.KindHealthReport || report.APIVersion != output.APIVersion {
		t.Errorf("envelope is %s/%s, want %s/%s",
			report.APIVersion, report.Kind, output.APIVersion, output.KindHealthReport)
	}

	if report.Summary.NodesChecked != 1 {
		t.Errorf("nodesChecked is %d, want 1", report.Summary.NodesChecked)
	}

	if len(report.Findings) == 0 {
		t.Fatalf("no findings in the report:\n%s", s.out)
	}

	// The catalog path is what makes a finding actionable, and it is filled by
	// the engine rather than by each rule.
	for _, f := range report.Findings {
		if f.Docs != "docs/findings/"+f.Code+".md" {
			t.Errorf("%s links to %q, want its catalog entry", f.Code, f.Docs)
		}

		if f.Next == "" {
			t.Errorf("%s carries no next command", f.Code)
		}
	}
}

// Every rule the catalog documents has to run. A command wired to a short list
// would report a cluster clean because it never asked the missing questions,
// and nothing else in the tree would notice.
func TestHealthRunsEveryRegisteredRule(t *testing.T) {
	t.Parallel()

	if len(rules.All()) != len(findings.Codes()) {
		t.Fatalf("rules.All() has %d rules and the catalog %d entries",
			len(rules.All()), len(findings.Codes()))
	}

	if got := health.RuleCount(); got != len(findings.Codes()) {
		t.Errorf("the command runs %d rules, want %d", got, len(findings.Codes()))
	}
}

// A denied read makes the run partial: the findings stand, the report names
// what it could not see, and the exit code says the answer is incomplete.
func TestHealthPartialRead(t *testing.T) {
	t.Parallel()

	s := &streams{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: s.out, ErrOut: s.err}

	clientset := fake.NewSimpleClientset(gpuNode("worker-1", 4, 4))
	clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "pods"}, "", errors.New("nope"))
	})

	o := &cmd.Options{
		Streams: io,
		Factory: &fakeFactory{
			clientset: clientset,
			dynamic:   newDynamic(),
		},
		Logger: output.NewLogger(io, output.LoggerOptions{NoColor: true}),
	}

	command := health.NewCommand(o)
	command.SetOut(s.out)
	command.SetErr(s.err)
	command.SetArgs([]string{"-o", "json"})
	command.SilenceUsage = true
	command.SilenceErrors = true

	err := command.Execute()

	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Errorf("exit code is %d, want %d: %v", got, output.ExitPartial, err)
	}

	var report struct {
		Partial     bool `json:"partial"`
		Unreachable []struct {
			Resource string `json:"resource"`
		} `json:"unreachable"`
	}
	if err := json.Unmarshal(s.out.Bytes(), &report); err != nil {
		t.Fatalf("parsing the report: %v\n%s", err, s.out)
	}

	if !report.Partial {
		t.Errorf("the report is not marked partial:\n%s", s.out)
	}

	var named bool

	for _, u := range report.Unreachable {
		if u.Resource == "pods" {
			named = true
		}
	}

	if !named {
		t.Errorf("the report does not name the denied read:\n%s", s.out)
	}
}

// --from-bundle replays a must-gather directory. A directory that is not one
// is a usage error rather than a crash.
func TestHealthFromBundleRejectsSomethingElse(t *testing.T) {
	t.Parallel()

	_, err := run(t, cluster{}, "--from-bundle", t.TempDir()+"/not-there")
	if err == nil {
		t.Fatal("health accepted a bundle directory that does not exist")
	}

	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code is %d, want %d: %v", got, output.ExitUsage, err)
	}
}

// The operator version decides which rules apply at all, so reading it off the
// controller's image tag has to survive the shapes a registry puts around one.
func TestOperatorVersionFromImage(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		image string
		want  string
	}{
		{"nvcr.io/nvidia/gpu-operator:v26.3.3", "v26.3.3"},
		{"gpu-operator:v26.3.3", "v26.3.3"},
		{"registry.example.com:5000/nvidia/gpu-operator:v26.3.3", "v26.3.3"},
		// A digest is not a version. Reading "sha256" out of one and gating a
		// rule on it would skip checks on every cluster deployed by digest.
		{"nvcr.io/nvidia/gpu-operator@sha256:" + strings.Repeat("a", 64), ""},
		{"registry.example.com:5000/nvidia/gpu-operator", ""},
		// Two colons in the name is not a reference this code can read, and
		// neither half of it is a version. Picking one would gate every rule
		// in the catalog on a string that is not one.
		{"gpu-operator:v1:2", ""},
		{"", ""},
	} {
		t.Run(tc.image, func(t *testing.T) {
			t.Parallel()

			if got := health.OperatorVersionFromImage(tc.image); got != tc.want {
				t.Errorf("OperatorVersionFromImage(%q) = %q, want %q", tc.image, got, tc.want)
			}
		})
	}
}

// The command's own table columns have to name the field they render, or
// --fields cannot ask for them and drops every one.
func TestHealthTableColumnsNameTheirFields(t *testing.T) {
	t.Parallel()

	report := healthpkg.NewHealthReport()

	// Against the paths --fields actually accepts, not against "not empty".
	// A column naming a path no projection knows is dropped the moment
	// --fields is given, and a test that only asked for a non-empty string
	// would have passed on every one of them.
	paths, err := output.FieldPaths(report)
	if err != nil {
		t.Fatalf("FieldPaths: %v", err)
	}

	known := make(map[string]bool, len(paths))
	for _, p := range paths {
		known[p] = true
	}

	for _, wide := range []bool{false, true} {
		for _, column := range report.TableColumns(wide) {
			if len(column.Fields) == 0 {
				t.Errorf("column %q names no field", column.Header)

				continue
			}

			for _, field := range column.Fields {
				if !known[field] {
					t.Errorf("column %q names %q, which is not a field of the report: %v",
						column.Header, field, paths)
				}
			}
		}
	}
}
