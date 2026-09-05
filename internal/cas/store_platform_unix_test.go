//go:build !windows

package cas

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestCASCoverageWalkRejectsSpecialObjects(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shard := filepath.Join(store.shaRoot, "aa")
	if err := os.Mkdir(shard, 0o755); err != nil {
		t.Fatal(err)
	}
	object := filepath.Join(shard, strings.Repeat("0", 62))
	if err := syscall.Mkfifo(object, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Walk(func(ObjectInfo) error { return nil }); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("Walk(FIFO object) = %v, want ErrUnsafeObject", err)
	}

}
