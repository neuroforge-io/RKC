//go:build linux

package resourceguard

import "syscall"

func currentSchedulingEnvelope(pid int) (schedulingEnvelope, error) {
	kernelPriority, err := syscall.Getpriority(syscall.PRIO_PROCESS, pid)
	if err != nil {
		return schedulingEnvelope{}, err
	}
	priority, _, errno := syscall.Syscall(syscall.SYS_IOPRIO_GET, 1, uintptr(pid), 0)
	if errno != 0 {
		return schedulingEnvelope{}, errno
	}
	// Linux exposes 20-nice through the raw getpriority syscall. syscall does
	// not apply libc's conversion, so normalize it before enforcing nice 19.
	return schedulingEnvelope{nice: 20 - kernelPriority, ioClass: int(priority >> 13)}, nil
}
