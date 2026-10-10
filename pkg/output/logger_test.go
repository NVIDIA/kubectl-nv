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
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"
)

func TestLoggerWritesToTheErrorStreamOnly(t *testing.T) {
	t.Parallel()

	streams, _, out, errOut := genericiooptions.NewTestIOStreams()
	logger := NewLogger(streams, true)

	logger.Infof("scanning %d nodes", 3)
	logger.Warnf("node %s denied exec", "node-a")
	logger.Errorf("listing nodes: %v", os.ErrPermission)

	if out.Len() != 0 {
		t.Errorf("stdout received %q, want nothing", out.String())
	}

	got := errOut.String()
	for _, want := range []string{"scanning 3 nodes", "node node-a denied exec", "listing nodes: permission denied"} {
		if !strings.Contains(got, want) {
			t.Errorf("stderr %q is missing %q", got, want)
		}
	}
	if n := strings.Count(got, "\n"); n != 3 {
		t.Errorf("stderr has %d lines, want 3:\n%s", n, got)
	}
}

func TestLoggerOmitsColorWhenDisabledOrNotATerminal(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		noColor bool
	}{
		{name: "no-color requested", noColor: true},
		{name: "not a terminal", noColor: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			streams, _, _, errOut := genericiooptions.NewTestIOStreams()
			logger := NewLogger(streams, tc.noColor)

			logger.Warnf("careful")
			logger.Errorf("broken")

			if strings.Contains(errOut.String(), "\033[") {
				t.Errorf("stderr carries an escape sequence: %q", errOut.String())
			}
		})
	}
}

func TestLoggerPaintsWhenColorIsOn(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	logger := &Logger{out: &buf, color: true}

	logger.Warnf("careful")
	logger.Errorf("broken")
	logger.Infof("plain")

	got := buf.String()
	if !strings.Contains(got, "\033[33mcareful\033[0m") {
		t.Errorf("warning is not yellow: %q", got)
	}
	if !strings.Contains(got, "\033[31mbroken\033[0m") {
		t.Errorf("error is not red: %q", got)
	}
	if strings.Contains(got, "\033[33mplain") || strings.Contains(got, "\033[31mplain") {
		t.Errorf("progress output is painted: %q", got)
	}
}

// fakeFdWriter reports a file descriptor without being a terminal.
type fakeFdWriter struct {
	bytes.Buffer
	fd uintptr
}

func (w *fakeFdWriter) Fd() uintptr { return w.fd }

func TestIsTerminal(t *testing.T) {
	t.Parallel()

	if isTerminal(&bytes.Buffer{}) {
		t.Error("isTerminal(*bytes.Buffer) = true, want false: a buffer has no descriptor")
	}

	// A regular file does have a descriptor, so only a real terminal check can
	// tell it apart from a terminal.
	f, err := os.Create(filepath.Join(t.TempDir(), "not-a-tty"))
	if err != nil {
		t.Fatalf("os.Create: %v", err)
	}
	defer f.Close()

	if isTerminal(f) {
		t.Error("isTerminal(regular file) = true, want false")
	}
	if isTerminal(&fakeFdWriter{fd: f.Fd()}) {
		t.Error("isTerminal(fd of a regular file) = true, want false")
	}
}

func TestNewLoggerDefaultsToTheErrorStream(t *testing.T) {
	t.Parallel()

	var errOut bytes.Buffer
	logger := NewLogger(genericiooptions.IOStreams{Out: &bytes.Buffer{}, ErrOut: &errOut}, false)

	if logger.out != &errOut {
		t.Error("logger does not write to the error stream")
	}
	if logger.color {
		t.Error("logger enabled color for a buffer")
	}
}
