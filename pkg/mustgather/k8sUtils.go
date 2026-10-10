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
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/openshift/client-go/config/clientset/versioned"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/scheme"
)

func getKubeconfigPath(path string) (string, error) {
	// 1. Check if the path is a file that exists
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	// 2. Check if KUBECONFIG environment variable is set
	kubeconfigEnv := os.Getenv("KUBECONFIG")
	if kubeconfigEnv != "" {
		return kubeconfigEnv, nil
	}

	// 3. If KUBECONFIG environment variable is not set, check for kubeconfig in the provided path or default path
	kubeconfigPath := filepath.Join(os.Getenv("HOME"), ".kube", "config")
	if len(os.Args) > 1 {
		// If a path is provided as a command-line argument, use that
		kubeconfigPath = os.Args[1]
	}

	// Check if the file exists
	if _, err := os.Stat(kubeconfigPath); err == nil {
		return kubeconfigPath, nil
	}

	// If the file does not exist, return an error
	return "", fmt.Errorf("kubeconfig file not found at %s", kubeconfigPath)
}

func createKubernetesClient(kubeconfig string) (*kubernetes.Clientset, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	return clientset, nil
}

func isOpenShiftCluster(kubeconfig string) (string, bool) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		fmt.Printf("Error creating Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	// Create OpenShift client
	osClient, err := versioned.NewForConfig(config)
	if err != nil {
		fmt.Printf("Error creating OpenShift client: %v\n", err)
		os.Exit(1)
	}

	// Retrieve ClusterVersion
	var openshiftVersion []byte
	openshiftVersion, err = osClient.RESTClient().Get().AbsPath("apis/config.openshift.io/v1/clusterversions/version").DoRaw(context.Background())
	if err != nil {
		return "", false
	}

	return string(openshiftVersion), true
}

func getOpenShiftMachines(kubeconfigPath string) ([]byte, error) {
	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		return nil, err
	}

	// Create OpenShift client
	osClient, err := versioned.NewForConfig(config)
	if err != nil {
		return nil, err
	}

	// Retrieve MachineSets
	var machines []byte
	machines, err = osClient.RESTClient().Get().AbsPath("apis/machine.openshift.io/v1beta1/namespaces/openshift-machine-api/machines").DoRaw(context.Background())
	if err != nil {
		return nil, err
	}

	return machines, nil
}

func executeRemoteCommand(pod *v1.Pod, command string) (string, string, error) {
	kubeCfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)
	restCfg, err := kubeCfg.ClientConfig()
	if err != nil {
		return "", "", err
	}
	coreClient, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return "", "", err
	}

	buf := &bytes.Buffer{}
	errBuf := &bytes.Buffer{}
	request := coreClient.CoreV1().RESTClient().
		Post().
		Namespace(pod.Namespace).
		Resource("pods").
		Name(pod.Name).
		SubResource("exec").
		VersionedParams(&v1.PodExecOptions{
			Command: []string{"/bin/sh", "-c", command},
			Stdin:   false,
			Stdout:  true,
			Stderr:  true,
			TTY:     true,
		}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(restCfg, "POST", request.URL())
	if err != nil {
		return "", "", err
	}
	err = exec.StreamWithContext(context.TODO(), remotecommand.StreamOptions{
		Stdout: buf,
		Stderr: errBuf,
	})
	if err != nil {
		return "", "", fmt.Errorf("%w Failed executing command %s on %v/%v", err, command, pod.Namespace, pod.Name)
	}

	return buf.String(), errBuf.String(), nil
}

// setKubernetesDefaults sets default values on the provided client config for accessing the
// Kubernetes API or returns an error if any of the defaults are impossible or invalid.
func setKubernetesDefaults(config *rest.Config) error {
	// TODO remove this hack.  This is allowing the GetOptions to be serialized.
	config.GroupVersion = &schema.GroupVersion{Group: "", Version: "v1"}

	if config.APIPath == "" {
		config.APIPath = "/api"
	}
	if config.NegotiatedSerializer == nil {
		// This codec factory ensures the resources are not converted. Therefore, resources
		// will not be round-tripped through internal versions. Defaulting does not happen
		// on the client.
		config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()
	}
	return rest.SetKubernetesDefaults(config)
}
