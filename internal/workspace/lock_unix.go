//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package workspace

import (
	"os"
	"syscall"
)

func lockExclusive(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func lockShared(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
}
func openLease(path string) (*os.File, error) { return os.OpenFile(path, os.O_RDWR, 0) }
