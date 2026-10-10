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

	"gopkg.in/yaml.v2"
	corev1 "k8s.io/api/core/v1"
	resourcev1 "k8s.io/api/resource/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
)

// File names written by cmd/adm/mustgather. gatherNodeInfo writes nodes.yaml,
// gatherPodInfo writes gpu_operand_pods.yaml, and gatherCRs writes
// cluster_policy.yaml. All three go through gopkg.in/yaml.v2.
const (
	fileNodes                    = "nodes.yaml"
	fileOperandPods              = "gpu_operand_pods.yaml"
	fileClusterPolicy            = "cluster_policy.yaml"
	resourceSlicesInBundleReason = "must-gather does not collect ResourceSlices"
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

// Bundle reads a must-gather directory in the layout must-gather writes
// today. The files carry gopkg.in/yaml.v2 output, which ignores the json tags
// on the Kubernetes API types and lowercases the Go field names instead, so
// they are decoded with the same library rather than with sigs.k8s.io/yaml.
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

// readYAML decodes the named file into out. A missing file becomes a
// PartialError naming that file, which is the normal case for a bundle
// gathered from a cluster where some read failed.
func (b *Bundle) readYAML(name string, out any) error {
	data, err := os.ReadFile(filepath.Join(b.dir, name))
	if err != nil {
		return &PartialError{Resources: []string{name}, Err: err}
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		return &PartialError{Resources: []string{name}, Err: fmt.Errorf("decode %s: %w", name, err)}
	}
	return nil
}

// Nodes returns the GPU nodes recorded in nodes.yaml.
//
// Only the label rules can fire offline. must-gather marshals the node list
// with yaml.v2, which cannot see the unexported fields of
// resource.Quantity, so a capacity entry survives as its format and loses its
// amount; and must-gather collects no ResourceSlices at all. The same
// selectGPUNodes the live path uses is still what decides, so the rules stay
// in one place.
func (b *Bundle) Nodes(ctx context.Context) ([]corev1.Node, error) {
	var list corev1.NodeList
	if err := b.readYAML(fileNodes, &list); err != nil {
		return nil, err
	}
	return selectGPUNodes(list.Items, nil), nil
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

// ResourceSlices always reports a partial read: must-gather writes no
// ResourceSlice data, so an offline replay cannot answer the question.
func (b *Bundle) ResourceSlices(ctx context.Context) ([]resourcev1.ResourceSlice, error) {
	return nil, &PartialError{
		Resources: []string{"resourceslices"},
		Err:       errors.New(resourceSlicesInBundleReason),
	}
}

// ClusterPolicies returns the single ClusterPolicy recorded in
// cluster_policy.yaml. must-gather writes the unstructured object map, so this
// file carries the real API field names rather than lowercased Go ones.
func (b *Bundle) ClusterPolicies(ctx context.Context) ([]unstructured.Unstructured, error) {
	var raw map[any]any
	if err := b.readYAML(fileClusterPolicy, &raw); err != nil {
		return nil, err
	}
	obj, ok := normalizeYAMLMap(raw).(map[string]any)
	if !ok {
		return nil, &PartialError{
			Resources: []string{fileClusterPolicy},
			Err:       errors.New("content is not a Kubernetes object"),
		}
	}
	return []unstructured.Unstructured{{Object: obj}}, nil
}

// normalizeYAMLMap converts the map[interface{}]interface{} values yaml.v2
// produces into the map[string]interface{} that unstructured.Unstructured
// requires. A key that is not a string is dropped: the Kubernetes object model
// has no place for it, and keeping it would make the object fail every
// accessor that walks it.
func normalizeYAMLMap(v any) any {
	switch t := v.(type) {
	case map[any]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			key, ok := k.(string)
			if !ok {
				continue
			}
			out[key] = normalizeYAMLMap(val)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[k] = normalizeYAMLMap(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i := range t {
			out[i] = normalizeYAMLMap(t[i])
		}
		return out
	default:
		return v
	}
}
