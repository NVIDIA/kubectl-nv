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
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v2"

	"github.com/NVIDIA/kubectl-nv/internal/logger"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type command struct {
	logger *logger.FunLogger
}

// Options defines the configurable parameters for the must-gather operation.
type Options struct {
	ArtifactDir string // Directory to store gathered artifacts/logs.
	Kubeconfig  string // Path to the Kubernetes configuration file.
}

// Gatherer holds all the initialized resources needed for must-gather operations.
type Gatherer struct {
	opts       *Options
	logFile    *os.File
	errLogFile *os.File
	config     *rest.Config
	clientset  *kubernetes.Clientset
}

// PodInfo groups the GPU operand pods and operator pods retrieved from the cluster.
type PodInfo struct {
	OperandPods  *v1.PodList
	OperatorPods *v1.PodList
}

// NewCommand constructs a must-gather command with the specified logger
func NewCommand(logger *logger.FunLogger) *cli.Command {
	c := command{
		logger: logger,
	}
	return c.build()
}

// build creates the CLI command
func (m command) build() *cli.Command {
	opts := Options{}

	// Create the 'must-gather' command
	mg := cli.Command{
		Name:  "must-gather",
		Usage: "collects the information from your cluster that is most likely needed for debugging issues",
		Before: func(c *cli.Context) error {
			return m.validateFlags(&opts)
		},
		Action: func(c *cli.Context) error {
			m.logger.Info("Using kubeconfig: %s", opts.Kubeconfig)

			// Start the loading animation
			m.logger.Wg.Add(1)
			go m.logger.Loading("Collecting cluster information")

			err := m.run(&opts)
			if err != nil {
				m.fail()
				m.logger.Error(err)
				m.logger.Exit(1)
			}
			// All done!
			m.done()
			m.logger.Info("Please send the contents of %s to NVIDIA support\n", opts.ArtifactDir)

			return nil
		},
	}

	mg.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "kubeconfig",
			Aliases:     []string{"k"},
			Usage:       "path to kubeconfig file",
			Value:       "-",
			Destination: &opts.Kubeconfig,
			EnvVars:     []string{"KUBECONFIG"},
		},
		&cli.StringFlag{
			Name: "artifacts-dir",
			Usage: "path to the directory where the artifacts will be stored. " +
				"Defaults to /tmp/nvidia-gpu-operator_<timestamp>",
			Value:       "",
			Destination: &opts.ArtifactDir,
			EnvVars:     []string{"ARTIFACT_DIR"},
		},
	}

	return &mg
}

// NewGatherer initializes a new Gatherer by performing the following tasks:
// 1. Create the artifact directory.
// 2. Create log files for stdout and stderr.
// 3. Build the Kubernetes REST config and apply default settings.
// 4. Create the Kubernetes clientset.
// If any step fails, any opened file resources are closed before returning.
func NewGatherer(opts *Options) (*Gatherer, error) {
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

	// Build the Kubernetes client configuration.
	config, err := clientcmd.BuildConfigFromFlags("", opts.Kubeconfig)
	if err != nil {
		if _, lerr := fmt.Fprintf(errLogFile, "Error creating Kubernetes REST config: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		logFile.Close()
		errLogFile.Close()
		return nil, err
	}

	// Set Kubernetes defaults (e.g., QPS limits, Burst, etc.).
	if err = setKubernetesDefaults(config); err != nil {
		if _, lerr := fmt.Fprintf(errLogFile, "Error setting Kubernetes defaults: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		logFile.Close()
		errLogFile.Close()
		return nil, err
	}

	// Create the Kubernetes clientset.
	clientset, err := createKubernetesClient(opts.Kubeconfig)
	if err != nil {
		if _, lerr := fmt.Fprintf(errLogFile, "Error creating Kubernetes client: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		logFile.Close()
		errLogFile.Close()
		return nil, err
	}

	// Return a fully initialized Gatherer.
	return &Gatherer{
		opts:       opts,
		logFile:    logFile,
		errLogFile: errLogFile,
		config:     config,
		clientset:  clientset,
	}, nil
}

func (m command) validateFlags(opts *Options) error {
	// Get the path to the kubeconfig file
	k, err := getKubeconfigPath(opts.Kubeconfig)
	if err != nil {
		return fmt.Errorf("error getting kubeconfig path: %v", err)
	}
	opts.Kubeconfig = k

	// Validate artifactDir
	if opts.ArtifactDir == "" {
		opts.ArtifactDir = fmt.Sprintf("/tmp/nvidia-gpu-operator_%s", time.Now().Format("20060102_1504"))
	}
	return nil
}

// gatherOpenShiftVersion checks if the cluster is OpenShift using the kubeconfig,
// and if so, writes the OpenShift version to "openshift_version.yaml" in the artifact directory.
func (g *Gatherer) gatherOpenShiftVersion() error {
	// Determine if the cluster is OpenShift.
	oversion, isOpenShift := isOpenShiftCluster(g.opts.Kubeconfig)
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
	_, isOpenShift := isOpenShiftCluster(g.opts.Kubeconfig)
	if isOpenShift {
		// Get machines data from an OpenShift cluster.
		machines, err := getOpenShiftMachines(g.opts.Kubeconfig)
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
		// Retrieve nodes using the Kubernetes clientset.
		nodes, err := g.clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
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
	// Using an empty namespace to list pods across all namespaces.
	namespace := ""

	// Retrieve the list of pods matching the label selector.
	podList, err := g.clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting pods: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return "", err
	}

	// Check if any pods were found.
	if len(podList.Items) == 0 {
		if _, lerr := fmt.Fprintf(g.logFile, "No pods found with label %s\n", labelSelector); lerr != nil {
			if _, lerr2 := fmt.Fprintf(g.errLogFile, "Error writing to log file: %v\n", lerr); lerr2 != nil {
				return "", fmt.Errorf("%v + error writing to stderr log file: %v", lerr, lerr2)
			}
		}
		return "", fmt.Errorf("no pods found with label %s", labelSelector)
	}

	// Extract the operator namespace from the first pod.
	operatorNamespace := podList.Items[0].Namespace
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
	// Create a dynamic client using the stored Kubernetes REST config.
	dynClient, err := dynamic.NewForConfig(g.config)
	if err != nil {
		return g.logError("error creating dynamic client: %v", err)
	}

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
	// Define the label selectors for nodes with NVIDIA PCI cards.
	gpuPciLabelSelectors := []string{
		"feature.node.kubernetes.io/pci-10de.present",
		"feature.node.kubernetes.io/pci-0302_10de.present",
		"feature.node.kubernetes.io/pci-0300_10de.present",
	}

	// Initialize an empty NodeList to collect GPU nodes.
	var gpuNodes v1.NodeList

	// Iterate over each label selector and append found nodes.
	for _, label := range gpuPciLabelSelectors {
		nodes, err := g.clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
			LabelSelector: label,
		})
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting nodes with NVIDIA PCI label '%s': %v\n", label, err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		gpuNodes.Items = append(gpuNodes.Items, nodes.Items...)
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
	// Retrieve all pods from the operatorNamespace as GPU operand pods.
	operandsPodList, err := g.clientset.CoreV1().Pods(operatorNamespace).List(context.Background(), metav1.ListOptions{})
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
	operatorPodList, err := g.clientset.CoreV1().Pods(operatorNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=gpu-operator",
	})
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
		OperandPods:  operandsPodList,
		OperatorPods: operatorPodList,
	}
	return podInfo, nil
}

// gatherPodLogs collects logs from the GPU operator pod (both current and previous logs)
// and from each GPU operand pod. It uses the provided PodInfo data structure and writes logs
// to files under the artifact directory. The operatorNamespace is used for operator pod log collection.
func (g *Gatherer) gatherPodLogs(podInfo *PodInfo, operatorNamespace string) error {
	// ------------------------------------------------------------------
	// Collect logs from the GPU Operator Pod (current logs)
	// ------------------------------------------------------------------
	// Expecting at least one operator pod; use the first one.
	if podInfo.OperatorPods == nil || len(podInfo.OperatorPods.Items) == 0 {
		return fmt.Errorf("no operator pods available for log collection")
	}
	operatorPod := podInfo.OperatorPods.Items[0]

	operatorLogsPath := filepath.Join(g.opts.ArtifactDir, "gpu_operator_logs.log")
	opLogFile, err := os.Create(operatorLogsPath)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating gpu_operator_logs.log: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer opLogFile.Close()

	// Get current logs from the operator pod.
	podLogOpts := v1.PodLogOptions{}
	req := g.clientset.CoreV1().Pods(operatorNamespace).GetLogs(operatorPod.Name, &podLogOpts)
	stream, err := req.Stream(context.Background())
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting pod logs for operator pod %s: %v\n", operatorPod.Name, err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer stream.Close()

	buf := make([]byte, 4096)
	for {
		n, rerr := stream.Read(buf)
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error reading logs for operator pod %s: %v\n", operatorPod.Name, rerr); lerr != nil {
				rerr = fmt.Errorf("%v + error writing to stderr log file: %v", rerr, lerr)
			}
			return rerr
		}
		if n > 0 {
			if _, werr := opLogFile.Write(buf[:n]); werr != nil {
				if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing to gpu_operator_logs.log: %v\n", werr); lerr != nil {
					werr = fmt.Errorf("%v + error writing to stderr log file: %v", werr, lerr)
				}
				return werr
			}
		}
	}

	// ------------------------------------------------------------------
	// Collect previous logs from the GPU Operator Pod
	// ------------------------------------------------------------------
	prevLogsPath := filepath.Join(g.opts.ArtifactDir, "gpu_operator_logs_previous.log")
	prevLogFile, err := os.Create(prevLogsPath)
	if err != nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating gpu_operator_logs_previous.log: %v\n", err); lerr != nil {
			err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer prevLogFile.Close()

	prevLogOpts := v1.PodLogOptions{Previous: true}
	req = g.clientset.CoreV1().Pods(operatorNamespace).GetLogs(operatorPod.Name, &prevLogOpts)
	prevStream, err := req.Stream(context.Background())
	if err != nil || prevStream == nil {
		if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting previous logs for operator pod %s: %v\n", operatorPod.Name, err); lerr != nil {
			return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
		}
		// If no previous logs, write a message and continue.
		if writeErr := writeStringToFile(prevLogFile, "No previous logs available\n"); writeErr != nil {
			return writeErr
		}
	} else {
		defer prevStream.Close()
		buf = make([]byte, 4096)
		for {
			n, rerr := prevStream.Read(buf)
			if rerr != nil {
				if rerr == io.EOF {
					break
				}
				if _, lerr := fmt.Fprintf(g.errLogFile, "Error reading previous logs for operator pod %s: %v\n", operatorPod.Name, rerr); lerr != nil {
					rerr = fmt.Errorf("%v + error writing to stderr log file: %v", rerr, lerr)
				}
				return rerr
			}
			if n > 0 {
				if _, werr := prevLogFile.Write(buf[:n]); werr != nil {
					if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing previous logs for operator pod %s: %v\n", operatorPod.Name, werr); lerr != nil {
						werr = fmt.Errorf("%v + error writing to stderr log file: %v", werr, lerr)
					}
					return werr
				}
			}
		}
	}

	// ------------------------------------------------------------------
	// Collect logs for each GPU Operand Pod
	// ------------------------------------------------------------------
	for _, pod := range podInfo.OperandPods.Items {
		operandLogPath := filepath.Join(g.opts.ArtifactDir, fmt.Sprintf("%s_logs.log", pod.Name))
		operandLogFile, err := os.Create(operandLogPath)
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error creating log file for pod %s: %v\n", pod.Name, err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		// Note: Avoid deferring inside loops; close explicitly after use.

		podLogOpts := v1.PodLogOptions{}
		req := g.clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &podLogOpts)
		operandStream, err := req.Stream(context.Background())
		if err != nil {
			operandLogFile.Close()
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting logs for operand pod %s: %v\n", pod.Name, err); lerr != nil {
				err = fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
			}
			return err
		}

		buf = make([]byte, 4096)
		for {
			n, rerr := operandStream.Read(buf)
			if rerr != nil {
				if rerr == io.EOF {
					break
				}
				if _, lerr := fmt.Fprintf(g.errLogFile, "Error reading logs for operand pod %s: %v\n", pod.Name, rerr); lerr != nil {
					rerr = fmt.Errorf("%v + error writing to stderr log file: %v", rerr, lerr)
				}
				operandStream.Close()
				operandLogFile.Close()
				return rerr
			}
			if n > 0 {
				if _, werr := operandLogFile.Write(buf[:n]); werr != nil {
					if _, lerr := fmt.Fprintf(g.errLogFile, "Error writing logs for operand pod %s: %v\n", pod.Name, werr); lerr != nil {
						werr = fmt.Errorf("%v + error writing to stderr log file: %v", werr, lerr)
					}
					operandStream.Close()
					operandLogFile.Close()
					return werr
				}
			}
		}
		// Close file and stream once finished with each operand pod.
		operandStream.Close()
		operandLogFile.Close()
	}

	return nil
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
	// Retrieve DaemonSets from the operator namespace.
	daemonSets, err := g.clientset.AppsV1().DaemonSets(operatorNamespace).List(context.Background(), metav1.ListOptions{})
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
	// Attempt to open /dev/null for output redirection.
	nullFile, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0666)
	var outpipe *os.File
	if err != nil {
		outpipe = os.Stdout
	} else {
		outpipe = nullFile
		defer nullFile.Close() // Only close if opened successfully.
	}

	// Initialize IOStreams and copy options.
	iostream := genericiooptions.IOStreams{
		In:     os.Stdin,
		Out:    outpipe,
		ErrOut: os.Stderr,
	}
	o := NewCopyOptions(iostream)
	o.Clientset = g.clientset
	o.ClientConfig = g.config

	// Define the labels to try.
	labels := []string{
		"app=nvidia-driver-daemonset",
		"app=nvidia-vgpu-manager-daemonset",
	}

	// Iterate over the labels.
	for _, label := range labels {
		// List pods matching the current label.
		podList, err := g.clientset.CoreV1().Pods(operatorNamespace).List(context.Background(), metav1.ListOptions{
			LabelSelector: label,
		})
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error getting pods for label %s: %v\n", label, err); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		// Skip if no pod found for this label.
		if len(podList.Items) == 0 {
			continue
		}

		// Use the first available pod.
		pod := podList.Items[0]

		// Execute remote command to run the bug report script.
		outLog, errLog, err := executeRemoteCommand(&pod, "cd /tmp && nvidia-bug-report.sh")
		if err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error executing remote command on pod %s (label %s): %v\n", pod.Name, label, err); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
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

		// Build the destination file spec.
		// We build a filename based on the label (sanitized for clarity).
		var destFileName string
		switch label {
		case "app=nvidia-driver-daemonset":
			destFileName = "nvidia-bug-report-nvidiaDriverDaemonSetPod.log.gz"
		case "app=nvidia-vgpu-manager-daemonset":
			destFileName = "nvidia-bug-report-nvidiaVgpuManagerDaemonSetPod.log.gz"
		default:
			destFileName = "nvidia-bug-report-unknown.log.gz"
		}
		destPath := filepath.Join(g.opts.ArtifactDir, destFileName)
		destSpec, err := extractFileSpec(destPath)
		if err != nil {
			return err
		}

		// Copy the bug report file from the pod to the destination.
		if err = o.copyFromPod(srcSpec, destSpec); err != nil {
			if _, lerr := fmt.Fprintf(g.errLogFile, "Error copying bug report from pod %s (label %s): %v\n", pod.Name, label, err); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	}
	return nil
}

func (m command) run(opts *Options) error {
	// Initialize the Gatherer with the provided options.
	g, err := NewGatherer(opts)
	if err != nil {
		return fmt.Errorf("failed to initialize Gatherer: %w", err)
	}
	defer g.Close()

	// 1. Gather OpenShift version (if applicable).
	if err := g.gatherOpenShiftVersion(); err != nil {
		return err
	}

	// 2. Gather node information.
	if err := g.gatherNodeInfo(); err != nil {
		return err
	}

	// 3. Retrieve the operator namespace.
	//    (Assumes getOperatorNamespace returns (string, error).)
	operatorNamespace, err := g.gatherOperatorNamespace()
	if err != nil {
		return err
	}

	// 4. Collect GPU Operator CRDs (ClusterPolicy and NvidiaDriver).
	if err := g.gatherGPUOperatorCRDs(); err != nil {
		return err
	}

	// 5. Gather GPU node information.
	if err := g.gatherGPUNodes(); err != nil {
		return err
	}

	// 6. Retrieve pod information (both operand and operator pods).
	podInfo, err := g.gatherPodInfo(operatorNamespace)
	if err != nil {
		return err
	}

	// 7. Collect logs from the pods using the obtained PodInfo.
	if err := g.gatherPodLogs(podInfo, operatorNamespace); err != nil {
		return err
	}

	// 8. Gather information about DaemonSets.
	if err := g.gatherDaemonSets(operatorNamespace); err != nil {
		return err
	}

	// 9. Run remote command on daemonset pods to generate and collect bug report.
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

func (m *command) done() {
	m.logger.Done <- struct{}{}
	m.logger.Wg.Wait()
}

func (m *command) fail() {
	m.logger.Fail <- struct{}{}
	m.logger.Wg.Wait()
}
