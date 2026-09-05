//go:build linux

package cas

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

func TestCASCoveragePutReportsClosedTemporaryDescriptor(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	reader := &actionAtEOFReader{payload: []byte("close the writer descriptor")}
	reader.action = func() error {
		path, err := onlyTemporaryObject(store.temporaryRoot)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			return err
		}
		for _, entry := range entries {
			target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
			if err != nil || target != path {
				continue
			}
			var descriptor int
			if _, err := fmt.Sscanf(entry.Name(), "%d", &descriptor); err != nil {
				return err
			}
			return syscall.Close(descriptor)
		}
		return errors.New("temporary descriptor was not found")
	}
	if _, err := store.Put(reader); err == nil || !strings.Contains(err.Error(), "protect temporary CAS object") {
		t.Fatalf("Put(closed descriptor) = %v", err)
	}
}

func TestCASCoveragePutCrossDevicePublicationFailsClosed(t *testing.T) {
	root := t.TempDir()
	temporaryRoot := filepath.Join(root, ".tmp")
	if err := os.Mkdir(temporaryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	shaRoot, err := os.MkdirTemp("/dev/shm", "rkc-cas-coverage-")
	if err != nil {
		t.Skipf("tmpfs unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(shaRoot) })
	rootIdentity, err := privatepath.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	shaIdentity, err := privatepath.Lstat(shaRoot)
	if err != nil {
		t.Fatal(err)
	}
	tempIdentity, err := privatepath.Lstat(temporaryRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootStat, rootOK := rootIdentity.Sys().(*syscall.Stat_t)
	shaStat, shaOK := shaIdentity.Sys().(*syscall.Stat_t)
	if !rootOK || !shaOK || rootStat.Dev == shaStat.Dev {
		t.Skip("fixture roots are not on distinct devices")
	}
	store := &Store{
		root: root, shaRoot: shaRoot, temporaryRoot: temporaryRoot,
		rootIdentity: rootIdentity, shaIdentity: shaIdentity, tempIdentity: tempIdentity,
	}
	payload := []byte("cross-device publication")
	if _, err := store.PutBytes(payload); err == nil || !strings.Contains(err.Error(), "install CAS object") {
		t.Fatalf("PutBytes(cross-device) = %v", err)
	}
	if _, err := privatepath.Lstat(filepath.Join(shaRoot, DigestBytes(payload)[:2], DigestBytes(payload)[2:])); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cross-device failure published an object: %v", err)
	}
}
