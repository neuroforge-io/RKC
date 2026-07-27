//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package server

import (
	"errors"
	"os"
	"os/exec"
	"time"
)

func configureWorkbenchProcess(_ *exec.Cmd) {}

func terminateWorkbenchProcess(command *exec.Cmd, completed <-chan error) error {
	if command == nil || command.Process == nil {
		return ErrWorkbenchCleanupUnproven
	}
	_ = command.Process.Signal(os.Interrupt)
	grace := time.NewTimer(250 * time.Millisecond)
	select {
	case <-completed:
		grace.Stop()
		// The direct process was reaped, but this platform has no supported
		// process-tree containment proof.
		return ErrWorkbenchCleanupUnproven
	case <-grace.C:
	}
	err := command.Process.Kill()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return ErrWorkbenchCleanupUnproven
	}
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-completed:
	case <-timer.C:
	}
	// Killing the direct process is a safe fallback, not proof that descendants
	// were contained or reaped.
	return ErrWorkbenchCleanupUnproven
}

func workbenchProcessGroupsSupported() bool {
	return false
}

func workbenchCleanupScope() string {
	return "direct_process"
}

func workbenchProcessAlive(_ int) bool {
	return false
}

func validateWorkbenchUserManagerEndpoint(_ string) error {
	return errors.New("user-systemd workbench transport is unsupported on this platform")
}
