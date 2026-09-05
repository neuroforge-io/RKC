//go:build windows

package safeoutput

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

func TestWindowsPersistentIdentitySurvivesRenameAndRejectsSubstitution(t *testing.T) {
	root, err := privatepath.MkdirTemp(t.TempDir(), "identity-")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	before, err := persistentPathIdentityToken(root, identity)
	if err != nil || before == "" {
		t.Fatalf("capture original identity = %q, %v", before, err)
	}
	moved := root + "-moved"
	if err := renameNoReplacePath(root, moved); err != nil {
		t.Fatal(err)
	}
	after, err := persistentPathIdentityToken(moved, identity)
	if err != nil || after != before {
		t.Fatalf("identity after rename = %q, %v", after, err)
	}
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := persistentPathIdentityToken(root, identity); err == nil {
		t.Fatal("substitute directory accepted for captured identity")
	}
	if _, err := persistentPathIdentityToken(moved, nil); err == nil {
		t.Fatal("missing directory identity accepted")
	}
	if _, err := persistentPathIdentityToken(filepath.Join(root, "missing"), identity); err == nil {
		t.Fatal("missing path accepted")
	}
}

func TestWindowsCompletePublicationReplacementAndAbort(t *testing.T) {
	target := filepath.Join(t.TempDir(), "atlas")
	for index, snapshot := range []string{"first", "replacement"} {
		transaction, err := Begin(target, "", index > 0, "atlas")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(transaction.Staging, "content"), []byte(snapshot), 0o600); err != nil {
			t.Fatal(err)
		}
		writeAtlasManifestFixture(t, transaction.Staging, snapshot)
		if err := transaction.Commit(snapshot); err != nil {
			t.Fatalf("publish %s: %v", snapshot, err)
		}
		data, err := os.ReadFile(filepath.Join(target, "content"))
		if err != nil || string(data) != snapshot {
			t.Fatalf("published content = %q, %v", data, err)
		}
	}
	transaction, err := Begin(target, "", true, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(transaction.Staging); !os.IsNotExist(err) {
		t.Fatalf("aborted staging remains: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "content")); err != nil || string(data) != "replacement" {
		t.Fatalf("Abort changed prior output = %q, %v", data, err)
	}
}
