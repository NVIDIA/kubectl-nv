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

package mustgather

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/kubectl/pkg/describe"
	"sigs.k8s.io/yaml"
)

// StatusOK is the manifest status of a collector that ran and wrote what it
// planned to. The other two forms are "skipped:<reason>" and "error:<text>",
// built by the gather as it runs.
const StatusOK = "ok"

// statusSkipped and statusError are the prefixes of the other two forms.
const (
	statusSkipped = "skipped:"
	statusError   = "error:"
)

// The placeholders a planned artifact name carries where the cluster supplies
// the rest of it. They are part of the contract the parity test and
// docs/must-gather.md are written against.
const (
	placeholderNode      = "<node>"
	placeholderPod       = "<pod>"
	placeholderContainer = "<container>"
	placeholderDaemonSet = "<daemonset>"
)

// Collector is one unit of a gather: a name the manifest records it under, the
// artifacts it plans to write, and the work itself.
//
// The artifacts are declared rather than discovered so that the set a gather
// would produce can be computed without a cluster, which is what the parity
// check against the gpu-operator script is made of.
type Collector struct {
	// Name is what the manifest records this collector as, and what the user
	// sees when it is skipped.
	Name string
	// Step is the progress line, printed before the collector runs.
	Step string
	// Artifacts are the files this collector writes, with a placeholder where
	// the cluster supplies part of the name.
	Artifacts []string
	// Run collects. It returns errSkipped when there was nothing to do, any
	// other error when the gather cannot carry on.
	Run func(g *Gatherer, s *gatherState) error
}

// errSkipped reports a collector that did not run, and why. The gather carries
// on past it and the manifest records the reason.
type errSkipped struct {
	reason string
}

func (e errSkipped) Error() string { return "skipped: " + e.reason }

// skip returns the error that records a collector as skipped.
func skip(format string, a ...any) error {
	return errSkipped{reason: fmt.Sprintf(format, a...)}
}

// gatherState is what one collector learned and the ones after it need. It is
// built lazily: a collector that does not need the operator namespace never
// causes it to be looked up.
type gatherState struct {
	// operatorNamespace is where the GPU operator and its operands run.
	operatorNamespace string
	// operatorPod is the GPU operator pod itself, or nil when the namespace
	// holds none.
	operatorPod *v1.Pod
	// operandPods is every pod of the operator namespace, narrowed to --node
	// when one was given. The operator pod is among them, as it is in the
	// script's own list.
	operandPods []v1.Pod
	// driverPods are the pods a bug report and the device usage are collected
	// from, found by the four labels a driver can carry.
	driverPods []driverTarget
	// execDenied names the collectors that were not allowed to exec. It is the
	// difference between a bundle that is missing the driver evidence because
	// the user asked for that and one that is missing it because RBAC said no.
	execDenied []string

	prepared bool
}

// Collectors is the registry, in the order a gather runs it.
//
// The order is the gpu-operator script's (lines 92 to 170 at f6d32bad44ee) and
// the base's: the cluster's own resources, then the node survey, then the
// namespace-scoped work. It is load bearing. The ClusterPolicy is the first
// file support asks for, and a cluster whose feature discovery has not
// labelled a node yet is exactly the cluster someone runs a must-gather on, so
// the node survey must not run in front of it.
func Collectors() []Collector {
	return []Collector{
		{
			Name:      "openshift-version",
			Step:      "Gathering the OpenShift version",
			Artifacts: []string{"openshift_version.yaml"},
			Run:       func(g *Gatherer, _ *gatherState) error { return g.gatherOpenShiftVersion() },
		},
		{
			Name:      "nodes",
			Step:      "Gathering node information",
			Artifacts: []string{"nodes.yaml", "machines.yaml"},
			Run:       func(g *Gatherer, _ *gatherState) error { return g.gatherNodeInfo() },
		},
		{
			Name:      "cluster-policy",
			Step:      "Gathering the ClusterPolicy",
			Artifacts: []string{"cluster_policy.yaml", "cluster_policy.missing"},
			Run:       func(g *Gatherer, _ *gatherState) error { return g.gatherClusterPolicy() },
		},
		{
			Name:      "nvidia-drivers",
			Step:      "Gathering the NVIDIADriver resources",
			Artifacts: []string{"nvidia_driver.yaml", "nvidia_driver.missing"},
			Run:       func(g *Gatherer, _ *gatherState) error { return g.gatherNVIDIADrivers() },
		},
		{
			Name:      "gpu-cluster",
			Step:      "Gathering the GPUCluster resources",
			Artifacts: []string{"gpu_cluster.yaml"},
			Run:       func(g *Gatherer, _ *gatherState) error { return g.gatherGPUCluster() },
		},
		{
			Name:      "hyperconverged",
			Step:      "Gathering the KubeVirt HyperConverged resources",
			Artifacts: []string{"hyperconverged.yaml"},
			Run:       func(g *Gatherer, _ *gatherState) error { return g.gatherHyperConverged() },
		},
		{
			Name:      "kubevirt",
			Step:      "Gathering the KubeVirt resources",
			Artifacts: []string{"kubevirt.yaml"},
			Run:       func(g *Gatherer, _ *gatherState) error { return g.gatherKubeVirt() },
		},
		{
			Name:      "device-classes",
			Step:      "Gathering the DRA device classes",
			Artifacts: []string{"deviceclasses.yaml"},
			Run:       func(g *Gatherer, _ *gatherState) error { return g.gatherDeviceClasses() },
		},
		{
			Name:      "resource-slices",
			Step:      "Gathering the DRA resource slices",
			Artifacts: []string{"resourceslices.yaml"},
			Run:       func(g *Gatherer, _ *gatherState) error { return g.gatherResourceSlices() },
		},
		{
			Name:      "gpu-nodes",
			Step:      "Gathering GPU node information",
			Artifacts: []string{"gpu_nodes.yaml", "gpu_nodes.labels", "gpu_nodes.status", "gpu_nodes.descr"},
			Run:       func(g *Gatherer, _ *gatherState) error { return g.gatherGPUNodes() },
		},
		{
			Name:      "operator-pod",
			Step:      "Gathering the GPU operator pod",
			Artifacts: []string{"gpu_operator_pod.yaml", "gpu_operator_pod.status"},
			Run:       (*Gatherer).gatherOperatorPod,
		},
		{
			Name:      "operand-pods",
			Step:      "Gathering the operand pods",
			Artifacts: []string{"gpu_operand_pods.yaml", "gpu_operand_pods.status", "gpu_operand_pod_images.txt"},
			Run:       (*Gatherer).gatherOperandPods,
		},
		{
			Name: "pod-logs",
			Step: "Gathering pod logs",
			Artifacts: []string{
				"gpu_operator_logs_" + placeholderContainer + ".log",
				"gpu_operator_logs_" + placeholderContainer + ".previous.log",
				placeholderPod + "_" + placeholderContainer + ".log",
				placeholderPod + "_" + placeholderContainer + ".previous.log",
			},
			Run: (*Gatherer).gatherPodLogs,
		},
		{
			Name:      "pod-descriptions",
			Step:      "Describing the operand pods",
			Artifacts: []string{placeholderPod + ".descr"},
			Run:       (*Gatherer).gatherPodDescriptions,
		},
		{
			Name: "daemonsets",
			Step: "Gathering the operand daemonsets",
			Artifacts: []string{
				"daemonsets.yaml",
				"gpu_operand_ds.status",
				"gpu_operand_ds_" + placeholderDaemonSet + ".descr",
			},
			Run: (*Gatherer).gatherDaemonSets,
		},
		{
			Name:      "bug-report",
			Step:      "Gathering bug reports from the driver daemonsets",
			Artifacts: []string{"nvidia-bug-report_" + placeholderNode + ".log.gz"},
			Run:       (*Gatherer).gatherBugReportFromDaemonSet,
		},
		{
			Name:      "device-usage",
			Step:      "Gathering GPU device usage",
			Artifacts: []string{"gpu_device_usage_" + placeholderNode + ".log"},
			Run:       (*Gatherer).gatherDeviceUsage,
		},
	}
}

// gatherLogs are the two files the gather writes about itself. They are opened
// by NewGatherer before the first collector runs, because everything after that
// point reports into them, so they belong to no collector and are named here.
var gatherLogs = []string{"must-gather.log", "must-gather.stderr.log"}

// PlannedArtifacts is every artifact name a bundle can hold, with the
// placeholders left in. It needs no cluster, which is what lets the parity
// check against the gpu-operator script run as a unit test.
func PlannedArtifacts() []string {
	names := append([]string{}, gatherLogs...)
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
	}
	for _, c := range Collectors() {
		for _, name := range c.Artifacts {
			if seen[name] {
				continue
			}
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// prepareState fills in what the namespace-scoped collectors share: the
// operator namespace, the pods of it narrowed to --node, and the operator pod.
// It runs once however many collectors ask for it, so the pod list a bundle is
// built from is one list rather than one per collector, and the files cannot
// disagree about which pods the cluster had.
func (g *Gatherer) prepareState(s *gatherState) error {
	if s.prepared {
		return nil
	}

	if s.operatorNamespace == "" {
		namespace, err := g.gatherOperatorNamespace()
		if err != nil {
			return err
		}
		s.operatorNamespace = namespace
		g.step("Found the GPU operator in namespace %s", namespace)
	}

	pods, err := g.listPods(s.operatorNamespace, "")
	if err != nil {
		return g.logError("error getting the pods of the operator namespace: %v", err)
	}
	s.operandPods = g.podsOnNode(pods.Items)

	// The operator pod is kept whatever --node says. It is a single pod whose
	// log is the record of what the operator did to every node, so narrowing a
	// bundle to one node does not make it less relevant.
	operator, err := g.listPods(s.operatorNamespace, operatorPodLabel)
	if err != nil {
		return g.logError("error getting the GPU operator pod: %v", err)
	}
	if len(operator.Items) > 0 {
		s.operatorPod = &operator.Items[0]
	}

	s.prepared = true
	return nil
}

// operatorPodLabel is what the GPU operator pod carries, and what the operator
// namespace is found through.
const operatorPodLabel = "app=gpu-operator"

// listPods lists every pod of a namespace matching selector, following the
// pagination the API server hands back.
func (g *Gatherer) listPods(namespace, selector string) (v1.PodList, error) {
	listFunc := func(opts metav1.ListOptions) (v1.PodList, string, error) {
		list, err := g.clientset.CoreV1().Pods(namespace).List(context.Background(), opts)
		if err != nil {
			return v1.PodList{}, "", err
		}
		return *list, list.Continue, nil
	}
	appendFunc := func(all *v1.PodList, page v1.PodList) {
		all.Items = append(all.Items, page.Items...)
	}
	return listAllPaginated[v1.PodList](500, selector, listFunc, appendFunc)
}

// podsOnNode narrows a pod list to --node when one was given. A support
// engineer asking about one node gets that node's evidence: anything else in
// the bundle is both larger than they asked for and misleading about where the
// evidence came from.
func (g *Gatherer) podsOnNode(pods []v1.Pod) []v1.Pod {
	if g.opts.Node == "" {
		return pods
	}
	kept := make([]v1.Pod, 0, len(pods))
	for _, pod := range pods {
		if pod.Spec.NodeName == g.opts.Node {
			kept = append(kept, pod)
		}
	}
	return kept
}

// wantsNode reports whether a node belongs in the bundle, which is every node
// unless --node named one.
func (g *Gatherer) wantsNode(name string) bool {
	return g.opts.Node == "" || g.opts.Node == name
}

// The custom resources a gather reads. Each is listed across every namespace,
// as the gpu-operator script does, so a resource in a namespace nobody expected
// is still collected.
var (
	clusterPolicyGVR  = schema.GroupVersionResource{Group: "nvidia.com", Version: "v1", Resource: "clusterpolicies"}
	nvidiaDriverGVR   = schema.GroupVersionResource{Group: "nvidia.com", Version: "v1alpha1", Resource: "nvidiadrivers"}
	gpuClusterGVR     = schema.GroupVersionResource{Group: "nvidia.com", Version: "v1alpha1", Resource: "gpuclusters"}
	hyperConvergedGVR = schema.GroupVersionResource{
		Group: "hco.kubevirt.io", Version: "v1beta1", Resource: "hyperconvergeds",
	}
	kubeVirtGVR    = schema.GroupVersionResource{Group: "kubevirt.io", Version: "v1", Resource: "kubevirts"}
	deviceClassGVR = schema.GroupVersionResource{Group: "resource.k8s.io", Version: "v1", Resource: "deviceclasses"}
)

// customResourceTimeout bounds a single read of a custom resource. The dynamic
// client has no deadline of its own, and a gather that hangs on one CRD never
// writes the bundle it had already collected.
const customResourceTimeout = 30 * time.Second

// listCustomResources reads every object of one kind across the cluster.
func (g *Gatherer) listCustomResources(gvr schema.GroupVersionResource) (*unstructured.UnstructuredList, error) {
	ctx, cancel := context.WithTimeout(context.Background(), customResourceTimeout)
	defer cancel()

	return g.dynClient.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
}

// absentCustomResource reports whether err means the cluster does not serve
// this kind, which for most of these is the common case rather than a failure.
func absentCustomResource(err error) bool {
	return apierrors.IsNotFound(err) || meta.IsNoMatchError(err)
}

// gatherOptionalCustomResource writes one kind of custom resource to fileName,
// and writes nothing at all when the cluster has none of it. The script logs
// "resource(s) not found in the cluster" and carries on in exactly these cases,
// and a bundle holding an empty list would read as a cluster that has the CRD
// installed with nothing created, which is a different cluster.
func (g *Gatherer) gatherOptionalCustomResource(gvr schema.GroupVersionResource, fileName, what string) error {
	list, err := g.listCustomResources(gvr)
	switch {
	case err == nil && len(list.Items) > 0:
		return g.writeCustomResourceList(list, fileName, what)
	case err == nil, absentCustomResource(err):
		return g.noteAbsent("%s resource(s) not found in the cluster", what)
	case apierrors.IsForbidden(err):
		return g.noteAbsent("not allowed to read %s", what)
	default:
		return g.logError(fmt.Sprintf("error getting %s: %%v", what), err)
	}
}

// writeCustomResourceList marshals a list and writes it as one artifact. The
// list is marshalled as it arrived, so the file is what "kubectl get -o yaml"
// would have produced rather than a Go rendering of it.
func (g *Gatherer) writeCustomResourceList(list *unstructured.UnstructuredList, fileName, what string) error {
	return g.writeCustomResources(list, fileName, what, false)
}

// writeCustomResources writes a list, or the single document a list of one
// collapses to when single is set.
func (g *Gatherer) writeCustomResources(list *unstructured.UnstructuredList, fileName, what string, single bool) error {
	var subject any = list
	if single && len(list.Items) == 1 {
		subject = list.Items[0].Object
	}
	data, err := yaml.Marshal(subject)
	if err != nil {
		return g.logError(fmt.Sprintf("error marshalling %s: %%v", what), err)
	}

	file, err := g.createArtifact(filepath.Join(g.opts.ArtifactDir, fileName))
	if err != nil {
		return g.logError(fmt.Sprintf("error creating %s: %%v", fileName), err)
	}
	defer file.Close()

	if _, err := file.Write(data); err != nil {
		return g.logError(fmt.Sprintf("error writing %s: %%v", fileName), err)
	}
	return nil
}

// noteAbsent records that a kind is not there. It is an answer about the
// cluster rather than a failure, so the gather carries on; the manifest marks
// the collector skipped, which is how a reader tells "the cluster has no
// KubeVirt" from "nobody looked".
func (g *Gatherer) noteAbsent(format string, a ...any) error {
	reason := fmt.Sprintf(format, a...)
	if _, err := fmt.Fprintln(g.logFile, reason); err != nil {
		return fmt.Errorf("error writing to log file: %w", err)
	}
	return errSkipped{reason: reason}
}

// gatherGPUCluster collects the GPUCluster resources.
func (g *Gatherer) gatherGPUCluster() error {
	return g.gatherOptionalCustomResource(gpuClusterGVR, "gpu_cluster.yaml", "GPUCluster")
}

// gatherHyperConverged collects the KubeVirt HyperConverged resources, which
// say how a cluster running virtual machines on its GPUs is configured.
func (g *Gatherer) gatherHyperConverged() error {
	return g.gatherOptionalCustomResource(hyperConvergedGVR, "hyperconverged.yaml", "HyperConverged")
}

// gatherKubeVirt collects the KubeVirt resources.
func (g *Gatherer) gatherKubeVirt() error {
	return g.gatherOptionalCustomResource(kubeVirtGVR, "kubevirt.yaml", "KubeVirt")
}

// gatherDeviceClasses collects the DRA device classes. They are collected whole
// rather than narrowed to NVIDIA's, because which classes exist at all is part
// of why a pod did or did not get a GPU.
func (g *Gatherer) gatherDeviceClasses() error {
	return g.gatherOptionalCustomResource(deviceClassGVR, "deviceclasses.yaml", "DeviceClass")
}

// gatherClusterPolicy collects the ClusterPolicy, the GPU operator's own
// configuration and the first thing support compares against what the customer
// says they applied.
func (g *Gatherer) gatherClusterPolicy() error {
	// A cluster has one ClusterPolicy, and the script writes it as the single
	// document "kubectl get -oyaml <name>" produces. Wrapping it in a list
	// would break every reader that reaches straight for .spec.
	return g.gatherOrMark(clusterPolicyGVR, "cluster_policy.yaml", "cluster_policy.missing", "ClusterPolicy", true)
}

// gatherNVIDIADrivers collects the NVIDIADriver resources across every
// namespace.
func (g *Gatherer) gatherNVIDIADrivers() error {
	// The drivers are listed across every namespace and written as a list, as
	// the script's "get nvidiadrivers.nvidia.com -A -oyaml" does: a cluster can
	// run more than one NVIDIADriver and each is a separate answer.
	return g.gatherOrMark(nvidiaDriverGVR, "nvidia_driver.yaml", "nvidia_driver.missing", "NVIDIADriver", false)
}

// gatherOrMark writes the resources, or a marker saying why the bundle has
// none. A zero-length marker tells a reader that the file is not there and
// nothing else: "the operator is not installed" and "you may not read its
// configuration" are different problems with different owners.
func (g *Gatherer) gatherOrMark(gvr schema.GroupVersionResource, fileName, markerName, what string, single bool) error {
	list, err := g.listCustomResources(gvr)
	switch {
	case err == nil && len(list.Items) > 0:
		return g.writeCustomResources(list, fileName, what, single)
	case err == nil, absentCustomResource(err):
		return g.writeMissingMarker(markerName, "not installed")
	case apierrors.IsForbidden(err):
		return g.writeMissingMarker(markerName, "forbidden")
	default:
		return g.writeMissingMarker(markerName, err.Error())
	}
}

// writeMissingMarker records that a resource is not in the bundle, and why.
func (g *Gatherer) writeMissingMarker(fileName, reason string) error {
	path := filepath.Join(g.opts.ArtifactDir, fileName)
	if err := g.writeMarker(path, []byte(reason+"\n")); err != nil {
		return g.logError(fmt.Sprintf("error writing %s: %%v", fileName), err)
	}
	if _, err := fmt.Fprintf(g.errLogFile, "%s: %s\n", fileName, reason); err != nil {
		return fmt.Errorf("error writing to stderr log file: %w", err)
	}
	return nil
}

// gatherOperatorPod writes the GPU operator pod as YAML and as the wide table
// a reader scans first.
func (g *Gatherer) gatherOperatorPod(s *gatherState) error {
	if err := g.prepareState(s); err != nil {
		return err
	}
	if s.operatorPod == nil {
		return fmt.Errorf("no pods found with label %s", operatorPodLabel)
	}

	pods := []v1.Pod{*s.operatorPod}
	list := v1.PodList{Items: pods}
	stampPodList(&list)
	data, err := yaml.Marshal(list)
	if err != nil {
		return g.logError("error marshalling the GPU operator pod: %v", err)
	}

	if err := g.writeArtifactFile("gpu_operator_pod.yaml", func(w io.Writer) error {
		_, werr := w.Write(data)
		return werr
	}); err != nil {
		return err
	}

	return g.writeArtifactFile("gpu_operator_pod.status", func(w io.Writer) error {
		return writePodTable(w, pods)
	})
}

// gatherOperandPods writes the pods of the operator namespace as YAML, as the
// wide table, and as the image list. The image list is how a support engineer
// tells which release of an operand they are looking at without reading the
// YAML of every pod.
func (g *Gatherer) gatherOperandPods(s *gatherState) error {
	if err := g.prepareState(s); err != nil {
		return err
	}

	stamped := v1.PodList{Items: s.operandPods}
	stampPodList(&stamped)
	data, err := yaml.Marshal(stamped)
	if err != nil {
		return g.logError("error marshalling the operand pods: %v", err)
	}

	if err := g.writeArtifactFile("gpu_operand_pods.yaml", func(w io.Writer) error {
		_, werr := w.Write(data)
		return werr
	}); err != nil {
		return err
	}

	if err := g.writeArtifactFile("gpu_operand_pods.status", func(w io.Writer) error {
		return writePodTable(w, s.operandPods)
	}); err != nil {
		return err
	}

	return g.writeArtifactFile("gpu_operand_pod_images.txt", func(w io.Writer) error {
		return writePodImages(w, s.operandPods)
	})
}

// writePodImages writes one line per pod naming every image it runs, in the
// form the script's jsonpath produces.
func writePodImages(w io.Writer, pods []v1.Pod) error {
	for i := range pods {
		pod := &pods[i]
		images := make([]string, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
		for _, c := range pod.Spec.InitContainers {
			images = append(images, c.Image)
		}
		for _, c := range pod.Spec.Containers {
			images = append(images, c.Image)
		}
		if _, err := fmt.Fprintf(w, "%s:\t%s\n", pod.Name, strings.Join(images, " ")); err != nil {
			return fmt.Errorf("writing the pod images: %w", err)
		}
	}
	return nil
}

// gatherPodDescriptions writes the describe output of every operand pod, which
// carries the events and the container states the YAML does not spell out.
func (g *Gatherer) gatherPodDescriptions(s *gatherState) error {
	if err := g.prepareState(s); err != nil {
		return err
	}

	var errs []error
	for i := range s.operandPods {
		pod := s.operandPods[i]
		err := g.writeArtifactFile(pod.Name+".descr", func(w io.Writer) error {
			text, derr := g.describePod(pod.Namespace, pod.Name)
			if derr != nil {
				return derr
			}
			_, werr := io.WriteString(w, text)
			return werr
		})
		if err == nil {
			continue
		}
		// A pod deleted between the listing and its description is one line of
		// the bundle missing, not a reason to call the collector failed. The
		// bundle still holds that pod's YAML and its logs.
		if apierrors.IsNotFound(err) {
			g.discardArtifact(filepath.Join(g.opts.ArtifactDir, pod.Name+".descr"))
			if _, lerr := fmt.Fprintf(g.errLogFile,
				"Pod %s went away before it could be described: %v\n", pod.Name, err); lerr != nil {
				errs = append(errs, fmt.Errorf("error writing to stderr log file: %w", lerr))
			}
			continue
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// gatherDaemonSets collects the operand daemonsets as YAML, as the status table
// and as one description each.
func (g *Gatherer) gatherDaemonSets(s *gatherState) error {
	if err := g.prepareState(s); err != nil {
		return err
	}

	listFunc := func(opts metav1.ListOptions) (appsv1.DaemonSetList, string, error) {
		ds, err := g.clientset.AppsV1().DaemonSets(s.operatorNamespace).List(context.Background(), opts)
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
		return g.logError("error getting daemonSets: %v", err)
	}

	stamped := daemonSets
	stampDaemonSetList(&stamped)
	data, err := yaml.Marshal(stamped)
	if err != nil {
		return g.logError("error marshalling daemonSets: %v", err)
	}

	if err := g.writeArtifactFile("daemonsets.yaml", func(w io.Writer) error {
		_, werr := w.Write(data)
		return werr
	}); err != nil {
		return err
	}

	if err := g.writeArtifactFile("gpu_operand_ds.status", func(w io.Writer) error {
		return writeDaemonSetTable(w, daemonSets.Items)
	}); err != nil {
		return err
	}

	var errs []error
	for i := range daemonSets.Items {
		set := daemonSets.Items[i]
		fileName := "gpu_operand_ds_" + set.Name + ".descr"
		derr := g.writeArtifactFile(fileName, func(w io.Writer) error {
			text, terr := g.describeDaemonSet(set.Namespace, set.Name)
			if terr != nil {
				return terr
			}
			_, werr := io.WriteString(w, text)
			return werr
		})
		switch {
		case derr == nil:
		case apierrors.IsNotFound(derr):
			g.discardArtifact(filepath.Join(g.opts.ArtifactDir, fileName))
			if _, lerr := fmt.Fprintf(g.errLogFile,
				"DaemonSet %s went away before it could be described: %v\n", set.Name, derr); lerr != nil {
				errs = append(errs, fmt.Errorf("error writing to stderr log file: %w", lerr))
			}
		default:
			errs = append(errs, derr)
		}
	}
	return errors.Join(errs...)
}

// describeSettings is what the describers are asked for. The events are part of
// what a describe is read for: a pod that cannot pull its image says so in its
// events and nowhere else.
var describeSettings = describe.DescriberSettings{ShowEvents: true, ChunkSize: 500}

// describeNode renders a node the way "kubectl describe node" does.
func (g *Gatherer) describeNode(name string) (string, error) {
	d := &describe.NodeDescriber{Interface: g.clientset}
	return d.Describe("", name, describeSettings)
}

// describePod renders a pod the way "kubectl describe pod" does.
func (g *Gatherer) describePod(namespace, name string) (string, error) {
	d := &describe.PodDescriber{Interface: g.clientset}
	return d.Describe(namespace, name, describeSettings)
}

// describeDaemonSet renders a daemonset the way "kubectl describe ds" does.
func (g *Gatherer) describeDaemonSet(namespace, name string) (string, error) {
	d := &describe.DaemonSetDescriber{Interface: g.clientset}
	return d.Describe(namespace, name, describeSettings)
}

// describeEach writes one description per item into a single file, separated
// by a blank line as "kubectl describe" separates them.
func describeEach[T any](w io.Writer, items []T, render func(T) (string, error)) error {
	for i, item := range items {
		if i > 0 {
			if _, err := io.WriteString(w, "\n\n"); err != nil {
				return fmt.Errorf("writing the description separator: %w", err)
			}
		}
		text, err := render(item)
		if err != nil {
			return err
		}
		if _, err := io.WriteString(w, text); err != nil {
			return fmt.Errorf("writing a description: %w", err)
		}
	}
	return nil
}
