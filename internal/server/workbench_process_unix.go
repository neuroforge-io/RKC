//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package server

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const (
	workbenchTerminationGrace = 2 * time.Second
	workbenchTerminationBound = 2 * time.Second
)

func configureWorkbenchProcess(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateWorkbenchProcess(command *exec.Cmd, completed <-chan error) error {
	if command == nil || command.Process == nil {
		return ErrWorkbenchCleanupUnproven
	}
	pid := command.Process.Pid
	_ = signalWorkbenchProcessGroup(pid, syscall.SIGTERM)
	reaped := false
	grace := time.NewTimer(workbenchTerminationGrace)
	defer grace.Stop()
	select {
	case <-completed:
		reaped = true
	case <-grace.C:
	}
	if workbenchProcessAlive(-pid) {
		_ = signalWorkbenchProcessGroup(pid, syscall.SIGKILL)
	}
	deadline := time.NewTimer(workbenchTerminationBound)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		groupGone := !workbenchProcessAlive(-pid)
		if reaped && groupGone {
			return nil
		}
		select {
		case <-completed:
			reaped = true
		case <-ticker.C:
		case <-deadline.C:
			return ErrWorkbenchCleanupUnproven
		}
	}
}

func signalWorkbenchProcessGroup(pid int, signal syscall.Signal) error {
	err := syscall.Kill(-pid, signal)
	if err == nil || errors.Is(err, syscall.ESRCH) {
		return nil
	}
	directErr := syscall.Kill(pid, signal)
	if directErr == nil || errors.Is(directErr, syscall.ESRCH) || errors.Is(directErr, os.ErrProcessDone) {
		return err
	}
	return errors.Join(err, directErr)
}

func workbenchProcessGroupsSupported() bool {
	return true
}

func workbenchCleanupScope() string {
	return "process_group"
}

func workbenchProcessAlive(pid int) bool {
	if pid == 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func validateWorkbenchUserManagerEndpoint(runtimeDirectory string) error {
	runtimeInfo, err := os.Lstat(runtimeDirectory)
	if err != nil {
		return err
	}
	runtimeStat, ok := runtimeInfo.Sys().(*syscall.Stat_t)
	if !ok || int(runtimeStat.Uid) != os.Geteuid() {
		return errors.New("XDG_RUNTIME_DIR is not owned by the current user")
	}
	busInfo, err := os.Lstat(runtimeDirectory + string(os.PathSeparator) + "bus")
	if err != nil || busInfo.Mode()&os.ModeSocket == 0 || busInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("user bus is not a direct Unix socket")
	}
	busStat, ok := busInfo.Sys().(*syscall.Stat_t)
	if !ok || int(busStat.Uid) != os.Geteuid() {
		return errors.New("user bus is not owned by the current user")
	}
	return nil
}
