//go:build darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd

package sqlite

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func writerLeasePlatformSupported() error { return nil }

func writerLeaseVerifyPlatform(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("writer lease has no native file identity")
	}
	if stat.Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("writer lease owner is uid %d, want %d", stat.Uid, os.Geteuid())
	}
	if stat.Nlink != 1 {
		return fmt.Errorf("writer lease has %d hard links, want 1", stat.Nlink)
	}
	return nil
}

func writerLeaseTryLock(file *os.File, exclusive bool) (bool, error) {
	mode := syscall.LOCK_SH
	if exclusive {
		mode = syscall.LOCK_EX
	}
	for {
		err := syscall.Flock(int(file.Fd()), mode|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
			return false, nil
		default:
			return false, err
		}
	}
}

func writerLeaseUnlock(file *os.File) error {
	if file == nil {
		return nil
	}
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
