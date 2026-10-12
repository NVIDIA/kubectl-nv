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
	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// Progress is where a gather says what it is doing. It is the subset of
// output.Logger the gatherer needs, declared here so that this package does not
// depend on how the commands choose to print.
type Progress interface {
	Infof(format string, a ...any)
	Warnf(format string, a ...any)
	Errorf(format string, a ...any)
}

// Error says how far a gather got before it failed, which is what decides
// whether the bundle it left behind is worth sending.
type Error struct {
	// Err is what went wrong.
	Err error
	// Partial reports that at least one artifact was collected before the
	// failure, so the bundle holds part of what was asked for.
	Partial bool
}

// Error implements error.
func (e *Error) Error() string { return e.Err.Error() }

// Unwrap returns the cause, so that errors.Is and errors.As reach it.
func (e *Error) Unwrap() error { return e.Err }

// ExitCode separates a bundle worth sending from no bundle at all: a gather
// that collected nothing says nothing about the cluster, while one that
// collected part of what was asked for is still worth looking at, and the
// error log says what is missing.
func (e *Error) ExitCode() int {
	if e.Partial {
		return output.ExitPartial
	}
	return output.ExitError
}

// failed wraps err with how far the gather got.
func (g *Gatherer) failed(err error) error {
	return &Error{Err: err, Partial: g.collected}
}

// step reports one collector starting. Progress goes to the error stream, so
// it never mixes with what a command puts on stdout.
func (g *Gatherer) step(format string, a ...any) {
	if g.progress == nil {
		return
	}
	g.progress.Infof(format, a...)
}

// note reports something the user should know about but that does not stop the
// gather, such as a part of the cluster that could not be read.
func (g *Gatherer) note(format string, a ...any) {
	if g.progress == nil {
		return
	}
	g.progress.Warnf(format, a...)
}
