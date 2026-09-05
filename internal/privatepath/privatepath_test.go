package privatepath

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateObjectsAndIdentityBinding(t *testing.T) {
	parent := t.TempDir()
	root, err := MkdirTemp(parent, "private-*-data")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(root, identity); err != nil {
		t.Fatal(err)
	}
	file, err := CreateTemp(root, "secret-*.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("private content"); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	fileIdentity, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := CheckFile(file.Name(), fileIdentity); err != nil {
		t.Fatal(err)
	}
	if err := CheckDir(file.Name(), fileIdentity); err == nil {
		t.Fatal("file accepted as private directory")
	}
	if err := CheckFile(root, identity); err == nil {
		t.Fatal("directory accepted as private file")
	}
	if err := CheckDir(root, nil); err == nil {
		t.Fatal("missing bound identity accepted")
	}
	if err := SyncDirectoryStable(root, identity); err != nil {
		t.Fatal(err)
	}
	if err := SyncDirectory(root); err != nil {
		t.Fatal(err)
	}
	if err := SyncDirectory(file.Name()); err == nil {
		t.Fatal("file accepted for directory synchronization")
	}
	if err := SyncDirectory(filepath.Join(root, "missing")); !os.IsNotExist(err) {
		t.Fatalf("missing directory error = %v", err)
	}
	moved := filepath.Join(parent, "moved")
	if err := Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, check := range []func(string, os.FileInfo) error{CheckDir, SyncDirectoryStable} {
		if err := check(root, identity); err == nil {
			t.Fatal("substitute directory accepted for original identity")
		}
	}
	if err := CheckDir(moved, identity); err != nil {
		t.Fatalf("exact moved directory rejected: %v", err)
	}
	if err := CheckFile(file.Name(), fileIdentity); !os.IsNotExist(err) {
		t.Fatalf("missing private file error = %v", err)
	}
}

func TestPrivateCreationAndRenameFailures(t *testing.T) {
	root := t.TempDir()
	if _, err := MkdirTemp(root, "../escape"); err == nil {
		t.Fatal("unsafe directory pattern accepted")
	}
	if file, err := CreateTemp(root, "../escape"); err == nil {
		_ = file.Close()
		t.Fatal("unsafe file pattern accepted")
	}
	missing := filepath.Join(root, "missing", "child")
	if _, err := MkdirTemp(missing, "new"); err == nil {
		t.Fatal("missing parent accepted")
	}
	if file, err := CreateTemp(missing, "new"); err == nil {
		_ = file.Close()
		t.Fatal("missing file parent accepted")
	}
	if err := Rename(missing, filepath.Join(root, "target")); err == nil {
		t.Fatal("missing rename source accepted")
	}
	for _, pair := range [][2]string{{"bad\x00path", "destination"}, {"source", "bad\x00path"}} {
		if err := Rename(pair[0], pair[1]); err == nil {
			t.Fatal("NUL rename path accepted")
		}
	}
}

func TestRenameReplacesOnlyAuthorizedFile(t *testing.T) {
	root := t.TempDir()
	first, second := filepath.Join(root, "first"), filepath.Join(root, "second")
	if err := os.WriteFile(first, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Rename(first, second); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(second)
	if err != nil || string(data) != "new" {
		t.Fatalf("replacement = %q, %v", data, err)
	}
}

func TestLstatBindsIdentityBeforeAnyComparisonOrRename(t *testing.T) {
	parent := t.TempDir()
	root, err := MkdirTemp(parent, "bound-")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	retained := root + "-retained"
	// No SameFile call is allowed before this adversarial rename: the original
	// Windows lazy metadata bug was masked whenever a previous check loaded IDs.
	if err := Rename(root, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	replacement, err := Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(identity, replacement) {
		t.Fatal("captured identity silently rebound to a replacement pathname")
	}
	if err := CheckDir(root, identity); err == nil {
		t.Fatal("replacement accepted as original directory")
	}
	if err := CheckDir(retained, identity); err != nil {
		t.Fatalf("retained original identity lost: %v", err)
	}
}
