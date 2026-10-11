package mustgather

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

// stubFactory hands out whatever the test asks for, including a failure.
type stubFactory struct {
	clientset kubernetes.Interface
	dynamic   dynamic.Interface
	config    *rest.Config
	err       error
}

func (f stubFactory) KubernetesClient() (kubernetes.Interface, error) {
	return f.clientset, f.err
}
func (f stubFactory) DynamicClient() (dynamic.Interface, error) { return f.dynamic, f.err }
func (f stubFactory) RESTConfig() (*rest.Config, error)         { return f.config, f.err }

func TestNewGatherer(t *testing.T) {
	working := stubFactory{clientset: fake.NewSimpleClientset(), config: &rest.Config{}}

	t.Run("a client it cannot build is reported", func(t *testing.T) {
		opts := &Options{ArtifactDir: t.TempDir()}
		broken := stubFactory{err: errors.New("no cluster today")}

		g, err := NewGatherer(opts, broken, discardProgress{})
		if err == nil {
			g.Close()
			t.Fatal("expected an error when the factory cannot build a client")
		}
		if !errors.Is(err, broken.err) {
			t.Errorf("error = %v, want it to wrap %v", err, broken.err)
		}
	})

	t.Run("an artifact directory it cannot create is reported", func(t *testing.T) {
		badDir := filepath.Join(t.TempDir(), "notexist", string([]byte{0}))
		opts := &Options{ArtifactDir: badDir}

		if _, err := NewGatherer(opts, working, discardProgress{}); err == nil {
			t.Error("expected an error for a bad artifact dir")
		}
	})

	t.Run("the pod file fetcher seam is carried over", func(t *testing.T) {
		called := false
		opts := &Options{
			ArtifactDir:  t.TempDir(),
			FetchPodFile: func(context.Context, *v1.Pod, string) error { called = true; return nil },
		}

		g, err := NewGatherer(opts, working, discardProgress{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		defer g.Close()

		if err := g.fetchBugReport(context.Background(), &v1.Pod{}, filepath.Join(opts.ArtifactDir, "report")); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !called {
			t.Error("the gatherer did not use the fetcher from the options")
		}
	})
}

func TestWriteStringToFile(t *testing.T) {
	f, err := os.CreateTemp("", "testfile")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(f.Name()) })
	err = writeStringToFile(f, "hello")
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	f.Close()
	// Test error case: closed file
	f2, _ := os.CreateTemp("", "testfile2")
	f2.Close()
	err = writeStringToFile(f2, "fail")
	if err == nil {
		t.Error("expected error writing to closed file")
	}
}
