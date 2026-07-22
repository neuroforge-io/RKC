package cas

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePrimitiveSafetyFailures(t *testing.T) {
	var nilStore *Store
	if err := nilStore.validateLayout(); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("nil validateLayout() = %v", err)
	}
	if _, _, err := nilStore.ensureShard("00"); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("nil ensureShard() = %v", err)
	}
	if _, _, err := (&Store{}).ensureShard("not-a-shard"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("ensureShard(invalid) = %v", err)
	}

	root := t.TempDir()
	store, err := Open(filepath.Join(root, "objects"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.validateShard(filepath.Join(store.shaRoot, "00"), nil); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("validateShard(nil identity) = %v", err)
	}
	if err := store.validateTemporaryFile(filepath.Join(store.temporaryRoot, "missing"), store.tempIdentity); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("validateTemporaryFile(missing) = %v", err)
	}
	if _, err := store.OpenObject("bad"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("OpenObject(invalid) = %v", err)
	}
	if err := store.Verify("bad"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("Verify(invalid) = %v", err)
	}
	if err := store.Walk(nil); err == nil {
		t.Fatal("Walk(nil) succeeded")
	}

	shardPath, shardIdentity, err := store.ensureShard("00")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.validateShard(shardPath, shardIdentity); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ensureShard("00"); err != nil {
		t.Fatalf("ensureShard(existing) = %v", err)
	}
	if err := os.Remove(shardPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shardPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ensureShard("00"); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("ensureShard(file) = %v", err)
	}
}

func TestFilesystemHelpersRejectIdentitySubstitution(t *testing.T) {
	base := t.TempDir()
	directory := filepath.Join(base, "directory")
	identity, err := ensureDirectory(directory, 0o700)
	if err != nil || !identity.IsDir() {
		t.Fatalf("ensureDirectory(new) = %v, %v", identity, err)
	}
	if _, err := ensureDirectory(directory, 0o700); err != nil {
		t.Fatalf("ensureDirectory(existing) = %v", err)
	}
	plain := filepath.Join(base, "plain")
	if err := os.WriteFile(plain, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureDirectory(plain, 0o700); err == nil {
		t.Fatal("ensureDirectory(file) succeeded")
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Fatal(err)
	}
	if err := rejectSymlinkComponents(filepath.Join(alias, "child")); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("rejectSymlinkComponents(alias) = %v", err)
	}
	if err := rejectSymlinkComponents(filepath.Join(base, "missing", "child")); err != nil {
		t.Fatalf("rejectSymlinkComponents(missing suffix) = %v", err)
	}

	if _, _, err := openStableRegular(filepath.Join(base, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("openStableRegular(missing) = %v", err)
	}
	if _, _, err := openStableRegular(directory); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("openStableRegular(directory) = %v", err)
	}
	opened, openedIdentity, err := openStableRegular(plain)
	if err != nil {
		t.Fatal(err)
	}
	if err := opened.Close(); err != nil {
		t.Fatal(err)
	}
	if openedIdentity.Size() != 7 {
		t.Fatalf("opened size = %d", openedIdentity.Size())
	}

	if err := removeExactRegularFile(plain, nil); err == nil {
		t.Fatal("removeExactRegularFile(nil identity) succeeded")
	}
	if err := removeExactRegularFile(filepath.Join(base, "missing"), openedIdentity); err != nil {
		t.Fatalf("removeExactRegularFile(missing) = %v", err)
	}
	if err := removeExactRegularFile(directory, openedIdentity); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("removeExactRegularFile(wrong path) = %v", err)
	}
	if err := removeExactRegularFile(plain, openedIdentity); err != nil {
		t.Fatalf("removeExactRegularFile(exact) = %v", err)
	}

	if validShardName("") || validShardName("A0") || validShardName("gg") || !validShardName("af") {
		t.Fatal("validShardName accepted or rejected an incorrect value")
	}
	if err := syncDirectoryStable(directory, nil); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("syncDirectoryStable(nil) = %v", err)
	}
	if err := syncDirectoryStable(filepath.Join(base, "missing"), identity); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("syncDirectoryStable(missing) = %v", err)
	}
	other, err := os.Lstat(base)
	if err != nil {
		t.Fatal(err)
	}
	if err := syncDirectoryStable(directory, other); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("syncDirectoryStable(wrong identity) = %v", err)
	}
	if err := syncDirectoryStable(directory, identity); err != nil {
		t.Fatalf("syncDirectoryStable(valid) = %v", err)
	}
	if err := syncDirectory(directory); err != nil {
		t.Fatalf("syncDirectory(valid) = %v", err)
	}
}

func TestWalkFiltersMetadataAndPropagatesCallbackFailure(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(store.shaRoot, "not-a-shard"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.shaRoot, "README"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := store.PutBytes([]byte("walk me"))
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("stop walking")
	if err := store.Walk(func(item ObjectInfo) error {
		if item.Digest != info.Digest || item.Size != info.Size || !strings.HasSuffix(item.Path, info.Digest[2:]) {
			t.Fatalf("Walk item = %+v", item)
		}
		return want
	}); !errors.Is(err, want) {
		t.Fatalf("Walk(callback error) = %v", err)
	}

	validShard := filepath.Join(store.shaRoot, "aa")
	if err := os.Symlink(t.TempDir(), validShard); err != nil {
		t.Fatal(err)
	}
	if err := store.Walk(func(ObjectInfo) error { return nil }); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("Walk(valid shard symlink) = %v", err)
	}
}

func TestValidateReadDetectsLengthAndPathReplacement(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.PutBytes([]byte("stable"))
	if err != nil {
		t.Fatal(err)
	}
	file, path, identity, shard, err := store.openObject(object.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := store.validateRead(path, file, identity, shard, identity.Size()+1); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("validateRead(wrong byte count) = %v", err)
	}
	retained := path + ".retained"
	if err := os.Rename(path, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("stable"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := store.validateRead(path, file, identity, shard, identity.Size()); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("validateRead(replaced pathname) = %v", err)
	}
}
