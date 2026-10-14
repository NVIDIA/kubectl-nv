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
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	// nodeEventFieldSelector keeps the server from sending the whole event
	// stream. A cluster's events are mostly pods being scheduled and images
	// being pulled; the ones that say anything about a GPU node are a rounding
	// error in that, and listing all of them to throw nearly all of them away
	// would be a large read on every run of a diagnostic command.
	nodeEventFieldSelector = "involvedObject.kind=Node"

	// eventInvolvedKindNode is what an event about a node carries, and what
	// the client side filter checks. The server selector above is the one that
	// makes the read small; this is what makes the result correct whatever the
	// server did with the selector.
	eventInvolvedKindNode = "Node"

	// fileNodeEvents is where a must-gather bundle would carry node events.
	// pkg/mustgather does not write it yet, so reading one is a partial read;
	// a gather that starts collecting them needs no change here.
	fileNodeEvents = "node_events.yaml"
)

// NodeEventReader is a source that can read the events about nodes.
//
// It is deliberately not part of Source. Every fake in every track implements
// that interface, so widening it would break all of them at once for a seam
// only one command needs. A caller asks for this one through ReadNodeEvents,
// which is the whole of the contract: a source that cannot answer returns
// nothing rather than failing.
type NodeEventReader interface {
	// NodeEvents returns the events whose involved object is a node.
	NodeEvents(ctx context.Context) ([]corev1.Event, error)
}

// ReadNodeEvents returns the node events src can give, or nothing when it has
// no way to read them.
//
// A source without the seam is not an error. The rules that read events have
// to cope with an empty set regardless, because a cluster may simply have
// none, and failing a run over a method a source does not implement would make
// every such source useless for a command that only wants the events if they
// are there.
func ReadNodeEvents(ctx context.Context, src Source) ([]corev1.Event, error) {
	reader, ok := src.(NodeEventReader)
	if !ok {
		return nil, nil
	}

	return reader.NodeEvents(ctx)
}

// NodeEvents returns the cluster's events about nodes.
//
// A denial makes the run partial rather than failing it: events are a
// secondary read, the node findings stand without them, and the report says
// what it could not see. Anything else means the read failed rather than was
// refused, and a run that could not talk to the API server has produced no
// findings rather than incomplete ones.
func (l *Live) NodeEvents(ctx context.Context) ([]corev1.Event, error) {
	events, err := listAll[corev1.Event](ctx, "", func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		opts.FieldSelector = nodeEventFieldSelector

		return l.clientset.CoreV1().Events("").List(ctx, opts)
	})
	switch {
	case err == nil:
		return onlyNodeEvents(events), nil
	case apierrors.IsForbidden(err):
		return nil, &PartialError{Resource: "events", Err: err}
	default:
		return nil, fmt.Errorf("listing node events: %w", err)
	}
}

// NodeEvents returns the node events a must-gather bundle recorded.
//
// pkg/mustgather does not collect them, so today this is always a partial
// read naming the file that is not there. That is the honest answer and it
// costs a bundle replay nothing: a bundle's pod list is already partial by
// construction, because it holds one namespace and the inventory asks for
// every one.
func (b *Bundle) NodeEvents(ctx context.Context) ([]corev1.Event, error) {
	var list corev1.EventList
	if err := b.readYAML(ctx, fileNodeEvents, &list); err != nil {
		return nil, err
	}

	return onlyNodeEvents(list.Items), nil
}

// onlyNodeEvents keeps the events about nodes.
//
// The live path already asks the server for those alone, so on a real cluster
// this filters nothing. It is here because the field selector is a request
// and not a guarantee: a bundle carries whatever was gathered, and a fake
// clientset applies no field selector at all, so without it a rule could be
// handed the whole event stream and nothing would have noticed.
func onlyNodeEvents(events []corev1.Event) []corev1.Event {
	var out []corev1.Event

	for _, e := range events {
		if e.InvolvedObject.Kind == eventInvolvedKindNode {
			out = append(out, e)
		}
	}

	return out
}
