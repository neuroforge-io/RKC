//go:build windows

package safeoutput

import (
	"errors"
	"fmt"

	"golang.org/x/sys/windows"
)

func exchangePaths(_, _ string) error {
	return errors.New("atomic pathname exchange is unavailable on Windows")
}

// MoveFileExW on one volume does not request MOVEFILE_REPLACE_EXISTING or
// COPY_ALLOWED. A concurrent destination wins without being overwritten. The
// WRITE_THROUGH request follows Microsoft's directory-move guidance; it is not
// a substitute for POSIX directory fsync guarantees.
// https://learn.microsoft.com/en-us/windows/win32/fileio/moving-directories
func renameNoReplacePath(first, second string) error {
	from, err := windows.UTF16PtrFromString(first)
	if err != nil {
		return fmt.Errorf("encode source path: %w", err)
	}
	to, err := windows.UTF16PtrFromString(second)
	if err != nil {
		return fmt.Errorf("encode destination path: %w", err)
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_WRITE_THROUGH)
}

func renameNoReplaceSupported() bool { return true }

func replacementHasNoMissingTargetWindow() bool { return false }

const replacementPlatformDescription = "Windows MoveFileExW no-replace write-through publication: force replacement retains the prior output in private quarantine across a bounded target-missing window; directory-entry power-loss durability is weaker than Unix directory fsync"
