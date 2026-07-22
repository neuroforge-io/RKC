//go:build windows

package safeoutput

import (
	"errors"
	"fmt"
	"syscall"
)

func exchangePaths(_, _ string) error {
	return errors.New("atomic pathname exchange is unavailable on Windows")
}

// MoveFileW atomically renames on one Windows volume and, unlike os.Rename on
// Windows, does not request MOVEFILE_REPLACE_EXISTING. A concurrently-created
// destination therefore wins without being overwritten.
func renameNoReplacePath(first, second string) error {
	from, err := syscall.UTF16PtrFromString(first)
	if err != nil {
		return fmt.Errorf("encode source path: %w", err)
	}
	to, err := syscall.UTF16PtrFromString(second)
	if err != nil {
		return fmt.Errorf("encode destination path: %w", err)
	}
	return syscall.MoveFile(from, to)
}

func renameNoReplaceSupported() bool { return true }

func replacementHasNoMissingTargetWindow() bool { return false }

const replacementPlatformDescription = "Windows MoveFileW no-replace publication: force replacement retains the prior output in quarantine across a bounded target-missing window"
