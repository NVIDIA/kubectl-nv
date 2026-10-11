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
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"

	"github.com/NVIDIA/kubectl-nv/pkg/output"
)

// forbiddenExecBody is what a real API server puts in the body of a 403 on the
// exec subresource. The SPDY upgrade never completes, so the client never gets
// a typed Status back: it gets this message inside a plain error, which is
// exactly why the classification cannot be apierrors.IsForbidden alone.
const forbiddenExecBody = `{"kind":"Status","apiVersion":"v1","status":"Failure",` +
	`"message":"pods \"nvidia-driver-daemonset-abcde\" is forbidden: User \"system:serviceaccount:support:reader\" ` +
	`cannot create resource \"pods/exec\" in API group \"\" in the namespace \"gpu-operator\"",` +
	`"reason":"Forbidden","code":403}`

// TestForbiddenExecThroughTheRealExecPath is the gap a seam-only test cannot
// see. Every unit test so far injected an apierrors.NewForbidden through the
// fetcher seam, and the live path never produces one: the SPDY upgrade fails
// before any Status is decoded, and the error reaches the collector as plain
// text. A gather against a cluster that denies pods/exec therefore exited 0
// and reported the driver evidence as merely absent.
func TestForbiddenExecThroughTheRealExecPath(t *testing.T) {
	var path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(forbiddenExecBody))
	}))
	t.Cleanup(server.Close)

	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "nvidia-driver-daemonset-abcde", Namespace: "gpu-operator"}}

	_, _, err := executeRemoteCommand(context.Background(), &rest.Config{Host: server.URL},
		pod, "nvidia-driver-ctr", "true")
	if err == nil {
		t.Fatal("executeRemoteCommand returned nil against a server that answers 403")
	}
	if !strings.Contains(path, "/pods/nvidia-driver-daemonset-abcde/exec") {
		t.Errorf("the request went to %q, want the exec subresource of the pod", path)
	}
	if !forbiddenExec(err) {
		t.Errorf("forbiddenExec(%v) = false, want the denial recognised", err)
	}
}

// TestForbiddenExecClassification covers the shapes the denial arrives in, and
// just as importantly the ones it must not swallow: a gather that calls every
// exec failure "forbidden" reports a broken driver pod as an RBAC problem and
// sends the user to their cluster administrator for nothing.
func TestForbiddenExecClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil is not a denial", err: nil, want: false},
		{
			name: "a typed Forbidden",
			err:  apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "driver", errors.New("nope")),
			want: true,
		},
		{
			name: "the upgrade failure a real server produces",
			err: errors.New(`unable to upgrade connection: pods "driver" is forbidden: User "u" ` +
				`cannot create resource "pods/exec" in API group "" in the namespace "gpu-operator"`),
			want: true,
		},
		{
			name: "a wrapped upgrade failure",
			err: fmt.Errorf("collecting bug report from pod driver: %w",
				errors.New(`unable to upgrade connection: pods "driver" is forbidden: cannot create resource "pods/exec"`)),
			want: true,
		},
		{
			name: "the command exited non-zero",
			err:  errors.New("command terminated with exit code 1"),
			want: false,
		},
		{
			name: "the pod went away",
			err:  apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "driver"),
			want: false,
		},
		{
			name: "the connection broke",
			err:  errors.New("unable to upgrade connection: dial tcp 10.0.0.1:443: connect: connection refused"),
			want: false,
		},
		{
			// "forbidden" can appear in a message that is not about RBAC at all.
			name: "an unrelated message carrying the word",
			err:  errors.New("the driver reported: operation forbidden by the device"),
			want: false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := forbiddenExec(c.err); got != c.want {
				t.Errorf("forbiddenExec(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestForbiddenExecExitsPartialThroughTheLivePath is the end to end shape of
// the first test: the denial has to survive from the exec all the way to the
// process exit code and the manifest, with both exec collectors named.
func TestForbiddenExecExitsPartialThroughTheLivePath(t *testing.T) {
	denied := errors.New(`unable to upgrade connection: pods "driver-1" is forbidden: ` +
		`cannot create resource "pods/exec" in API group "" in the namespace "gpu-operator"`)

	opts := &Options{
		FetchPodFile: func(context.Context, *v1.Pod, string) error { return denied },
		RunInPod:     func(context.Context, *v1.Pod, string, string) (string, error) { return "", denied },
	}
	dir, names, err := runGather(t, opts, twoNodeCluster()...)
	if err == nil {
		t.Fatal("Run returned nil, want the denied exec reported")
	}
	if got := output.ExitCodeFor(err); got != output.ExitPartial {
		t.Errorf("exit code = %d, want %d (error: %v)", got, output.ExitPartial, err)
	}
	for _, want := range []string{"bug-report", "device-usage"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name the skipped collector %s:\n%v", want, err)
		}
	}
	for _, name := range names {
		if strings.HasPrefix(name, "nvidia-bug-report_") {
			t.Errorf("bundle holds %s, but every exec was denied", name)
		}
	}

	m := readManifest(t, dir)
	// device-usage still gets an entry. A collector that is missing from the
	// manifest is one a reader cannot tell apart from a collector that does
	// not exist.
	for _, collector := range []string{"bug-report", "device-usage"} {
		if status := m.statusOf(t, collector); !strings.HasPrefix(status, "skipped:forbidden") {
			t.Errorf("%s status = %q, want it to start with skipped:forbidden", collector, status)
		}
	}
}

// TestExecTimeoutIsRecordedPerPod covers a driver pod that accepts the exec and
// then never answers. Without a deadline the gather blocks on it forever and
// the user never gets the bundle that the rest of the cluster would have filled
// in; the pod that hung has to be recorded and the gather has to carry on.
func TestExecTimeoutIsRecordedPerPod(t *testing.T) {
	started := make(chan struct{}, 4)

	opts := &Options{
		ExecTimeout: 50 * time.Millisecond,
		FetchPodFile: func(ctx context.Context, _ *v1.Pod, _ string) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		},
		RunInPod: func(ctx context.Context, _ *v1.Pod, _, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		},
	}

	dir, _, err := runGather(t, opts, twoNodeCluster()...)
	if err == nil {
		t.Fatal("Run returned nil, want the hung pods reported")
	}
	if len(started) == 0 {
		t.Fatal("the fetcher was never called")
	}

	m := readManifest(t, dir)
	status := m.statusOf(t, "bug-report")
	want := fmt.Sprintf("error:timeout after %s", 50*time.Millisecond)
	if !strings.Contains(status, want) {
		t.Errorf("bug-report status = %q, want it to carry %q", status, want)
	}

	// The gather carried on: the collectors that do not exec still wrote their
	// artifacts, and the manifest that records all of this exists at all.
	if len(m.Files) == 0 {
		t.Error("the manifest indexes no files, so the gather did not carry on past the hung exec")
	}
}

// TestManifestIsWrittenAfterACollectorFails keeps the bundle index in a bundle
// whose gather ended badly. That is exactly the bundle whose reader most needs
// to know which collectors ran.
func TestManifestIsWrittenAfterACollectorFails(t *testing.T) {
	clientset := fake.NewSimpleClientset(twoNodeCluster()...)
	clientset.PrependReactor("list", "daemonsets", failWith(errors.New("the API server is having a bad day")))

	dir, names, err := runGatherWith(t, &Options{}, clientset)
	if err == nil {
		t.Fatal("Run returned nil, want the daemonset failure reported")
	}
	if !contains(names, ManifestFileName) {
		t.Fatalf("the bundle holds no %s after a collector failed: %v", ManifestFileName, names)
	}

	m := readManifest(t, dir)
	if status := m.statusOf(t, "daemonsets"); !strings.HasPrefix(status, "error:") {
		t.Errorf("daemonsets status = %q, want it to start with error:", status)
	}
}
