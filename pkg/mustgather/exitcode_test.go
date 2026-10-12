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
	"errors"
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// TestRunExitCodes separates the two failures a support engineer has to tell
// apart: a bundle that is missing some of what was asked for, which is still
// worth sending, and no bundle at all, which means nothing was learned about
// the cluster.
func TestRunExitCodes(t *testing.T) {
	cases := []struct {
		name     string
		failOn   string
		wantCode int
		wantFile string
	}{
		{
			// Nodes are the first thing collected, so failing here means the
			// bundle holds nothing about the cluster.
			name:     "nothing could be collected",
			failOn:   "nodes",
			wantCode: output.ExitError,
		},
		{
			// Nodes are already on disk by the time the pods fail.
			name:     "part of the cluster was collected",
			failOn:   "pods",
			wantCode: output.ExitPartial,
			wantFile: "nodes.yaml",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "artifacts")
			clientset := fake.NewSimpleClientset(gpuNode("gpu-node-1"))
			clientset.PrependReactor("list", c.failOn,
				func(k8stesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("the API server is having a bad day")
				})

			err := Run(&Options{ArtifactDir: dir},
				stubFactory{clientset: clientset, config: offlineConfig()},
				discardProgress{})
			if err == nil {
				t.Fatal("Run returned nil, want the collection failure")
			}
			if got := output.ExitCodeFor(err); got != c.wantCode {
				t.Errorf("exit code = %d, want %d (error: %v)", got, c.wantCode, err)
			}
			if c.wantFile != "" {
				if _, serr := os.Stat(filepath.Join(dir, c.wantFile)); serr != nil {
					t.Errorf("expected %s to have been collected before the failure: %v", c.wantFile, serr)
				}
			}
		})
	}
}

// TestRunCannotStart covers the failure before anything is written at all: no
// clients, so no bundle, so nothing can be concluded from the run.
func TestRunCannotStart(t *testing.T) {
	broken := stubFactory{err: errors.New("no cluster today")}

	err := Run(&Options{ArtifactDir: filepath.Join(t.TempDir(), "artifacts")}, broken, discardProgress{})
	if err == nil {
		t.Fatal("Run returned nil, want the client failure")
	}
	if got := output.ExitCodeFor(err); got != output.ExitError {
		t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitError, err)
	}
}

// discardProgress is a Progress that says nothing.
type discardProgress struct{}

func (discardProgress) Infof(string, ...any)  {}
func (discardProgress) Warnf(string, ...any)  {}
func (discardProgress) Errorf(string, ...any) {}
