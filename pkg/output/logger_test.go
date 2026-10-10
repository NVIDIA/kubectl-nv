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
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// alwaysTerminal and neverTerminal stand in for the terminal check so that the
// color decision can be exercised without a pty.
func alwaysTerminal(_ io.Writer) bool { return true }

func neverTerminal(_ io.Writer) bool { return false }

func TestLoggerWritesToTheErrorStreamOnly(t *testing.T) {
	t.Parallel()

	streams, _, out, errOut := genericiooptions.NewTestIOStreams()
	logger := NewLogger(streams, LoggerOptions{NoColor: true})

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

func TestLoggerColorDecision(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		opts      LoggerOptions
		wantPaint bool
	}{
		{
			name:      "a terminal with color allowed",
			opts:      LoggerOptions{IsTerminal: alwaysTerminal},
			wantPaint: true,
		},
		{
			// The case the review called out: a real terminal plus --no-color
			// still has to come out clean, or the flag does nothing.
			name:      "a terminal with no-color",
			opts:      LoggerOptions{NoColor: true, IsTerminal: alwaysTerminal},
			wantPaint: false,
		},
		{
			name:      "not a terminal",
			opts:      LoggerOptions{IsTerminal: neverTerminal},
			wantPaint: false,
		},
		{
			name:      "not a terminal and no-color",
			opts:      LoggerOptions{NoColor: true, IsTerminal: neverTerminal},
			wantPaint: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			streams, _, _, errOut := genericiooptions.NewTestIOStreams()
			logger := NewLogger(streams, tc.opts)

			logger.Warnf("careful")
			logger.Errorf("broken")
			logger.Infof("plain")

			got := errOut.String()
			if tc.wantPaint {
				if !strings.Contains(got, "\033[33mcareful\033[0m") {
					t.Errorf("warning is not yellow: %q", got)
				}
				if !strings.Contains(got, "\033[31mbroken\033[0m") {
					t.Errorf("error is not red: %q", got)
				}
				// Progress carries no color at all, not even a bare reset:
				// the whole line has to come out as plain text.
				lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
				if last := lines[len(lines)-1]; last != "plain" {
					t.Errorf("progress line = %q, want %q with no escape at all", last, "plain")
				}

				return
			}

			if strings.Contains(got, "\033[") {
				t.Errorf("stderr carries an escape sequence: %q", got)
			}
			for _, want := range []string{"careful", "broken", "plain"} {
				if !strings.Contains(got, want) {
					t.Errorf("stderr %q lost %q along with the color", got, want)
				}
			}
		})
	}
}

// NO_COLOR is the convention every other CLI on the box honours.
func TestLoggerHonoursNoColorEnvironment(t *testing.T) {
	t.Setenv("NO_COLOR", "1")

	streams, _, _, errOut := genericiooptions.NewTestIOStreams()
	logger := NewLogger(streams, LoggerOptions{IsTerminal: alwaysTerminal})

	logger.Warnf("careful")

	if strings.Contains(errOut.String(), "\033[") {
		t.Errorf("NO_COLOR was ignored: %q", errOut.String())
	}
	if !strings.Contains(errOut.String(), "careful") {
		t.Errorf("stderr %q lost the message", errOut.String())
	}
}

func TestLoggerEmptyNoColorEnvironmentDoesNotDisableColor(t *testing.T) {
	t.Setenv("NO_COLOR", "")

	streams, _, _, errOut := genericiooptions.NewTestIOStreams()
	logger := NewLogger(streams, LoggerOptions{IsTerminal: alwaysTerminal})

	logger.Warnf("careful")

	if !strings.Contains(errOut.String(), "\033[33m") {
		t.Errorf("an empty NO_COLOR disabled color: %q", errOut.String())
	}
}

// Node names, taints and container logs come from the cluster. A control
// character in one of them must not reach the terminal, or anything that can
// write a node name can move the cursor and repaint the screen.
func TestLoggerStripsControlCharacters(t *testing.T) {
	t.Parallel()

	streams, _, _, errOut := genericiooptions.NewTestIOStreams()
	logger := NewLogger(streams, LoggerOptions{NoColor: true})

	logger.Warnf("node %s is tainted", "bad\033[31mname\x00end\x07")
	logger.Infof("column\there\nand a second line")

	got := errOut.String()
	if strings.ContainsAny(got, "\033\x00\x07") {
		t.Errorf("stderr still carries a control character: %q", got)
	}
	if !strings.Contains(got, "node bad[31mnameend is tainted\n") {
		t.Errorf("stderr = %q, want the text with only the control bytes removed", got)
	}
	// A tab and a newline are how a terminal lays text out, so they stay.
	if !strings.Contains(got, "column\there\nand a second line\n") {
		t.Errorf("stderr = %q, want the tab and the newline kept", got)
	}
}

// Progress is written from whatever goroutine noticed something, so the writes
// have to be serialised: without the lock this test is red under -race.
func TestLoggerIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	streams, _, _, errOut := genericiooptions.NewTestIOStreams()
	logger := NewLogger(streams, LoggerOptions{IsTerminal: alwaysTerminal})

	const goroutines = 8
	const perGoroutine = 25

	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := range goroutines {
		go func() {
			defer wg.Done()

			for i := range perGoroutine {
				switch i % 3 {
				case 0:
					logger.Infof("g%d i%d", g, i)
				case 1:
					logger.Warnf("g%d i%d", g, i)
				default:
					logger.Errorf("g%d i%d", g, i)
				}
			}
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimRight(errOut.String(), "\n"), "\n")
	if len(lines) != goroutines*perGoroutine {
		t.Errorf("stderr has %d lines, want %d: writes interleaved", len(lines), goroutines*perGoroutine)
	}

	// Every line has to have survived whole, not half of one and half of another.
	seen := make(map[string]bool, len(lines))
	for _, line := range lines {
		seen[line] = true
	}
	for g := range goroutines {
		for i := range perGoroutine {
			want := fmt.Sprintf("g%d i%d", g, i)
			if !containsLineWith(seen, want) {
				t.Errorf("stderr lost or mangled %q", want)
			}
		}
	}
}

// containsLineWith reports whether any seen line carries want, color and all.
func containsLineWith(seen map[string]bool, want string) bool {
	for line := range seen {
		if strings.Contains(line, want) {
			return true
		}
	}

	return false
}

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
}

func TestNewLoggerDefaultsToTheErrorStreamAndTheRealTerminalCheck(t *testing.T) {
	t.Parallel()

	var errOut bytes.Buffer
	logger := NewLogger(genericiooptions.IOStreams{Out: &bytes.Buffer{}, ErrOut: &errOut}, LoggerOptions{})

	if logger.out != &errOut {
		t.Error("logger does not write to the error stream")
	}
	// No IsTerminal was passed, so the real check runs and a buffer is not one.
	if logger.color {
		t.Error("logger enabled color for a buffer")
	}
}
