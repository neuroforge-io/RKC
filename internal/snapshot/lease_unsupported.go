//go:build !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd

package snapshot

import (
	"errors"
	"os"
)

func lockFileExclusive(_ *os.File) (bool, error) {
	return false, errors.New("snapshot transaction leases are unsupported on this platform")
}
