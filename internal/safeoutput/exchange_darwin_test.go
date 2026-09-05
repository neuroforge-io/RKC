//go:build darwin

package safeoutput

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDarwinPublicationPrimitivesPreserveDirectories(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	for path, content := range map[string]string{first: "first", second: "second"} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "content"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	firstIdentity, err := os.Lstat(first)
	if err != nil {
		t.Fatal(err)
	}
	secondIdentity, err := os.Lstat(second)
	if err != nil {
		t.Fatal(err)
	}
	assertDirectory := func(path, content string, identity os.FileInfo) {
		t.Helper()
		info, err := os.Lstat(path)
		if err != nil || !os.SameFile(identity, info) {
			t.Fatalf("directory identity at %s changed: %v", path, err)
		}
		data, err := os.ReadFile(filepath.Join(path, "content"))
		if err != nil || string(data) != content {
			t.Fatalf("content at %s = %q, %v", path, data, err)
		}
	}
	if err := renameNoReplacePath(first, second); !os.IsExist(err) {
		t.Fatalf("existing destination was not refused: %v", err)
	}
	assertDirectory(first, "first", firstIdentity)
	assertDirectory(second, "second", secondIdentity)
	if err := exchangePaths(first, second); err != nil {
		t.Fatal(err)
	}
	assertDirectory(first, "second", secondIdentity)
	assertDirectory(second, "first", firstIdentity)
	third := filepath.Join(root, "third")
	if err := renameNoReplacePath(first, third); err != nil {
		t.Fatal(err)
	}
	assertDirectory(third, "second", secondIdentity)
	if _, err := os.Lstat(first); !os.IsNotExist(err) {
		t.Fatalf("old name still exists after move: %v", err)
	}
	if err := exchangePaths(first, second); !os.IsNotExist(err) {
		t.Fatalf("exchange with missing source = %v", err)
	}
	assertDirectory(second, "first", firstIdentity)
}

func TestDarwinRenameRejectsInvalidPaths(t *testing.T) {
	for _, rename := range []func(string, string) error{renameNoReplacePath, exchangePaths} {
		if err := rename("bad\x00path", "destination"); err == nil {
			t.Fatal("accepted NUL source")
		}
		if err := rename("source", "bad\x00path"); err == nil {
			t.Fatal("accepted NUL destination")
		}
	}
}
