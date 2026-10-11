package mustgather

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

func TestNewCopyOptions(t *testing.T) {
	ioStreams := genericiooptions.IOStreams{}
	opt := NewCopyOptions(ioStreams)
	if opt == nil {
		t.Fatal("expected non-nil CopyOptions")
	}
	if opt.IOStreams != ioStreams {
		t.Error("IOStreams not set correctly")
	}
}

func TestExtractFileSpec(t *testing.T) {
	cases := []struct {
		name    string
		arg     string
		want    fileSpec
		wantErr bool
	}{
		{"local path", "/tmp/foo", fileSpec{File: newLocalPath("/tmp/foo")}, false},
		{"pod:file", "pod1:/etc/passwd", fileSpec{PodName: "pod1", File: newRemotePath("/etc/passwd")}, false},
		{"ns/pod:file", "ns1/pod2:/etc/passwd", fileSpec{PodNamespace: "ns1", PodName: "pod2", File: newRemotePath("/etc/passwd")}, false},
		{"bad format", ":/etc/passwd", fileSpec{}, true},
		{"too many slashes", "a/b/c:/foo", fileSpec{}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := extractFileSpec(c.arg)
			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, wantErr %v", err, c.wantErr)
			}
			if !c.wantErr && !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %+v, want %+v", got, c.want)
			}
		})
	}
}

func TestLocalPathHelpers(t *testing.T) {
	p := newLocalPath("/tmp/foo/bar.txt")
	if p.Dir().String() != filepath.Dir("/tmp/foo/bar.txt") {
		t.Error("Dir() failed")
	}
	if p.Base().String() != filepath.Base("/tmp/foo/bar.txt") {
		t.Error("Base() failed")
	}
	if p.Clean().String() != filepath.Clean("/tmp/foo/bar.txt") {
		t.Error("Clean() failed")
	}
	joined := p.Join(newLocalPath("baz.txt"))
	if joined.String() != filepath.Join("/tmp/foo/bar.txt", "baz.txt") {
		t.Error("Join() failed")
	}
}

func TestRemotePathHelpers(t *testing.T) {
	p := newRemotePath("/foo/bar.txt")
	if p.Dir().String() != "/foo" {
		t.Error("Dir() failed")
	}
	if p.Base().String() != "bar.txt" {
		t.Error("Base() failed")
	}
	if p.Clean().String() != "/foo/bar.txt" {
		t.Error("Clean() failed")
	}
	joined := p.Join(newRemotePath("baz.txt"))
	if joined.String() != "/foo/bar.txt/baz.txt" {
		t.Error("Join() failed")
	}
}

func TestIsRelative(t *testing.T) {
	base := newLocalPath("/tmp/foo")
	target := newLocalPath("/tmp/foo")
	if !isRelative(base, target) {
		t.Error("expected same path to be relative")
	}
	if isRelative(base, newLocalPath("/etc")) {
		t.Error("expected different path to not be relative")
	}
}

// TestCopiedFilesAreNotWorldReadable covers the one artifact that does not come
// from the gatherer's own writer: the bug report is copied out of a pod, and it
// used to be created with os.Create, which leaves it 0644 under the caller's
// umask. It is the most sensitive file in a bundle whose default home is /tmp,
// where every account on the machine can look, and every other file in that
// bundle is 0600.
func TestCopiedFilesAreNotWorldReadable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nvidia-bug-report_gpu-node-1.log.gz")

	file, err := createCopiedFile(path)
	if err != nil {
		t.Fatalf("createCopiedFile: %v", err)
	}
	if _, err := file.WriteString("report"); err != nil {
		t.Fatalf("writing the copied file: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("closing the copied file: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("the copied file is mode %04o, want 0600", got)
	}
}

// TestCopiedFilesAreTruncated keeps a second copy from leaving the tail of a
// longer first one behind it.
func TestCopiedFilesAreTruncated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report")
	if err := os.WriteFile(path, []byte("a much longer earlier report"), 0o600); err != nil {
		t.Fatal(err)
	}

	file, err := createCopiedFile(path)
	if err != nil {
		t.Fatalf("createCopiedFile: %v", err)
	}
	if _, err := file.WriteString("short"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "short" {
		t.Errorf("the file holds %q, want only what the second copy wrote", string(data))
	}
}

// TestCopyTargetsTheExecContainer pins the copy to the container the bug
// report was written in.
//
// The copy is a second exec, running tar. Leaving its container unset lets the
// API server pick the first one in spec order, which on a driver pod carrying
// a sidecar is not where nvidia-bug-report.sh just wrote its file: the report
// is written in one container and looked for in another, and the copy fails
// with "file not found" on a report that exists.
func TestCopyTargetsTheExecContainer(t *testing.T) {
	cases := []struct {
		name string
		pod  *v1.Pod
		want string
	}{
		{
			name: "the annotated container",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "nvidia-driver-daemonset-abcde",
					Annotations: map[string]string{defaultContainerAnnotation: "nvidia-driver-ctr"},
				},
				Spec: v1.PodSpec{Containers: []v1.Container{
					{Name: "openshift-driver-toolkit-ctr"},
					{Name: "nvidia-driver-ctr"},
				}},
			},
			want: "nvidia-driver-ctr",
		},
		{
			name: "the first container when there is no annotation",
			pod: &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "nvidia-driver-daemonset-abcde"},
				Spec: v1.PodSpec{Containers: []v1.Container{
					{Name: "nvidia-driver-ctr"},
					{Name: "sidecar"},
				}},
			},
			want: "nvidia-driver-ctr",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := copyOptionsFor(genericiooptions.IOStreams{}, c.pod)
			if o.Container != c.want {
				t.Errorf("the copy targets container %q, want %q", o.Container, c.want)
			}
			// The exec and the copy have to agree, or the two land in
			// different containers however either one is chosen.
			if got := execContainer(c.pod); got != o.Container {
				t.Errorf("the exec runs in %q and the copy in %q, want one container", got, o.Container)
			}
		})
	}
}
