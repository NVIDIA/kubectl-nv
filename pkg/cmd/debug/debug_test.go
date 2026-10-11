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

package debug_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/adm"
	debugcmd "github.com/NVIDIA/kubectl-nv/pkg/cmd/debug"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// operatorNamespace is where the fixture runs the operator and its operands.
const operatorNamespace = "gpu-operator"

type fakeFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
	config    *rest.Config
}

func (f *fakeFactory) KubernetesClient() (kubernetes.Interface, error) { return f.clientset, nil }
func (f *fakeFactory) DynamicClient() (dynamic.Interface, error)       { return f.dynamic, nil }
func (f *fakeFactory) RESTConfig() (*rest.Config, error)               { return f.config, nil }

// offlineConfig is a REST configuration whose transport answers everything
// with a 404, so nothing in the gather reaches a socket.
func offlineConfig() *rest.Config {
	return &rest.Config{
		Host: "https://kubectl-nv.invalid",
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			w := httptest.NewRecorder()
			http.NotFound(w, req)
			resp := w.Result()
			resp.Request = req

			return resp, nil
		}),
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// customResourceListKinds names every custom resource the gather lists, so
// that the dynamic fake answers with an empty list rather than panicking.
func customResourceListKinds() map[schema.GroupVersionResource]string {
	return map[schema.GroupVersionResource]string{
		{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}:           "ClusterPolicyList",
		{Group: "nvidia.com", Version: "v1alpha1", Resource: "nvidiadrivers"}:       "NVIDIADriverList",
		{Group: "nvidia.com", Version: "v1alpha1", Resource: "gpuclusters"}:         "GPUClusterList",
		{Group: "hco.kubevirt.io", Version: "v1beta1", Resource: "hyperconvergeds"}: "HyperConvergedList",
		{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}:                "KubeVirtList",
		{Group: "resource.k8s.io", Version: "v1", Resource: "deviceclasses"}:        "DeviceClassList",
	}
}

// gpuNode is a node advertising the given number of GPUs.
func gpuNode(name string, gpus int64) *corev1.Node {
	q := *resource.NewQuantity(gpus, resource.DecimalSI)

	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"nvidia.com/gpu.product":                  "NVIDIA-T4",
				"nvidia.com/gpu.count":                    "4",
				"nvidia.com/cuda.driver-version.full":     "550.107.02",
				"nvidia.com/cuda.runtime-version.full":    "12.5",
				"nvidia.com/mig.strategy":                 "single",
				"nvidia.com/gpu-driver-upgrade-state":     "upgrade-done",
				"nvidia.com/gpu.deploy.driver":            "true",
				"nvidia.com/gpu.deploy.container-toolkit": "true",
			},
		},
		Status: corev1.NodeStatus{
			Capacity:    corev1.ResourceList{"nvidia.com/gpu": q},
			Allocatable: corev1.ResourceList{"nvidia.com/gpu": q},
			Conditions: []corev1.NodeCondition{
				{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
			},
		},
	}
}

// operandPod is one GPU Operator pod on a node.
func operandPod(name, node, app string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: operatorNamespace,
			Labels:    map[string]string{"app": app},
		},
		Spec: corev1.PodSpec{
			NodeName:   node,
			Containers: []corev1.Container{{Name: app, Image: "nvcr.io/nvidia/" + app + ":v26.3.3"}},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: app, Ready: true}},
		},
	}
}

// laneOperands are the six operands the fixture runs on each worker. Six is
// what the one-screen assertion is made against: it is what a GPU Operator
// install with the driver, the toolkit, the plugin, discovery, DCGM and the
// validator puts on a node.
var laneOperands = []string{
	"nvidia-driver-daemonset",
	"nvidia-container-toolkit-daemonset",
	"nvidia-device-plugin-daemonset",
	"gpu-feature-discovery",
	"nvidia-dcgm-exporter",
	"nvidia-operator-validator",
}

// cluster is two GPU workers with the six operands on each, the operator pod
// on the second one, a node with no GPUs at all and a node the labels claim
// GPUs for.
//
// The operator pod is kept off worker-1 so that the node the one-screen
// assertion is made against carries exactly the six operands a GPU Operator
// install puts on a worker, and not seven.
func cluster() []runtime.Object {
	objects := []runtime.Object{
		gpuNode("worker-1", 4),
		gpuNode("worker-2", 4),
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "plain-worker"}},
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name:   "control-plane",
				Labels: map[string]string{"nvidia.com/gpu.present": "true"},
			},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			},
		},
		operandPod("gpu-operator-0", "worker-2", "gpu-operator"),
	}
	for _, worker := range []string{"worker-1", "worker-2"} {
		for _, app := range laneOperands {
			objects = append(objects, operandPod(app+"-on-"+worker, worker, app))
		}
	}

	return objects
}

// run is one execution of kubectl nv debug: the streams it wrote, the bundle
// directory it was pointed at, and the error it ended with.
type run struct {
	out *bytes.Buffer
	err *bytes.Buffer
	dir string
}

// gatherSeam is how a test replaces the two calls that would reach into a pod.
type gatherSeam struct {
	// fetch stands in for the bug report copy. A nil one writes a file naming
	// the node, which is what the live path produces.
	fetch mustgather.PodFileFetcher
	// runInPod stands in for the device usage exec.
	runInPod mustgather.PodCommandRunner
}

// runDebug executes kubectl nv debug over the fixture cluster, running the
// real gatherer: the bundle, its manifest and the collector statuses this
// command reports are the gatherer's own, not a second implementation of them.
func runDebug(t *testing.T, seam gatherSeam, args ...string) (*run, error) {
	t.Helper()

	r := &run{out: &bytes.Buffer{}, err: &bytes.Buffer{}, dir: filepath.Join(t.TempDir(), "artifacts")}
	streams := genericiooptions.IOStreams{In: strings.NewReader(""), Out: r.out, ErrOut: r.err}

	factory := &fakeFactory{
		clientset: fake.NewSimpleClientset(cluster()...),
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(), customResourceListKinds()),
		config: offlineConfig(),
	}
	o := &cmd.Options{
		Streams: streams,
		Factory: factory,
		Logger:  output.NewLogger(streams, output.LoggerOptions{NoColor: true}),
	}

	gather := func(opts *mustgather.Options) error {
		opts.FetchPodFile = seam.fetch
		if opts.FetchPodFile == nil {
			opts.FetchPodFile = func(_ context.Context, pod *corev1.Pod, destPath string) error {
				return os.WriteFile(destPath, []byte(pod.Spec.NodeName), 0o600)
			}
		}
		opts.RunInPod = seam.runInPod
		if opts.RunInPod == nil {
			opts.RunInPod = func(_ context.Context, pod *corev1.Pod, _, _ string) (string, error) {
				return pod.Spec.NodeName, nil
			}
		}

		return mustgather.Run(opts, factory, o.Log())
	}

	parent := &cobra.Command{Use: "kubectl-nv", Version: "v9.9.9"}
	parent.AddCommand(debugcmd.NewCommand(o, gather))
	parent.SetArgs(append([]string{"debug", "--artifacts-dir", r.dir}, args...))
	parent.SetOut(r.out)
	parent.SetErr(r.err)
	parent.SilenceUsage = true
	parent.SilenceErrors = true

	return r, parent.Execute()
}

// reportJSON is the DebugReport as a consumer parses it.
type reportJSON struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Node struct {
		Kind     string `json:"kind"`
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Capacity int64 `json:"capacity"`
		Operands []struct {
			Name      string `json:"name"`
			Component string `json:"component"`
		} `json:"operands"`
	} `json:"node"`
	Bundle struct {
		Directory  string `json:"directory"`
		Manifest   string `json:"manifest"`
		Collectors struct {
			OK      int `json:"ok"`
			Skipped int `json:"skipped"`
			Error   int `json:"error"`
		} `json:"collectors"`
		Skipped []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"skipped"`
		Errored []struct {
			Name   string `json:"name"`
			Reason string `json:"reason"`
		} `json:"errored"`
		Files int   `json:"files"`
		Bytes int64 `json:"bytes"`
	} `json:"bundle"`
	Status struct {
		Findings    []any `json:"findings"`
		Partial     bool  `json:"partial"`
		Unreachable []struct {
			Resource string `json:"resource"`
			Node     string `json:"node"`
			Reason   string `json:"reason"`
		} `json:"unreachable"`
	} `json:"status"`
}

// decodeReport parses the JSON a run put on stdout.
func decodeReport(t *testing.T, r *run) reportJSON {
	t.Helper()

	var got reportJSON
	if err := json.Unmarshal(r.out.Bytes(), &got); err != nil {
		t.Fatalf("decoding the report: %v\n%s", err, r.out.String())
	}

	return got
}

// TestDebugReportShape pins what a consumer gets at -o json.
func TestDebugReportShape(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "worker-1", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv debug: %v\nstderr: %s", err, r.err)
	}

	got := decodeReport(t, r)

	if got.APIVersion != output.APIVersion {
		t.Errorf("apiVersion = %q, want %q", got.APIVersion, output.APIVersion)
	}
	if got.Kind != output.KindDebugReport {
		t.Errorf("kind = %q, want %q", got.Kind, output.KindDebugReport)
	}
	if got.Metadata.Name != "worker-1" {
		t.Errorf("metadata.name = %q, want worker-1", got.Metadata.Name)
	}

	// The node is the inventory's own GPUNode, stamped as one.
	if got.Node.Kind != output.KindGPUNode {
		t.Errorf("node.kind = %q, want %q", got.Node.Kind, output.KindGPUNode)
	}
	if got.Node.Metadata.Name != "worker-1" {
		t.Errorf("node.metadata.name = %q, want worker-1", got.Node.Metadata.Name)
	}
	if got.Node.Capacity != 4 {
		t.Errorf("node.capacity = %d, want 4", got.Node.Capacity)
	}
	if len(got.Node.Operands) != len(laneOperands) {
		t.Errorf("node has %d operands, want %d", len(got.Node.Operands), len(laneOperands))
	}

	if got.Bundle.Directory != r.dir {
		t.Errorf("bundle.directory = %q, want %q", got.Bundle.Directory, r.dir)
	}
	if want := filepath.Join(r.dir, mustgather.ManifestFileName); got.Bundle.Manifest != want {
		t.Errorf("bundle.manifest = %q, want %q", got.Bundle.Manifest, want)
	}

	// The counts are the gatherer's own statuses, so they have to add up to
	// the registry rather than to whatever this command chose to look at.
	total := got.Bundle.Collectors.OK + got.Bundle.Collectors.Skipped + got.Bundle.Collectors.Error
	if want := len(mustgather.Collectors()); total != want {
		t.Errorf("the counts add up to %d collectors, want the registry's %d", total, want)
	}
	if got.Bundle.Collectors.OK == 0 {
		t.Error("no collector is counted ok, so this gather proved nothing")
	}
	if len(got.Bundle.Skipped) != got.Bundle.Collectors.Skipped {
		t.Errorf("%d skipped collectors are named but the count says %d",
			len(got.Bundle.Skipped), got.Bundle.Collectors.Skipped)
	}
	if len(got.Bundle.Errored) != got.Bundle.Collectors.Error {
		t.Errorf("%d errored collectors are named but the count says %d",
			len(got.Bundle.Errored), got.Bundle.Collectors.Error)
	}

	// Counted off the bundle as it landed, not off what the collectors meant
	// to write.
	entries, derr := os.ReadDir(r.dir)
	if derr != nil {
		t.Fatalf("reading the bundle: %v", derr)
	}
	var files int
	var bytes int64
	for _, e := range entries {
		if e.Name() == mustgather.ManifestFileName {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			continue
		}
		files++
		bytes += info.Size()
	}
	if got.Bundle.Files != files {
		t.Errorf("bundle.files = %d, the directory holds %d indexed files", got.Bundle.Files, files)
	}
	if got.Bundle.Bytes != bytes {
		t.Errorf("bundle.bytes = %d, the indexed files are %d bytes", got.Bundle.Bytes, bytes)
	}
}

// TestDebugReportIsNeverNull covers the guarantee on the wire: a consumer
// ranges over every list without a null check first.
func TestDebugReportIsNeverNull(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "worker-1", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv debug: %v\nstderr: %s", err, r.err)
	}

	var raw map[string]any
	if uerr := json.Unmarshal(r.out.Bytes(), &raw); uerr != nil {
		t.Fatalf("decoding the report: %v\n%s", uerr, r.out.String())
	}

	bundle, ok := raw["bundle"].(map[string]any)
	if !ok {
		t.Fatalf("bundle is %T, want an object", raw["bundle"])
	}
	for _, key := range []string{"skipped", "errored"} {
		if _, isArray := bundle[key].([]any); !isArray {
			t.Errorf("bundle.%s is %#v, want an array", key, bundle[key])
		}
	}

	status, ok := raw["status"].(map[string]any)
	if !ok {
		t.Fatalf("status is %T, want an object", raw["status"])
	}
	if _, isArray := status["findings"].([]any); !isArray {
		t.Errorf("status.findings is %#v, want an array", status["findings"])
	}
}

// TestDebugYAMLCarriesTheSameReport covers the second machine format. The
// printers marshal through the JSON tags, so a field that YAML spells
// differently is a field a consumer cannot find.
func TestDebugYAMLCarriesTheSameReport(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "worker-1", "-o", "yaml")
	if err != nil {
		t.Fatalf("kubectl nv debug: %v\nstderr: %s", err, r.err)
	}

	var got reportJSON
	if uerr := yaml.Unmarshal(r.out.Bytes(), &got); uerr != nil {
		t.Fatalf("the YAML report does not parse: %v\n%s", uerr, r.out.String())
	}

	if got.Kind != output.KindDebugReport {
		t.Errorf("kind = %q, want %q", got.Kind, output.KindDebugReport)
	}
	if got.Metadata.Name != "worker-1" {
		t.Errorf("metadata.name = %q, want worker-1", got.Metadata.Name)
	}
	if got.Node.Metadata.Name != "worker-1" {
		t.Errorf("node.metadata.name = %q, want worker-1", got.Node.Metadata.Name)
	}
}

// TestDebugNameOutput covers -o name, which is what a shell pipeline reads.
func TestDebugNameOutput(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "worker-1", "-o", "name")
	if err != nil {
		t.Fatalf("kubectl nv debug: %v\nstderr: %s", err, r.err)
	}

	if got, want := r.out.String(), "debugreport/worker-1\n"; got != want {
		t.Errorf("-o name printed %q, want %q", got, want)
	}
}

// maxSummaryLines is the one-screen promise. The whole point of this command
// over "info plus must-gather" is that the summary fits in a terminal, so the
// bound is asserted rather than hoped for.
const maxSummaryLines = 30

// TestDebugTableFitsOneScreen covers the default output. The node has six
// operands, which is what a full GPU Operator install puts on one.
func TestDebugTableFitsOneScreen(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "worker-1")
	if err != nil {
		t.Fatalf("kubectl nv debug: %v\nstderr: %s", err, r.err)
	}

	text := r.out.String()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) > maxSummaryLines {
		t.Errorf("the summary is %d lines, want at most %d:\n%s", len(lines), maxSummaryLines, text)
	}

	// What has to be on that screen. The GPU triple and the two next commands
	// are the whole reason it is a summary rather than a dump.
	for _, want := range []string{
		"worker-1",
		"NVIDIA-T4",
		"0/4/4",
		"550.107.02",
		"12.5",
		"device-plugin",
		"upgrade-done",
		r.dir,
		"kubectl nv health --node worker-1",
		"kubectl nv health --from-bundle " + r.dir,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, text)
		}
	}

	// One line per operand, each saying what it is and how it is doing.
	for _, app := range laneOperands {
		line := app + "-on-worker-1"
		if !strings.Contains(text, line) {
			t.Errorf("the summary does not list the operand %q:\n%s", line, text)
		}
	}
}

// TestDebugRejectsANodeWithNoGPUs pins the message and the exit code. It is
// the one kubectl nv info uses: a user who typed the same name into both
// commands must not be told two different things.
func TestDebugRejectsANodeWithNoGPUs(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "plain-worker")
	if err == nil {
		t.Fatalf("kubectl nv debug plain-worker returned nil, want a usage error\nstdout: %s", r.out)
	}
	if got, want := err.Error(), "node plain-worker has no GPUs and no GPU labels"; got != want {
		t.Errorf("error = %q, want exactly %q", got, want)
	}
	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code = %d, want %d", got, output.ExitUsage)
	}

	// Nothing was gathered: the name was wrong, so there is nothing to send.
	if _, serr := os.Stat(r.dir); !os.IsNotExist(serr) {
		t.Errorf("a bundle was written to %s for a node that has no GPUs", r.dir)
	}
}

// TestDebugRejectsAnUnknownNode covers the typo, which is the common case.
func TestDebugRejectsAnUnknownNode(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "worker-77")
	if err == nil {
		t.Fatalf("kubectl nv debug worker-77 returned nil, want a usage error\nstdout: %s", r.out)
	}
	if !strings.Contains(err.Error(), "worker-77") {
		t.Errorf("error = %q, want it to name the node that was asked for", err.Error())
	}
	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code = %d, want %d", got, output.ExitUsage)
	}
}

// TestDebugAcceptsAHintedNode covers the node a user most often asks about:
// one that carries an NVIDIA label and advertises no GPUs. That it should have
// GPUs and does not is the answer, so it is gathered rather than refused.
func TestDebugAcceptsAHintedNode(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "control-plane", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv debug control-plane: %v\nstderr: %s", err, r.err)
	}
	if got := decodeReport(t, r); got.Metadata.Name != "control-plane" {
		t.Errorf("metadata.name = %q, want control-plane", got.Metadata.Name)
	}
}

// TestDebugTakesExactlyOneNode covers the argument count on both sides.
func TestDebugTakesExactlyOneNode(t *testing.T) {
	t.Parallel()

	for name, args := range map[string][]string{
		"no node at all": {},
		"two nodes":      {"worker-1", "worker-2"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := runDebug(t, gatherSeam{}, args...)
			if err == nil {
				t.Fatalf("kubectl nv debug %v returned nil, want a usage error", args)
			}
			if got := output.ExitCodeFor(err); got != output.ExitUsage {
				t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitUsage, err)
			}
		})
	}
}

// errDeniedExec is the API server refusing the subresource, in the shape a denial
// actually arrives in: an upgrade that failed before any Status was decoded.
var errDeniedExec = errors.New(`unable to upgrade connection: pods "nvidia-driver-daemonset-on-worker-1" ` +
	`is forbidden: User "system:serviceaccount:support:reader" cannot create resource "pods/exec"`)

// TestDebugExitsPartialWhenTheExecIsDenied is the difference between asking
// for no driver evidence and being told no. A run that reported exit 0 here
// would file a support ticket that cannot be answered.
func TestDebugExitsPartialWhenTheExecIsDenied(t *testing.T) {
	t.Parallel()

	seam := gatherSeam{
		fetch:    func(context.Context, *corev1.Pod, string) error { return errDeniedExec },
		runInPod: func(context.Context, *corev1.Pod, string, string) (string, error) { return "", errDeniedExec },
	}

	r, err := runDebug(t, seam, "worker-1", "-o", "json")
	if err == nil {
		t.Fatalf("kubectl nv debug returned nil, want the denied exec reported\nstdout: %s", r.out)
	}
	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitPartial, err)
	}

	// The report is still printed: the bundle is worth sending, and what is
	// missing from it is exactly what the report has to say.
	got := decodeReport(t, r)
	if !got.Status.Partial {
		t.Error("status.partial is false, but the gather was refused every exec")
	}
	if len(got.Status.Unreachable) == 0 {
		t.Error("status.unreachable is empty, so nothing says which part of the cluster is missing")
	}

	var denied []string
	for _, s := range got.Bundle.Skipped {
		if strings.HasPrefix(s.Reason, "forbidden") {
			denied = append(denied, s.Name)
		}
	}
	sort.Strings(denied)
	if want := []string{"bug-report", "device-usage"}; strings.Join(denied, ",") != strings.Join(want, ",") {
		t.Errorf("the collectors recorded as forbidden are %v, want %v", denied, want)
	}
}

// TestDebugNoExecSucceeds covers the user who will never have the exec. Being
// told no is not the same as asking for no: this exits 0, and the collectors
// it did not run are counted as skipped rather than as ok.
func TestDebugNoExecSucceeds(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "worker-1", "--no-exec", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv debug --no-exec: %v\nstderr: %s", err, r.err)
	}
	if got := output.ExitCodeFor(err); got != output.ExitOK {
		t.Errorf("exit code = %d, want %d", got, output.ExitOK)
	}

	got := decodeReport(t, r)
	if got.Status.Partial {
		t.Error("status.partial is true, but --no-exec is the user asking for exactly this")
	}

	skipped := map[string]string{}
	for _, s := range got.Bundle.Skipped {
		skipped[s.Name] = s.Reason
	}
	for _, collector := range []string{"bug-report", "device-usage"} {
		if got, ok := skipped[collector]; !ok || got != "--no-exec" {
			t.Errorf("%s is recorded as %q (listed: %v), want it skipped for --no-exec",
				collector, got, skipped)
		}
	}

	for _, name := range bundleNames(t, r.dir) {
		if strings.HasPrefix(name, "nvidia-bug-report_") {
			t.Errorf("the bundle holds %s, but --no-exec was given", name)
		}
	}
}

// bundleNames lists what a bundle directory holds.
func bundleNames(t *testing.T, dir string) []string {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the bundle: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}

	return names
}

// TestDebugScopesTheGatherToTheNode is the whole reason this command is not
// "info and then must-gather". A gather that collected the cluster would hand
// the user the other nodes' evidence and say it was about this one.
func TestDebugScopesTheGatherToTheNode(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "worker-1")
	if err != nil {
		t.Fatalf("kubectl nv debug: %v\nstderr: %s", err, r.err)
	}

	// The manifest is the gatherer's own record of what it was asked for.
	data, rerr := os.ReadFile(filepath.Join(r.dir, mustgather.ManifestFileName))
	if rerr != nil {
		t.Fatalf("reading the bundle manifest: %v", rerr)
	}
	var m struct {
		Flags struct {
			Node string `json:"node"`
		} `json:"flags"`
	}
	if uerr := json.Unmarshal(data, &m); uerr != nil {
		t.Fatalf("the manifest does not parse: %v\n%s", uerr, data)
	}
	if m.Flags.Node != "worker-1" {
		t.Errorf("manifest flags.node = %q, want worker-1", m.Flags.Node)
	}

	// Compared whole: "worker-1" is a prefix of nothing here, but a bundle for
	// worker-10 would pass a substring check against worker-1.
	names := bundleNames(t, r.dir)
	if !contains(names, "nvidia-bug-report_worker-1.log.gz") {
		t.Errorf("the bundle holds no bug report for worker-1: %v", names)
	}
	if contains(names, "nvidia-bug-report_worker-2.log.gz") {
		t.Errorf("the bundle holds worker-2's bug report, which was not asked for: %v", names)
	}
}

func contains(names []string, want string) bool {
	for _, name := range names {
		if name == want {
			return true
		}
	}

	return false
}

// TestDebugGatherFlagsMatchMustGather pins the promise that this is the same
// gather. A flag that drifted in name, default or help text here would mean a
// support engineer who learned must-gather has to learn this command too, and
// a script that passes the same arguments to both would stop working.
func TestDebugGatherFlagsMatchMustGather(t *testing.T) {
	t.Parallel()

	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	noop := func(*mustgather.Options) error { return nil }

	want := gatherFlagsOf(t, mustGatherCommand(t, adm.NewCommand(o, noop)))
	got := gatherFlagsOf(t, debugcmd.NewCommand(o, noop))

	if len(want) == 0 {
		t.Fatal("adm must-gather declares none of the gather flags, so this compared nothing")
	}
	for name, spec := range want {
		mine, ok := got[name]
		if !ok {
			t.Errorf("kubectl nv debug has no --%s, which adm must-gather declares", name)

			continue
		}
		if mine != spec {
			t.Errorf("--%s is %+v on debug and %+v on adm must-gather", name, mine, spec)
		}
	}
}

// flagSpec is a flag as a user meets it.
type flagSpec struct {
	defaultValue string
	usage        string
}

// gatherFlagNames are the flags that belong to the gather itself, as opposed
// to the ones a command adds around it.
var gatherFlagNames = []string{"artifacts-dir", "since", "no-exec", "exec-timeout"}

// gatherFlagsOf reads the gather's flags off a command.
func gatherFlagsOf(t *testing.T, c *cobra.Command) map[string]flagSpec {
	t.Helper()

	out := map[string]flagSpec{}
	for _, name := range gatherFlagNames {
		f := c.Flags().Lookup(name)
		if f == nil {
			continue
		}
		out[name] = flagSpec{defaultValue: f.DefValue, usage: f.Usage}
	}

	return out
}

// mustGatherCommand digs the leaf out of the adm tree.
func mustGatherCommand(t *testing.T, admCommand *cobra.Command) *cobra.Command {
	t.Helper()

	for _, child := range admCommand.Commands() {
		if child.Name() == "must-gather" {
			return child
		}
	}
	t.Fatal("adm has no must-gather subcommand")

	return nil
}

// TestDebugDoesNotOfferTheNodeFlag covers the one flag that must not be here.
// The node is the positional argument; a --node alongside it would let the two
// disagree, and the bundle would be about whichever one the gather read.
func TestDebugDoesNotOfferTheNodeFlag(t *testing.T) {
	t.Parallel()

	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	c := debugcmd.NewCommand(o, func(*mustgather.Options) error { return nil })

	if f := c.Flags().Lookup("node"); f != nil {
		t.Error("kubectl nv debug declares --node, which would disagree with the node argument")
	}
}

// TestDebugRejectsAnUnusableWindow covers the validation the gather flags
// carry. A sub-second log window collects nothing, and the answer has to be a
// usage error rather than an empty bundle.
func TestDebugRejectsAnUnusableWindow(t *testing.T) {
	t.Parallel()

	for name, args := range map[string][]string{
		"since below a second":        {"--since", "500ms"},
		"exec timeout below a second": {"--exec-timeout", "10ms"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := runDebug(t, gatherSeam{}, append([]string{"worker-1"}, args...)...)
			if err == nil {
				t.Fatalf("kubectl nv debug %v returned nil, want a usage error", args)
			}
			if got := output.ExitCodeFor(err); got != output.ExitUsage {
				t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitUsage, err)
			}
		})
	}
}

// TestDebugExecTimeoutDefaultsToTheGathers covers the one flag whose zero
// value would be read as "no deadline at all".
func TestDebugExecTimeoutDefaultsToTheGathers(t *testing.T) {
	t.Parallel()

	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	c := debugcmd.NewCommand(o, func(*mustgather.Options) error { return nil })

	f := c.Flags().Lookup("exec-timeout")
	if f == nil {
		t.Fatal("kubectl nv debug has no --exec-timeout")
	}
	if got, want := f.DefValue, mustgather.DefaultExecTimeout.String(); got != want {
		t.Errorf("--exec-timeout defaults to %q, want %q", got, want)
	}
}

// TestDebugProgressStaysOffStdout keeps the pipeline someone writes around
// this command working: stdout is the report and nothing else.
func TestDebugProgressStaysOffStdout(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "worker-1", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv debug: %v\nstderr: %s", err, r.err)
	}

	if !json.Valid(bytes.TrimSpace(r.out.Bytes())) {
		t.Errorf("stdout is not the report on its own:\n%s", r.out.String())
	}
	if r.err.Len() == 0 {
		t.Error("nothing was reported on stderr, so the progress went somewhere else or nowhere")
	}
}
