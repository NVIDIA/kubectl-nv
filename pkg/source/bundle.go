/**
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package source

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/yaml"
)

// File names written by cmd/adm/mustgather. gatherNodeInfo writes nodes.yaml,
// gatherPodInfo writes gpu_operand_pods.yaml, and gatherCRs writes
// cluster_policy.yaml.
const (
	fileNodes          = "nodes.yaml"
	fileOperandPods    = "gpu_operand_pods.yaml"
	fileClusterPolicy  = "cluster_policy.yaml"
	fileResourceSlices = "resourceslices.yaml"
)

// MissingBundleError reports that the path given to NewBundle is not a
// readable must-gather directory.
type MissingBundleError struct {
	Dir string
	Err error
}

func (e *MissingBundleError) Error() string {
	return fmt.Sprintf("must-gather bundle %q is not a readable directory: %v", e.Dir, e.Err)
}

// Unwrap exposes the stat failure to errors.Is, so a caller can still ask
// whether the path was absent or merely unreadable.
func (e *MissingBundleError) Unwrap() error { return e.Err }

// Bundle reads a must-gather directory. The files carry Kubernetes-native
// YAML: the json field names, with quantities as the strings the API serves,
// which is what "kubectl get -o yaml" prints and what gpu-operator's
// hack/must-gather.sh writes. They are decoded with sigs.k8s.io/yaml, the same
// path the API machinery uses, so a quantity keeps its amount.
//
// An earlier must-gather marshalled the API types with gopkg.in/yaml.v2, which
// ignores the json tags, lowercases the Go field names and cannot see the
// unexported fields of resource.Quantity, so every capacity amount was lost on
// the way out. That format is not read: no release ever shipped it.
type Bundle struct {
	dir string
}

// NewBundle opens the must-gather directory at dir. The directory is checked
// up front; individual files are read lazily, because a bundle is routinely
// missing some of them.
func NewBundle(dir string) (*Bundle, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, &MissingBundleError{Dir: dir, Err: err}
	}
	if !info.IsDir() {
		return nil, &MissingBundleError{Dir: dir, Err: fs.ErrInvalid}
	}
	return &Bundle{dir: dir}, nil
}

// readFile decodes the named file into out, reporting the plain failure. Both
// the missing-file and the undecodable-file case name the file, so a caller
// can report either one without adding the name back.
func (b *Bundle) readFile(name string, out any) error {
	data, err := os.ReadFile(filepath.Join(b.dir, name))
	if err != nil {
		return err
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	return nil
}

// readYAML decodes the named file into out and turns any failure into a
// PartialError naming that file, which is the normal case for a bundle
// gathered from a cluster where some read failed.
func (b *Bundle) readYAML(name string, out any) error {
	if err := b.readFile(name, out); err != nil {
		return &PartialError{Resources: []string{name}, Err: err}
	}
	return nil
}

// Nodes returns the GPU nodes recorded in nodes.yaml. The same classifyNodes
// the live path uses is what decides, so the rule lives in one place.
func (b *Bundle) Nodes(ctx context.Context) ([]corev1.Node, error) {
	gpus, _, err := b.classify(ctx)
	return gpus, err
}

// HintedNodes returns the nodes in nodes.yaml that carry an NVIDIA discovery
// label without backing it with capacity or a slice.
func (b *Bundle) HintedNodes(ctx context.Context) ([]corev1.Node, error) {
	_, hinted, err := b.classify(ctx)
	return hinted, err
}

// classify reads nodes.yaml and, when the bundle carries one, the slice file.
//
// A missing nodes.yaml is the end of it: there is nothing to classify. A
// missing slice file is not, but it does mean one of the two membership rules
// could not be evaluated, so both sets come back with a PartialError naming
// resourceslices. The answer is wrong in a known direction: a node that is a
// GPU node only by its slice falls out of both sets rather than into the
// wrong one.
func (b *Bundle) classify(ctx context.Context) (gpus, hinted []corev1.Node, err error) {
	var list corev1.NodeList
	if err := b.readYAML(fileNodes, &list); err != nil {
		return nil, nil, err
	}

	slices, sliceErr := b.ResourceSlices(ctx)
	gpus, hinted = classifyNodes(list.Items, sliceNodeNames(slices))
	return gpus, hinted, sliceErr
}

// Pods returns the operand pods recorded in gpu_operand_pods.yaml, filtered by
// namespace and selector the way the API server would have filtered them.
func (b *Bundle) Pods(ctx context.Context, namespace, selector string) ([]corev1.Pod, error) {
	sel := labels.Everything()
	if selector != "" {
		parsed, err := labels.Parse(selector)
		if err != nil {
			return nil, fmt.Errorf("parse selector %q: %w", selector, err)
		}
		sel = parsed
	}

	var list corev1.PodList
	if err := b.readYAML(fileOperandPods, &list); err != nil {
		return nil, err
	}

	out := make([]corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		p := list.Items[i]
		if namespace != "" && p.Namespace != namespace {
			continue
		}
		if !sel.Matches(labels.Set(p.Labels)) {
			continue
		}
		out = append(out, p)
	}
	return out, nil
}

// ResourceSlices returns the slices recorded in resourceslices.yaml that were
// published by DRADriverName.
//
// A bundle gathered before must-gather started collecting them has no such
// file, and neither does one where the slice read was denied. That is not an
// empty cluster, so it is reported as a PartialError naming resourceslices
// rather than as an empty success. The error names the API resource, not the
// file, because that is what the caller could not learn about.
func (b *Bundle) ResourceSlices(ctx context.Context) ([]resourcev1.ResourceSlice, error) {
	var list resourcev1.ResourceSliceList
	if err := b.readFile(fileResourceSlices, &list); err != nil {
		return nil, &PartialError{Resources: []string{"resourceslices"}, Err: err}
	}

	out := make([]resourcev1.ResourceSlice, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.Driver == DRADriverName {
			out = append(out, list.Items[i])
		}
	}
	return out, nil
}

// ClusterPolicies returns the single ClusterPolicy recorded in
// cluster_policy.yaml.
func (b *Bundle) ClusterPolicies(ctx context.Context) ([]unstructured.Unstructured, error) {
	var obj map[string]any
	if err := b.readYAML(fileClusterPolicy, &obj); err != nil {
		return nil, err
	}
	if len(obj) == 0 {
		return nil, &PartialError{
			Resources: []string{fileClusterPolicy},
			Err:       errors.New("content is not a Kubernetes object"),
		}
	}
	return []unstructured.Unstructured{{Object: obj}}, nil
}
