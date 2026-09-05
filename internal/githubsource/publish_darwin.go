package githubsource

import (
	"golang.org/x/sys/unix"
	"os"
)

func publishNoReplace(root *os.Root, from, to string) error {
	directory, err := root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return unix.RenameatxNp(int(directory.Fd()), from, int(directory.Fd()), to, unix.RENAME_EXCL)
}
