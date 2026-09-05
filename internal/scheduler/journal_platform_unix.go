//go:build !windows

package scheduler

import (
	"errors"
	"os"
)

func createJournalRoot(path string) error {
	return os.MkdirAll(path, 0o700)
}

func createJournalFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|os.O_EXCL, 0o600)
}

func secureJournalRoot(path string, identity os.FileInfo) error {
	return validateJournalRootPrivacy(path, identity)
}

func secureJournalFile(path string, identity os.FileInfo) error {
	return validateJournalFilePrivacy(path, identity)
}

func validateJournalRootPrivacy(_ string, identity os.FileInfo) error {
	if identity == nil || identity.Mode().Perm()&0o077 != 0 {
		return errors.New("scheduler journal root must be accessible only by its owner")
	}
	return nil
}

func validateJournalFilePrivacy(_ string, identity os.FileInfo) error {
	if identity == nil || identity.Mode().Perm()&0o077 != 0 {
		return errors.New("scheduler journal file must be accessible only by its owner")
	}
	return nil
}

func syncStableJournalDirectory(path string, identity os.FileInfo) error {
	return syncStableCacheDirectory(path, identity)
}
