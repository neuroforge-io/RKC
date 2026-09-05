//go:build !windows

package main

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/neuroforge-io/RKC/internal/safeoutput"
)

func TestSynthesisRejectsFIFO(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "dataset.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := synthesisDatasetIdentity(fifo); !errors.Is(err, safeoutput.ErrUnsafeTarget) {
		t.Fatalf("FIFO dataset identity = %v", err)
	}
}
