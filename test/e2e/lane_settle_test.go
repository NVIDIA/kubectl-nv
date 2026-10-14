//go:build e2e

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

package e2e

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

// This file is the one thing the healthy gate needs before it may read
// anything: a lane that has finished starting.
//
// hack/mokka/up.sh waits for the operand DaemonSets and the ClusterPolicy, so
// in the ordinary case this returns on its first look. It is here anyway, and
// it is not redundant, because the two waits answer different questions. The
// lane script asks "did this lane come up", once, as the lane is handed over;
// this asks "is it up now", of whatever cluster the suite was pointed at,
// which may have been handed over minutes ago by a target that does not run
// up.sh at all. The failure it exists to stop is specific and was observed:
// dcgm-exporter's readiness probe carries a 45 second initialDelaySeconds, and
// for those 45 seconds `kubectl nv health` correctly reports NV-DCGM-001 at
// warning on a lane that is merely still starting.

// laneSettleTimeout bounds the wait. Five minutes: the exporter's initial delay is
// 45 seconds and an image pull on a cold runner is the rest of it. A lane that
// has not settled by then is not slow, it is broken, and the failure says
// which workload is holding it up.
const laneSettleTimeout = 5 * time.Minute

// laneSettlePoll is how often the lane is asked again.
const laneSettlePoll = 5 * time.Second

// clusterPolicyGVR is the GPU Operator's own object. It is addressed
// dynamically because the CRD is not vendored and its schema moves between
// operator versions; only status.state is read, and that field is as old as
// the CRD.
var clusterPolicyGVR = schema.GroupVersionResource{
	Group:    "nvidia.com",
	Version:  "v1",
	Resource: "clusterpolicies",
}

// settleLane blocks until every DaemonSet in the operator namespace has its
// pods rolled out and the ClusterPolicy says ready.
//
// A cluster with no ClusterPolicy CRD is not failed on: a lane without the GPU
// Operator is one this has nothing to say about, and treating an unreadable
// policy as "not ready" would hang every such run for five minutes before
// failing it.
func settleLane(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(laneSettleTimeout)
	var holding []string

	for {
		holding = unsettledWorkloads(t)
		if len(holding) == 0 {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(laneSettlePoll)
	}

	t.Fatalf("after %s the lane has not settled:\n  %s", laneSettleTimeout, strings.Join(holding, "\n  "))
}

// unsettledWorkloads names what is still coming up, so that a timeout says
// which operand is holding the lane rather than only that something is.
func unsettledWorkloads(t *testing.T) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var holding []string

	sets, err := clusterClient(t).AppsV1().DaemonSets(operatorNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing the DaemonSets in %s: %v", operatorNamespace, err)
	}
	for i := range sets.Items {
		ds := &sets.Items[i]
		desired := ds.Status.DesiredNumberScheduled

		// A DaemonSet with nothing to schedule is settled, not starting. The
		// operator creates nvidia-device-plugin-mps-control-daemon on every
		// lane and gives it a node selector that matches nothing unless MPS
		// sharing is configured, so its desired count is 0 for ever; reading
		// that as "no pod yet" held this wait to its full five minutes on a
		// lane that was ready in seconds.
		//
		// Whether the operands that matter were placed at all is not this
		// wait's question. hack/mokka/up.sh asks it by name, against the
		// worker count, before it hands the lane over.
		if desired == 0 {
			continue
		}

		if ds.Status.NumberReady != desired || ds.Status.UpdatedNumberScheduled != desired {
			holding = append(holding, fmt.Sprintf("%s: desired=%d ready=%d updated=%d",
				ds.Name, desired, ds.Status.NumberReady, ds.Status.UpdatedNumberScheduled))
		}
	}

	holding = append(holding, unreadyClusterPolicies(t)...)

	return holding
}

// unreadyClusterPolicies names every ClusterPolicy whose status.state is not
// ready, by name.
//
// Every one of them and not the first: the operator reconciles one policy and
// marks the rest ignored, and which one the API server lists first is not
// something a test may rely on. An ignored duplicate has no status at all, so
// it is reported here as having no state yet, which is the honest answer for a
// wait: a lane with a leftover policy on it is a lane somebody should look at
// before the gate reads it.
//
// A cluster with no such collection produces nothing. A lane without the GPU
// Operator is one this has nothing to say about, and treating an unreadable
// policy as "not ready" would hang every such run until the deadline.
func unreadyClusterPolicies(t *testing.T) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	config, err := clientcmd.BuildConfigFromFlags("", kubeconfigPath)
	if err != nil {
		t.Fatalf("building a client configuration from %s: %v", kubeconfigPath, err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatalf("building a dynamic client: %v", err)
	}

	list, err := client.Resource(clusterPolicyGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}

	var holding []string
	for i := range list.Items {
		policy := &list.Items[i]
		state, found, err := unstructured.NestedString(policy.Object, "status", "state")
		switch {
		case err != nil || !found:
			holding = append(holding, fmt.Sprintf("ClusterPolicy %s has no status.state yet", policy.GetName()))
		case !strings.EqualFold(state, "ready"):
			holding = append(holding, fmt.Sprintf("ClusterPolicy %s is %q, want ready", policy.GetName(), state))
		}
	}

	return holding
}

// laneProfile is the Mokka profile the lane was brought up with. The lane
// targets pass it in the environment, and t4 is what the Makefile defaults to.
func laneProfile() string {
	if profile := os.Getenv("MOKKA_PROFILE"); profile != "" {
		return profile
	}

	return "t4"
}

// profileGPUCountRE pulls the arms out of mokka::profile_gpu_count, which look
// like `gb300|gb200|t4) printf '4' ;;`.
var profileGPUCountRE = regexp.MustCompile(`^\s*([a-z0-9|]+)\)\s*printf\s*'(\d+)'`)

// profileGPUCount is how many simulated GPUs a profile puts on each worker,
// read out of hack/mokka/lib.sh.
//
// Out of the lane script and not out of the cluster, deliberately. The whole
// point of the number here is to be a witness the cluster did not provide: a
// capacity checked only against the API server agrees with it whenever the
// device plugin advertises the wrong thing, which is one of the faults this
// suite injects on purpose.
func profileGPUCount(t *testing.T, profile string) int64 {
	t.Helper()

	const libPath = "../../hack/mokka/lib.sh"

	raw, err := os.ReadFile(libPath)
	if err != nil {
		t.Fatalf("reading %s, which is where the profile GPU counts live: %v", libPath, err)
	}

	for _, line := range strings.Split(string(raw), "\n") {
		match := profileGPUCountRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		count, err := strconv.ParseInt(match[2], 10, 64)
		if err != nil {
			continue
		}
		for _, name := range strings.Split(match[1], "|") {
			if name == profile {
				return count
			}
		}
	}

	t.Fatalf("%s names no GPU count for profile %q", libPath, profile)

	return 0
}
