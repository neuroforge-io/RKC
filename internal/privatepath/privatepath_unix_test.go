//go:build !windows

package privatepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUnixPrivacyIsRecheckedWithoutRepair(t *testing.T) {
	root, err := MkdirTemp(t.TempDir(), "private-")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(root, identity); err == nil {
		t.Fatal("public permissions accepted using stale private mode bits")
	}
	current, _ := os.Lstat(root)
	if current.Mode().Perm() != 0o755 {
		t.Fatal("privacy check modified permissions")
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(link, identity); err == nil {
		t.Fatal("symlink accepted")
	}
	if err := SyncDirectory(link); err == nil {
		t.Fatal("directory synchronization followed symlink")
	}
}

func TestUnixDirectoryOpenFailuresRemainVisible(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can open directories without permission bits")
	}
	root, err := MkdirTemp(t.TempDir(), "private-")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	if err := SyncDirectoryStable(root, identity); !os.IsPermission(err) {
		t.Fatalf("directory synchronization hid an access error: %v", err)
	}
}
