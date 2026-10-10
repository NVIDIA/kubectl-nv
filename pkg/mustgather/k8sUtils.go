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

	"github.com/openshift/client-go/config/clientset/versioned"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/kubectl/pkg/scheme"
)

// isOpenShiftCluster reports the cluster version of an OpenShift cluster. A
// cluster that does not answer the OpenShift version endpoint is not one, which
// covers a client that cannot even be built: there is nothing to gather from
// OpenShift either way, and the rest of the gather still has work to do.
func isOpenShiftCluster(config *rest.Config) (string, bool) {
	// Create OpenShift client
	osClient, err := versioned.NewForConfig(config)
	if err != nil {
		return "", false
	}

	// Retrieve ClusterVersion
	var openshiftVersion []byte
	openshiftVersion, err = osClient.RESTClient().Get().AbsPath("apis/config.openshift.io/v1/clusterversions/version").DoRaw(context.Background())
	if err != nil {
		return "", false
	}

	return string(openshiftVersion), true
}

func getOpenShiftMachines(config *rest.Config) ([]byte, error) {
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

// executeRemoteCommand runs command inside pod and returns its stdout and
// stderr. It goes through restCfg, the configuration the rest of the gather
// uses, rather than loading the default kubeconfig again: the exec has to land
// on the cluster the user named with --kubeconfig and --context.
func executeRemoteCommand(restCfg *rest.Config, pod *v1.Pod, command string) (string, string, error) {
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
