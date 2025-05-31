package logger

import (
	"bytes"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
)

func TestNewLogger(t *testing.T) {
	l := NewLogger()
	if l == nil {
		t.Fatal("expected non-nil logger")
	}
	if l.Out != os.Stderr {
		t.Error("expected default Out to be os.Stderr")
	}
}

func TestFunLogger_Info(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger()
	l.Out = &buf
	l.Info("hello %s", "world")
	if got := buf.String(); got != "hello world\n" {
		t.Errorf("got %q, want %q", got, "hello world\n")
	}
}

func TestFunLogger_Check_Warning_Error(t *testing.T) {
	l := NewLogger()
	l.Out = io.Discard
	l.Check("checked %d", 1)
	l.Warning("warn %d", 2)
	l.Error(errors.New("fail"))
	// visually inspect output or redirect os.Stdout/os.Stderr if needed
}

func TestFunLogger_Loading(t *testing.T) {
	l := NewLogger()
	l.Out = io.Discard
	l.Wg = &sync.WaitGroup{}
	l.Done = make(chan struct{})
	l.Fail = make(chan struct{})
	l.Wg.Add(1)
	go l.Loading("loading test")
	l.Done <- struct{}{}
	l.Wg.Wait()
}
