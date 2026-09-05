// Package privatepath provides identity-bound private filesystem objects.
// Unix permissions and Windows protected current-user ACLs have separate
// implementations; callers must never infer Windows privacy from Unix mode bits.
package privatepath

import (
	"errors"
	"os"
)

// CheckDir verifies an exact real directory remains private to its owner.
// Existing paths are checked, never repaired or granted additional authority.
func CheckDir(path string, identity os.FileInfo) error {
	return checkPrivate(path, identity, true)
}

// CheckFile verifies an exact regular file remains private to its owner.
func CheckFile(path string, identity os.FileInfo) error {
	return checkPrivate(path, identity, false)
}

func checkIdentity(path string, identity os.FileInfo, directory bool) error {
	current, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if identity == nil || current.IsDir() != directory ||
		(!directory && !current.Mode().IsRegular()) ||
		current.Mode()&os.ModeSymlink != 0 || !os.SameFile(identity, current) {
		return errors.New("private path identity or type changed")
	}
	return nil
}

// SyncDirectory requests the platform's available directory durability after
// verifying its identity. On Unix this fsyncs the directory. Windows has no
// equivalent supported unprivileged directory flush: it validates the directory
// only, while Rename requests write-through moves and callers flush file data.
// Windows callers must not claim Unix directory-entry power-loss guarantees.
func SyncDirectory(path string) error {
	identity, err := os.Lstat(path)
	if err != nil {
		return err
	}
	return SyncDirectoryStable(path, identity)
}

// SyncDirectoryStable applies SyncDirectory's platform contract to an already
// bound directory identity. Missing, replaced, symlinked and non-directory paths
// fail even on platforms without a directory flush primitive.
func SyncDirectoryStable(path string, identity os.FileInfo) error {
	if err := checkIdentity(path, identity, true); err != nil {
		return err
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil {
		return err
	}
	if !opened.IsDir() || !os.SameFile(identity, opened) {
		return errors.New("directory identity changed while opening it")
	}
	if err := syncOpenedDirectory(directory); err != nil {
		return err
	}
	return checkIdentity(path, identity, true)
}
