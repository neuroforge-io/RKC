//go:build !linux && !windows

package safeoutput

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestUnsupportedPlatformFailsBeforeMovingTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "atlas")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalizeOwnedAtlasFixture(t, target, "old")
	if err := renameNoReplacePath(filepath.Join(root, "source"), target); !errors.Is(err, errAtomicNoReplaceUnavailable) {
		t.Fatalf("renameNoReplacePath() = %v", err)
	}
	transaction, err := Begin(target, "", true, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transaction.Staging, "new.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAtlasManifestFixture(t, transaction.Staging, "new")
	if err := transaction.Commit("new"); !errors.Is(err, errAtomicNoReplaceUnavailable) {
		t.Fatalf("Commit() = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "old.txt")); err != nil || string(data) != "preserve" {
		t.Fatalf("unsupported publication changed target: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(transaction.Staging, "new.txt")); err != nil {
		t.Fatalf("failed publication lost staging: %v", err)
	}
}
