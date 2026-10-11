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
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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

	if got := found[0]["severity"]; got != string(output.SeverityWarning) {
		t.Errorf("severity is %v, want %q: the event dates the state, so the finding is not downgraded",
			got, output.SeverityWarning)
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

	if flag.DefValue != rules.DefaultUpgradeStuckAfter.String() {
		t.Errorf("the flag default is %q, want %q", flag.DefValue, rules.DefaultUpgradeStuckAfter)
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
