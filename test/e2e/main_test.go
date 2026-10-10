//go:build e2e

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

// Package e2e exercises the built kubectl-nv binary against a live cluster, as
// a user runs it: a process with arguments, an exit code and two streams. It
// knows nothing about the packages the binary is built from, so a refactor that
// keeps the behaviour keeps these tests green, and a change to the exit codes or
// to the shape of the bundle breaks them.
//
// The suite needs a cluster. KUBECONFIG names it, KUBECTL_NV_BIN names the
// binary under test, and "make e2e-mokka" supplies both from a Mokka lane.
package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"testing"
	"time"
)

// commandTimeout bounds a single run of the binary. A gather over a lane takes
// seconds; this is long enough that a slow runner is not called a failure, and
// short enough that a wedged exec is reported as a timeout rather than as the
// whole suite running out of time with no output.
const commandTimeout = 10 * time.Minute

// allowSkipEnvVar lets a developer run "go test -tags e2e ./..." without a
// cluster. CI never sets it: a suite that silently skips is a suite that is
// not run, and the e2e job exists to catch exactly what the unit tests cannot.
const allowSkipEnvVar = "E2E_ALLOW_SKIP"

var (
	// kubeconfigPath is the cluster every test runs against.
	kubeconfigPath string
	// binaryPath is the kubectl-nv under test.
	binaryPath string
)

func TestMain(m *testing.M) {
	if err := resolveEnvironment(); err != nil {
		if os.Getenv(allowSkipEnvVar) == "1" {
			fmt.Fprintf(os.Stderr, "e2e: skipping, %v\n", err)
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "e2e: cannot run, %v\n", err)
		fmt.Fprintf(os.Stderr, "e2e: run \"make e2e-mokka\", or set %s=1 to skip instead of failing\n", allowSkipEnvVar)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// resolveEnvironment reads the two variables the suite is pointed at its
// subject with, and checks each names something usable before any test runs.
// Finding out inside the first test that the binary is a directory costs a
// confusing failure in a test that is about something else.
func resolveEnvironment() error {
	kubeconfigPath = os.Getenv("KUBECONFIG")
	if kubeconfigPath == "" {
		return errors.New("KUBECONFIG is not set; it must name the kubeconfig of the cluster to test against")
	}
	info, err := os.Stat(kubeconfigPath)
	if err != nil {
		return fmt.Errorf("KUBECONFIG=%q: %w", kubeconfigPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("KUBECONFIG=%q is a directory, want a kubeconfig file", kubeconfigPath)
	}

	binaryPath = os.Getenv("KUBECTL_NV_BIN")
	if binaryPath == "" {
		return errors.New("KUBECTL_NV_BIN is not set; it must name the kubectl-nv binary to test")
	}
	binInfo, err := os.Stat(binaryPath)
	if err != nil {
		return fmt.Errorf("KUBECTL_NV_BIN=%q: %w", binaryPath, err)
	}
	if binInfo.IsDir() || binInfo.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("KUBECTL_NV_BIN=%q is not an executable file (mode %04o)", binaryPath, binInfo.Mode().Perm())
	}

	return nil
}

// result is one run of the binary: what it put on each stream and what it
// exited with.
type result struct {
	stdout string
	stderr string
	code   int
}

// run executes the binary against the suite's cluster. The kubeconfig is
// passed as a flag rather than left to the environment, so that a test says
// which cluster it meant and a stray KUBECONFIG in the runner's shell cannot
// redirect it.
func run(t *testing.T, args ...string) result {
	t.Helper()
	return runWithKubeconfig(t, kubeconfigPath, args...)
}

// runWithKubeconfig executes the binary against the named kubeconfig, for the
// tests that are about what happens when it is not the lane's.
func runWithKubeconfig(t *testing.T, kubeconfig string, args ...string) result {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), commandTimeout)
	defer cancel()

	full := append([]string{"--kubeconfig", kubeconfig}, args...)
	cmd := exec.CommandContext(ctx, binaryPath, full...)
	// A plugin that asks a question in a pipeline hangs the pipeline. Closing
	// stdin turns that into an immediate failure instead of a timeout.
	cmd.Stdin = nil
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// NO_COLOR keeps the escape sequences out of the stream a test matches on,
	// whatever the runner's terminal is.
	cmd.Env = append(os.Environ(), "NO_COLOR=1")

	err := cmd.Run()
	if ctx.Err() != nil {
		t.Fatalf("%s %v did not finish within %s\nstderr:\n%s", binaryPath, full, commandTimeout, stderr.String())
	}

	code := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		code = exitErr.ExitCode()
	default:
		t.Fatalf("running %s %v: %v", binaryPath, full, err)
	}

	t.Logf("%s %v exited %d", binaryPath, full, code)

	return result{stdout: stdout.String(), stderr: stderr.String(), code: code}
}
