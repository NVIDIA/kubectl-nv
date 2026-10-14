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

package health_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/kubectl-nv/pkg/cmd"
	"github.com/NVIDIA/kubectl-nv/pkg/cmd/health"
	"github.com/NVIDIA/kubectl-nv/pkg/health/rules"
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// upgradingCluster is the healthy cluster with its one GPU node parked in a
// driver upgrade state, and the event the upgrade controller recorded when it
// put it there.
//
// The event is the only thing that dates the state: the label carries no
// transition time, so without it NV-UPG-001 cannot say how long the node has
// been stuck and reports at info instead.
func upgradingCluster(enteredAgo time.Duration) cluster {
	c := healthyCluster()

	for _, object := range c.objects {
		node, ok := object.(*corev1.Node)
		if !ok {
			continue
		}

		node.Labels["nvidia.com/gpu-driver-upgrade-state"] = "cordon-required"
	}

	c.objects = append(c.objects, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: "worker-1.17a", Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: "Node", Name: "worker-1"},
		Reason:         "GPUDriverUpgrade",
		Message:        "Successfully updated node state label to cordon-required",
		LastTimestamp:  metav1.NewTime(time.Now().Add(-enteredAgo)),
	})

	return c
}

// findingsFor decodes the JSON report and returns the findings of one code.
func findingsFor(t *testing.T, raw string, code string) []map[string]any {
	t.Helper()

	var report struct {
		Findings []map[string]any `json:"findings"`
	}

	if err := json.Unmarshal([]byte(raw), &report); err != nil {
		t.Fatalf("decoding the report: %v\n%s", err, raw)
	}

	var out []map[string]any

	for _, finding := range report.Findings {
		if finding["code"] == code {
			out = append(out, finding)
		}
	}

	return out
}

// The flag is what the rule's window is, so it has to reach the rule. A flag
// parsed and dropped on the floor looks exactly like a flag that works until
// somebody sets it.
func TestHealthUpgradeStuckAfterReachesTheRule(t *testing.T) {
	t.Parallel()

	c := upgradingCluster(45 * time.Minute)

	// The default window is 30 minutes, so 45 minutes in cordon-required is a
	// stuck node, and the event dating the state keeps the finding at
	// warning rather than downgrading it.
	s, _ := run(t, c, "-o", "json")

	found := findingsFor(t, s.out.String(), "NV-UPG-001")
	if len(found) != 1 {
		t.Fatalf("got %d NV-UPG-001 findings at the default window, want 1:\n%s", len(found), s.out)
	}

	if got := found[0]["severity"]; got != "warning" {
		t.Errorf("severity is %v, want %q: the event dates the state, so the finding is not downgraded",
			got, "warning")
	}

	if got := found[0]["evidence"].(map[string]any)["upgradeStuckAfter"]; got != "30m0s" {
		t.Errorf("the finding was measured against %v, want the 30m0s default", got)
	}

	// Raising the window past the event's age has to silence the same node.
	// If the flag never reached the rule this run is identical to the one
	// above.
	s, err := run(t, c, "-o", "json", "--upgrade-stuck-after", "2h")
	if err != nil {
		t.Fatalf("health: %v\nstdout:\n%s\nstderr:\n%s", err, s.out, s.err)
	}

	if found := findingsFor(t, s.out.String(), "NV-UPG-001"); len(found) != 0 {
		t.Fatalf("got %d NV-UPG-001 findings at --upgrade-stuck-after 2h, want none:\n%s", len(found), s.out)
	}

	// And lowering it past the age has to bring it back, so that the run
	// above cannot pass because the rule simply stopped firing.
	s, _ = run(t, c, "-o", "json", "--upgrade-stuck-after", "1m")

	if found := findingsFor(t, s.out.String(), "NV-UPG-001"); len(found) != 1 {
		t.Fatalf("got %d NV-UPG-001 findings at --upgrade-stuck-after 1m, want 1:\n%s", len(found), s.out)
	}
}

// The flag's default is the rule's default, written in one place. Two
// defaults that drift apart make the documented window and the one the rule
// uses different numbers.
func TestHealthUpgradeStuckAfterDefaultsToTheRuleDefault(t *testing.T) {
	t.Parallel()

	command := health.NewCommand(&cmd.Options{})

	flag := command.Flags().Lookup("upgrade-stuck-after")
	if flag == nil {
		t.Fatal("the health command registers no --upgrade-stuck-after flag")
	}

	// A literal, not the constant the flag is built from. Comparing the
	// implementation against itself would pass whatever the default became,
	// and the default is documented in docs/findings/NV-UPG-001.md as half an
	// hour.
	if flag.DefValue != "30m0s" {
		t.Errorf("the flag default is %q, want %q", flag.DefValue, "30m0s")
	}

	if rules.DefaultUpgradeStuckAfter.String() != "30m0s" {
		t.Errorf("the rule default is %q, want %q", rules.DefaultUpgradeStuckAfter, "30m0s")
	}
}

// A window of zero would report every node the instant it entered an upgrade
// step, and a negative one every node always. Neither is a window anybody
// means, and it is worth catching before the scan rather than after the wait
// for it.
func TestHealthRejectsANonPositiveUpgradeStuckAfter(t *testing.T) {
	t.Parallel()

	for _, value := range []string{"0", "0s", "-5m"} {
		t.Run(value, func(t *testing.T) {
			t.Parallel()

			_, err := run(t, healthyCluster(), "--upgrade-stuck-after", value)
			if err == nil {
				t.Fatalf("health accepted --upgrade-stuck-after %s", value)
			}

			if got := output.ExitCodeFor(err); got != output.ExitUsage {
				t.Errorf("the error exits %d, want %d: %v", got, output.ExitUsage, err)
			}

			if !strings.Contains(err.Error(), "upgrade-stuck-after") {
				t.Errorf("the error does not name the flag: %v", err)
			}
		})
	}
}

// A window that is not a duration is a mistake worth catching before the scan
// rather than after the wait for it.
func TestHealthRejectsAnUnreadableUpgradeStuckAfter(t *testing.T) {
	t.Parallel()

	_, err := run(t, healthyCluster(), "--upgrade-stuck-after", "half an hour")
	if err == nil {
		t.Fatal("health accepted a window that is not a duration")
	}

	if !strings.Contains(err.Error(), "upgrade-stuck-after") {
		t.Errorf("the error does not name the flag: %v", err)
	}
}

// The ComputeDomains read is wired, and a cluster that serves no such CRD is
// not a partial read: nothing was denied and nothing is missing.
func TestHealthReadsComputeDomainsWithoutTheCRD(t *testing.T) {
	t.Parallel()

	s, err := run(t, healthyCluster())
	if err != nil {
		t.Fatalf("health: %v\nstdout:\n%s\nstderr:\n%s", err, s.out, s.err)
	}

	if strings.Contains(s.out.String()+s.err.String(), "computedomains") {
		t.Errorf("a cluster without the DRA driver reported a partial read:\n%s\n%s", s.out, s.err)
	}
}

// computeDomainGVR is what the dynamic fake has to be told about, or it
// invents a list kind and the read comes back empty.
var computeDomainGVR = schema.GroupVersionResource{
	Group: "resource.nvidia.com", Version: "v1beta1", Resource: "computedomains",
}

// runWithComputeDomain executes the command against a cluster that serves the
// ComputeDomain CRD and holds one domain.
//
// It builds its own clients rather than going through run, because both halves
// of the read have to be there: the discovery that says the cluster serves the
// resource, and the dynamic client that holds the object. A fake missing
// either answers "no ComputeDomains", which is what a dropped read looks like
// too.
func runWithComputeDomain(t *testing.T, domain *unstructured.Unstructured, deny bool, args ...string) (*streams, error) {
	t.Helper()

	clientset := fake.NewSimpleClientset(healthyCluster().objects...)
	clientset.Resources = append(clientset.Resources, &metav1.APIResourceList{
		GroupVersion: computeDomainGVR.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: computeDomainGVR.Resource, Kind: "ComputeDomain"}},
	})

	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			clusterPolicyGVR: "ClusterPolicyList",
			computeDomainGVR: "ComputeDomainList",
		},
		clusterPolicy("cluster-policy", "ready"), domain)

	if deny {
		dyn.PrependReactor("list", computeDomainGVR.Resource, func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: computeDomainGVR.Group, Resource: computeDomainGVR.Resource},
				"", errors.New("denied"))
		})
	}

	s := &streams{out: &bytes.Buffer{}, err: &bytes.Buffer{}}
	io := genericiooptions.IOStreams{In: strings.NewReader(""), Out: s.out, ErrOut: s.err}

	command := health.NewCommand(&cmd.Options{
		Streams: io,
		Factory: &fakeFactory{clientset: clientset, dynamic: dyn},
		Logger:  output.NewLogger(io, output.LoggerOptions{NoColor: true}),
	})
	command.SetOut(s.out)
	command.SetErr(s.err)
	command.SetArgs(args)
	command.SilenceUsage = true
	command.SilenceErrors = true

	return s, command.Execute()
}

// A ComputeDomain the cluster serves has to reach Input.ComputeDomains, or
// NV-CD-001 runs over an empty set on every cluster and reports every one of
// them clean.
func TestHealthComputeDomainsReachTheRule(t *testing.T) {
	t.Parallel()

	domain := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "resource.nvidia.com/v1beta1",
		"kind":       "ComputeDomain",
		"metadata":   map[string]any{"name": "cd-a", "namespace": "workload"},
		"status": map[string]any{
			"status": "NotReady",
			"nodes": []any{map[string]any{
				"name": "worker-1", "cliqueID": "uuid.1", "ipAddress": "10.0.0.1", "status": "NotReady",
			}},
		},
	}}

	s, _ := runWithComputeDomain(t, domain, false, "-o", "json")

	found := findingsFor(t, s.out.String(), "NV-CD-001")
	if len(found) == 0 {
		t.Fatalf("the ComputeDomain never reached the rule:\n%s", s.out)
	}

	if got := found[0]["evidence"].(map[string]any)["computeDomain"]; got != "workload/cd-a" {
		t.Errorf("the finding names %v, want workload/cd-a", got)
	}
}

// A denied ComputeDomains read is a partial run, not a silent one. NV-CD-001
// then evaluates an empty set, and a report that did not say so would tell a
// reader the cluster's ComputeDomains are fine when nobody looked at them.
func TestHealthDeniedComputeDomainsIsPartial(t *testing.T) {
	t.Parallel()

	domain := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "resource.nvidia.com/v1beta1",
		"kind":       "ComputeDomain",
		"metadata":   map[string]any{"name": "cd-a", "namespace": "workload"},
	}}

	s, err := runWithComputeDomain(t, domain, true)
	if err == nil {
		t.Fatalf("health succeeded with the ComputeDomains read denied:\n%s", s.out)
	}

	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Errorf("the error exits %d, want %d: %v", got, output.ExitPartial, err)
	}

	if !strings.Contains(err.Error(), "computedomains") {
		t.Errorf("the error does not name what could not be read: %v", err)
	}
}
