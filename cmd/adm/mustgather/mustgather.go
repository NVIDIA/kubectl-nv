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
	"os"
	"path/filepath"
	"time"

	"github.com/urfave/cli/v2"
	"gopkg.in/yaml.v2"

	"github.com/NVIDIA/kubectl-nv/internal/logger"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/tools/clientcmd"
)

type command struct {
	logger *logger.FunLogger
}

type options struct {
	kubeconfig  string
	artifactDir string
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
	opts := options{}

	// Create the 'must-gather' command
	mg := cli.Command{
		Name:  "must-gather",
		Usage: "collects the information from your cluster that is most likely needed for debugging issues",
		Before: func(c *cli.Context) error {
			return m.validateFlags(&opts)
		},
		Action: func(c *cli.Context) error {
			m.logger.Info("Using kubeconfig: %s", opts.kubeconfig)

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
			m.logger.Info("Please send the contents of %s to NVIDIA support\n", opts.artifactDir)

			return nil
		},
	}

	mg.Flags = []cli.Flag{
		&cli.StringFlag{
			Name:        "kubeconfig",
			Aliases:     []string{"k"},
			Usage:       "path to kubeconfig file",
			Value:       "-",
			Destination: &opts.kubeconfig,
			EnvVars:     []string{"KUBECONFIG"},
		},
		&cli.StringFlag{
			Name: "artifacts-dir",
			Usage: "path to the directory where the artifacts will be stored. " +
				"Defaults to /tmp/nvidia-gpu-operator_<timestamp>",
			Value:       "",
			Destination: &opts.artifactDir,
			EnvVars:     []string{"ARTIFACT_DIR"},
		},
	}

	return &mg
}

func (m command) validateFlags(opts *options) error {
	// Get the path to the kubeconfig file
	k, err := getKubeconfigPath(opts.kubeconfig)
	if err != nil {
		return fmt.Errorf("error getting kubeconfig path: %v", err)
	}
	opts.kubeconfig = k

	// Validate artifactDir
	if opts.artifactDir == "" {
		opts.artifactDir = fmt.Sprintf("/tmp/nvidia-gpu-operator_%s", time.Now().Format("20060102_1504"))
	}
	return nil
}

func (m command) run(opts *options) error {
	// Create the ARTIFACT_DIR
	if err := os.MkdirAll(opts.artifactDir, os.ModePerm); err != nil {
		return err
	}

	// Redirect stdout and stderr to logs
	logFile, err := os.Create(filepath.Join(opts.artifactDir, "must-gather.log"))
	if err != nil {
		return err
	}
	defer logFile.Close()
	errLogFile, err := os.Create(filepath.Join(opts.artifactDir, "must-gather.stderr.log"))
	if err != nil {
		return err
	}
	defer errLogFile.Close()

	// Create the Kubernetes client
	config, err := clientcmd.BuildConfigFromFlags("", opts.kubeconfig)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating Kubernetes rest config: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	err = setKubernetesDefaults(config)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error Setting up Kubernetes rest config: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	clientset, err := createKubernetesClient(opts.kubeconfig)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating Kubernetes client: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	// Check if the cluster is OpenShift
	oversion, isOpenShift := isOpenShiftCluster(opts.kubeconfig)
	if isOpenShift {
		oversionFile, err := os.Create(filepath.Join(opts.artifactDir, "openshift_version.yaml"))
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating openshift_version.yaml: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		defer oversionFile.Close()
		_, err = oversionFile.WriteString(oversion)
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing openshift_version.yaml: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	}

	// Get Machines / Nodes
	if isOpenShift {
		machines, err := getOpenShiftMachines(opts.kubeconfig)
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting machines: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}

		machinesFile, err := os.Create(filepath.Join(opts.artifactDir, "machines.yaml"))
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating machines.yaml: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		defer machinesFile.Close()

		_, err = machinesFile.Write(machines)
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing machines.yaml: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	} else {
		nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{})
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting nodes: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}

		nodesFile, err := os.Create(filepath.Join(opts.artifactDir, "nodes.yaml"))
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating nodes.yaml: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		defer nodesFile.Close()

		data, err := yaml.Marshal(nodes)
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error marshalling nodes: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}

		_, err = nodesFile.Write(data)
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing nodes.yaml: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	}

	// Get the operator namespaces
	labelSelector := "app=gpu-operator"
	namespace := ""

	podList, err := clientset.CoreV1().Pods(namespace).List(context.TODO(), metav1.ListOptions{
		LabelSelector: labelSelector,
	})
	if err != nil {
		fmt.Printf("Error getting pods: %v\n", err)
		return err
	}
	if len(podList.Items) == 0 {
		fmt.Printf("No pods found with label %s\n", labelSelector)
		return err
	}
	operatorNamespace := podList.Items[0].Namespace
	if _, lerr := logFile.WriteString(fmt.Sprintf("Operator namespace: %s\n", operatorNamespace)); lerr != nil {
		fmt.Printf("Error writing to log file: %v\n", lerr)
	}

	// Get clusterPolicy CR
	data, err := clientset.RESTClient().Get().AbsPath("/apis/nvidia.com/v1/clusterpolicies/cluster-policy").Do(context.Background()).Raw()
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting clusterPolicy: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		if _, ferr := os.Create(filepath.Join(opts.artifactDir, "cluster_policy.missing")); ferr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, ferr)
		}
		return err
	}

	clusterPolicyFile, err := os.Create(filepath.Join(opts.artifactDir, "cluster_policy.yaml"))
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating cluster_policy.yaml: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer clusterPolicyFile.Close()

	_, err = clusterPolicyFile.Write(data)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing cluster_policy.yaml: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	// Get the labels of the nodes with NVIDIA PCI cards
	gpuPciLabelSelector := []string{
		"feature.node.kubernetes.io/pci-10de.present",
		"feature.node.kubernetes.io/pci-0302_10de.present",
		"feature.node.kubernetes.io/pci-0300_10de.present"}
	gpuNodes := v1.NodeList{}
	for _, label := range gpuPciLabelSelector {
		nodes, err := clientset.CoreV1().Nodes().List(context.Background(), metav1.ListOptions{
			LabelSelector: label,
		})
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error could not find nodes with NVIDIA PCI labels: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
				return err
			}
		} else if errors.IsNotFound(err) {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error could not find nodes with NVIDIA PCI label: %v\n", label)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
				return err
			}
		} else {
			gpuNodes.Items = append(gpuNodes.Items, nodes.Items...)
		}
	}

	if len(gpuNodes.Items) == 0 {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error could not find nodes with NVIDIA PCI labels: %v\n", gpuPciLabelSelector)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	gpuNodesFile, err := os.Create(filepath.Join(opts.artifactDir, "gpu_nodes.yaml"))
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating gpu_nodes.yaml: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer gpuNodesFile.Close()

	data, err = yaml.Marshal(gpuNodes)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error marshalling gpuNodes: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	_, err = gpuNodesFile.Write(data)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing gpu_nodes.yaml: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	// Get the GPU Operands pods
	operandsPodList, err := clientset.CoreV1().Pods(operatorNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting pods: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	gpuOperandPods, err := os.Create(filepath.Join(opts.artifactDir, "gpu_operand_pods.yaml"))
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating gpu_operand_pods.yaml: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer gpuOperandPods.Close()

	data, err = yaml.Marshal(operandsPodList)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error marshalling podList: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	_, err = gpuOperandPods.Write(data)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing gpu_operand_pods.yaml: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			return err
		}
		return err
	}

	// Get the GPU Operator pod
	gpuOperatorPods, err := clientset.CoreV1().Pods(operatorNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=gpu-operator",
	})
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting pods: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	if len(gpuOperatorPods.Items) == 0 {
		if _, lerr := errLogFile.WriteString("Error could not find GPU Operator pod\n"); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	gpuOperatorPod := gpuOperatorPods.Items[0]

	gpuOperatorPodsFile, err := os.Create(filepath.Join(opts.artifactDir, "gpu_operator_pod.yaml"))
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating gpu_operator_pod.yaml: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer gpuOperatorPodsFile.Close()

	data, err = yaml.Marshal(gpuOperatorPod)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error marshalling podList: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	_, err = gpuOperatorPodsFile.Write(data)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing gpu_operator_pod.yaml: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	// Get the GPU Operator logs
	gpuOperatorLogs, err := os.Create(filepath.Join(opts.artifactDir, "gpu_operator_logs.log"))
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating gpu_operator_logs.log: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer gpuOperatorLogs.Close()

	podLogOpts := v1.PodLogOptions{}
	req := clientset.CoreV1().Pods(operatorNamespace).GetLogs(gpuOperatorPod.Name, &podLogOpts)
	podLogs, err := req.Stream(context.Background())
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting pod logs: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer podLogs.Close()

	buf := make([]byte, 4096)
	for {
		n, err := podLogs.Read(buf)
		if err != nil {
			break
		}
		_, err = gpuOperatorLogs.Write(buf[:n])
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing pod logs: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	}

	// Get previous GPU Operator logs
	gpuOperatorLogs, err = os.Create(filepath.Join(opts.artifactDir, "gpu_operator_logs_previous.log"))
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating gpu_operator_logs_previous.log: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer gpuOperatorLogs.Close()

	podLogOpts = v1.PodLogOptions{
		Previous: true,
	}
	req = clientset.CoreV1().Pods(operatorNamespace).GetLogs(gpuOperatorPod.Name, &podLogOpts)
	podLogs, err = req.Stream(context.Background())
	if err != nil || podLogs == nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting pod logs: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			return err
		}
		// If no previous logs, available, write a message to the log file
		_, err = gpuOperatorLogs.WriteString("No previous logs available\n")
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing pod logs: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	} else {
		defer podLogs.Close()
	}

	// If no previous logs, available, podLogs will be empty
	if podLogs != nil {
		buf = make([]byte, 4096)
		for {
			n, err := podLogs.Read(buf)
			if err != nil {
				break
			}
			_, err = gpuOperatorLogs.Write(buf[:n])
			if err != nil {
				if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing pod logs: %v\n", err)); lerr != nil {
					err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
				}
				return err
			}
		}
	}

	// Get the GPU Operand logs per pod
	for _, pod := range operandsPodList.Items {
		operandLogs, err := os.Create(filepath.Join(opts.artifactDir, fmt.Sprintf("%s_logs.log", pod.Name)))
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating %s_logs.log: %v\n", pod.Name, err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		defer operandLogs.Close()

		podLogOpts := v1.PodLogOptions{}
		req := clientset.CoreV1().Pods(operatorNamespace).GetLogs(pod.Name, &podLogOpts)
		podLogs, err := req.Stream(context.Background())
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting pod logs: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		defer podLogs.Close()

		buf := make([]byte, 4096)
		for {
			n, err := podLogs.Read(buf)
			if err != nil {
				break
			}
			_, err = operandLogs.Write(buf[:n])
			if err != nil {
				if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing pod logs: %v\n", err)); lerr != nil {
					err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
				}
				return err
			}
		}
	}

	// Get DaemonSets
	daemonSets, err := clientset.AppsV1().DaemonSets(operatorNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting daemonSets: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	daemonSetsFile, err := os.Create(filepath.Join(opts.artifactDir, "daemonsets.yaml"))
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error creating daemonsets.yaml: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	defer daemonSetsFile.Close()

	data, err = yaml.Marshal(daemonSets)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error marshalling daemonSets: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	_, err = daemonSetsFile.Write(data)
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error writing daemonsets.yaml: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}

	// run kubectl exec -n operatorNamespace <pod> -- bash -c 'cd /tmp && nvidia-bug-report.sh' on
	// pod with label app=nvidia-driver-daemonset and app=nvidia-vgpu-manager-daemonset
	// Open /dev/null for writing all tar output to
	var outpipe = os.File{}
	nullFile, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0666)
	if err != nil {
		outpipe = *os.Stdout
	} else {
		outpipe = *nullFile
	}
	defer nullFile.Close()

	iostream := genericiooptions.IOStreams{In: os.Stdin, Out: &outpipe, ErrOut: os.Stderr}
	o := NewCopyOptions(iostream)
	o.Clientset = clientset
	o.ClientConfig = config

	// Get the NVIDIA Driver DaemonSet pod
	nvidiaDriverDaemonSetPods, err := clientset.CoreV1().Pods(operatorNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=nvidia-driver-daemonset",
	})
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting pods: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	// If pods are found get the nvidia-bug-report.log.gz
	if len(nvidiaDriverDaemonSetPods.Items) != 0 {
		nvidiaDriverDaemonSetPod := nvidiaDriverDaemonSetPods.Items[0]

		outLog, errLog, err := executeRemoteCommand(&nvidiaDriverDaemonSetPod, "cd /tmp && nvidia-bug-report.sh")
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error executing remote command on NVIDIA Driver DaemonSet: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			if _, lerr := errLogFile.WriteString(errLog); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		if _, lerr := logFile.WriteString(outLog); lerr != nil {
			fmt.Printf("Error writing to log file: %v\n", lerr)
		}

		srcSpec, err := extractFileSpec(fmt.Sprintf("%s/%s:/tmp/nvidia-bug-report.log.gz", nvidiaDriverDaemonSetPod.Namespace, nvidiaDriverDaemonSetPod.Name))
		if err != nil {
			return err
		}
		destSpec, err := extractFileSpec(filepath.Join(opts.artifactDir, "nvidia-bug-report-nvidiaDriverDaemonSetPod.log.gz"))
		if err != nil {
			return err
		}
		if err = o.copyFromPod(srcSpec, destSpec); err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error copying from NVIDIA Driver DaemonSet: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	}

	// Get the NVIDIA vGPU Manager DaemonSet pod
	nvidiaVGPUDaemonSetPods, err := clientset.CoreV1().Pods(operatorNamespace).List(context.Background(), metav1.ListOptions{
		LabelSelector: "app=nvidia-vgpu-manager-daemonset",
	})
	if err != nil {
		if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error getting pods: %v\n", err)); lerr != nil {
			err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
		}
		return err
	}
	// If pods are found get the nvidia-bug-report.log.gz
	if len(nvidiaVGPUDaemonSetPods.Items) != 0 {
		nvidiaVGPUDaemonSetPod := nvidiaVGPUDaemonSetPods.Items[0]
		outLog, errLog, err := executeRemoteCommand(&nvidiaVGPUDaemonSetPod, "cd /tmp && nvidia-bug-report.sh")
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error executing remote command on NVIDIA vGPU Manager DaemonSet: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			if _, lerr := errLogFile.WriteString(errLog); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		if _, lerr := logFile.WriteString(outLog); lerr != nil {
			fmt.Printf("Error writing to log file: %v\n", lerr)
		}

		srcSpec, err := extractFileSpec(fmt.Sprintf("%s/%s:/tmp/nvidia-bug-report.log.gz", nvidiaVGPUDaemonSetPod.Namespace, nvidiaVGPUDaemonSetPod.Name))
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error extracting file spec: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		destSpec, err := extractFileSpec(filepath.Join(opts.artifactDir, "nvidia-bug-report-nvidiaVGPUDaemonSetPod.log.gz"))
		if err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error extracting file spec: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
		if err = o.copyFromPod(srcSpec, destSpec); err != nil {
			if _, lerr := errLogFile.WriteString(fmt.Sprintf("Error copying from NVIDIA vGPU Manager DaemonSet: %v\n", err)); lerr != nil {
				err = fmt.Errorf("%v+ error writing to stderr log file: %v", err, lerr)
			}
			return err
		}
	}

	return nil
}

func (m *command) done() {
	m.logger.Done <- struct{}{}
	m.logger.Wg.Wait()
}

func (m *command) fail() {
	m.logger.Fail <- struct{}{}
	m.logger.Wg.Wait()
}
