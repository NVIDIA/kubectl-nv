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
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/adm"
	debugcmd "github.com/NVIDIA/kubectl-nv/pkg/cmd/debug"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/mcp"
	"github.com/NVIDIA/kubectl-nv/pkg/mustgather"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
	"github.com/NVIDIA/kubectl-nv/pkg/source"
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

// exactCluster is the node the exact-summary assertion is made against: more
// capacity than allocatable, a pod holding two GPUs, MIG in the mixed strategy
// mid-configuration, and two operands that are not in the same state.
//
// The operand pods are named so that nothing can read the component off the
// pod name: a summary that printed the pod name would print "dp-xyz" where
// this expects the component the rest of the tool uses.
func exactCluster() []runtime.Object {
	return []runtime.Object{
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{
				Name: "worker-1",
				Labels: map[string]string{
					"nvidia.com/gpu.product":               "NVIDIA-H100-80GB-HBM3",
					"nvidia.com/gpu.count":                 "8",
					"nvidia.com/cuda.driver-version.full":  "550.127.05",
					"nvidia.com/cuda.runtime-version.full": "12.4",
					"nvidia.com/mig.strategy":              "mixed",
					"nvidia.com/mig.config":                "all-balanced",
					"nvidia.com/mig.config.state":          "success",
					"nvidia.com/mig.capable":               "true",
					"nvidia.com/gpu-driver-upgrade-state":  "upgrade-done",
				},
			},
			Status: corev1.NodeStatus{
				Capacity:    corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(8, resource.DecimalSI)},
				Allocatable: corev1.ResourceList{"nvidia.com/gpu": *resource.NewQuantity(7, resource.DecimalSI)},
				Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionTrue, Reason: "KubeletReady"},
				},
			},
		},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-2"}},
		// Not an operand: another team's training job, which is where the two
		// requested GPUs come from.
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "trainer-0", Namespace: "team-a"},
			Spec: corev1.PodSpec{
				NodeName: "worker-1",
				Containers: []corev1.Container{{
					Name: "train",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							"nvidia.com/gpu": *resource.NewQuantity(2, resource.DecimalSI),
						},
					},
				}},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "gpu-operator-0", Namespace: operatorNamespace,
				Labels: map[string]string{"app": "gpu-operator"},
			},
			Spec:   corev1.PodSpec{NodeName: "worker-2", Containers: []corev1.Container{{Name: "gpu-operator"}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "dp-xyz", Namespace: operatorNamespace,
				Labels: map[string]string{"app": "nvidia-device-plugin-daemonset"},
			},
			Spec: corev1.PodSpec{NodeName: "worker-1", Containers: []corev1.Container{{Name: "plugin"}}},
			Status: corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "plugin", Ready: true}},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: "val-abc", Namespace: operatorNamespace,
				Labels: map[string]string{"app": "nvidia-operator-validator"},
			},
			Spec: corev1.PodSpec{NodeName: "worker-1", Containers: []corev1.Container{{Name: "validator"}}},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
				ContainerStatuses: []corev1.ContainerStatus{
					{Name: "validator", Ready: false, RestartCount: 3},
				},
			},
		},
	}
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

// reactor is one API call the fixture cluster answers with an error instead
// of with objects.
type reactor struct {
	verb     string
	resource string
	err      error
}

// fixture is the cluster and the environment one run sees.
type fixture struct {
	// objects is the cluster. The empty value is cluster().
	objects []runtime.Object
	// reactors are the reads the cluster refuses, installed on the typed
	// client: the inventory reads nodes, pods and resourceslices through it.
	reactors []reactor
	// seam replaces the two calls that would reach into a pod.
	seam gatherSeam
	// dir overrides the artifacts directory. The empty value is a fresh one
	// under the test's own temporary directory.
	dir string
	// noDirFlag leaves --artifacts-dir off the command line entirely.
	noDirFlag bool
	// kubeContext is the kubeconfig context the manifest should record. It is
	// never empty by default: an empty one would send kubeconfigContext to the
	// loader, and so to whatever kubeconfig the machine running the test has.
	kubeContext string
	// version is what the root reports, which the manifest records as the tool
	// that produced the bundle.
	version string
	// manifestCorruption overwrites the bundle's index with something that
	// is not JSON once the gather has written it, which is what a truncated
	// write or a full disk leaves behind.
	manifestCorruption bool
}

// runDebug executes kubectl nv debug over the default fixture cluster.
func runDebug(t *testing.T, seam gatherSeam, args ...string) (*run, error) {
	t.Helper()

	return runDebugWith(t, fixture{seam: seam}, args...)
}

// runDebugWith executes kubectl nv debug over the given fixture, running the
// real gatherer: the bundle, its manifest and the collector statuses this
// command reports are the gatherer's own, not a second implementation of them.
func runDebugWith(t *testing.T, f fixture, args ...string) (*run, error) {
	t.Helper()

	if f.objects == nil {
		f.objects = cluster()
	}
	if f.dir == "" {
		f.dir = filepath.Join(t.TempDir(), "artifacts")
	}
	if f.kubeContext == "" {
		f.kubeContext = "fixture-context"
	}
	if f.version == "" {
		f.version = "v9.9.9"
	}

	r := &run{out: &bytes.Buffer{}, err: &bytes.Buffer{}, dir: f.dir}
	streams := genericiooptions.IOStreams{In: strings.NewReader(""), Out: r.out, ErrOut: r.err}

	clientset := fake.NewSimpleClientset(f.objects...)
	for _, re := range f.reactors {
		err := re.err
		clientset.PrependReactor(re.verb, re.resource,
			func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, err })
	}

	factory := &fakeFactory{
		clientset: clientset,
		dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(), customResourceListKinds()),
		config: offlineConfig(),
	}
	o := &cmd.Options{
		Streams:     streams,
		Factory:     factory,
		Logger:      output.NewLogger(streams, output.LoggerOptions{NoColor: true}),
		ConfigFlags: isolatedConfigFlags(t, f.kubeContext),
	}

	gather := func(opts *mustgather.Options) error {
		opts.FetchPodFile = f.seam.fetch
		if opts.FetchPodFile == nil {
			opts.FetchPodFile = func(_ context.Context, pod *corev1.Pod, destPath string) error {
				return os.WriteFile(destPath, []byte(pod.Spec.NodeName), 0o600)
			}
		}
		opts.RunInPod = f.seam.runInPod
		if opts.RunInPod == nil {
			opts.RunInPod = func(_ context.Context, pod *corev1.Pod, _, _ string) (string, error) {
				return pod.Spec.NodeName, nil
			}
		}

		err := mustgather.Run(opts, factory, o.Log())
		if f.manifestCorruption {
			path := filepath.Join(opts.ArtifactDir, mustgather.ManifestFileName)
			if werr := os.WriteFile(path, []byte("{this is not json"), 0o600); werr != nil {
				t.Fatalf("corrupting the bundle index: %v", werr)
			}
		}

		return err
	}

	parent := &cobra.Command{Use: "kubectl-nv", Version: f.version}
	parent.AddCommand(debugcmd.NewCommand(o, gather))
	full := []string{"debug"}
	if !f.noDirFlag {
		full = append(full, "--artifacts-dir", r.dir)
	}
	parent.SetArgs(append(full, args...))
	parent.SetOut(r.out)
	parent.SetErr(r.err)
	parent.SilenceUsage = true
	parent.SilenceErrors = true

	return r, parent.Execute()
}

// isolatedConfigFlags points the kubectl configuration at a kubeconfig that
// does not exist and names the context explicitly. A test must never read the
// kubeconfig of whoever is running it, and an empty context would send
// kubeconfigContext to the loader to find out what the current one is.
func isolatedConfigFlags(t *testing.T, context string) *genericclioptions.ConfigFlags {
	t.Helper()

	flags := genericclioptions.NewConfigFlags(false)
	path := filepath.Join(t.TempDir(), "no-kubeconfig-here")
	flags.KubeConfig = &path
	flags.Context = &context

	return flags
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

// maxSummaryLines is the one-screen promise, asserted here against a run of
// the whole command. How the bound is held whatever the cluster says is
// pkg/cmd/debug's own summary_test.go, which drives the renderer directly.
const maxSummaryLines = 30

// TestDebugSummaryIsExactly pins every line of the summary against a literal.
//
// Two lines carry what only this run knows: the bundle's directory, and how
// many files it came to. The directory is substituted in from the run, and the
// file count is compared against the directory as it landed on disk. Every
// other character is written out here, because a summary whose fields are
// only checked for being present is a summary that can print the wrong node's
// numbers and stay green.
func TestDebugSummaryIsExactly(t *testing.T) {
	t.Parallel()

	r, err := runDebugWith(t, fixture{objects: exactCluster()}, "worker-1")
	if err != nil {
		t.Fatalf("kubectl nv debug: %v\nstderr: %s", err, r.err)
	}

	lines := strings.Split(strings.TrimRight(r.out.String(), "\n"), "\n")
	if len(lines) > maxSummaryLines {
		t.Errorf("the summary is %d lines, want at most %d", len(lines), maxSummaryLines)
	}

	want := []string{
		"Node:            worker-1",
		"Product:         NVIDIA-H100-80GB-HBM3",
		"GPUs:            2/7/8  (requested/allocatable/capacity)",
		"Driver:          550.127.05  (CUDA 12.4)",
		"Mode:            device-plugin",
		"MIG:             mixed  config=all-balanced (success)",
		"Upgrade state:   upgrade-done",
		"Status:          Ready",
		"Operands:",
		"  nvidia-device-plugin-daemonset     Running   ready=true  restarts=0",
		"  nvidia-operator-validator          Pending   ready=false restarts=3",
		"Bundle:          " + r.dir,
	}
	for i, wantLine := range want {
		if i >= len(lines) {
			t.Fatalf("the summary stops after %d lines, want at least %d:\n%s",
				len(lines), len(want), r.out.String())
		}
		if lines[i] != wantLine {
			t.Errorf("line %d is\n  %q\nwant\n  %q", i+1, lines[i], wantLine)
		}
	}

	// The counts are the gatherer's own, taken from the manifest the test
	// parses for itself rather than from the line being checked.
	ok, skipped, failed := manifestCounts(t, r.dir)
	files, _ := bundleSize(t, r.dir)
	collectors := lines[len(want)]
	wantPrefix := fmt.Sprintf("Collectors:      %d ok, %d skipped, %d error  (%d files, ",
		ok, skipped, failed, files)
	if !strings.HasPrefix(collectors, wantPrefix) {
		t.Errorf("the collectors line is\n  %q\nwant it to start with\n  %q", collectors, wantPrefix)
	}

	// The last three lines are the two commands to run next, whole: the point
	// of the summary is that a reader can copy one of them.
	tail := lines[len(lines)-3:]
	wantTail := []string{
		"Next:",
		"  kubectl nv health --node worker-1",
		"  kubectl nv health --from-bundle " + r.dir,
	}
	for i := range wantTail {
		if tail[i] != wantTail[i] {
			t.Errorf("the tail line %d is\n  %q\nwant\n  %q", i+1, tail[i], wantTail[i])
		}
	}

	// Whatever is between the collectors line and the tail is the folded
	// absence line, and nothing else: this cluster refused nothing.
	middle := lines[len(want)+1 : len(lines)-3]
	if len(middle) != 1 {
		t.Fatalf("there are %d lines between the collectors and the next commands, want 1:\n%s",
			len(middle), strings.Join(middle, "\n"))
	}
	// The folded line names as many collectors as fit and counts the rest.
	// Every one of them is in -o json, which TestDebugReportShape checks
	// against the manifest; what is pinned here is that the screen says how
	// many it did not have room for rather than ending mid-name.
	wantFolded := "  skipped (nothing to collect): openshift-version, gpu-cluster, hyperconverged (+5 more)"
	if middle[0] != wantFolded {
		t.Errorf("the folded line is\n  %q\nwant\n  %q", middle[0], wantFolded)
	}
}

// TestDebugSummarySaysWhenItIsIncomplete is the same screen for the run that
// was refused the driver evidence. The status line is the one a reader scans
// first, so it has to carry the verdict, and the refusal has to be on a line
// of its own rather than folded in with the things that were simply absent.
func TestDebugSummarySaysWhenItIsIncomplete(t *testing.T) {
	t.Parallel()

	seam := gatherSeam{
		fetch:    func(context.Context, *corev1.Pod, string) error { return errDeniedExec },
		runInPod: func(context.Context, *corev1.Pod, string, string) (string, error) { return "", errDeniedExec },
	}

	r, err := runDebugWith(t, fixture{objects: cluster(), seam: seam}, "worker-1")
	if err == nil {
		t.Fatal("kubectl nv debug returned nil, want the denied exec reported")
	}

	lines := strings.Split(strings.TrimRight(r.out.String(), "\n"), "\n")
	if len(lines) > maxSummaryLines {
		t.Errorf("the summary is %d lines, want at most %d", len(lines), maxSummaryLines)
	}

	wantStatus := "Status:          Ready  (partial: see status.unreachable at -o json)"
	if !containsLine(lines, wantStatus) {
		t.Errorf("the status line does not say the run is incomplete:\nwant %q\ngot\n%s",
			wantStatus, r.out.String())
	}

	// The two collectors the exec was needed for, each on a line of its own
	// with the refusal named, cut to the width of the screen. These are the
	// gatherer's own words: what this command owes a reader is that they
	// arrive whole as far as the line goes, and are not folded in with the
	// collectors that simply had nothing to collect.
	for _, want := range []string{
		"  skipped bug-report: forbidden: collecting bug report (label app=nvidia-driver...",
		"  skipped device-usage: forbidden: device usage from pod nvidia-driver-daemonset-...",
	} {
		if !containsLine(lines, want) {
			t.Errorf("the refused collector is not named on a line of its own:\nwant %q\ngot\n%s",
				want, r.out.String())
		}
	}

	// The absences are still folded, so a refusal never costs its line to a
	// resource nobody installed.
	if !containsLine(lines,
		"  skipped (nothing to collect): openshift-version, gpu-cluster, hyperconverged (+3 more)") {
		t.Errorf("the folded line is not what was expected:\n%s", r.out.String())
	}
}

// containsLine reports whether lines holds want, compared whole.
func containsLine(lines []string, want string) bool {
	for _, line := range lines {
		if line == want {
			return true
		}
	}

	return false
}

// manifestCounts classifies the bundle's own index, so that a count printed in
// the summary is checked against the gatherer rather than against itself.
func manifestCounts(t *testing.T, dir string) (ok, skipped, failed int) {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(dir, mustgather.ManifestFileName))
	if err != nil {
		t.Fatalf("reading the bundle manifest: %v", err)
	}
	var m struct {
		Collectors []struct {
			Status string `json:"status"`
		} `json:"collectors"`
	}
	if uerr := json.Unmarshal(raw, &m); uerr != nil {
		t.Fatalf("the manifest does not parse: %v", uerr)
	}

	for _, c := range m.Collectors {
		switch {
		case c.Status == mustgather.StatusOK:
			ok++
		case strings.HasPrefix(c.Status, "skipped:"):
			skipped++
		default:
			failed++
		}
	}

	return ok, skipped, failed
}

// bundleSize counts the files a bundle holds and what they add up to, off the
// directory rather than off the manifest.
func bundleSize(t *testing.T, dir string) (int, int64) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the bundle: %v", err)
	}

	var files int
	var total int64
	for _, e := range entries {
		if e.Name() == mustgather.ManifestFileName {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil || !info.Mode().IsRegular() {
			continue
		}
		files++
		total += info.Size()
	}

	return files, total
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
	// The same message kubectl nv info gives, compared whole: a user who typed
	// the same name into both commands must not be told two different things,
	// and a script branching on it reads the words.
	if got, want := err.Error(), "node worker-77 has no GPUs and no GPU labels"; got != want {
		t.Errorf("error = %q, want exactly %q", got, want)
	}
	if got := output.ExitCodeFor(err); got != output.ExitUsage {
		t.Errorf("exit code = %d, want %d", got, output.ExitUsage)
	}
	if _, serr := os.Stat(r.dir); !os.IsNotExist(serr) {
		t.Errorf("a bundle was written to %s for a node the cluster does not have", r.dir)
	}
	if r.out.Len() != 0 {
		t.Errorf("stdout is %q, want nothing: there is no report to print", r.out.String())
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

	// The gather's own error, not a report that something is missing. Being
	// refused the exec is the one thing here the user can take to their
	// cluster administrator, so it has to survive to the message they read.
	var gatherErr *mustgather.Error
	if !errors.As(err, &gatherErr) {
		t.Fatalf("error is %T (%v), want it to unwrap to a *mustgather.Error", err, err)
	}
	if want := "not allowed to exec into the driver pods"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to say %q", err.Error(), want)
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
// gather. A flag that drifted in name, type, default or help text would mean a
// support engineer who learned must-gather has to learn this command too, and
// a script passing the same arguments to both would stop working.
//
// The comparison is driven by AddGatherFlags into a scratch set rather than by
// a list written out here, so a flag added to the gather is required on both
// commands without this test being edited. Both commands now register from that
// helper, so this is a floor under the arrangement rather than the only thing
// holding it up: it goes red if either one starts registering its own again.
func TestDebugGatherFlagsMatchMustGather(t *testing.T) {
	t.Parallel()

	t.Run("every gather flag is on both commands, spelled the same", func(t *testing.T) {
		t.Parallel()

		debugFlags, admFlags := bothCommands(t)

		scratch := pflag.NewFlagSet("gather", pflag.ContinueOnError)
		mustgather.AddGatherFlags(scratch, &mustgather.Options{})

		declared := 0
		scratch.VisitAll(func(f *pflag.Flag) {
			declared++
			for command, set := range map[string]*pflag.FlagSet{"debug": debugFlags, "adm must-gather": admFlags} {
				got := set.Lookup(f.Name)
				if got == nil {
					t.Errorf("%s has no --%s, which the gather declares", command, f.Name)

					continue
				}
				if got.Value.Type() != f.Value.Type() {
					t.Errorf("%s --%s is a %s, the gather declares a %s",
						command, f.Name, got.Value.Type(), f.Value.Type())
				}
				if got.DefValue != f.DefValue {
					t.Errorf("%s --%s defaults to %q, the gather declares %q",
						command, f.Name, got.DefValue, f.DefValue)
				}
				if got.Usage != f.Usage {
					t.Errorf("%s --%s says\n  %q\nthe gather says\n  %q",
						command, f.Name, got.Usage, f.Usage)
				}
			}
		})
		if declared == 0 {
			t.Fatal("AddGatherFlags declared nothing, so this compared nothing")
		}
	})

	t.Run("debug offers everything must-gather does except the node flag", func(t *testing.T) {
		t.Parallel()

		debugFlags, admFlags := bothCommands(t)

		seen := 0
		admFlags.VisitAll(func(f *pflag.Flag) {
			// The node is debug's positional argument. A command offering both
			// would let the two disagree about which node the bundle is of.
			if f.Name == "node" {
				return
			}
			seen++
			if debugFlags.Lookup(f.Name) == nil {
				t.Errorf("adm must-gather has --%s and debug does not", f.Name)
			}
		})
		if seen == 0 {
			t.Fatal("adm must-gather declares no local flags, so this compared nothing")
		}
		if debugFlags.Lookup("node") != nil {
			t.Error("debug declares --node, which would disagree with its node argument")
		}
	})

	t.Run("both refuse the same unusable windows with the same words", func(t *testing.T) {
		t.Parallel()

		for _, args := range [][]string{{"--since", "999ms"}, {"--since", "-1h"}} {
			debugErr := errorFromDebug(t, args)
			admErr := errorFromMustGather(t, args)

			switch {
			case debugErr == nil || admErr == nil:
				t.Errorf("%v: debug returned %v and adm must-gather returned %v, want both to refuse it",
					args, debugErr, admErr)
			case debugErr.Error() != admErr.Error():
				t.Errorf("%v: debug says\n  %q\nadm must-gather says\n  %q",
					args, debugErr.Error(), admErr.Error())
			case output.ExitCodeFor(debugErr) != output.ExitCodeFor(admErr):
				t.Errorf("%v: debug exits %d and adm must-gather exits %d",
					args, output.ExitCodeFor(debugErr), output.ExitCodeFor(admErr))
			}
		}
	})
}

// TestArtifactDirDefaultsToTheEnvironmentOnBothCommands covers the one default
// that is not a constant. It is read when the flag is registered, so a command
// that captured it any earlier, or that reached for the variable itself at run
// time, would answer differently from the other one.
//
// It is not parallel, and neither is it a subtest of a parallel test: it sets
// an environment variable, which the whole process shares.
func TestArtifactDirDefaultsToTheEnvironmentOnBothCommands(t *testing.T) {
	const dir = "/tmp/artifact-dir-from-the-environment"
	t.Setenv("ARTIFACT_DIR", dir)

	debugFlags, admFlags := bothCommands(t)

	for command, set := range map[string]*pflag.FlagSet{"debug": debugFlags, "adm must-gather": admFlags} {
		f := set.Lookup("artifacts-dir")
		if f == nil {
			t.Errorf("%s has no --artifacts-dir", command)

			continue
		}
		if f.DefValue != dir {
			t.Errorf("%s --artifacts-dir defaults to %q, want the environment's %q",
				command, f.DefValue, dir)
		}
	}
}

// bothCommands builds the two commands that run the gather and returns their
// flag sets.
func bothCommands(t *testing.T) (debugFlags, admFlags *pflag.FlagSet) {
	t.Helper()

	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	noop := func(*mustgather.Options) error { return nil }

	admCommand := adm.NewCommand(o, noop)
	for _, child := range admCommand.Commands() {
		if child.Name() == "must-gather" {
			return debugcmd.NewCommand(o, noop).Flags(), child.Flags()
		}
	}
	t.Fatal("adm has no must-gather subcommand")

	return nil, nil
}

// errorFromDebug runs kubectl nv debug with the given flags over a cluster it
// must never reach, because the flags are refused before anything is read.
func errorFromDebug(t *testing.T, args []string) error {
	t.Helper()

	_, err := runDebug(t, gatherSeam{}, append([]string{"worker-1"}, args...)...)

	return err
}

// errorFromMustGather runs kubectl nv adm must-gather with the given flags.
func errorFromMustGather(t *testing.T, args []string) error {
	t.Helper()

	o := &cmd.Options{
		Streams:     genericiooptions.NewTestIOStreamsDiscard(),
		ConfigFlags: isolatedConfigFlags(t, "fixture-context"),
	}
	admCommand := adm.NewCommand(o, func(*mustgather.Options) error { return nil })

	parent := &cobra.Command{Use: "kubectl-nv", Version: "v9.9.9"}
	parent.AddCommand(admCommand)
	parent.SetArgs(append([]string{"adm", "must-gather", "--artifacts-dir", t.TempDir()}, args...))
	parent.SetOut(io.Discard)
	parent.SetErr(io.Discard)
	parent.SilenceUsage = true
	parent.SilenceErrors = true

	return parent.Execute()
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
// carry, on both sides of the floor. A sub-second log window collects nothing
// and a sub-second exec timeout cannot finish a bug report; a window of
// exactly a second is the smallest one that can, and rejecting it would be a
// gate that fires on the value it was meant to allow.
func TestDebugRejectsAnUnusableWindow(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		args   []string
		reject bool
	}{
		"since just below a second":        {args: []string{"--since", "999ms"}, reject: true},
		"negative since":                   {args: []string{"--since", "-1h"}, reject: true},
		"exec timeout just below a second": {args: []string{"--exec-timeout", "999ms"}, reject: true},
		"since of exactly a second":        {args: []string{"--since", "1s"}},
		"exec timeout of exactly a second": {args: []string{"--exec-timeout", "1s"}},
	}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			r, err := runDebug(t, gatherSeam{}, append([]string{"worker-1"}, c.args...)...)
			if !c.reject {
				if err != nil {
					t.Fatalf("kubectl nv debug %v: %v\nstderr: %s", c.args, err, r.err)
				}

				return
			}

			if err == nil {
				t.Fatalf("kubectl nv debug %v returned nil, want a usage error", c.args)
			}
			if got := output.ExitCodeFor(err); got != output.ExitUsage {
				t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitUsage, err)
			}
			// Nothing was gathered: the flag is the user's to fix, and a
			// directory left behind would be a bundle for a run that never
			// happened.
			if _, serr := os.Stat(r.dir); !os.IsNotExist(serr) {
				t.Errorf("a bundle was written to %s although %v was refused", r.dir, c.args)
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

// forbidden is the API server refusing a read, in the typed shape the
// inventory classifies as a partial result rather than as a failed run.
func forbidden(resource string) error {
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: resource}, "",
		errors.New(`User "system:serviceaccount:support:reader" cannot list `+resource))
}

// TestDebugExitsPartialWhenPartOfTheInventoryIsDenied covers the half of exit
// 3 that has nothing to do with the gather. A support engineer reading this
// report has to be able to tell "the node has no ResourceSlices" from "nobody
// was allowed to look", and the exit code is what a script branches on.
func TestDebugExitsPartialWhenPartOfTheInventoryIsDenied(t *testing.T) {
	t.Parallel()

	r, err := runDebugWith(t, fixture{
		reactors: []reactor{{verb: "list", resource: "resourceslices", err: forbidden("resourceslices")}},
	}, "worker-1", "-o", "json")
	if err == nil {
		t.Fatalf("kubectl nv debug returned nil, want the denied read reported\nstdout: %s", r.out)
	}
	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitPartial, err)
	}

	// The read's own error comes back, not a summary of it. A caller that
	// branches on what was refused, rather than on the exit code alone, has to
	// be able to reach the cause; and the message a user reads has to name the
	// resource rather than say only that something is missing.
	var partial *source.PartialError
	if !errors.As(err, &partial) {
		t.Fatalf("error is %T (%v), want it to unwrap to a *source.PartialError", err, err)
	}
	if partial.Resource != "resourceslices" {
		t.Errorf("the partial error is about %q, want resourceslices", partial.Resource)
	}
	if !strings.Contains(err.Error(), "resourceslices") {
		t.Errorf("error = %q, want it to name what could not be read", err.Error())
	}

	// The report is still printed: the node was read and the bundle collected,
	// and what is missing is exactly what the report has to say.
	got := decodeReport(t, r)
	if !got.Status.Partial {
		t.Error("status.partial is false, but a read was refused")
	}
	if len(got.Status.Unreachable) != 1 {
		t.Fatalf("status.unreachable has %d entries, want exactly 1: %+v",
			len(got.Status.Unreachable), got.Status.Unreachable)
	}
	if entry := got.Status.Unreachable[0]; entry.Resource != "resourceslices" {
		t.Errorf("status.unreachable[0].resource = %q, want resourceslices", entry.Resource)
	}
	if got.Status.Unreachable[0].Reason == "" {
		t.Error("status.unreachable[0].reason is empty, so nothing says why it could not be read")
	}

	// The gather itself was not refused anything, so the bundle is whole.
	if len(got.Bundle.Errored) != 0 {
		t.Errorf("the gather reports errored collectors %+v, want none", got.Bundle.Errored)
	}
}

// TestDebugOnADRANodeWithTheSliceListDenied is the case the exit code exists
// for. A node is a GPU node under DRA only because a ResourceSlice names it,
// so with that list refused the node falls out of the inventory entirely.
// Reporting "no GPUs and no GPU labels" would state a fact about the cluster
// that nobody established, and exit 2 would tell a script the name was wrong.
func TestDebugOnADRANodeWithTheSliceListDenied(t *testing.T) {
	t.Parallel()

	// A node with no GPU capacity and no hint label: in this cluster only a
	// ResourceSlice could make it a GPU node.
	objects := append(cluster(), &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "dra-worker"},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
		},
	})

	t.Run("denied, the answer is incomplete", func(t *testing.T) {
		t.Parallel()

		r, err := runDebugWith(t, fixture{
			objects:  objects,
			reactors: []reactor{{verb: "list", resource: "resourceslices", err: forbidden("resourceslices")}},
		}, "dra-worker")
		if err == nil {
			t.Fatalf("kubectl nv debug returned nil, want the denied read reported\nstdout: %s", r.out)
		}
		if got := output.ExitCodeFor(err); got != output.ExitPartial {
			t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitPartial, err)
		}
		if strings.Contains(err.Error(), "has no GPUs") {
			t.Errorf("error = %q, which claims the node has no GPUs after refusing to look", err.Error())
		}
	})

	t.Run("allowed, the same node really has none", func(t *testing.T) {
		t.Parallel()

		_, err := runDebugWith(t, fixture{objects: objects}, "dra-worker")
		if err == nil {
			t.Fatal("kubectl nv debug returned nil, want a usage error")
		}
		if got := output.ExitCodeFor(err); got != output.ExitUsage {
			t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitUsage, err)
		}
	})
}

// TestDebugGatherFailureWinsOverTheInventoryFailure pins the precedence. The
// two carry different exit codes and both can happen at once; the gather's is
// the one that says whether there is a bundle to send at all, which is the
// more severe answer and the one a script has to see.
func TestDebugGatherFailureWinsOverTheInventoryFailure(t *testing.T) {
	t.Parallel()

	r, err := runDebugWith(t, fixture{
		reactors: []reactor{{verb: "list", resource: "resourceslices", err: forbidden("resourceslices")}},
		dir:      filepath.Join(unwritableParent(t), "artifacts"),
	}, "worker-1")
	if err == nil {
		t.Fatalf("kubectl nv debug returned nil, want the failed gather reported\nstdout: %s", r.out)
	}
	if got := output.ExitCodeFor(err); got != output.ExitError {
		t.Errorf("exit code = %d, want %d: the inventory was only partial but the gather could not run "+
			"at all (error: %v)", got, output.ExitError, err)
	}
}

// unwritableParent returns a path under a regular file, which no directory can
// be created below.
func unwritableParent(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "a-file-not-a-directory")
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("writing the stand-in file: %v", err)
	}

	return path
}

// TestDebugCannotWriteTheBundle covers the gather that never started. Nothing
// was collected, so nothing can be concluded about the cluster, and exit 4 is
// what says so.
func TestDebugCannotWriteTheBundle(t *testing.T) {
	t.Parallel()

	r, err := runDebugWith(t, fixture{dir: filepath.Join(unwritableParent(t), "artifacts")}, "worker-1")
	if err == nil {
		t.Fatalf("kubectl nv debug returned nil, want the failed gather reported\nstdout: %s", r.out)
	}
	if got := output.ExitCodeFor(err); got != output.ExitError {
		t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitError, err)
	}

	// There is no bundle, so neither of the two things that would send the
	// user to one may be printed.
	text := r.out.String() + r.err.String()
	for _, never := range []string{"--from-bundle", "send the contents of"} {
		if strings.Contains(text, never) {
			t.Errorf("the output offers %q for a bundle that was never written:\n%s", never, text)
		}
	}
}

// TestDebugCannotReadTheNodes covers the cluster that cannot be read at all.
// A failed node list is not a partial answer: without it there is no node to
// report on, so nothing is printed, nothing is gathered, and exit 4 says to
// conclude nothing from the run.
func TestDebugCannotReadTheNodes(t *testing.T) {
	t.Parallel()

	r, err := runDebugWith(t, fixture{
		reactors: []reactor{{
			verb: "list", resource: "nodes",
			err: apierrors.NewInternalError(errors.New("the API server is having a bad day")),
		}},
	}, "worker-1", "-o", "json")
	if err == nil {
		t.Fatalf("kubectl nv debug returned nil, want the failed read reported\nstdout: %s", r.out)
	}
	if got := output.ExitCodeFor(err); got != output.ExitError {
		t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitError, err)
	}
	if r.out.Len() != 0 {
		t.Errorf("stdout is %q, want nothing: there is no node to report on", r.out.String())
	}
	if _, serr := os.Stat(r.dir); !os.IsNotExist(serr) {
		t.Errorf("a bundle was written to %s although the cluster could not be read", r.dir)
	}
}

// TestDebugReportsAnUnreadableManifest covers the bundle whose own index could
// not be read back. The collector counts then describe nothing, and a report
// that printed zeroes for them would say the gather collected nothing when it
// may have collected everything.
func TestDebugReportsAnUnreadableManifest(t *testing.T) {
	t.Parallel()

	seam := gatherSeam{}
	r, err := runDebugWith(t, fixture{seam: seam, manifestCorruption: true}, "worker-1", "-o", "json")
	if err == nil {
		t.Fatalf("kubectl nv debug returned nil, want the unreadable index reported\nstdout: %s", r.out)
	}
	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitPartial, err)
	}

	got := decodeReport(t, r)
	if !got.Status.Partial {
		t.Error("status.partial is false, but the bundle index could not be read")
	}
	var named bool
	for _, entry := range got.Status.Unreachable {
		if entry.Resource == mustgather.ManifestFileName {
			named = true
		}
	}
	if !named {
		t.Errorf("nothing in status.unreachable names %s: %+v",
			mustgather.ManifestFileName, got.Status.Unreachable)
	}
	if got.Bundle.Manifest != "" {
		t.Errorf("bundle.manifest = %q, want it empty: there is no index to point at", got.Bundle.Manifest)
	}
}

// TestDebugRecordsTheContextAndTheToolVersion pins what the bundle says
// produced it. A bundle that names no context gets filed against the wrong
// cluster, and one that names no version sends support to the wrong source.
func TestDebugRecordsTheContextAndTheToolVersion(t *testing.T) {
	t.Parallel()

	r, err := runDebugWith(t, fixture{kubeContext: "prod-eu-west", version: "v4.2.0"}, "worker-1")
	if err != nil {
		t.Fatalf("kubectl nv debug: %v\nstderr: %s", err, r.err)
	}

	var m struct {
		Context     string `json:"context"`
		ToolVersion string `json:"toolVersion"`
	}
	raw, rerr := os.ReadFile(filepath.Join(r.dir, mustgather.ManifestFileName))
	if rerr != nil {
		t.Fatalf("reading the bundle manifest: %v", rerr)
	}
	if uerr := json.Unmarshal(raw, &m); uerr != nil {
		t.Fatalf("the manifest does not parse: %v\n%s", uerr, raw)
	}

	if m.Context != "prod-eu-west" {
		t.Errorf("manifest context = %q, want prod-eu-west", m.Context)
	}
	if m.ToolVersion != "v4.2.0" {
		t.Errorf("manifest toolVersion = %q, want the version the root carries", m.ToolVersion)
	}
}

// TestDebugBuildsTheNodeTheWayInfoDoes covers the detail flag. The conditions
// are only filled when the inventory is asked for the detailed view, so a
// report built without it is thinner than it claims while every other
// assertion here still passes.
func TestDebugBuildsTheNodeTheWayInfoDoes(t *testing.T) {
	t.Parallel()

	r, err := runDebug(t, gatherSeam{}, "worker-1", "-o", "json")
	if err != nil {
		t.Fatalf("kubectl nv debug: %v\nstderr: %s", err, r.err)
	}

	var report struct {
		Node struct {
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
			ResourceSlices []string `json:"resourceSlices"`
		} `json:"node"`
	}
	if uerr := json.Unmarshal(r.out.Bytes(), &report); uerr != nil {
		t.Fatalf("decoding the report: %v\n%s", uerr, r.out.String())
	}

	if len(report.Node.Conditions) == 0 {
		t.Error("the report's node has no conditions, so it was not built with the detailed view")
	}
	for _, c := range report.Node.Conditions {
		if c.Type == "Ready" && c.Status != "True" {
			t.Errorf("the Ready condition is %q, want True", c.Status)
		}
	}
	if report.Node.ResourceSlices == nil {
		t.Error("resourceSlices is null, want an empty array")
	}
}

// TestDebugIsServedWithTheExecForcedAndTheDirectoryPinned covers how this
// command reaches a model. It runs a gather, which reaches into a pod and
// writes to the machine kubectl-nv runs on, so the server has to force
// --no-exec on it and pin the directory rather than letting the caller choose
// either. Both come from annotations this command sets on itself.
func TestDebugIsServedWithTheExecForcedAndTheDirectoryPinned(t *testing.T) {
	t.Parallel()

	o := &cmd.Options{Streams: genericiooptions.NewTestIOStreamsDiscard()}
	parent := &cobra.Command{Use: "kubectl-nv"}
	parent.AddCommand(debugcmd.NewCommand(o, func(*mustgather.Options) error { return nil }))

	var tool mcp.Tool
	for _, candidate := range mcp.Tools(parent) {
		if candidate.Name == "debug" {
			tool = candidate
		}
	}
	if tool.Name == "" {
		t.Fatal("debug is not offered as a tool at all")
	}

	if !tool.ForceNoExec {
		t.Error("the server would serve debug with the exec available")
	}
	if !tool.HasNoExecFlag {
		t.Error("debug has no --no-exec flag, so the server could not force it off")
	}
	if !tool.WritesHost {
		t.Error("debug is not reported as writing to the host, so its directory would not be pinned")
	}
	if got := strings.Join(tool.HostDirFlags, ","); got != "artifacts-dir" {
		t.Errorf("the host directory flags are %q, want exactly artifacts-dir", got)
	}

	properties, ok := tool.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("the input schema has no properties object: %#v", tool.InputSchema)
	}
	for _, never := range []string{"artifacts-dir", "no-exec"} {
		if _, offered := properties[never]; offered {
			t.Errorf("the input schema offers %q, which the server pins rather than takes", never)
		}
	}
	if _, offered := properties["args"]; !offered {
		t.Error("the input schema takes no positional argument, so a model could not name the node")
	}
}
