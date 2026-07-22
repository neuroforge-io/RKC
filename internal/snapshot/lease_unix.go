//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package snapshot

import (
	"errors"
	"os"
	"syscall"
)

func lockFileExclusive(file *os.File) (bool, error) {
	err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	return false, err
}
