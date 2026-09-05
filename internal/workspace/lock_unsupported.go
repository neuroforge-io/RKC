//go:build !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !windows

package workspace

import (
	"errors"
	"os"
)

func lockExclusive(_ *os.File) error {
	return errors.New("workspace writer locks are unsupported on this platform")
}

func lockShared(file *os.File) error          { return lockExclusive(file) }
func openLease(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDWR, 0) }
