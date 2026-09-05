//go:build darwin

package safeoutput

import "golang.org/x/sys/unix"

// Darwin's renamex_np provides both operations on supporting filesystems. There
// is intentionally no check-then-rename fallback when the filesystem refuses
// either flag: a concurrent destination must never be overwritten.
// https://github.com/apple-oss-distributions/xnu/blob/main/bsd/man/man2/rename.2
func exchangePaths(first, second string) error {
	return unix.RenamexNp(first, second, unix.RENAME_SWAP)
}

func renameNoReplacePath(first, second string) error {
	return unix.RenamexNp(first, second, unix.RENAME_EXCL)
}

func renameNoReplaceSupported() bool { return true }

func replacementHasNoMissingTargetWindow() bool { return true }

const replacementPlatformDescription = "macOS renamex_np(RENAME_SWAP): the target pathname remains continuously present; unsupported filesystems fail closed"
