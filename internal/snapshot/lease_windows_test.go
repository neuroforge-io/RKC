//go:build windows

package snapshot

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

func TestWindowsLeaseSurvivesRenameAndBlocksRecovery(t *testing.T) {
	root := t.TempDir()
	building := filepath.Join(root, "building")
	if err := os.Mkdir(building, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(building, "lease")
	lease, err := createTransactionLease(path)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if _, live, err := acquireAbandonedTransactionLease(path); err != nil || !live {
		t.Fatalf("active lease = live %t, %v", live, err)
	}
	published := filepath.Join(root, "published")
	if err := privatepath.Rename(building, published); err != nil {
		t.Fatalf("live lease prevented directory publication: %v", err)
	}
	path = filepath.Join(published, "lease")
	if err := lease.validate(path); err != nil {
		t.Fatalf("renamed lease lost identity: %v", err)
	}
	if _, live, err := acquireAbandonedTransactionLease(path); err != nil || !live {
		t.Fatalf("renamed active lease = live %t, %v", live, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, live, err := acquireAbandonedTransactionLease(path)
	if err != nil || live || recovered == nil {
		t.Fatalf("released lease = recovered %v, live %t, %v", recovered, live, err)
	}
	_ = recovered.Close()
}

func TestWindowsLeaseRejectsInvalidDescriptorAndPaths(t *testing.T) {
	file := os.NewFile(uintptr(1<<30), "invalid-rkc-lease")
	acquired, err := lockFileExclusive(file)
	if acquired || err == nil {
		t.Fatalf("invalid descriptor lock = %t, %v", acquired, err)
	}
	if _, err := openLeaseFile("bad\x00path", true); err == nil {
		t.Fatal("NUL lease path accepted")
	}
	if _, err := openLeaseFile(filepath.Join(t.TempDir(), "missing"), false); err == nil {
		t.Fatal("missing existing lease accepted")
	}
}
