//go:build windows

package safeoutput

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsRenameNoReplaceNeverClobbersDestination(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	destination := filepath.Join(root, "destination")
	if err := os.WriteFile(source, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("destination"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplacePath(source, destination); err == nil {
		t.Fatal("renameNoReplacePath replaced an existing destination")
	}
	if data, err := os.ReadFile(source); err != nil || string(data) != "source" {
		t.Fatalf("source after refused rename = %q, %v", data, err)
	}
	if data, err := os.ReadFile(destination); err != nil || string(data) != "destination" {
		t.Fatalf("destination after refused rename = %q, %v", data, err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := renameNoReplacePath(source, destination); err != nil {
		t.Fatalf("renameNoReplacePath(missing destination) = %v", err)
	}
	if data, err := os.ReadFile(destination); err != nil || string(data) != "source" {
		t.Fatalf("published destination = %q, %v", data, err)
	}
}
