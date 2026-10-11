/**
# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package mustgather

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"sigs.k8s.io/yaml"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// Options defines the configurable parameters for the must-gather operation.
type Options struct {
	ArtifactDir string // Directory to store gathered artifacts/logs.
	Debug       bool   // Name every artifact as it is written.

	// Node narrows the whole gather to one node: its node object, the operand
	// pods on it, their logs, its bug report and its device usage. The
	// cluster-scoped objects are collected whatever it says, because they are
	// what the node's state has to be read against.
	Node string

	// Since bounds the window of the current container logs. Zero means the
	// whole log.
	Since time.Duration

	// NoExec keeps the gather out of the driver pods: no bug report and no
	// device usage, and the manifest records both as skipped by flag.
	NoExec bool

	// ExecTimeout bounds one exec or one copy out of a driver pod. Zero means
	// DefaultExecTimeout. A pod that accepted the connection and never answers
	// costs one file rather than the whole bundle.
	ExecTimeout time.Duration

	// ToolVersion is the kubectl-nv version the manifest records the bundle as
	// having been collected by.
	ToolVersion string

	// Context is the kubeconfig context the bundle was collected through, for
	// the manifest. A bundle that does not say which cluster it came from is a
	// bundle that gets filed against the wrong one.
	Context string

	// FetchPodFile replaces the live exec-and-copy that pulls a bug report out
	// of a driver pod. It is the seam the tests drive the gatherer through
	// without a cluster to exec into; a nil value means the live one.
	FetchPodFile PodFileFetcher

	// RunInPod replaces the live exec used by the device usage collector.
	RunInPod PodCommandRunner
}

// PodCommandRunner runs command in one container of pod and returns its
// standard output. It stops when ctx is done.
type PodCommandRunner func(ctx context.Context, pod *v1.Pod, container, command string) (string, error)

// Clients hands the gatherer the clients it works through. The command layer
// has a factory that satisfies this; a test supplies fakes. It is declared
// here, where it is used, so that the gatherer stays a library the commands
// call rather than one that calls back into them.
type Clients interface {
	KubernetesClient() (kubernetes.Interface, error)
	DynamicClient() (dynamic.Interface, error)
	RESTConfig() (*rest.Config, error)
}

// RunFunc is the signature a must-gather runs through, so that a caller can put
// its own implementation in front of the real one.
type RunFunc func(opts *Options) error

// PodFileFetcher runs the bug-report command inside pod and copies the resulting
// file out of the pod to destPath on the local filesystem. It stops when ctx is
// done, which is how a pod that never answers costs one artifact rather than
// the gather.
type PodFileFetcher func(ctx context.Context, pod *v1.Pod, destPath string) error

// podLogStreamer opens a log stream for the given pod. The caller owns the
// returned stream and is responsible for closing it.
type podLogStreamer func(ctx context.Context, namespace, podName string, opts *v1.PodLogOptions) (io.ReadCloser, error)

// Gatherer holds all the initialized resources needed for must-gather operations.
type Gatherer struct {
	opts       *Options
	logFile    *os.File
	errLogFile *os.File
	config     *rest.Config
	clientset  kubernetes.Interface
	dynClient  dynamic.Interface

	// progress carries the running commentary to the user's error stream. The
	// artifacts themselves go to the log files, never here.
	progress Progress

	// collected reports that at least one artifact was written, which is what
	// tells a bundle worth sending from no bundle at all.
	collected bool

	// fetchPodFile, streamPodLogs and runInPod are injection seams for tests.
	// When nil the Gatherer falls back to the live cluster implementations.
	fetchPodFile  PodFileFetcher
	streamPodLogs podLogStreamer
	runInPod      PodCommandRunner

	// results is the per-collector record the manifest is written from, and
	// current is the collector whose artifacts are being counted.
	results []CollectorStatus
	current *CollectorStatus
}

// fetchBugReport copies the bug report generated inside pod to destPath, using
// the injected fetcher when one is set.
func (g *Gatherer) fetchBugReport(ctx context.Context, pod *v1.Pod, destPath string) error {
	if g.fetchPodFile != nil {
		return g.fetchPodFile(ctx, pod, destPath)
	}
	return g.execAndCopyBugReport(ctx, pod, destPath)
}

// podLogs opens a log stream for the given pod, using the injected streamer when
// one is set.
func (g *Gatherer) podLogs(ctx context.Context, namespace, podName string, opts *v1.PodLogOptions) (io.ReadCloser, error) {
	if g.streamPodLogs != nil {
		return g.streamPodLogs(ctx, namespace, podName, opts)
	}
	return g.clientset.CoreV1().Pods(namespace).GetLogs(podName, opts).Stream(ctx)
}

// execAndCopyBugReport is the live-cluster PodFileFetcher: it runs
// nvidia-bug-report.sh inside the pod and copies /tmp/nvidia-bug-report.log.gz
// out to destPath.
func (g *Gatherer) execAndCopyBugReport(ctx context.Context, pod *v1.Pod, destPath string) error {
	nullFile, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0666)
	var outpipe *os.File
	if err != nil {
		outpipe = os.Stderr
	} else {
		outpipe = nullFile
		defer nullFile.Close() // Only close if opened successfully.
	}
	iostream := genericiooptions.IOStreams{
		In:     os.Stdin,
		Out:    outpipe,
		ErrOut: os.Stderr,
	}
	o := copyOptionsFor(iostream, pod)
	o.Clientset = g.clientset
	o.ClientConfig = g.config
	container := o.Container

	// Execute remote command to run the bug report script.
	outLog, errLog, err := executeRemoteCommand(
		ctx, g.config, pod, container, "cd /tmp && nvidia-bug-report.sh")
	if err != nil {
		if _, lerr := g.errLogFile.WriteString(errLog); lerr != nil {
			err = fmt.Errorf("%v+ error writing error output to stderr log file: %v", err, lerr)
		}
		return err
	}
	// Write the command output to the main log file.
	if _, lerr := g.logFile.WriteString(outLog); lerr != nil {
		g.note("Error writing to log file: %v", lerr)
	}

	// Build the source file spec from the pod:
	// Format: "<namespace>/<pod-name>:/tmp/nvidia-bug-report.log.gz"
	srcSpecStr := fmt.Sprintf("%s/%s:/tmp/nvidia-bug-report.log.gz", pod.Namespace, pod.Name)
	srcSpec, err := extractFileSpec(srcSpecStr)
	if err != nil {
		return err
	}

	destSpec, err := extractFileSpec(destPath)
	if err != nil {
		return err
	}

	return o.copyFromPod(srcSpec, destSpec)
}

// PodInfo groups the GPU operand pods and operator pods retrieved from the cluster.
type PodInfo struct {
	OperandPods  *v1.PodList
	OperatorPods *v1.PodList
}

// NewGatherer initializes a new Gatherer by performing the following tasks:
// 1. Create the artifact directory.
// 2. Create log files for stdout and stderr.
// 3. Take the clients from the factory.
// If any step fails, any opened file resources are closed before returning.
func NewGatherer(opts *Options, f Clients, progress Progress) (*Gatherer, error) {
	// Before anything is created: a directory that already holds a bundle
	// would end up holding two runs' files with nothing to tell them apart.
	if err := checkArtifactDir(opts.ArtifactDir); err != nil {
		return nil, err
	}

	// Create the ARTIFACT_DIR.
	// The bundle holds cluster data and lands under /tmp, where every account on
	// the machine can look.
	if err := os.MkdirAll(opts.ArtifactDir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create artifact directory: %w", err)
	}

	// Create the stdout log file.
	logPath := filepath.Join(opts.ArtifactDir, "must-gather.log")
	logFile, err := createArtifactFile(logPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %w", err)
	}

	// Create the stderr log file.
	errLogPath := filepath.Join(opts.ArtifactDir, "must-gather.stderr.log")
	errLogFile, err := createArtifactFile(errLogPath)
	if err != nil {
		logFile.Close()
		return nil, fmt.Errorf("failed to create error log file: %w", err)
	}

	// Take the REST config and the clients from the factory. Every client the
	// gather uses comes from here, so a test that hands over fakes leaves no
	// path to a real cluster.
	config, err := f.RESTConfig()
	if err != nil {
		err = logTo(errLogFile, "Error creating Kubernetes REST config: %v\n", err)
		logFile.Close()
		errLogFile.Close()
		return nil, err
	}

	clientset, err := f.KubernetesClient()
	if err != nil {
		err = logTo(errLogFile, "Error creating Kubernetes client: %v\n", err)
		logFile.Close()
		errLogFile.Close()
		return nil, err
	}

	dynClient, err := f.DynamicClient()
	if err != nil {
		err = logTo(errLogFile, "Error creating dynamic client: %v\n", err)
		logFile.Close()
		errLogFile.Close()
		return nil, err
	}

	// Return a fully initialized Gatherer.
	return &Gatherer{
		opts:         opts,
		logFile:      logFile,
		errLogFile:   errLogFile,
		config:       config,
		clientset:    clientset,
		dynClient:    dynClient,
		progress:     progress,
		fetchPodFile: opts.FetchPodFile,
		runInPod:     opts.RunInPod,
	}, nil
}

// logTo records err in the stderr log file and returns it, folding in a failure
// to write that record rather than losing either one.
func logTo(errLogFile *os.File, format string, err error) error {
	if _, lerr := fmt.Fprintf(errLogFile, format, err); lerr != nil {
		return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
	}
	return err
}

// gatherOpenShiftVersion checks if the cluster is OpenShift using the kubeconfig,
// and if so, writes the OpenShift version to "openshift_version.yaml" in the artifact directory.
func (g *Gatherer) gatherOpenShiftVersion() error {
	// Determine if the cluster is OpenShift.
	oversion, isOpenShift := isOpenShiftCluster(g.config)
	if !isOpenShift {
		return skip("not an OpenShift cluster")
	}
	return g.writeArtifactFile("openshift_version.yaml", func(w io.Writer) error {
		_, werr := io.WriteString(w, oversion)
		return werr
	})
}

// gatherNodeInfo collects node information.
// For OpenShift clusters, it retrieves machine data; otherwise, it lists Kubernetes nodes.
func (g *Gatherer) gatherNodeInfo() error {
	// An OpenShift cluster is described by its machines; anywhere else the
	// nodes are the whole answer.
	_, isOpenShift := isOpenShiftCluster(g.config)
	if isOpenShift {
		// Get machines data from an OpenShift cluster.
		machines, err := getOpenShiftMachines(g.config)
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting machines: %v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}

		return g.writeArtifactFile("machines.yaml", func(w io.Writer) error {
			_, werr := w.Write(machines)
			return werr
		})
	} else {
		listFunc := func(opts metav1.ListOptions) (v1.NodeList, string, error) {
			nl, err := g.clientset.CoreV1().Nodes().List(context.Background(), opts)
			if err != nil {
				return v1.NodeList{}, "", err
			}
			return *nl, nl.Continue, nil
		}
		appendFunc := func(all *v1.NodeList, page v1.NodeList) {
			all.Items = append(all.Items, page.Items...)
		}
		nodes, err := listAllPaginated[v1.NodeList](500, "", listFunc, appendFunc)
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting nodes: %v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		nodes.Items = g.nodesWanted(nodes.Items)

		// Marshal nodes into YAML format.
		stampNodeList(&nodes)
		data, merr := yaml.Marshal(nodes)
		if merr != nil {
			return g.logError("error marshalling nodes: %v", merr)
		}

		return g.writeArtifactFile("nodes.yaml", func(w io.Writer) error {
			_, werr := w.Write(data)
			return werr
		})
	}
}

// gatherOperatorNamespace retrieves the namespace of the operator pods
// by using the label "app=gpu-operator". An empty namespace is used to search across all namespaces.
// It returns the operator namespace as a string and an error if any.
func (g *Gatherer) gatherOperatorNamespace() (string, error) {
	labelSelector := "app=gpu-operator"
	listFunc := func(opts metav1.ListOptions) (v1.PodList, string, error) {
		pl, err := g.clientset.CoreV1().Pods("").List(context.TODO(), opts)
		if err != nil {
			return v1.PodList{}, "", err
		}
		return *pl, pl.Continue, nil
	}
	appendFunc := func(all *v1.PodList, page v1.PodList) {
		all.Items = append(all.Items, page.Items...)
	}
	pods, err := listAllPaginated[v1.PodList](500, labelSelector, listFunc, appendFunc)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting pods: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return "", err
	}

	if len(pods.Items) == 0 {
		if _, lerr := fmt.Fprintf(g.logFile, "No pods found with label %s\n", labelSelector); lerr != nil {
			if _, lerr2 := fmt.Fprintf(g.errLogFile, "Error writing to log file: %v\n", lerr); lerr2 != nil {
				return "", fmt.Errorf("%v + error writing to stderr log file: %v", lerr, lerr2)
			}
		}
		return "", fmt.Errorf("no pods found with label %s", labelSelector)
	}

	operatorNamespace := pods.Items[0].Namespace
	if _, lerr := fmt.Fprintf(g.logFile, "Operator namespace: %s\n", operatorNamespace); lerr != nil {
		if _, lerr2 := fmt.Fprintf(g.errLogFile, "Error writing to log file: %v\n", lerr); lerr2 != nil {
			return "", fmt.Errorf("%v + error writing to stderr log file: %v", lerr, lerr2)
		}
	}

	return operatorNamespace, nil
}

// logError records err in the stderr log under the caller's description and
// returns it wrapped in that description.
//
// format carries the caller's own verb for the error, as in "error getting
// nodes: %v". Appending a second verb to it, which this used to do, left every
// error ending in a formatting fault and broke errors.Is: nothing downstream
// could tell a NotFound from a Forbidden through one of these.
func (g *Gatherer) logError(format string, err error) error {
	if _, lerr := fmt.Fprintf(g.errLogFile, format+"\n", err); lerr != nil {
		return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
	}
	// The description without its verb becomes the wrapper, so the cause is
	// carried by %w exactly once and stays reachable.
	return fmt.Errorf("%s: %w", strings.TrimSuffix(strings.TrimSpace(fmt.Sprintf(format, "")), ":"), err)
}

// gatherGPUNodes collects nodes that have NVIDIA PCI labels and writes the result as YAML to a file.
func (g *Gatherer) gatherGPUNodes() error {
	gpuPciLabelSelectors := []string{
		"feature.node.kubernetes.io/pci-10de.present",
		"feature.node.kubernetes.io/pci-0302_10de.present",
		"feature.node.kubernetes.io/pci-0300_10de.present",
	}
	var gpuNodes v1.NodeList
	appendFunc := func(all *v1.NodeList, page v1.NodeList) {
		all.Items = append(all.Items, page.Items...)
	}
	for _, label := range gpuPciLabelSelectors {
		listFunc := func(opts metav1.ListOptions) (v1.NodeList, string, error) {
			nl, err := g.clientset.CoreV1().Nodes().List(context.Background(), opts)
			if err != nil {
				return v1.NodeList{}, "", err
			}
			return *nl, nl.Continue, nil
		}
		nl, err := listAllPaginated[v1.NodeList](500, label, listFunc, appendFunc)
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting nodes with NVIDIA PCI label '%s': %v\n", label, err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		gpuNodes.Items = append(gpuNodes.Items, nl.Items...)
	}

	// A node carrying more than one of the PCI labels is listed once. The
	// script deduplicates by node name and a bundle that reports the same node
	// twice makes a reader count GPU nodes wrong.
	gpuNodes.Items = g.nodesWanted(dedupeNodes(gpuNodes.Items))

	if len(gpuNodes.Items) == 0 {
		if _, lerr := fmt.Fprintf(g.errLogFile, "No nodes found with NVIDIA PCI label\n"); lerr != nil {
			_ = fmt.Errorf("error writing to stderr log file: %v", lerr)
		}
		// Nothing to survey is a fact about the cluster, not a failure of the
		// gather. A cluster whose feature discovery has not labelled a node yet
		// is exactly the cluster someone runs a must-gather on, and its
		// ClusterPolicy and operator logs are what say why. Reporting this as
		// an error used to take them with it.
		if g.opts.Node != "" {
			return skip("%s carries no NVIDIA PCI label", g.opts.Node)
		}
		return skip("no GPU nodes")
	}

	// Marshal the collected GPU nodes into YAML.
	stamped := gpuNodes
	stampNodeList(&stamped)
	data, err := yaml.Marshal(stamped)
	if err != nil {
		return g.logError("error marshalling gpuNodes: %v", err)
	}

	if werr := g.writeArtifactFile("gpu_nodes.yaml", func(w io.Writer) error {
		_, e := w.Write(data)
		return e
	}); werr != nil {
		return werr
	}

	return g.gatherGPUNodeText(gpuNodes.Items)
}

// gatherGPUNodeText writes the three files the script gets from a jsonpath of
// the labels, from "kubectl get -o wide" and from "kubectl describe". They are
// what a support engineer reads before opening any YAML.
func (g *Gatherer) gatherGPUNodeText(nodes []v1.Node) error {
	if err := g.writeArtifactFile("gpu_nodes.labels", func(w io.Writer) error {
		return writeNodeLabels(w, nodes)
	}); err != nil {
		return err
	}

	if err := g.writeArtifactFile("gpu_nodes.status", func(w io.Writer) error {
		return writeNodeTable(w, nodes)
	}); err != nil {
		return err
	}

	return g.writeArtifactFile("gpu_nodes.descr", func(w io.Writer) error {
		return describeEach(w, nodes, func(node v1.Node) (string, error) {
			return g.describeNode(node.Name)
		})
	})
}

// writeNodeLabels writes each node's name and then its labels, one per line, in
// the form the script's sed pipeline produces. The labels are sorted so that
// two bundles of the same cluster can be diffed.
func writeNodeLabels(w io.Writer, nodes []v1.Node) error {
	for i := range nodes {
		node := &nodes[i]
		if _, err := fmt.Fprintf(w, "%s\n", node.Name); err != nil {
			return fmt.Errorf("writing the node name: %w", err)
		}
		keys := make([]string, 0, len(node.Labels))
		for key := range node.Labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if _, err := fmt.Fprintf(w, "- %s=%s\n", key, node.Labels[key]); err != nil {
				return fmt.Errorf("writing a node label: %w", err)
			}
		}
		if _, err := fmt.Fprintln(w); err != nil {
			return fmt.Errorf("writing the node separator: %w", err)
		}
	}
	return nil
}

// dedupeNodes keeps the first appearance of each node name.
func dedupeNodes(nodes []v1.Node) []v1.Node {
	seen := make(map[string]bool, len(nodes))
	kept := make([]v1.Node, 0, len(nodes))
	for _, node := range nodes {
		if seen[node.Name] {
			continue
		}
		seen[node.Name] = true
		kept = append(kept, node)
	}
	return kept
}

// nodesWanted narrows a node list to --node when one was given.
func (g *Gatherer) nodesWanted(nodes []v1.Node) []v1.Node {
	if g.opts.Node == "" {
		return nodes
	}
	kept := make([]v1.Node, 0, 1)
	for _, node := range nodes {
		if g.wantsNode(node.Name) {
			kept = append(kept, node)
		}
	}
	return kept
}

// writeArtifactFile creates one artifact and hands it to write. It is the shape
// every text artifact of the bundle is produced in, so that none of them can
// forget the mode or the manifest entry.
func (g *Gatherer) writeArtifactFile(name string, write func(io.Writer) error) error {
	file, err := g.createArtifact(filepath.Join(g.opts.ArtifactDir, name))
	if err != nil {
		return g.logError(fmt.Sprintf("error creating %s: %%v", name), err)
	}

	if werr := write(file); werr != nil {
		file.discard()
		return g.logError(fmt.Sprintf("error writing %s: %%v", name), werr)
	}
	if cerr := file.commit(); cerr != nil {
		return g.logError(fmt.Sprintf("error writing %s: %%v", name), cerr)
	}
	return nil
}

// containerNames lists the containers of the pod whose logs are collected: init
// containers first, then regular containers, each in spec order. Ephemeral
// containers, which "kubectl logs --all-containers" also covers, are left out:
// they are attached for live debugging and are gone by the time a bug report is
// read.
func containerNames(pod *v1.Pod) []string {
	names := make([]string, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	for _, c := range pod.Spec.InitContainers {
		names = append(names, c.Name)
	}
	for _, c := range pod.Spec.Containers {
		names = append(names, c.Name)
	}
	return names
}

// writePrefixedLines copies src to dst, prefixing every line with the
// "[pod/<pod>/<container>] " marker that "kubectl logs --prefix" emits.
func writePrefixedLines(dst io.Writer, src io.Reader, podName, container string) error {
	prefix := fmt.Sprintf("[pod/%s/%s] ", podName, container)
	reader := bufio.NewReader(src)
	for {
		line, rerr := reader.ReadString('\n')
		if len(line) > 0 {
			if _, werr := io.WriteString(dst, prefix+line); werr != nil {
				return fmt.Errorf("error writing log line: %w", werr)
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return nil
			}
			return fmt.Errorf("error reading log stream: %w", rerr)
		}
	}
}

// writeStringToFile is a helper function that writes a string to the provided
// writer.
func writeStringToFile(file io.StringWriter, s string) error {
	if _, err := file.WriteString(s); err != nil {
		return fmt.Errorf("error writing string to file: %w", err)
	}
	return nil
}

// Run collects everything NVIDIA support asks for from the cluster the factory
// points at, into opts.ArtifactDir. progress carries the running commentary;
// the artifacts themselves go to files under the artifact directory.
func Run(opts *Options, f Clients, progress Progress) error {
	g, err := NewGatherer(opts, f, progress)
	if err != nil {
		// A usage error is the user's to fix and carries its own exit code;
		// wrapping it as a failed gather would report a typo as a cluster that
		// could not be read.
		var usage *output.UsageError
		if errors.As(err, &usage) {
			return err
		}
		// Nothing was collected, so there is no bundle to send.
		return &Error{Err: fmt.Errorf("failed to initialize Gatherer: %w", err)}
	}
	defer g.Close()

	// The manifest is written whichever way the gather ended, including a
	// panic on the way out. A bundle that stops halfway is exactly the one
	// whose reader most needs to know which collectors ran, and an index
	// written only on the success path is one a later early return can lose.
	defer func() {
		if err := g.writeManifest(); err != nil {
			g.note("could not write %s: %v", ManifestFileName, err)
		}
	}()

	if err := g.checkNode(); err != nil {
		return err
	}

	collectErr := g.collect()

	if collectErr != nil {
		return g.failed(collectErr)
	}
	return nil
}

// checkNode makes sure --node names a node the cluster has. A typo used to run
// the whole gather and produce a complete-looking bundle holding nothing about
// any node, which reads as a cluster with no GPUs rather than as a name that
// does not exist.
func (g *Gatherer) checkNode() error {
	if g.opts.Node == "" {
		return nil
	}
	if !usableInAName(g.opts.Node) {
		return output.NewUsageError("--node %q is not a usable node name", g.opts.Node)
	}

	ctx, cancel := context.WithTimeout(context.Background(), customResourceTimeout)
	defer cancel()

	_, err := g.clientset.CoreV1().Nodes().Get(ctx, g.opts.Node, metav1.GetOptions{})
	switch {
	case err == nil:
		return nil
	case apierrors.IsNotFound(err):
		return output.NewUsageError("--node %s: the cluster has no such node", g.opts.Node)
	default:
		// Anything else is the cluster being unreadable, which the collectors
		// report in their own way; one failed GET must not stop the gather.
		g.note("could not check that node %s exists: %v", g.opts.Node, err)
		return nil
	}
}

// collect runs every collector of the registry in turn, recording what each
// one did. A collector that had nothing to do is noted and the gather carries
// on; a collector that failed stops it, and what was written before it stays
// on disk for the caller to decide what it is worth from the exit code.
func (g *Gatherer) collect() error {
	s := &gatherState{}
	var failed []string

	for _, c := range Collectors() {
		g.begin(c.Name)
		if c.Step != "" {
			g.step("%s", c.Step)
		}

		err := c.Run(g, s)
		var skipped errSkipped
		switch {
		case err == nil:
			g.end(StatusOK)
		case errors.As(err, &skipped):
			g.end(statusSkipped + skipped.reason)
		default:
			// One collector that failed must not cost the user every artifact
			// behind it. The failure is recorded against its own collector and
			// the gather carries on; the exit code below says the bundle is
			// incomplete and the manifest says which part.
			g.end(statusError + err.Error())
			g.note("%s: %v", c.Name, err)
			failed = append(failed, c.Name)
		}
	}

	// Being told no is not the same as asking for no. A bundle missing the
	// driver evidence because RBAC refused the exec is incomplete, and a script
	// that files it as complete files an unanswerable support ticket.
	// A denied exec is reported on its own, because its cause is one thing the
	// user can act on rather than a list of collectors that each failed for
	// their own reason.
	if len(s.execDenied) > 0 {
		return fmt.Errorf("not allowed to exec into the driver pods; these collectors were skipped: %s",
			strings.Join(s.execDenied, ", "))
	}

	if len(failed) > 0 {
		return fmt.Errorf("these collectors failed: %s", strings.Join(failed, ", "))
	}

	return nil
}

// begin starts counting the artifacts of one collector.
func (g *Gatherer) begin(name string) {
	g.results = append(g.results, CollectorStatus{Name: name, Artifacts: []string{}})
	g.current = &g.results[len(g.results)-1]
}

// end records how the current collector finished.
func (g *Gatherer) end(status string) {
	if g.current != nil {
		g.current.Status = status
		g.current = nil
	}
}

// Close releases any resources held by the Gatherer.
func (g *Gatherer) Close() {
	if g.logFile != nil {
		g.logFile.Close()
	}
	if g.errLogFile != nil {
		g.errLogFile.Close()
	}
}

// listAllPaginated fetches all items from a paginated List call.
// selector: label selector string (can be empty)
// listFunc: function that takes ListOptions and returns a list and error
func listAllPaginated[T any](limit int64, selector string, listFunc func(metav1.ListOptions) (T, string, error), appendFunc func(*T, T)) (T, error) {
	var all T
	var cont string
	for {
		opts := metav1.ListOptions{Limit: limit, Continue: cont, LabelSelector: selector}
		page, next, err := listFunc(opts)
		if err != nil {
			return all, err
		}
		appendFunc(&all, page)
		if next == "" {
			break
		}
		cont = next
	}
	return all, nil
}

// TODO(P2): For long-lived or repeated resource access, use shared informers/caches instead of direct polling.
// See: https://pkg.go.dev/k8s.io/client-go/informers

// createArtifactFile makes one file of the bundle. Artifacts carry cluster
// data and the bundle sits under /tmp by default, so they are readable only by
// the user who collected them.
func createArtifactFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
}

// The typed clients strip the apiVersion and kind off the objects they decode,
// so every artifact has to have them put back. Without them the files are not
// what "kubectl get -o yaml" produces, and a script that reads .kind, or a
// "kubectl apply --dry-run" against the bundle, has nothing to go on.
func stampNodeList(l *v1.NodeList) {
	l.APIVersion, l.Kind = "v1", "NodeList"
	for i := range l.Items {
		l.Items[i].APIVersion, l.Items[i].Kind = "v1", "Node"
	}
}

func stampPodList(l *v1.PodList) {
	l.APIVersion, l.Kind = "v1", "PodList"
	for i := range l.Items {
		l.Items[i].APIVersion, l.Items[i].Kind = "v1", "Pod"
	}
}

func stampDaemonSetList(l *appsv1.DaemonSetList) {
	l.APIVersion, l.Kind = "apps/v1", "DaemonSetList"
	for i := range l.Items {
		l.Items[i].APIVersion, l.Items[i].Kind = "apps/v1", "DaemonSet"
	}
}

// artifactTempSuffix marks a file that is still being written. A reader of a
// bundle never sees one: it is renamed into place when the write finishes and
// removed when it does not.
const artifactTempSuffix = ".part"

// artifactFile is one artifact being written.
//
// It lands under a temporary name and is renamed into place by commit. A
// collector that starts writing and then fails used to leave a half-written
// file behind, and the manifest, written afterwards, indexed it with the
// digest it actually had: the bundle was self-consistent and a reader took a
// truncated YAML for the cluster's whole state.
type artifactFile struct {
	file  *os.File
	final string
	g     *Gatherer
}

// Write implements io.Writer.
func (a *artifactFile) Write(p []byte) (int, error) { return a.file.Write(p) }

// WriteString implements io.StringWriter.
func (a *artifactFile) WriteString(s string) (int, error) { return a.file.WriteString(s) }

// commit puts the artifact in place under its own name and records it.
func (a *artifactFile) commit() error {
	if err := a.file.Close(); err != nil {
		_ = os.Remove(a.file.Name())
		return fmt.Errorf("closing %s: %w", filepath.Base(a.final), err)
	}
	if err := os.Rename(a.file.Name(), a.final); err != nil {
		_ = os.Remove(a.file.Name())
		return fmt.Errorf("putting %s in place: %w", filepath.Base(a.final), err)
	}
	a.g.recordArtifact(a.final)
	return nil
}

// discard throws away a write that did not finish.
func (a *artifactFile) discard() {
	_ = a.file.Close()
	if err := os.Remove(a.file.Name()); err != nil && !os.IsNotExist(err) {
		_, _ = fmt.Fprintf(a.g.errLogFile,
			"Error removing the unfinished artifact %s: %v\n", filepath.Base(a.file.Name()), err)
	}
}

// createArtifact starts one artifact of the bundle. The caller commits it or
// discards it; nothing is recorded until it commits.
func (g *Gatherer) createArtifact(path string) (*artifactFile, error) {
	file, err := createArtifactFile(path + artifactTempSuffix)
	if err != nil {
		return nil, err
	}
	return &artifactFile{file: file, final: path, g: g}, nil
}

// recordArtifact notes that the bundle now holds one more file, and which
// collector put it there. The bug report is copied out of a pod rather than
// opened here, so it records itself through this.
func (g *Gatherer) recordArtifact(path string) {
	g.collected = true
	g.indexArtifact(path)
}

// recordMarker notes a file that records what the cluster does not have. It
// belongs in the manifest like any other artifact, but it is not something
// collected: a bundle holding nothing but "not installed" markers says the
// gather reached the cluster and found nothing, and calling that a partial
// success would tell a script the bundle is worth reading.
func (g *Gatherer) recordMarker(path string) {
	g.indexArtifact(path)
}

// indexArtifact adds one file to the current collector's list.
func (g *Gatherer) indexArtifact(path string) {
	name := filepath.Base(path)
	if g.current != nil {
		g.current.Artifacts = append(g.current.Artifacts, name)
	}
	// With debug on, the bundle is listed as it is built: a support engineer
	// chasing a missing file needs to know whether it was never written or
	// written and empty.
	if g.opts.Debug {
		g.step("Wrote %s", name)
	}
}

// writeMarker writes a file recording what the cluster does not have.
func (g *Gatherer) writeMarker(path string, data []byte) error {
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	g.recordMarker(path)
	return nil
}
