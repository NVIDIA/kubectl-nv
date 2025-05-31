package adm

import (
	"testing"

	"github.com/NVIDIA/kubectl-nv/internal/logger"
)

func TestNewCommand(t *testing.T) {
	log := logger.NewLogger()
	cmd := NewCommand(log)
	if cmd == nil {
		t.Fatal("expected non-nil command")
	}
	if cmd.Name != "adm" {
		t.Errorf("expected command name 'adm', got '%s'", cmd.Name)
	}
	if len(cmd.Subcommands) == 0 {
		t.Error("expected at least one subcommand")
	}
}
