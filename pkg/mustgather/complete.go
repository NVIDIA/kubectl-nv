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
	"io"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/yaml"

	"github.com/NVIDIA/kubectl-nv/pkg/inventory"
)

// The two files that make a bundle answerable rather than merely informative.
//
// Without them every bundle read is partial: the inventory needs every pod
// that asks for GPUs to say how many of a node's are in use, and the operand
// pods are the operator namespace alone; and NV-UPG-001 dates an upgrade step
// from the node's events, which nothing collected. So "kubectl nv list
// --from-bundle" and "kubectl nv health --from-bundle" exited 3 on every
// bundle there had ever been, which is exit 3 saying nothing.
const (
	// gpuPodsFile holds every pod of the cluster that asks for GPUs.
	gpuPodsFile = "gpu_pods.yaml"
	// nodeEventsFile holds the events whose involved object is a node.
	nodeEventsFile = "node_events.yaml"
	// computeDomainsFile holds the NVIDIA DRA driver's ComputeDomains, which
	// NV-CD-001 reads. computeDomainsMissingFile is the marker written in its
	// place when the cluster serves none, saying why.
	computeDomainsFile        = "compute_domains.yaml"
	computeDomainsMissingFile = "compute_domains.missing"
)

// computeDomainGVR is the NVIDIA DRA driver's ComputeDomain collection. The
// version is the one pkg/source reads, so a bundle and a live cluster cannot
// disagree about which API this is.
var computeDomainGVR = schema.GroupVersionResource{
	Group: "resource.nvidia.com", Version: "v1beta1", Resource: "computedomains",
}

// gatherComputeDomains collects the ComputeDomains, or a marker saying why the
// bundle has none.
//
// A cluster without the DRA driver serves no such collection, which is an
// answer rather than a denial, and the marker is what lets a reader tell the
// two apart: without it, every bundle from a cluster that never installed the
// driver would read as a bundle whose ComputeDomain read was refused.
func (g *Gatherer) gatherComputeDomains(_ *gatherState) error {
	return g.gatherOrMark(
		computeDomainGVR, computeDomainsFile, computeDomainsMissingFile, "ComputeDomain", false)
}

// projectGPUPod keeps the fields of a GPU pod that a reader of the bundle
// reads, and drops the rest.
//
// A bundle goes to another company, and these pods are not NVIDIA's: they are
// whatever a tenant is running. The command they run, the arguments they run
// it with, their environment and their annotations are theirs, and no reader
// of a bundle reads any of it. Redaction is not the answer for them, because a
// rewrite can only take out what it recognises and what is left is still
// somebody's to decide about; not collecting them is.
//
// Every field below is kept because something reads it:
//
//	metadata.name, namespace     inventory.operandFromPod, and what a reader
//	                             identifies a pod by
//	metadata.uid                 what two reads of one cluster line a pod up by
//	metadata.labels              inventory.componentOf and isOperand
//	metadata.ownerReferences     inventory.DriverComponent, for the daemonset
//	spec.nodeName                inventory.requestedByNode, which keys by it
//	spec.overhead                the GPUs a runtime class reserves per pod
//	spec.containers              name, resources and restartPolicy, which is
//	spec.initContainers          all inventory.PodSpecGPURequest reads
//	status.phase                 inventory.requestedByNode, which drops a
//	                             Succeeded or Failed pod
func projectGPUPod(pod *v1.Pod) v1.Pod {
	return v1.Pod{
		TypeMeta: pod.TypeMeta,
		ObjectMeta: metav1.ObjectMeta{
			Name:            pod.Name,
			Namespace:       pod.Namespace,
			UID:             pod.UID,
			Labels:          pod.Labels,
			OwnerReferences: pod.OwnerReferences,
		},
		Spec: v1.PodSpec{
			NodeName:       pod.Spec.NodeName,
			Overhead:       pod.Spec.Overhead,
			Containers:     projectGPUContainers(pod.Spec.Containers),
			InitContainers: projectGPUContainers(pod.Spec.InitContainers),
		},
		Status: v1.PodStatus{Phase: pod.Status.Phase},
	}
}

// projectGPUContainers keeps the three fields of a container that decide what
// a pod asks for: its name, so a reader can tell two of them apart, its
// resources, and whether an init container is a sidecar.
func projectGPUContainers(containers []v1.Container) []v1.Container {
	if containers == nil {
		return nil
	}

	out := make([]v1.Container, 0, len(containers))
	for i := range containers {
		out = append(out, v1.Container{
			Name:          containers[i].Name,
			Resources:     containers[i].Resources,
			RestartPolicy: containers[i].RestartPolicy,
		})
	}

	return out
}

// gatherGPUPods writes every pod of the cluster that asks for GPUs, in every
// namespace, narrowed to --node when one was given, and projected down to what
// a reader reads.
//
// Every namespace, because that is the question: the GPUs of a node are used
// by whatever landed on it, and a tenant's training job is not in the operator
// namespace. Only the pods that ask for GPUs, because the alternative is the
// whole cluster's pod list in a file somebody mails to support.
//
// What counts as asking is inventory.PodSpecGPURequest, the one effective
// request rule: the same function the live read uses, so a bundle and a
// cluster cannot disagree about which pods matter.
func (g *Gatherer) gatherGPUPods(_ *gatherState) error {
	// Narrowed by the API server when --node named one. The client filter
	// below still runs, because a field selector is the server's optimisation
	// and the bundle's contents must not depend on one.
	all, err := g.listPodsOnNode()
	if err != nil {
		return g.logError("error listing the pods of the cluster: %v", err)
	}

	wanted := g.podsOnNode(all.Items)
	kept := make([]v1.Pod, 0, len(wanted))
	for i := range wanted {
		if inventory.PodSpecGPURequest(&wanted[i].Spec) > 0 {
			kept = append(kept, projectGPUPod(&wanted[i]))
		}
	}

	list := v1.PodList{Items: kept}
	stampPodList(&list)
	data, err := yaml.Marshal(list)
	if err != nil {
		return g.logError("error marshalling the GPU pods: %v", err)
	}

	return g.writeArtifactFile(gpuPodsFile, func(w io.Writer) error {
		_, werr := w.Write(data)

		return werr
	})
}

// gatherNodeEvents writes the cluster's events about nodes, narrowed to --node
// when one was given.
//
// They are what dates a driver upgrade step. A node sitting in
// cordon-required is not a finding; a node that has been sitting there for an
// hour is, and only the event says which of the two it is.
func (g *Gatherer) gatherNodeEvents(_ *gatherState) error {
	// The server does the first cut: a busy cluster has far more events than
	// nodes, and every one of them would otherwise cross the wire. The client
	// filter below still runs, because the bundle's contents must not depend
	// on a server honouring a selector.
	selector := "involvedObject.kind=Node"
	if g.opts.Node != "" {
		selector += ",involvedObject.name=" + g.opts.Node
	}

	listFunc := func(opts metav1.ListOptions) (v1.EventList, string, error) {
		opts.FieldSelector = selector
		list, err := g.clientset.CoreV1().Events(metav1.NamespaceAll).List(context.Background(), opts)
		if err != nil {
			return v1.EventList{}, "", err
		}

		return *list, list.Continue, nil
	}
	appendFunc := func(all *v1.EventList, page v1.EventList) {
		all.Items = append(all.Items, page.Items...)
	}

	all, err := listAllPaginated[v1.EventList](500, "", listFunc, appendFunc)
	if err != nil {
		return g.logError("error listing the events of the cluster: %v", err)
	}

	kept := make([]v1.Event, 0, len(all.Items))
	for i := range all.Items {
		event := all.Items[i]
		if event.InvolvedObject.Kind != "Node" || !g.wantsNode(event.InvolvedObject.Name) {
			continue
		}
		kept = append(kept, event)
	}

	list := v1.EventList{Items: kept}
	stampEventList(&list)
	data, err := yaml.Marshal(list)
	if err != nil {
		return g.logError("error marshalling the node events: %v", err)
	}

	return g.writeArtifactFile(nodeEventsFile, func(w io.Writer) error {
		_, werr := w.Write(data)

		return werr
	})
}

// stampEventList puts the envelope back on a list the client stripped, for the
// reason stampPodList does: a reader of the file switches on apiVersion and
// kind, and an API list serialised by client-go carries neither.
func stampEventList(l *v1.EventList) {
	l.APIVersion = "v1"
	l.Kind = "EventList"
	for i := range l.Items {
		l.Items[i].APIVersion = "v1"
		l.Items[i].Kind = "Event"
	}
}
