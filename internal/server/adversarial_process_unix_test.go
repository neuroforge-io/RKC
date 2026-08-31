//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package server

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestWorkbenchUnixProcessBoundariesFailClosedWithoutSignalingLiveProcesses(t *testing.T) {
	if err := terminateWorkbenchProcess(nil, nil); !errors.Is(err, ErrWorkbenchCleanupUnproven) {
		t.Fatalf("nil process termination = %v", err)
	}
	if descendants, err := verifyWorkbenchProcessCompletion(nil); descendants || !errors.Is(err, ErrWorkbenchCleanupUnproven) {
		t.Fatalf("nil process verification = descendants:%t err:%v", descendants, err)
	}

	// Unix permits obtaining a handle for an absent PID without probing it. This
	// deliberately unreachable PID exercises cleanup/reaping mechanics without
	// sending a signal to a process that exists.
	const absentPID = 1 << 30
	absent, err := os.FindProcess(absentPID)
	if err != nil {
		t.Fatal(err)
	}
	command := &exec.Cmd{Process: absent}
	completed := make(chan error, 1)
	completed <- nil
	if err := terminateWorkbenchProcess(command, completed); err != nil {
		t.Fatalf("already-completed absent process termination = %v", err)
	}
	if descendants, err := verifyWorkbenchProcessCompletion(command); descendants || err != nil {
		t.Fatalf("absent process verification = descendants:%t err:%v", descendants, err)
	}
	if err := terminateWorkbenchProcessGroup(absentPID); err != nil {
		t.Fatalf("absent process-group termination = %v", err)
	}
	if !waitWorkbenchProcessGroupGone(absentPID, time.Millisecond) {
		t.Fatal("absent process group was reported alive")
	}
	if err := signalWorkbenchProcessGroup(absentPID, syscall.SIGTERM); err != nil {
		t.Fatalf("signaling absent process group = %v", err)
	}
	if workbenchProcessAlive(0) || !workbenchProcessAlive(os.Getpid()) {
		t.Fatal("process liveness boundary was misclassified")
	}

	// Checking liveness with signal zero is non-mutating. This proves that the
	// bounded wait expires for a live group without directing a signal at it.
	if waitWorkbenchProcessGroupGone(syscall.Getpgrp(), time.Millisecond) {
		t.Fatal("live current process group was reported gone")
	}

	// An unreported waiter is a cleanup-integrity failure even if the leader PID
	// no longer exists. The bounded 500ms wait is intentional and starts no work.
	if err := terminateWorkbenchProcess(command, nil); !errors.Is(err, ErrWorkbenchCleanupUnproven) {
		t.Fatalf("unreported process reaping = %v", err)
	}
}

func TestWorkbenchUnixUserBusEndpointRequiresDirectOwnedSocket(t *testing.T) {
	if err := validateWorkbenchUserManagerEndpoint(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing runtime directory was accepted")
	}

	runtimeDirectory := t.TempDir()
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkbenchUserManagerEndpoint(runtimeDirectory); err == nil || !strings.Contains(err.Error(), "direct Unix socket") {
		t.Fatalf("missing user bus error = %v", err)
	}
	busPath := filepath.Join(runtimeDirectory, "bus")
	if err := os.WriteFile(busPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := validateWorkbenchUserManagerEndpoint(runtimeDirectory); err == nil || !strings.Contains(err.Error(), "direct Unix socket") {
		t.Fatalf("regular-file user bus error = %v", err)
	}
	if err := os.Remove(busPath); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", busPath)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := validateWorkbenchUserManagerEndpoint(runtimeDirectory); err != nil {
		t.Fatalf("owned direct Unix socket rejected: %v", err)
	}
}
