package cas

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

type actionAtEOFReader struct {
	payload []byte
	sent    bool
	action  func() error
}

func (reader *actionAtEOFReader) Read(buffer []byte) (int, error) {
	if !reader.sent {
		reader.sent = true
		return copy(buffer, reader.payload), nil
	}
	if reader.action != nil {
		action := reader.action
		reader.action = nil
		if err := action(); err != nil {
			return 0, err
		}
	}
	return 0, io.EOF
}

func onlyTemporaryObject(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	if len(entries) != 1 {
		return "", fmt.Errorf("temporary object count = %d, want 1", len(entries))
	}
	return filepath.Join(root, entries[0].Name()), nil
}

func TestCASCoverageOpenInputAndTopologyFailures(t *testing.T) {
	t.Run("relative root beneath removed working directory", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("removing the current directory is Unix-specific")
		}
		original, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		vanished := filepath.Join(t.TempDir(), "vanished")
		if err := os.Mkdir(vanished, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chdir(vanished); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chdir(original) })
		if err := os.Remove(vanished); err != nil {
			t.Fatal(err)
		}
		if _, err := Open("relative"); err == nil || !strings.Contains(err.Error(), "resolve CAS root") {
			t.Fatalf("Open(relative beneath removed cwd) = %v", err)
		}
		if err := os.Chdir(original); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("temporary layout is a file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "sha256"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".tmp"), []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root); err == nil || !strings.Contains(err.Error(), "temporary layout") {
			t.Fatalf("Open(file temporary layout) = %v", err)
		}
	})

	t.Run("object layout is a file", func(t *testing.T) {
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "sha256"), []byte("occupied"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(root); err == nil || !strings.Contains(err.Error(), "object layout") {
			t.Fatalf("Open(file object layout) = %v", err)
		}
	})

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(nil); err == nil || !strings.Contains(err.Error(), "reader is nil") {
		t.Fatalf("Put(nil) = %v", err)
	}
	if _, err := store.readBytes("bad", true, 1); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("readBytes(invalid digest) = %v, want ErrInvalidDigest", err)
	}
	validDigest := strings.Repeat("0", 64)
	var nilStore *Store
	if _, err := nilStore.verifyExisting(validDigest, -1); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("nil verifyExisting() = %v, want ErrStoreChanged", err)
	}
	if _, _, _, _, err := nilStore.openObject(validDigest); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("nil openObject() = %v, want ErrStoreChanged", err)
	}
	if _, err := store.verifyExistingInShard(validDigest, -1, filepath.Join(store.shaRoot, "00"), nil); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("verifyExistingInShard(nil identity) = %v, want ErrStoreChanged", err)
	}

	shardFile := filepath.Join(store.shaRoot, "00")
	if err := os.WriteFile(shardFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Verify(validDigest); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("Verify(file shard) = %v, want ErrStoreChanged", err)
	}
	if _, _, _, _, err := store.openObject(validDigest); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("openObject(file shard) = %v, want ErrStoreChanged", err)
	}
}

func TestCASCoveragePutRejectsTemporaryPathReplacement(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("replace the temporary pathname after the complete write")
	reader := &actionAtEOFReader{payload: payload}
	reader.action = func() error {
		path, err := onlyTemporaryObject(store.temporaryRoot)
		if err != nil {
			return err
		}
		if err := os.Rename(path, path+".retained"); err != nil {
			return err
		}
		return os.WriteFile(path, []byte("attacker replacement"), 0o600)
	}
	if _, err := store.Put(reader); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("Put(replaced temporary pathname) = %v, want ErrUnsafeObject", err)
	}
	if present, err := store.Has(DigestBytes(payload)); err != nil || present {
		t.Fatalf("Has(rejected payload) = %v, %v; want false, nil", present, err)
	}
}

func TestCASCoveragePutReportsClosedTemporaryDescriptor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("descriptor discovery uses Linux procfs")
	}
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
	if runtime.GOOS != "linux" {
		t.Skip("cross-device fixture uses Linux tmpfs")
	}
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
	rootIdentity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	shaIdentity, err := os.Lstat(shaRoot)
	if err != nil {
		t.Fatal(err)
	}
	tempIdentity, err := os.Lstat(temporaryRoot)
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
	if _, err := os.Lstat(filepath.Join(shaRoot, DigestBytes(payload)[:2], DigestBytes(payload)[2:])); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("cross-device failure published an object: %v", err)
	}
}

func TestCASCoveragePermissionFailures(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission-denial fixtures require an unprivileged Unix process")
	}

	t.Run("path inspection", func(t *testing.T) {
		store, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(store.shaRoot, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(store.shaRoot, 0o755) })
		if _, err := store.Path(strings.Repeat("1", 64)); err == nil || !strings.Contains(err.Error(), "inspect CAS shard") {
			t.Fatalf("Path(inaccessible layout) = %v", err)
		}
	})

	t.Run("temporary creation", func(t *testing.T) {
		store, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(store.temporaryRoot, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(store.temporaryRoot, 0o700) })
		if _, err := store.PutBytes([]byte("denied")); err == nil || !strings.Contains(err.Error(), "create temporary CAS object") {
			t.Fatalf("PutBytes(inaccessible temporary root) = %v", err)
		}
	})

	t.Run("shard creation", func(t *testing.T) {
		store, err := Open(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(store.shaRoot, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(store.shaRoot, 0o755) })
		if _, _, err := store.ensureShard("ab"); err == nil || !strings.Contains(err.Error(), "create CAS shard") {
			t.Fatalf("ensureShard(read-only layout) = %v", err)
		}
	})

	t.Run("directory creation and component inspection", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.Chmod(parent, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o700) })
		if _, err := ensureDirectory(filepath.Join(parent, "child"), 0o700); err == nil {
			t.Fatal("ensureDirectory beneath read-only parent succeeded")
		}
		if err := os.Chmod(parent, 0); err != nil {
			t.Fatal(err)
		}
		if err := rejectSymlinkComponents(filepath.Join(parent, "child")); err == nil {
			t.Fatal("rejectSymlinkComponents beneath inaccessible parent succeeded")
		}
	})

	t.Run("regular file open", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "unreadable")
		if err := os.WriteFile(path, []byte("payload"), 0); err != nil {
			t.Fatal(err)
		}
		if _, _, err := openStableRegular(path); err == nil {
			t.Fatal("openStableRegular(unreadable) succeeded")
		}
	})
}

func TestCASCoverageWalkRejectsSpecialObjectsAndPostWalkMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("FIFO fixture is Unix-specific")
	}
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

	other, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.PutBytes([]byte("post-walk mutation")); err != nil {
		t.Fatal(err)
	}
	originalTemporary := other.temporaryRoot + ".original"
	if err := other.Walk(func(ObjectInfo) error {
		if err := os.Rename(other.temporaryRoot, originalTemporary); err != nil {
			return err
		}
		return os.Mkdir(other.temporaryRoot, 0o700)
	}); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("Walk(post-walk layout mutation) = %v, want ErrStoreChanged", err)
	}

	broken, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(broken.temporaryRoot, broken.temporaryRoot+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := broken.Walk(func(ObjectInfo) error { return nil }); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("Walk(invalid initial layout) = %v, want ErrStoreChanged", err)
	}
}

func TestCASCoverageValidationAndSyncFailures(t *testing.T) {
	if err := rejectSymlinkComponents("."); err != nil {
		t.Fatalf("rejectSymlinkComponents(dot) = %v", err)
	}
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	moved := store.temporaryRoot + ".moved"
	if err := os.Rename(store.temporaryRoot, moved); err != nil {
		t.Fatal(err)
	}
	if err := store.validateShard(filepath.Join(store.shaRoot, "00"), store.shaIdentity); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("validateShard(invalid layout) = %v, want ErrStoreChanged", err)
	}
	if err := store.validateTemporaryFile(filepath.Join(moved, "missing"), store.tempIdentity); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("validateTemporaryFile(invalid layout) = %v, want ErrStoreChanged", err)
	}

	stable, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	shardPath, shardIdentity, err := stable.ensureShard("ab")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(shardPath, shardPath+".moved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(shardPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := stable.validateShard(shardPath, shardIdentity); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("validateShard(replaced shard) = %v, want ErrStoreChanged", err)
	}

	if runtime.GOOS == "linux" {
		procIdentity, err := os.Lstat("/proc")
		if err != nil {
			t.Fatal(err)
		}
		if err := syncDirectoryStable("/proc", procIdentity); err == nil {
			t.Fatal("syncDirectoryStable(/proc) succeeded, want filesystem sync error")
		}
		if err := syncDirectory("/proc"); err == nil {
			t.Fatal("syncDirectory(/proc) succeeded, want filesystem sync error")
		}
	}
}
