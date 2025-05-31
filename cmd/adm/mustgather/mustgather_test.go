package mustgather

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/NVIDIA/kubectl-nv/internal/logger"
)

func TestNewCommand(t *testing.T) {
	log := logger.NewLogger()
	cmd := NewCommand(log)
	if cmd == nil {
		t.Fatal("expected non-nil command")
	}
	if cmd.Name != "must-gather" {
		t.Errorf("expected command name 'must-gather', got '%s'", cmd.Name)
	}
	if len(cmd.Flags) == 0 {
		t.Error("expected flags to be set")
	}
}

func TestNewGatherer(t *testing.T) {
	dir := t.TempDir()
	opts := &Options{ArtifactDir: dir, Kubeconfig: "/dev/null"}
	g, err := NewGatherer(opts)
	if err == nil {
		g.Close()
	}
	// Should fail due to invalid kubeconfig
	if err == nil {
		t.Error("expected error for invalid kubeconfig")
	}

	// Valid directory, but log file creation error
	badDir := filepath.Join(dir, "notexist", string([]byte{0}))
	opts2 := &Options{ArtifactDir: badDir, Kubeconfig: "/dev/null"}
	_, err2 := NewGatherer(opts2)
	if err2 == nil {
		t.Error("expected error for bad artifact dir")
	}
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
