//go:build scale

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

// Tests for the decisions the scale suite makes about whether the lane has
// stopped moving. The cluster fetches the numbers; these decide what they
// mean, so they are driven here through the states a 2,000 node lane passes on
// its way up, with no cluster anywhere.
package e2e

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// scaleDaemonSet builds one DaemonSet status: how many nodes it wants a pod
// on, how many it has placed one on, and how many of those are Ready.
func scaleDaemonSet(namespace, name string, desired, current, ready int32) appsv1.DaemonSet {
	return appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Status: appsv1.DaemonSetStatus{
			DesiredNumberScheduled: desired,
			CurrentNumberScheduled: current,
			NumberReady:            ready,
		},
	}
}

func TestPendingDaemonSets(t *testing.T) {
	t.Parallel()

	t.Run("all placed", func(t *testing.T) {
		t.Parallel()

		got := scalePendingDaemonSets([]appsv1.DaemonSet{
			scaleDaemonSet("kube-system", "kube-proxy", 2002, 2002, 2002),
			scaleDaemonSet("mokka", "nvml-mock", 2002, 2002, 2002),
		})
		if len(got) != 0 {
			t.Errorf("scalePendingDaemonSets = %v, want none", got)
		}
	})

	t.Run("one still placing", func(t *testing.T) {
		t.Parallel()

		got := scalePendingDaemonSets([]appsv1.DaemonSet{
			scaleDaemonSet("kube-system", "kube-proxy", 2002, 2002, 2002),
			scaleDaemonSet("kube-system", "kindnet", 2002, 1500, 1500),
		})
		if len(got) != 1 || got[0] != "kube-system/kindnet (1500 of 2002)" {
			t.Errorf("scalePendingDaemonSets = %v, want one entry naming kindnet at 1500 of 2002", got)
		}
	})

	t.Run("placed but not ready still counts as settled", func(t *testing.T) {
		t.Parallel()

		// A fake node's pods are marked Ready by KWOK, not by a kubelet.
		// Waiting on numberReady would be waiting on a different controller
		// than the one whose work moves the measurement, and on a lane where
		// KWOK lags it would never finish at all.
		got := scalePendingDaemonSets([]appsv1.DaemonSet{
			scaleDaemonSet("kube-system", "kube-proxy", 2002, 2002, 0),
		})
		if len(got) != 0 {
			t.Errorf("scalePendingDaemonSets = %v, want none: every pod is placed", got)
		}
	})

	t.Run("a DaemonSet that wants no nodes is settled", func(t *testing.T) {
		t.Parallel()

		got := scalePendingDaemonSets([]appsv1.DaemonSet{scaleDaemonSet("gpu-operator", "mig-manager", 0, 0, 0)})
		if len(got) != 0 {
			t.Errorf("scalePendingDaemonSets = %v, want none", got)
		}
	})
}

func TestSettled(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		pending  []string
		total    int
		previous int
		want     bool
		reason   string
	}{
		{
			"two equal counts and nothing pending",
			nil, 6042, 6042, true, "",
		},
		{
			// The very first poll. One observation cannot be compared with
			// anything, and calling the lane settled on it would mean
			// measuring whatever happened to be true the instant the suite
			// started.
			"the first poll is never settled",
			nil, 6042, -1, false,
			"the pod count is 6042, with nothing yet to compare it against",
		},
		{
			"the pod count is still climbing",
			nil, 6042, 5800, false,
			"the pod count moved from 5800 to 6042",
		},
		{
			// A steady pod count is not enough on its own: a DaemonSet that
			// has not yet noticed two thousand new nodes has created nothing,
			// so the count sits perfectly still while the lane is not ready.
			"a steady count while a DaemonSet is still placing",
			[]string{"kube-system/kindnet (1500 of 2002)"}, 6042, 6042, false,
			"DaemonSets still placing pods: kube-system/kindnet (1500 of 2002)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			got, reason := laneSettled(tc.pending, tc.total, tc.previous)
			if got != tc.want {
				t.Errorf("laneSettled = %v, want %v (reason %q)", got, tc.want, reason)
			}
			if reason != tc.reason {
				t.Errorf("reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

func TestOnFakeNode(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]bool{
		"kwok-gpu-1":              true,
		"kwok-gpu-2000":           true,
		"kubectl-nv-scale-worker": false,
		"":                        false,
		"kwok-gpu":                false,
	} {
		if got := scaleOnFakeNode(name); got != want {
			t.Errorf("scaleOnFakeNode(%q) = %v, want %v", name, got, want)
		}
	}
}
