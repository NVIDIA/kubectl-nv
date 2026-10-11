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

package source

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// event builds one event about an object of the given kind.
func event(name, kind, involved string) corev1.Event {
	return corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "default"},
		InvolvedObject: corev1.ObjectReference{Kind: kind, Name: involved},
		Reason:         "Reason" + name,
	}
}

// NodeEvents is what a rule about upgrade timing reads: a node stuck
// mid-upgrade looks fine in its labels and tells the truth in its events.
//
// Only the events about nodes. A cluster's event stream is mostly pods being
// scheduled and images being pulled, and handing a rule the whole of it would
// be handing it the wrong haystack.
func TestLiveNodeEventsReturnsOnlyNodeEvents(t *testing.T) {
	t.Parallel()

	clientset := fake.NewSimpleClientset(
		runtimeObjects(
			event("a", "Node", "worker-1"),
			event("b", "Pod", "nvidia-device-plugin-daemonset-xk2p9"),
			event("c", "Node", "worker-2"),
		)...,
	)

	live := NewLive(clientset, nil)

	events, err := live.NodeEvents(context.Background())
	if err != nil {
		t.Fatalf("NodeEvents: %v", err)
	}

	got := make([]string, 0, len(events))
	for _, e := range events {
		got = append(got, e.InvolvedObject.Name)
	}

	if want := []string{"worker-1", "worker-2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("NodeEvents returned %v, want %v", got, want)
	}
}

// The field selector is what keeps the read small on a real cluster: the fake
// client does not apply one, so without the client-side filter above this
// would pass while the live path shipped the whole event stream.
func TestLiveNodeEventsAsksTheServerForNodeEventsOnly(t *testing.T) {
	t.Parallel()

	clientset := fake.NewSimpleClientset()

	var seen string

	clientset.PrependReactor("list", "events", func(action k8stesting.Action) (bool, runtime.Object, error) {
		list, ok := action.(k8stesting.ListAction)
		if !ok {
			return false, nil, nil
		}

		seen = list.GetListRestrictions().Fields.String()

		return true, &corev1.EventList{}, nil
	})

	if _, err := NewLive(clientset, nil).NodeEvents(context.Background()); err != nil {
		t.Fatalf("NodeEvents: %v", err)
	}

	if want := "involvedObject.kind=Node"; seen != want {
		t.Errorf("the list asked for %q, want %q", seen, want)
	}
}

// Events are a secondary read, so a denial makes the run partial rather than
// failing it: the node findings stand without them, and the report says what
// it could not see.
func TestLiveNodeEventsForbiddenIsPartial(t *testing.T) {
	t.Parallel()

	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "events"}, "", errors.New("nope"))
	})

	_, err := NewLive(clientset, nil).NodeEvents(context.Background())

	var partial *PartialError
	if !errors.As(err, &partial) {
		t.Fatalf("NodeEvents returned %T (%v), want a *PartialError", err, err)
	}

	if partial.Resource != "events" {
		t.Errorf("the partial error names %q, want events", partial.Resource)
	}

	if partial.ExitCode() != 3 {
		t.Errorf("the partial error exits %d, want 3", partial.ExitCode())
	}
}

// Anything that is not a denial means the read failed rather than was refused,
// and that is a plain error: a run that could not talk to the API server has
// not produced incomplete findings, it has produced none.
func TestLiveNodeEventsOtherErrorsAreNotPartial(t *testing.T) {
	t.Parallel()

	boom := errors.New("connection refused")

	clientset := fake.NewSimpleClientset()
	clientset.PrependReactor("list", "events", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, boom
	})

	_, err := NewLive(clientset, nil).NodeEvents(context.Background())

	var partial *PartialError
	if errors.As(err, &partial) {
		t.Fatalf("NodeEvents returned a partial error for a transport failure: %v", err)
	}

	if !errors.Is(err, boom) {
		t.Errorf("NodeEvents returned %v, want it to wrap the cause", err)
	}
}

// A must-gather bundle holds no events: pkg/mustgather does not collect them.
// Saying so as a partial read is the honest answer, and it costs a bundle
// replay nothing, because the bundle pod list is already partial by
// construction.
func TestBundleNodeEventsIsPartial(t *testing.T) {
	t.Parallel()

	bundle, err := NewBundle(t.TempDir())
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}

	events, err := bundle.NodeEvents(context.Background())
	if len(events) != 0 {
		t.Errorf("NodeEvents returned %d events from an empty bundle", len(events))
	}

	var partial *PartialError
	if !errors.As(err, &partial) {
		t.Fatalf("NodeEvents returned %T (%v), want a *PartialError", err, err)
	}

	if partial.Resource != fileNodeEvents {
		t.Errorf("the partial error names %q, want %q", partial.Resource, fileNodeEvents)
	}
}

// A bundle that does carry the file is read from it, so that a gather which
// starts collecting events needs no change here.
func TestBundleNodeEventsReadsTheFileWhenItIsThere(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeBundleFile(t, dir, fileNodeEvents, `apiVersion: v1
kind: EventList
items:
- apiVersion: v1
  kind: Event
  metadata:
    name: a
    namespace: default
  involvedObject:
    kind: Node
    name: worker-1
  reason: DriverUpgradeStarted
- apiVersion: v1
  kind: Event
  metadata:
    name: b
    namespace: default
  involvedObject:
    kind: Pod
    name: some-pod
  reason: Pulled
`)

	bundle, err := NewBundle(dir)
	if err != nil {
		t.Fatalf("NewBundle: %v", err)
	}

	events, err := bundle.NodeEvents(context.Background())
	if err != nil {
		t.Fatalf("NodeEvents: %v", err)
	}

	if len(events) != 1 {
		t.Fatalf("NodeEvents returned %d events, want only the node one: %+v", len(events), events)
	}

	if events[0].Reason != "DriverUpgradeStarted" {
		t.Errorf("NodeEvents returned %q, want the node event", events[0].Reason)
	}
}

// The helper is what the health command calls, because Source has no event
// method and must not grow one: every fake in every track implements that
// interface, and widening it breaks all of them.
func TestReadNodeEvents(t *testing.T) {
	t.Parallel()

	t.Run("a source that reads events", func(t *testing.T) {
		t.Parallel()

		clientset := fake.NewSimpleClientset(runtimeObjects(event("a", "Node", "worker-1"))...)

		events, err := ReadNodeEvents(context.Background(), NewLive(clientset, nil))
		if err != nil {
			t.Fatalf("ReadNodeEvents: %v", err)
		}

		if len(events) != 1 {
			t.Errorf("ReadNodeEvents returned %d events, want 1", len(events))
		}
	})

	// A source that cannot read events is not an error. The rules that read
	// them have to cope with an empty set anyway, because a cluster may simply
	// have none, and failing a run over a seam a source does not implement
	// would make every such source useless.
	t.Run("a source that does not", func(t *testing.T) {
		t.Parallel()

		events, err := ReadNodeEvents(context.Background(), eventlessSource{})
		if err != nil {
			t.Fatalf("ReadNodeEvents: %v", err)
		}

		if events != nil {
			t.Errorf("ReadNodeEvents returned %+v, want nothing", events)
		}
	})
}

// eventlessSource is a Source with no NodeEvents method, which is what every
// Source outside this package is.
type eventlessSource struct{ Source }

// writeBundleFile drops one artifact into a bundle directory.
func writeBundleFile(t *testing.T, dir, name, body string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

// runtimeObjects turns events into what the fake clientset takes.
func runtimeObjects(events ...corev1.Event) []runtime.Object {
	out := make([]runtime.Object, 0, len(events))
	for i := range events {
		out = append(out, &events[i])
	}

	return out
}
