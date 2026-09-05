package githubsource

import (
	"os"
	"path/filepath"
	"syscall"
)

func publishNoReplace(root *os.Root, from, to string) error {
	source, err := syscall.UTF16PtrFromString(filepath.Join(root.Name(), from))
	if err != nil {
		return err
	}
	target, err := syscall.UTF16PtrFromString(filepath.Join(root.Name(), to))
	if err != nil {
		return err
	}
	return syscall.MoveFile(source, target)
}
