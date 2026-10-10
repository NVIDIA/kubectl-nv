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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

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

	// defaultMaxFileBytes caps a single file read. A must-gather directory
	// arrives in a support ticket, so its size is not this process's to
	// trust; 256 MiB is far above any real node list and far below enough to
	// exhaust a laptop.
	defaultMaxFileBytes int64 = 256 << 20
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

// LegacyBundleError reports a bundle written by kubectl-nv v0.1.x, whose files
// carry lowercased Go field names instead of the json ones.
//
// It is worth a name of its own because the failure is otherwise invisible: a
// file holding a quantity gives itself away when the decoder refuses the
// value, but a pod list has no quantity in it and decodes cleanly into a list
// of empty objects. A reader would see a bundle with no pods in it rather than
// a bundle it cannot read.
type LegacyBundleError struct {
	Name string
}

func (e *LegacyBundleError) Error() string {
	return fmt.Sprintf("%s is from a bundle written by kubectl-nv v0.1.x, which this version cannot read; re-run must-gather", e.Name)
}

// UnsafeBundleFileError reports a bundle entry that is not a regular file. A
// symlink could point anywhere on the machine, and opening a FIFO with no
// writer blocks forever, so neither is followed.
type UnsafeBundleFileError struct {
	Name string
	Mode fs.FileMode
}

func (e *UnsafeBundleFileError) Error() string {
	return fmt.Sprintf("%s is not a regular file (mode %s); refusing to read it", e.Name, e.Mode.Type())
}

// BundleFileTooLargeError reports a bundle entry past the read cap.
type BundleFileTooLargeError struct {
	Name  string
	Limit int64
}

func (e *BundleFileTooLargeError) Error() string {
	return fmt.Sprintf("%s is larger than the %d byte limit", e.Name, e.Limit)
}

// Bundle reads a must-gather directory. The files carry Kubernetes-native
// YAML: the json field names, with quantities as the strings the API serves,
// which is what "kubectl get -o yaml" prints and what gpu-operator's
// hack/must-gather.sh writes. They are decoded with sigs.k8s.io/yaml, the same
// path the API machinery uses, so a quantity keeps its amount.
//
// Nothing is cached between calls. A directory can be replaced while this
// process holds it, and two reads that disagree are better than one read that
// quietly describes a directory that is no longer there.
type Bundle struct {
	dir string
	// maxFileBytes caps one file read. Tests lower it; nothing else writes it.
	maxFileBytes int64
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

	return &Bundle{dir: dir, maxFileBytes: defaultMaxFileBytes}, nil
}

// readFile decodes the named bundle entry into out, reporting the plain
// failure. The caller names the resource, so nothing here repeats the file
// name that the caller is about to print.
//
// The entry is checked with Lstat and must be a regular file: a symlink in a
// bundle from a support ticket could address anything on this machine, and a
// FIFO would block the open forever, so both are refused before anything is
// opened. The read itself is capped.
func (b *Bundle) readFile(ctx context.Context, name string, out any) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	path := filepath.Join(b.dir, name)
	info, err := os.Lstat(path)
	if err != nil {
		// Report the bare cause: "no such file or directory" rather than the
		// whole path, which the caller already names.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			return pathErr.Err
		}

		return err
	}
	if !info.Mode().IsRegular() {
		return &UnsafeBundleFileError{Name: name, Mode: info.Mode()}
	}
	if info.Size() > b.maxFileBytes {
		return &BundleFileTooLargeError{Name: name, Limit: b.maxFileBytes}
	}

	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	// Read one byte past the cap so that a file which grew between the Lstat
	// and the open is still caught.
	data, err := io.ReadAll(io.LimitReader(f, b.maxFileBytes+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > b.maxFileBytes {
		return &BundleFileTooLargeError{Name: name, Limit: b.maxFileBytes}
	}

	if legacy := isLegacyDocument(data); legacy {
		return &LegacyBundleError{Name: name}
	}

	return yaml.Unmarshal(data, out)
}

// isLegacyDocument reports whether data looks like the gopkg.in/yaml.v2 dump
// kubectl-nv v0.1.x wrote. Those files are a marshalled Go struct, so their
// top level carries the lowercased embedded field names; a native document
// carries apiVersion, kind, metadata and items instead, and never these.
func isLegacyDocument(data []byte) bool {
	var top map[string]any
	if err := yaml.Unmarshal(data, &top); err != nil {
		// Not a mapping at the top level, so not one of those files. Let the
		// real decode report whatever is actually wrong with it.
		return false
	}

	for _, key := range []string{"typemeta", "listmeta", "objectmeta"} {
		if _, found := top[key]; found {
			return true
		}
	}

	return false
}

// readYAML decodes the named file into out and turns any failure into a
// PartialError naming that file, which is the normal case for a bundle
// gathered from a cluster where some read failed.
//
// The structural failures are not partial and pass through untouched: a
// bundle in the old format, a symlink, an oversize file and a dead context all
// mean this bundle cannot be read at all.
func (b *Bundle) readYAML(ctx context.Context, name string, out any) error {
	err := b.readFile(ctx, name, out)
	if err == nil {
		return nil
	}
	if isStructuralBundleError(err) {
		return err
	}

	return &PartialError{Resource: name, Err: err}
}

// isStructuralBundleError reports whether err says the bundle itself cannot be
// read, as opposed to one resource being absent from it.
func isStructuralBundleError(err error) bool {
	var legacy *LegacyBundleError
	var unsafeFile *UnsafeBundleFileError
	var tooLarge *BundleFileTooLargeError

	return errors.As(err, &legacy) ||
		errors.As(err, &unsafeFile) ||
		errors.As(err, &tooLarge) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

// Nodes returns the GPU nodes recorded in nodes.yaml. The same classifyNodes
// the live path uses is what decides, so the rule lives in one place.
func (b *Bundle) Nodes(ctx context.Context) ([]corev1.Node, error) {
	gpus, _, err := b.ClassifyNodes(ctx)
	return gpus, err
}

// HintedNodes returns the nodes in nodes.yaml that carry an NVIDIA discovery
// label without backing it with capacity or a slice.
func (b *Bundle) HintedNodes(ctx context.Context) ([]corev1.Node, error) {
	_, hinted, err := b.ClassifyNodes(ctx)
	return hinted, err
}

// ClassifyNodes reads nodes.yaml and, when the bundle carries one, the slice
// file, and sorts the nodes into the two sets. Both files are decoded on every
// call.
//
// A missing nodes.yaml is the end of it: there is nothing to classify. A
// missing slice file is not, but it does mean one of the two membership rules
// could not be evaluated, so both sets come back with a PartialError naming
// resourceslices. The answer is then wrong in a known direction: a node that
// is a GPU node only by its slice falls out of both sets rather than into the
// wrong one.
func (b *Bundle) ClassifyNodes(ctx context.Context) (gpu, hinted []corev1.Node, err error) {
	var list corev1.NodeList
	if err := b.readYAML(ctx, fileNodes, &list); err != nil {
		return nil, nil, err
	}

	slices, sliceErr := b.ResourceSlices(ctx)
	if sliceErr != nil && isStructuralBundleError(sliceErr) {
		return nil, nil, sliceErr
	}

	gpu, hinted = classifyNodes(list.Items, sliceNodeNames(slices))
	return gpu, hinted, sliceErr
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
	if err := b.readFile(ctx, fileResourceSlices, &list); err != nil {
		if isStructuralBundleError(err) {
			return nil, err
		}

		return nil, &PartialError{
			Resource: "resourceslices",
			Err:      fmt.Errorf("%s: %w", fileResourceSlices, err),
		}
	}

	out := make([]resourcev1.ResourceSlice, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Spec.Driver == DRADriverName {
			out = append(out, list.Items[i])
		}
	}

	return out, nil
}

// Pods returns the operand pods recorded in gpu_operand_pods.yaml, filtered by
// namespace and selector the way the API server would have filtered them.
//
// must-gather records the operator namespace and nothing else, so the bundle
// cannot answer for the rest of the cluster. A question about another
// namespace, or about every namespace, is answered with what is there plus a
// PartialError naming pods: an empty result would otherwise read as "no such
// pods" when it means "this bundle never looked".
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
	if err := b.readYAML(ctx, fileOperandPods, &list); err != nil {
		return nil, err
	}

	recorded := make(map[string]struct{}, 1)
	out := make([]corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		p := list.Items[i]
		recorded[p.Namespace] = struct{}{}
		if namespace != "" && p.Namespace != namespace {
			continue
		}
		if !sel.Matches(labels.Set(p.Labels)) {
			continue
		}
		out = append(out, p)
	}

	if _, covered := recorded[namespace]; namespace == "" || !covered {
		return out, &PartialError{
			Resource: "pods",
			Err:      fmt.Errorf("%s holds only %s", fileOperandPods, describeNamespaces(recorded)),
		}
	}

	return out, nil
}

// describeNamespaces renders the namespaces a bundle recorded, for the error
// that says what it could not answer for.
func describeNamespaces(recorded map[string]struct{}) string {
	if len(recorded) == 0 {
		return "no namespaces"
	}

	names := make([]string, 0, len(recorded))
	for ns := range recorded {
		names = append(names, ns)
	}
	sort.Strings(names)

	if len(names) == 1 {
		return "namespace " + names[0]
	}

	return "namespaces " + strings.Join(names, ", ")
}

// ClusterPolicies returns the single ClusterPolicy recorded in
// cluster_policy.yaml.
func (b *Bundle) ClusterPolicies(ctx context.Context) ([]unstructured.Unstructured, error) {
	var obj map[string]any
	if err := b.readYAML(ctx, fileClusterPolicy, &obj); err != nil {
		return nil, err
	}
	if len(obj) == 0 {
		return nil, &PartialError{
			Resource: fileClusterPolicy,
			Err:      errors.New("content is not a Kubernetes object"),
		}
	}

	return []unstructured.Unstructured{{Object: obj}}, nil
}
