//go:build !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd

package sqlite

import (
	"errors"
	"os"
)

var errWriterLeaseUnsupported = errors.New("sqlite writer leases are unsupported on this platform")

func writerLeasePlatformSupported() error { return errWriterLeaseUnsupported }

func writerLeaseVerifyPlatform(_ *os.File) error { return errWriterLeaseUnsupported }

func writerLeaseTryLock(_ *os.File, _ bool) (bool, error) {
	return false, errWriterLeaseUnsupported
}

func writerLeaseUnlock(_ *os.File) error { return nil }
