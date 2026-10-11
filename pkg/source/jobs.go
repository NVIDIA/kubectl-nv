/**
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
**/

package source

import (
	"context"
	"errors"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Job returns the batch/v1 Job named in namespace.
//
// It is a Get rather than a list filtered by name: the caller knows both the
// namespace and the name, so one object is what it wants. A list would need
// list RBAC over every job in the namespace and would carry all of them back
// to find one, on a cluster where a namespace can hold thousands.
//
// NotFound is passed through as the API server reported it, so that the
// caller can tell "there is no such job", which is a question the user got
// wrong and exits 2, from "this run could not read it".
//
// A denied read is a partial read: the topology itself was still readable, so
// the levels and their free GPUs are worth printing with a note that the job
// could not be looked up.
func (l *Live) Job(ctx context.Context, namespace, name string) (*batchv1.Job, error) {
	job, err := l.clientset.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil:
		return job, nil
	case apierrors.IsNotFound(err):
		return nil, err
	case apierrors.IsForbidden(err):
		return nil, &PartialError{Resource: "jobs", Err: err}
	default:
		return nil, fmt.Errorf("reading job %s/%s: %w", namespace, name, err)
	}
}

// Job reports that a bundle cannot answer. must-gather records the operator's
// own pods and nothing of the workload, so there is no job in it; reporting
// NotFound would tell the user their job does not exist, which this bundle
// has no way of knowing.
func (b *Bundle) Job(_ context.Context, _, _ string) (*batchv1.Job, error) {
	return nil, &PartialError{
		Resource: "jobs",
		Err:      errors.New("must-gather does not collect the workload objects"),
	}
}
