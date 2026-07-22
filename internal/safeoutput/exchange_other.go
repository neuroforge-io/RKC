//go:build !linux && !windows

package safeoutput

import "errors"

func exchangePaths(_, _ string) error {
	return errors.New("atomic pathname exchange is unavailable on this platform")
}

func renameNoReplacePath(_, _ string) error {
	return errAtomicNoReplaceUnavailable
}

func renameNoReplaceSupported() bool { return false }

func replacementHasNoMissingTargetWindow() bool { return false }

const replacementPlatformDescription = "unsupported platform: publication fails closed because atomic no-replace rename is unavailable"
