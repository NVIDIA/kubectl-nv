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
	"net/http"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// DefaultExecTimeout bounds one exec or one copy out of a driver pod. A bug
// report takes tens of seconds on a busy node, so this is generous; what it
// rules out is a pod that accepted the connection and will never answer, which
// without a deadline costs the user the whole bundle rather than one file.
const DefaultExecTimeout = 2 * time.Minute

// forbiddenExec reports whether err is the API server refusing to let us into
// the pod.
//
// It cannot be apierrors.IsForbidden alone. An exec is an HTTP upgrade, and a
// denial fails that upgrade before any Status is decoded: client-go hands back
// a plain error carrying the Status message as text. A gather that only
// recognised the typed form exited 0 against a cluster that had refused it
// every driver pod, and reported the missing evidence as merely absent.
//
// The match is deliberately narrow. Calling every exec failure "forbidden"
// would report a wedged driver container as an RBAC problem and send the user
// to their cluster administrator for nothing.
func forbiddenExec(err error) bool {
	if err == nil {
		return false
	}
	if apierrors.IsForbidden(err) {
		return true
	}

	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Code == http.StatusForbidden {
		return true
	}

	message := strings.ToLower(err.Error())
	if !strings.Contains(message, "forbidden") {
		return false
	}
	// The text the API server puts in the body of a 403 on the subresource.
	// "unable to upgrade connection" on its own is any failed upgrade,
	// including a refused connection, so it only counts next to the words RBAC
	// actually uses.
	for _, marker := range []string{"pods/exec", "cannot create resource", "is forbidden:"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// execTimeout is how long one exec or copy may take.
func (g *Gatherer) execTimeout() time.Duration {
	if g.opts.ExecTimeout > 0 {
		return g.opts.ExecTimeout
	}
	return DefaultExecTimeout
}

// execContext returns the deadline one exec or copy runs under, and the
// timeout it was given, so that a caller can name it in what it records.
func (g *Gatherer) execContext() (context.Context, context.CancelFunc, time.Duration) {
	timeout := g.execTimeout()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	return ctx, cancel, timeout
}

// execFailure turns what one exec or copy returned into what the manifest
// should record for it. A deadline that ran out is named as such: "the pod did
// not answer in time" and "the pod said no" send a reader to different places.
func execFailure(ctx context.Context, timeout time.Duration, what string, pod *v1.Pod, err error) error {
	if errors.Is(err, context.DeadlineExceeded) || (ctx != nil && errors.Is(ctx.Err(), context.DeadlineExceeded)) {
		return &timeoutError{what: what, pod: pod.Name, timeout: timeout}
	}
	return fmt.Errorf("%s from pod %s: %w", what, pod.Name, err)
}

// timeoutError is one exec or copy that ran out of time.
type timeoutError struct {
	what    string
	pod     string
	timeout time.Duration
}

func (e *timeoutError) Error() string {
	return fmt.Sprintf("timeout after %s: %s from pod %s", e.timeout, e.what, e.pod)
}

// Timeout reports the deadline that was exceeded, so a caller can tell this
// apart from any other failure without matching on the message.
func (e *timeoutError) Timeout() time.Duration { return e.timeout }

// timedOut reports whether err is one of ours for a deadline that ran out.
func timedOut(err error) bool {
	var t *timeoutError
	return errors.As(err, &t)
}
