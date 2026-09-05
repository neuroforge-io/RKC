//go:build windows

package snapshot

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsSnapshotIDsRemainPublicAndUseBoundedLocalNames(t *testing.T) {
	store := openTestStore(t)
	ids := []string{"rkc:snapshot:0123456789abcdef", "RKC:snapshot:0123456789abcdef", "CON", strings.Repeat("x", 255)}
	seen := map[string]bool{}
	for _, id := range ids {
		name := snapshotDirectoryName(id)
		if !validSnapshotDirectoryName(name) || len(name) != 73 || seen[name] {
			t.Fatalf("invalid or colliding Windows name for %q: %q", id, name)
		}
		seen[name] = true
		transaction, err := store.Begin(id, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := transaction.WriteBundle(testBundle(id, "fixture")); err != nil {
			t.Fatal(err)
		}
		if err := transaction.Commit(); err != nil {
			t.Fatal(err)
		}
		if transaction.dir != filepath.Join(store.Root(), "snapshots", name) {
			t.Fatalf("wrong local snapshot path: %s", transaction.dir)
		}
		current, err := store.CurrentID()
		if err != nil || current != id {
			t.Fatalf("CURRENT rewrote public ID: %q, %v", current, err)
		}
		bundle, _, record, err := store.Load(id)
		if err != nil || bundle.Snapshot.ID != id || record.SnapshotID != id {
			t.Fatalf("Load rewrote public identity: %+v, %v", record, err)
		}
		if _, err := store.Begin(id, nil); !errors.Is(err, ErrSnapshotExists) {
			t.Fatalf("duplicate public ID accepted: %v", err)
		}
	}
	records, err := store.List()
	if err != nil || len(records) != len(ids) {
		t.Fatalf("List = %d records, %v", len(records), err)
	}
	for _, record := range records {
		if !seen[snapshotDirectoryName(record.SnapshotID)] {
			t.Fatalf("List returned unknown public ID %q", record.SnapshotID)
		}
	}
	// Moving a valid record beneath a different valid hash name must not grant it
	// the second public identity during Load or List.
	from := filepath.Join(store.Root(), "snapshots", snapshotDirectoryName(ids[0]))
	to := filepath.Join(store.Root(), "snapshots", snapshotDirectoryName("another-id"))
	if err := os.Rename(from, to); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err == nil {
		t.Fatal("List accepted a record under another identity's directory name")
	}
	if _, _, _, err := store.Load("another-id"); err == nil {
		t.Fatal("Load accepted a record under another public ID")
	}
}

func TestWindowsRejectsNoncanonicalSnapshotDirectoryNames(t *testing.T) {
	for _, name := range []string{"", "rkc:snapshot:abc", "CON", "snapshot-" + strings.Repeat("A", 64), "snapshot-" + strings.Repeat("g", 64), "snapshot-" + strings.Repeat("a", 63), "../snapshot-" + strings.Repeat("a", 64)} {
		if validSnapshotDirectoryName(name) {
			t.Fatalf("unsafe or noncanonical snapshot directory accepted: %q", name)
		}
	}
}
