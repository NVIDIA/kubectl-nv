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

package output

import (
	"fmt"
	"io"

	"github.com/mattn/go-isatty"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// ANSI escapes used for the severity of a line.
const (
	colorReset  = "\033[0m"
	colorYellow = "\033[33m"
	colorRed    = "\033[31m"
)

// fdWriter is a writer that has a file descriptor to ask about.
type fdWriter interface {
	io.Writer
	Fd() uintptr
}

// Logger writes progress and warnings to the error stream. Nothing it prints
// ever reaches stdout, which belongs to the machine readable output alone, and
// it never asks the user anything: every command has to stay usable in a pipe.
type Logger struct {
	out   io.Writer
	color bool
}

// NewLogger returns a logger writing to the error stream of streams. Color is
// used only when the caller did not pass --no-color and that stream is a
// terminal.
func NewLogger(streams genericiooptions.IOStreams, noColor bool) *Logger {
	return &Logger{
		out:   streams.ErrOut,
		color: !noColor && isTerminal(streams.ErrOut),
	}
}

// isTerminal reports whether w is a terminal.
func isTerminal(w io.Writer) bool {
	f, ok := w.(fdWriter)
	if !ok {
		return false
	}

	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// Infof reports progress.
func (l *Logger) Infof(format string, a ...any) {
	l.write("", format, a...)
}

// Warnf reports something the user should look at but that does not stop the run.
func (l *Logger) Warnf(format string, a ...any) {
	l.write(colorYellow, format, a...)
}

// Errorf reports something that went wrong.
func (l *Logger) Errorf(format string, a ...any) {
	l.write(colorRed, format, a...)
}

// write paints the message when color is on and puts one line on the error
// stream. A write that fails is dropped: progress output is not worth failing a
// run over, and the caller has no better stream to be told about it on.
func (l *Logger) write(color, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if l.color && color != "" {
		msg = color + msg + colorReset
	}

	fmt.Fprintln(l.out, msg)
}
