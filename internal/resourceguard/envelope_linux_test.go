//go:build linux

package resourceguard

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestLinuxThreadIDsAreCompleteSortedAndStrict(t *testing.T) {
	procRoot := t.TempDir()
	taskRoot := filepath.Join(procRoot, "42", "task")
	for _, name := range []string{"99", "42", "77"} {
		if err := os.MkdirAll(filepath.Join(taskRoot, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(taskRoot, "noise"), []byte("ignored non-directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	threadIDs, err := linuxThreadIDs(procRoot, 42)
	if err != nil || !reflect.DeepEqual(threadIDs, []int{42, 77, 99}) {
		t.Fatalf("thread IDs = %#v, %v", threadIDs, err)
	}
	if err := os.Mkdir(filepath.Join(taskRoot, "invalid"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := linuxThreadIDs(procRoot, 42); err == nil {
		t.Fatal("invalid task directory was accepted")
	}
	if _, err := linuxThreadIDs(procRoot, 404); err == nil {
		t.Fatal("missing task directory was accepted")
	}
}
