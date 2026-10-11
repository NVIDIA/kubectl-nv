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
	"k8s.io/apimachinery/pkg/runtime"
)

// Jobs lists the batch/v1 Jobs in namespace. An empty namespace means every
// namespace, which is the convention Pods already follows.
//
// It is a list rather than a get by name because that is the shape a bundle
// can refuse uniformly and the shape the pager already walks. The caller
// picks the job it was asked about out of the result, and an absent one is a
// question about something that is not there rather than a failed read.
//
// A denied list is a partial read: the topology itself was still readable, so
// the levels and their free GPUs are worth printing with a note that the job
// could not be looked up.
func (l *Live) Jobs(ctx context.Context, namespace string) ([]batchv1.Job, error) {
	jobs, err := listAll[batchv1.Job](ctx, "", func(ctx context.Context, opts metav1.ListOptions) (runtime.Object, error) {
		return l.clientset.BatchV1().Jobs(namespace).List(ctx, opts)
	})
	switch {
	case err == nil:
		return jobs, nil
	case apierrors.IsForbidden(err):
		return nil, &PartialError{Resource: "jobs", Err: err}
	default:
		return nil, fmt.Errorf("listing jobs in namespace %q: %w", namespace, err)
	}
}

// Jobs reports that a bundle cannot answer. must-gather records the
// operator's own pods and nothing of the workload, so there is no job list in
// it; an empty success would read as a namespace with no jobs.
func (b *Bundle) Jobs(_ context.Context, _ string) ([]batchv1.Job, error) {
	return []batchv1.Job{}, &PartialError{
		Resource: "jobs",
		Err:      errors.New("must-gather does not collect the workload objects"),
	}
}
