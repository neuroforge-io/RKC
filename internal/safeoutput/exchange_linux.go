//go:build linux

package safeoutput

import (
	"fmt"
	"runtime"
	"syscall"
	"unsafe"
)

// Linux RENAME_EXCHANGE atomically swaps two existing pathnames. The syscall
// numbers below are pinned to Linux's per-architecture UAPI tables; keeping the
// tiny wrapper here avoids adding a runtime dependency solely for one syscall.
// Unknown Linux architectures fail closed with ENOSYS.
const (
	atFDCWD         = -100
	renameNoReplace = 1
	renameExchange  = 2
)

var renameat2Numbers = map[string]uintptr{
	"386":      353,
	"amd64":    316,
	"arm":      382,
	"arm64":    276,
	"loong64":  276,
	"mips":     4351,
	"mips64":   5311,
	"mips64le": 5311,
	"mipsle":   4351,
	"ppc64":    357,
	"ppc64le":  357,
	"riscv64":  276,
	"s390x":    347,
}

func renameat2(first, second string, flags uintptr) error {
	number, supported := renameat2Numbers[runtime.GOARCH]
	if !supported {
		return syscall.ENOSYS
	}
	firstPointer, err := syscall.BytePtrFromString(first)
	if err != nil {
		return fmt.Errorf("encode first exchange path: %w", err)
	}
	secondPointer, err := syscall.BytePtrFromString(second)
	if err != nil {
		return fmt.Errorf("encode second exchange path: %w", err)
	}
	directoryFD := atFDCWD
	_, _, errno := syscall.Syscall6(
		number,
		uintptr(directoryFD), uintptr(unsafe.Pointer(firstPointer)),
		uintptr(directoryFD), uintptr(unsafe.Pointer(secondPointer)),
		flags, 0,
	)
	runtime.KeepAlive(firstPointer)
	runtime.KeepAlive(secondPointer)
	if errno != 0 {
		return errno
	}
	return nil
}

func exchangePaths(first, second string) error {
	return renameat2(first, second, renameExchange)
}

func renameNoReplacePath(first, second string) error {
	return renameat2(first, second, renameNoReplace)
}

func renameNoReplaceSupported() bool { return true }

func replacementHasNoMissingTargetWindow() bool { return true }

const replacementPlatformDescription = "Linux renameat2(RENAME_EXCHANGE): the target pathname remains continuously present"
