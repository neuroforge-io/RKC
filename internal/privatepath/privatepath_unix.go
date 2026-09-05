//go:build !windows

package privatepath

import (
	"errors"
	"os"
)

// MkdirTemp creates a new owner-only directory, using os.MkdirTemp patterns.
func MkdirTemp(parent, pattern string) (string, error) { return os.MkdirTemp(parent, pattern) }

// CreateTemp atomically creates a new owner-only regular file.
func CreateTemp(parent, pattern string) (*os.File, error) { return os.CreateTemp(parent, pattern) }

func checkPrivate(path string, identity os.FileInfo, directory bool) error {
	if err := checkIdentity(path, identity, directory); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(identity, current) || current.Mode().Perm()&0o077 != 0 {
		return errors.New("path must be accessible only by its owner")
	}
	return nil
}

func syncOpenedDirectory(directory *os.File) error { return directory.Sync() }

// Rename has os.Rename semantics. Callers must already have authorized any
// replacement and must flush file data before calling it.
func Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }
