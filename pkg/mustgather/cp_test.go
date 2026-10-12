package mustgather

import (
	"path/filepath"
	"reflect"
	"testing"

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
