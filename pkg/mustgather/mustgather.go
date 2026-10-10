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
	"time"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"gopkg.in/yaml.v2"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Options defines the configurable parameters for the must-gather operation.
type Options struct {
	ArtifactDir string // Directory to store gathered artifacts/logs.
	Debug       bool   // Report every gathering step as it happens.

	// FetchPodFile replaces the live exec-and-copy that pulls a bug report out
	// of a driver pod. It is the seam the tests drive the gatherer through
	// without a cluster to exec into; a nil value means the live one.
	FetchPodFile PodFileFetcher
}

// RunFunc is the signature a must-gather runs through, so that a caller can put
// its own implementation in front of the real one.
type RunFunc func(opts *Options) error

// PodFileFetcher runs the bug-report command inside pod and copies the resulting
// file out of the pod to destPath on the local filesystem.
type PodFileFetcher func(pod *v1.Pod, destPath string) error

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

	// progress carries the running commentary to the user's terminal. The
	// artifacts themselves go to the log files, never here.
	progress io.Writer

	// fetchPodFile and streamPodLogs are injection seams for tests. When nil the
	// Gatherer falls back to the live cluster implementations.
	fetchPodFile  PodFileFetcher
	streamPodLogs podLogStreamer
}

// step reports a gathering step to the user. Steps are only printed with debug
// logging on, because a normal run should stay quiet enough to read.
func (g *Gatherer) step(format string, a ...any) {
	if !g.opts.Debug || g.progress == nil {
		return
	}
	fmt.Fprintf(g.progress, format+"\n", a...)
}

// fetchBugReport copies the bug report generated inside pod to destPath, using
// the injected fetcher when one is set.
func (g *Gatherer) fetchBugReport(pod *v1.Pod, destPath string) error {
	if g.fetchPodFile != nil {
		return g.fetchPodFile(pod, destPath)
	}
	return g.execAndCopyBugReport(pod, destPath)
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
func (g *Gatherer) execAndCopyBugReport(pod *v1.Pod, destPath string) error {
	nullFile, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0666)
	var outpipe *os.File
	if err != nil {
		outpipe = os.Stdout
	} else {
		outpipe = nullFile
		defer nullFile.Close() // Only close if opened successfully.
	}
	iostream := genericiooptions.IOStreams{
		In:     os.Stdin,
		Out:    outpipe,
		ErrOut: os.Stderr,
	}
	o := NewCopyOptions(iostream)
	o.Clientset = g.clientset
	o.ClientConfig = g.config

	// Execute remote command to run the bug report script.
	outLog, errLog, err := executeRemoteCommand(g.config, pod, "cd /tmp && nvidia-bug-report.sh")
	if err != nil {
		if _, lerr := g.errLogFile.WriteString(errLog); lerr != nil {
			err = fmt.Errorf("%v+ error writing error output to stderr log file: %v", err, lerr)
		}
		return err
	}
	// Write the command output to the main log file.
	if _, lerr := g.logFile.WriteString(outLog); lerr != nil {
		fmt.Printf("Error writing to log file: %v\n", lerr)
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
func NewGatherer(opts *Options, f cmd.Factory, progress io.Writer) (*Gatherer, error) {
	// Create the ARTIFACT_DIR.
	if err := os.MkdirAll(opts.ArtifactDir, os.ModePerm); err != nil {
		return nil, fmt.Errorf("failed to create artifact directory: %w", err)
	}

	// Create the stdout log file.
	logPath := filepath.Join(opts.ArtifactDir, "must-gather.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, fmt.Errorf("failed to create log file: %w", err)
	}

	// Create the stderr log file.
	errLogPath := filepath.Join(opts.ArtifactDir, "must-gather.stderr.log")
	errLogFile, err := os.Create(errLogPath)
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
	if isOpenShift {
		// Construct the file path for the OpenShift version file.
		versionFilePath := filepath.Join(g.opts.ArtifactDir, "openshift_version.yaml")
		// Create the file.
		file, err := os.Create(versionFilePath)
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating openshift_version.yaml: %v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		defer file.Close() // Ensure the file is closed when done.

		// Write the OpenShift version information into the file.
		if _, err := file.WriteString(oversion); err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing openshift_version.yaml: %v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	}
	return nil
}

// gatherNodeInfo collects node information.
// For OpenShift clusters, it retrieves machine data; otherwise, it lists Kubernetes nodes.
func (g *Gatherer) gatherNodeInfo() error {
	// Determine if the cluster is OpenShift.
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

		// Create machines.yaml file.
		machinesFile, err := os.Create(filepath.Join(g.opts.ArtifactDir, "machines.yaml"))
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating machines.yaml: %v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		defer machinesFile.Close() // Ensure file closure.

		if _, err = machinesFile.Write(machines); err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing machines.yaml: %v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
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

		// Create nodes.yaml file.
		nodesFile, err := os.Create(filepath.Join(g.opts.ArtifactDir, "nodes.yaml"))
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating nodes.yaml: %v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		defer nodesFile.Close() // Ensure file closure.

		// Marshal nodes into YAML format.
		data, err := yaml.Marshal(nodes)
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error marshalling nodes: %v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}

		if _, err = nodesFile.Write(data); err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing nodes.yaml: %v\n", err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	}
	return nil
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

// logError writes an error message to g.errLogFile with the provided format and returns the wrapped error.
func (g *Gatherer) logError(format string, err error) error {
	_, lerr := fmt.Fprintf(g.errLogFile, format, err)
	if lerr != nil {
		return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
	}
	return fmt.Errorf(format+": %w", err)
}

// gatherGPUOperatorCRDs collects the GPU operator custom resources:
// the ClusterPolicy and NvidiaDriver CRs. These resources are cluster-scoped,
// so we do not specify a namespace.
func (g *Gatherer) gatherGPUOperatorCRDs() error {
	dynClient := g.dynClient

	// Set a context with timeout for our dynamic calls.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// ---------------------------
	// ClusterPolicy CR handling
	// ---------------------------
	clusterPolicyGVR := schema.GroupVersionResource{
		Group:    "nvidia.com",
		Version:  "v1",
		Resource: "clusterpolicies",
	}

	cr, err := dynClient.Resource(clusterPolicyGVR).
		Get(ctx, "cluster-policy", metav1.GetOptions{})
	if err != nil {
		// Write marker file to indicate missing ClusterPolicy CR.
		missingPath := filepath.Join(g.opts.ArtifactDir, "cluster_policy.missing")
		_ = os.WriteFile(missingPath, []byte{}, 0644)
		_, lerr := fmt.Fprintf(g.errLogFile, "Error getting clusterPolicy: %v\n", err)
		if lerr != nil {
			return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		// Continue processing to collect the other CR.
	} else {
		data, err := yaml.Marshal(cr.Object)
		if err != nil {
			return g.logError("error marshalling clusterPolicy: %v", err)
		}
		clusterPolicyFilePath := filepath.Join(g.opts.ArtifactDir, "cluster_policy.yaml")
		clusterPolicyFile, err := os.Create(clusterPolicyFilePath)
		if err != nil {
			return g.logError("error creating cluster_policy.yaml: %v", err)
		}
		defer clusterPolicyFile.Close()
		if _, err = clusterPolicyFile.Write(data); err != nil {
			return g.logError("error writing cluster_policy.yaml: %v", err)
		}
	}

	// ---------------------------
	// NvidiaDriver CR handling
	// ---------------------------
	nvidiaDriverGVR := schema.GroupVersionResource{
		Group:    "nvidia.com",
		Version:  "v1alpha1",
		Resource: "nvidiadrivers",
	}

	cr, err = dynClient.Resource(nvidiaDriverGVR).
		Get(ctx, "nvidia-driver", metav1.GetOptions{})
	if err != nil {
		// Write marker file to indicate missing NvidiaDriver CR.
		missingPath := filepath.Join(g.opts.ArtifactDir, "nvidia_driver.missing")
		_ = os.WriteFile(missingPath, []byte{}, 0644)
		_, lerr := fmt.Fprintf(g.errLogFile, "Error getting nvidiaDriver: %v\n", err)
		if lerr != nil {
			return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		// Not returning error since absence is acceptable.
	} else {
		data, err := yaml.Marshal(cr.Object)
		if err != nil {
			return g.logError("error marshalling nvidiaDriver: %v", err)
		}
		nvidiaDriverFilePath := filepath.Join(g.opts.ArtifactDir, "nvidia_driver.yaml")
		nvidiaDriverFile, err := os.Create(nvidiaDriverFilePath)
		if err != nil {
			return g.logError("error creating nvidia_driver.yaml: %v", err)
		}
		defer nvidiaDriverFile.Close()
		if _, err = nvidiaDriverFile.Write(data); err != nil {
			return g.logError("error writing nvidia_driver.yaml: %v", err)
		}
	}

	return nil
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

	// If no GPU nodes were found, log and return an error.
	if len(gpuNodes.Items) == 0 {
		if _, lerr := fmt.Fprintf(g.errLogFile, "No nodes found with NVIDIA PCI label\n"); lerr != nil {
			_ = fmt.Errorf("error writing to stderr log file: %v", lerr)
		}
		return fmt.Errorf("no nodes found with NVIDIA PCI label")
	}

	// Create the file to store the GPU node information.
	gpuNodesFilePath := filepath.Join(g.opts.ArtifactDir, "gpu_nodes.yaml")
	gpuNodesFile, err := os.Create(gpuNodesFilePath)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating gpu_nodes.yaml: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer gpuNodesFile.Close()

	// Marshal the collected GPU nodes into YAML.
	data, err := yaml.Marshal(gpuNodes)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error marshalling gpuNodes: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	// Write the YAML data to the file.
	if _, err = gpuNodesFile.Write(data); err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing gpu_nodes.yaml: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	return nil
}

// gatherPodInfo retrieves all pods from the provided operatorNamespace.
// It writes the entire pod list (operands) to "gpu_operand_pods.yaml" and
// the GPU operator pods (filtered with label "app=gpu-operator") to "gpu_operator_pod.yaml".
// It returns a PodInfo struct containing both pod lists.
func (g *Gatherer) gatherPodInfo(operatorNamespace string) (*PodInfo, error) {
	listFunc := func(opts metav1.ListOptions) (v1.PodList, string, error) {
		pl, err := g.clientset.CoreV1().Pods(operatorNamespace).List(context.Background(), opts)
		if err != nil {
			return v1.PodList{}, "", err
		}
		return *pl, pl.Continue, nil
	}
	appendFunc := func(all *v1.PodList, page v1.PodList) {
		all.Items = append(all.Items, page.Items...)
	}
	operandsPodList, err := listAllPaginated[v1.PodList](500, "", listFunc, appendFunc)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting pods: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return nil, err
	}

	// Write the operands pod list to "gpu_operand_pods.yaml".
	operandFilePath := filepath.Join(g.opts.ArtifactDir, "gpu_operand_pods.yaml")
	operandFile, err := os.Create(operandFilePath)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating gpu_operand_pods.yaml: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return nil, err
	}
	defer operandFile.Close()

	data, err := yaml.Marshal(operandsPodList)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error marshalling operands pod list: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return nil, err
	}

	if _, err = operandFile.Write(data); err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing gpu_operand_pods.yaml: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return nil, err
	}

	// Retrieve GPU operator pods using the label selector "app=gpu-operator".
	listFuncOp := func(opts metav1.ListOptions) (v1.PodList, string, error) {
		pl, err := g.clientset.CoreV1().Pods(operatorNamespace).List(context.Background(), opts)
		if err != nil {
			return v1.PodList{}, "", err
		}
		return *pl, pl.Continue, nil
	}
	operatorPodList, err := listAllPaginated[v1.PodList](500, "app=gpu-operator", listFuncOp, appendFunc)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting GPU operator pods: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return nil, err
	}
	if len(operatorPodList.Items) == 0 {
		if _, lerr := fmt.Fprintf(g.errLogFile, "No pods found with label app=gpu-operator\n"); lerr != nil {
			_ = fmt.Errorf("no pods found with label app=gpu-operator + error writing to stderr log file: %v", lerr)
		}
		return nil, fmt.Errorf("no pods found with label app=gpu-operator")
	}

	// Write the operator pod list to "gpu_operator_pod.yaml".
	operatorFilePath := filepath.Join(g.opts.ArtifactDir, "gpu_operator_pod.yaml")
	operatorFile, err := os.Create(operatorFilePath)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating gpu_operator_pod.yaml: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return nil, err
	}
	defer operatorFile.Close()

	data, err = yaml.Marshal(operatorPodList)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error marshalling GPU operator pod list: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return nil, err
	}

	if _, err = operatorFile.Write(data); err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing gpu_operator_pod.yaml: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return nil, err
	}

	// Return the pod info struct containing both operands and operator pods.
	podInfo := &PodInfo{
		OperandPods:  &operandsPodList,
		OperatorPods: &operatorPodList,
	}
	return podInfo, nil
}

// gatherPodLogs collects logs from the GPU operator pod (both current and previous logs)
// and from each GPU operand pod. It uses the provided PodInfo data structure and writes logs
// to files under the artifact directory, one file per container. The operatorNamespace is
// used for operator pod log collection.
func (g *Gatherer) gatherPodLogs(podInfo *PodInfo, operatorNamespace string) error {
	// Expecting at least one operator pod; use the first one.
	if podInfo.OperatorPods == nil || len(podInfo.OperatorPods.Items) == 0 {
		return fmt.Errorf("no operator pods available for log collection")
	}
	operatorPod := podInfo.OperatorPods.Items[0]

	// Collect current logs from the GPU Operator Pod.
	for _, container := range containerNames(&operatorPod) {
		fileName := fmt.Sprintf("gpu_operator_logs_%s.log", container)
		if err := g.collectContainerLogs(operatorNamespace, &operatorPod, container, fileName, false); err != nil {
			return err
		}
	}

	// Collect previous logs from the GPU Operator Pod.
	for _, container := range containerNames(&operatorPod) {
		fileName := fmt.Sprintf("gpu_operator_logs_%s.previous.log", container)
		if err := g.collectContainerLogs(operatorNamespace, &operatorPod, container, fileName, true); err != nil {
			return err
		}
	}

	// Collect logs for each GPU Operand Pod.
	if podInfo.OperandPods != nil {
		for i := range podInfo.OperandPods.Items {
			pod := podInfo.OperandPods.Items[i]
			for _, container := range containerNames(&pod) {
				fileName := fmt.Sprintf("%s_%s.log", pod.Name, container)
				if err := g.collectContainerLogs(pod.Namespace, &pod, container, fileName, false); err != nil {
					return err
				}
			}
		}
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

// collectContainerLogs writes the logs of a single container to fileName under
// the artifact directory. The log request always names its container, because
// the API server refuses a request without one for a pod running more than one
// container, and always asks for timestamps.
func (g *Gatherer) collectContainerLogs(namespace string, pod *v1.Pod, container, fileName string, previous bool) error {
	logPath := filepath.Join(g.opts.ArtifactDir, fileName)
	logFile, err := os.Create(logPath)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating %s: %v\n", fileName, err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer logFile.Close()

	podLogOpts := v1.PodLogOptions{
		Container:  container,
		Timestamps: true,
		Previous:   previous,
	}
	stream, err := g.podLogs(context.Background(), namespace, pod.Name, &podLogOpts)
	if err != nil || stream == nil {
		if previous {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting previous logs for pod %s container %s: %v\n", pod.Name, container, err); lerr != nil {
				return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			// If no previous logs, write a message and continue.
			return writeStringToFile(logFile, "No previous logs available\n")
		}
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting logs for pod %s container %s: %v\n", pod.Name, container, err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer stream.Close()

	writer := bufio.NewWriter(logFile)
	if err := writePrefixedLines(writer, stream, pod.Name, container); err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing %s: %v\n", fileName, err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	// The buffer holds the tail of the log until this point, so a failure here
	// truncates the artifact silently.
	if err := writer.Flush(); err != nil {
		err = fmt.Errorf("error flushing %s: %w", fileName, err)
		if _, lerr := fmt.Fprintf(g.errLogFile, "%v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	return nil
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

// writeStringToFile is a helper function that writes a string to the provided file.
func writeStringToFile(file *os.File, s string) error {
	if _, err := file.WriteString(s); err != nil {
		return fmt.Errorf("error writing string to file: %w", err)
	}
	return nil
}

// gatherDaemonSets collects daemon sets from the operatorNamespace and writes the result to "daemonsets.yaml".
func (g *Gatherer) gatherDaemonSets(operatorNamespace string) error {
	listFunc := func(opts metav1.ListOptions) (appsv1.DaemonSetList, string, error) {
		ds, err := g.clientset.AppsV1().DaemonSets(operatorNamespace).List(context.Background(), opts)
		if err != nil {
			return appsv1.DaemonSetList{}, "", err
		}
		return *ds, ds.Continue, nil
	}
	appendFunc := func(all *appsv1.DaemonSetList, page appsv1.DaemonSetList) {
		all.Items = append(all.Items, page.Items...)
	}
	daemonSets, err := listAllPaginated[appsv1.DaemonSetList](500, "", listFunc, appendFunc)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting daemonSets: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	// Create the file to store the daemon sets data.
	daemonSetsFilePath := filepath.Join(g.opts.ArtifactDir, "daemonsets.yaml")
	daemonSetsFile, err := os.Create(daemonSetsFilePath)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating daemonsets.yaml: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer daemonSetsFile.Close()

	// Marshal the daemonSets object into YAML.
	data, err := yaml.Marshal(daemonSets)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error marshalling daemonSets: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	// Write the YAML data to the file.
	if _, err = daemonSetsFile.Write(data); err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing daemonsets.yaml: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	return nil
}

// gatherBugReportFromDaemonSet runs a remote command on pods matching
// the labels "app=nvidia-driver-daemonset" and "app=nvidia-vgpu-manager-daemonset"
// to generate a bug report, and then copies the resulting file from the pod
// to the artifact directory.
func (g *Gatherer) gatherBugReportFromDaemonSet(operatorNamespace string) error {
	labels := []string{
		"app=nvidia-driver-daemonset",
		"app=nvidia-vgpu-manager-daemonset",
	}
	appendFunc := func(all *v1.PodList, page v1.PodList) {
		all.Items = append(all.Items, page.Items...)
	}
	var errs []error
	// One ledger of destination names for every daemonset: a node can run both a
	// driver pod and a vGPU manager pod, and each pass must see what the previous
	// one already wrote.
	taken := make(map[string]bool)
	for _, label := range labels {
		listFunc := func(opts metav1.ListOptions) (v1.PodList, string, error) {
			pl, err := g.clientset.CoreV1().Pods(operatorNamespace).List(context.Background(), opts)
			if err != nil {
				return v1.PodList{}, "", err
			}
			return *pl, pl.Continue, nil
		}
		podList, err := listAllPaginated[v1.PodList](500, label, listFunc, appendFunc)
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting pods for label %s: %v\n", label, err); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		if err := g.collectBugReports(label, podList.Items, taken); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// collectBugReports copies the bug report out of each of the given daemonset
// pods into the artifact directory. label identifies the daemonset the pods
// belong to and is used for error reporting. taken carries the destination
// names already used across every daemonset, and is added to here.
func (g *Gatherer) collectBugReports(label string, pods []v1.Pod, taken map[string]bool) error {
	var errs []error

	for i := range pods {
		pod := pods[i]

		// A pod that never got scheduled has no node to name a report after, and
		// one that is not running cannot answer an exec. Record the gap and move on
		// rather than spending a doomed exec on it.
		if pod.Spec.NodeName == "" {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Skipping pod %s (label %s): not scheduled to a node\n", pod.Name, label); lerr != nil {
				errs = append(errs, fmt.Errorf("error writing to stderr log file: %w", lerr))
			}
			continue
		}
		if pod.Status.Phase != v1.PodRunning {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Skipping pod %s (label %s): pod is %s, not Running\n", pod.Name, label, pod.Status.Phase); lerr != nil {
				errs = append(errs, fmt.Errorf("error writing to stderr log file: %w", lerr))
			}
			continue
		}

		// Name the report after the node the pod runs on, matching the gpu-operator
		// must-gather script. Naming it after the daemonset instead would make every
		// pod of that daemonset write to the same file. A node already collected
		// from, because it runs a second pod of this daemonset mid-rollout or a pod
		// of another one, gets the pod name too.
		destFileName := fmt.Sprintf("nvidia-bug-report_%s.log.gz", pod.Spec.NodeName)
		if taken[destFileName] {
			destFileName = fmt.Sprintf("nvidia-bug-report_%s_%s.log.gz", pod.Spec.NodeName, pod.Name)
		}
		taken[destFileName] = true
		destPath := filepath.Join(g.opts.ArtifactDir, destFileName)

		// One unreachable pod must not cost us the reports of the nodes behind it,
		// so collect what we can and report the failures together at the end.
		if err := g.fetchBugReport(&pod, destPath); err != nil {
			err = fmt.Errorf("collecting bug report from pod %s (label %s): %w", pod.Name, label, err)
			if _, lerr := fmt.Fprintf(g.errLogFile, "%v\n", err); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			errs = append(errs, err)
			continue
		}
	}

	return errors.Join(errs...)
}

// Run collects everything NVIDIA support asks for from the cluster the factory
// points at, into opts.ArtifactDir. progress carries the running commentary;
// the artifacts themselves go to files under the artifact directory.
func Run(opts *Options, f cmd.Factory, progress io.Writer) error {
	// Initialize the Gatherer with the provided options.
	g, err := NewGatherer(opts, f, progress)
	if err != nil {
		return fmt.Errorf("failed to initialize Gatherer: %w", err)
	}
	defer g.Close()

	// 1. Gather OpenShift version (if applicable).
	g.step("Gathering the OpenShift version")
	if err := g.gatherOpenShiftVersion(); err != nil {
		return err
	}

	// 2. Gather node information.
	g.step("Gathering node information")
	if err := g.gatherNodeInfo(); err != nil {
		return err
	}

	// 3. Retrieve the operator namespace.
	//    (Assumes getOperatorNamespace returns (string, error).)
	operatorNamespace, err := g.gatherOperatorNamespace()
	if err != nil {
		return err
	}
	g.step("Found the GPU operator in namespace %s", operatorNamespace)

	// 4. Collect GPU Operator CRDs (ClusterPolicy and NvidiaDriver).
	g.step("Gathering the GPU operator custom resources")
	if err := g.gatherGPUOperatorCRDs(); err != nil {
		return err
	}

	// 5. Gather GPU node information.
	g.step("Gathering GPU node information")
	if err := g.gatherGPUNodes(); err != nil {
		return err
	}

	// 6. Retrieve pod information (both operand and operator pods).
	g.step("Gathering pod information")
	podInfo, err := g.gatherPodInfo(operatorNamespace)
	if err != nil {
		return err
	}

	// 7. Collect logs from the pods using the obtained PodInfo.
	g.step("Gathering pod logs")
	if err := g.gatherPodLogs(podInfo, operatorNamespace); err != nil {
		return err
	}

	// 8. Gather information about DaemonSets.
	g.step("Gathering daemonsets")
	if err := g.gatherDaemonSets(operatorNamespace); err != nil {
		return err
	}

	// 9. Run remote command on daemonset pods to generate and collect bug report.
	g.step("Gathering bug reports from the driver daemonsets")
	if err := g.gatherBugReportFromDaemonSet(operatorNamespace); err != nil {
		return err
	}

	return nil
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
