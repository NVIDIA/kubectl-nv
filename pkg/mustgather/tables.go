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

// The .status files of a bundle are what the gpu-operator script gets from
// "kubectl get -o wide". kubectl renders them from a Table the API server
// builds, and so does this package: writePodStatus asks the server for that
// Table and prints it, because the server knows pod states that cannot be
// re-derived correctly from the objects alone.
//
// What follows is the fallback for a server that will not serve a Table, and
// the renderer for the node and daemonset tables. The column sets match
// kubectl's so a reader can scan a bundle the way they scan a terminal.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/duration"
	"k8s.io/cli-runtime/pkg/printers"
	"k8s.io/client-go/rest"
	"k8s.io/kubectl/pkg/scheme"
)

// notApplicable is what kubectl prints for a column a resource has no value
// for. An empty cell would shift every column after it.
const notApplicable = "<none>"

// writeTable renders a header and its rows through the tab writer kubectl
// itself uses, so the columns line up the same way.
func writeTable(w io.Writer, header []string, rows [][]string) error {
	tw := printers.GetNewTabWriter(w)
	if _, err := fmt.Fprintln(tw, strings.Join(header, "\t")); err != nil {
		return fmt.Errorf("writing the table header: %w", err)
	}
	for _, row := range rows {
		if _, err := fmt.Fprintln(tw, strings.Join(row, "\t")); err != nil {
			return fmt.Errorf("writing a table row: %w", err)
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("flushing the table: %w", err)
	}
	return nil
}

// age is the column kubectl shows for how long an object has existed.
func age(stamp metav1.Time) string {
	if stamp.IsZero() {
		return notApplicable
	}
	return duration.HumanDuration(time.Since(stamp.Time))
}

// writeNodeTable renders nodes the way "kubectl get nodes -o wide" does.
func writeNodeTable(w io.Writer, nodes []v1.Node) error {
	header := []string{
		"NAME", "STATUS", "ROLES", "AGE", "VERSION",
		"INTERNAL-IP", "EXTERNAL-IP", "OS-IMAGE", "KERNEL-VERSION", "CONTAINER-RUNTIME",
	}

	rows := make([][]string, 0, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		internal, external := nodeAddresses(node)
		rows = append(rows, []string{
			node.Name,
			nodeStatus(node),
			nodeRoles(node),
			age(node.CreationTimestamp),
			orNone(node.Status.NodeInfo.KubeletVersion),
			internal,
			external,
			orNone(node.Status.NodeInfo.OSImage),
			orNone(node.Status.NodeInfo.KernelVersion),
			orNone(node.Status.NodeInfo.ContainerRuntimeVersion),
		})
	}

	return writeTable(w, header, rows)
}

// nodeStatus is the Ready condition, with the marker kubectl appends when the
// node is cordoned.
func nodeStatus(node *v1.Node) string {
	status := "Unknown"
	for _, condition := range node.Status.Conditions {
		if condition.Type != v1.NodeReady {
			continue
		}
		switch condition.Status {
		case v1.ConditionTrue:
			status = "Ready"
		case v1.ConditionFalse:
			status = "NotReady"
		default:
			status = "Unknown"
		}
	}
	if node.Spec.Unschedulable {
		status += ",SchedulingDisabled"
	}
	return status
}

// nodeRoles reads the roles off the node-role labels, which is where kubectl
// reads them from too.
func nodeRoles(node *v1.Node) string {
	const prefix = "node-role.kubernetes.io/"

	var roles []string
	for key, value := range node.Labels {
		switch {
		case strings.HasPrefix(key, prefix):
			if role := strings.TrimPrefix(key, prefix); role != "" {
				roles = append(roles, role)
			}
		case key == "kubernetes.io/role" && value != "":
			roles = append(roles, value)
		}
	}
	if len(roles) == 0 {
		return notApplicable
	}
	sort.Strings(roles)
	return strings.Join(roles, ",")
}

// nodeAddresses returns the internal and external addresses of a node.
func nodeAddresses(node *v1.Node) (string, string) {
	internal, external := notApplicable, notApplicable
	for _, address := range node.Status.Addresses {
		switch address.Type {
		case v1.NodeInternalIP:
			internal = address.Address
		case v1.NodeExternalIP:
			external = address.Address
		}
	}
	return internal, external
}

// writePodTable renders pods the way "kubectl get pods -o wide" does.
func writePodTable(w io.Writer, pods []v1.Pod) error {
	header := []string{
		"NAME", "READY", "STATUS", "RESTARTS", "AGE",
		"IP", "NODE", "NOMINATED NODE", "READINESS GATES",
	}

	rows := make([][]string, 0, len(pods))
	for i := range pods {
		pod := &pods[i]
		rows = append(rows, []string{
			pod.Name,
			podReady(pod),
			podStatus(pod),
			fmt.Sprintf("%d", podRestarts(pod)),
			age(pod.CreationTimestamp),
			orNone(pod.Status.PodIP),
			orNone(pod.Spec.NodeName),
			orNone(pod.Status.NominatedNodeName),
			podReadinessGates(pod),
		})
	}

	return writeTable(w, header, rows)
}

// podReady is the "ready/total" column, counted over the regular containers as
// kubectl counts it.
func podReady(pod *v1.Pod) string {
	ready := 0
	for _, status := range pod.Status.ContainerStatuses {
		if status.Ready {
			ready++
		}
	}
	return fmt.Sprintf("%d/%d", ready, len(pod.Spec.Containers))
}

// podStatus is the phase, replaced by the reason when one is set. A driver
// container stuck pulling its image is Pending by phase, and Pending is not a
// diagnosis.
func podStatus(pod *v1.Pod) string {
	if pod.DeletionTimestamp != nil {
		return "Terminating"
	}
	if pod.Status.Reason != "" {
		return pod.Status.Reason
	}
	for _, status := range pod.Status.ContainerStatuses {
		if waiting := status.State.Waiting; waiting != nil && waiting.Reason != "" {
			return waiting.Reason
		}
	}
	return string(pod.Status.Phase)
}

// podRestarts totals the restarts of every container, init containers included.
func podRestarts(pod *v1.Pod) int32 {
	var total int32
	for _, status := range pod.Status.InitContainerStatuses {
		total += status.RestartCount
	}
	for _, status := range pod.Status.ContainerStatuses {
		total += status.RestartCount
	}
	return total
}

// podReadinessGates is the "satisfied/total" of the gates a pod declares.
func podReadinessGates(pod *v1.Pod) string {
	if len(pod.Spec.ReadinessGates) == 0 {
		return notApplicable
	}

	satisfied := 0
	for _, gate := range pod.Spec.ReadinessGates {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == gate.ConditionType && condition.Status == v1.ConditionTrue {
				satisfied++
			}
		}
	}
	return fmt.Sprintf("%d/%d", satisfied, len(pod.Spec.ReadinessGates))
}

// writeDaemonSetTable renders daemonsets the way "kubectl get ds" does.
func writeDaemonSetTable(w io.Writer, sets []appsv1.DaemonSet) error {
	header := []string{
		"NAME", "DESIRED", "CURRENT", "READY", "UP-TO-DATE", "AVAILABLE", "NODE SELECTOR", "AGE",
	}

	rows := make([][]string, 0, len(sets))
	for i := range sets {
		set := &sets[i]
		selector := notApplicable
		if len(set.Spec.Template.Spec.NodeSelector) > 0 {
			selector = labels.Set(set.Spec.Template.Spec.NodeSelector).String()
		}
		rows = append(rows, []string{
			set.Name,
			fmt.Sprintf("%d", set.Status.DesiredNumberScheduled),
			fmt.Sprintf("%d", set.Status.CurrentNumberScheduled),
			fmt.Sprintf("%d", set.Status.NumberReady),
			fmt.Sprintf("%d", set.Status.UpdatedNumberScheduled),
			fmt.Sprintf("%d", set.Status.NumberAvailable),
			selector,
			age(set.CreationTimestamp),
		})
	}

	return writeTable(w, header, rows)
}

// orNone is a cell value, or kubectl's marker for an empty one.
func orNone(value string) string {
	if value == "" {
		return notApplicable
	}
	return value
}

// tableAccept is what kubectl sends to ask the API server to render a resource
// as a Table instead of as objects.
const tableAccept = "application/json;as=Table;v=v1;g=meta.k8s.io, application/json"

// writePodStatus writes the pod table a .status file carries.
//
// The server is asked for the Table and the answer is printed, which is what
// kubectl does. That matters because the server knows pod states this package
// would otherwise have to re-derive from the pod's phase and container
// statuses, and got wrong: an init container in a crash loop reads
// Init:CrashLoopBackOff from the server and "Pending" from the phase.
//
// pods is the fallback, rendered here, for a server that will not serve a
// Table and for every test that runs against a fake client. An empty .status
// file is worse than one rendered a little differently.
func (g *Gatherer) writePodStatus(w io.Writer, namespace, name string, pods []v1.Pod) error {
	table, err := g.fetchPodTable(namespace, name)
	if err == nil {
		return printTable(w, table)
	}

	if _, lerr := fmt.Fprintf(g.errLogFile,
		"Falling back to a locally rendered table for %s: %v\n", namespace, err); lerr != nil {
		return fmt.Errorf("%v + error writing to stderr log file: %v", err, lerr)
	}
	return writePodTable(w, pods)
}

// fetchPodTable asks the API server for the pods of a namespace as a Table.
// An empty name means every pod of the namespace.
func (g *Gatherer) fetchPodTable(namespace, name string) (*metav1.Table, error) {
	if g.config == nil {
		return nil, errors.New("no REST configuration")
	}

	config := rest.CopyConfig(g.config)
	config.GroupVersion = &schema.GroupVersion{Group: "", Version: "v1"}
	config.APIPath = "/api"
	config.NegotiatedSerializer = scheme.Codecs.WithoutConversion()

	client, err := rest.RESTClientFor(config)
	if err != nil {
		return nil, fmt.Errorf("building a client for the table request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), customResourceTimeout)
	defer cancel()

	request := client.Get().Namespace(namespace).Resource("pods").SetHeader("Accept", tableAccept)
	if name != "" {
		request = request.Name(name)
	}

	raw, err := request.DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("asking for a table of the pods in %s: %w", namespace, err)
	}

	var table metav1.Table
	if err := json.Unmarshal(raw, &table); err != nil {
		return nil, fmt.Errorf("decoding the table of the pods in %s: %w", namespace, err)
	}
	if table.Kind != "Table" {
		return nil, fmt.Errorf("the server answered with a %q, not a Table", table.Kind)
	}
	return &table, nil
}

// printTable renders a server-side Table the way kubectl prints one, with the
// wide columns the bundle is read for.
func printTable(w io.Writer, table *metav1.Table) error {
	printer := printers.NewTablePrinter(printers.PrintOptions{Wide: true})
	if err := printer.PrintObj(table, w); err != nil {
		return fmt.Errorf("printing the table: %w", err)
	}
	return nil
}
