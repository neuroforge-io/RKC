package cas

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

type failingReader struct {
	data []byte
	done bool
}

func (reader *failingReader) Read(buffer []byte) (int, error) {
	if !reader.done {
		reader.done = true
		return copy(buffer, reader.data), nil
	}
	return 0, errors.New("injected read failure")
}

func TestDigestNormalizationAndPathSafety(t *testing.T) {
	payload := []byte("content-addressed")
	digest := DigestBytes(payload)
	if len(digest) != 64 {
		t.Fatalf("DigestBytes() length = %d, want 64", len(digest))
	}

	for _, input := range []string{digest, " SHA256:" + strings.ToUpper(digest) + " ", "sha256:" + digest} {
		normalized, err := NormalizeDigest(input)
		if err != nil {
			t.Fatalf("NormalizeDigest(%q): %v", input, err)
		}
		if normalized != digest {
			t.Fatalf("NormalizeDigest(%q) = %q, want %q", input, normalized, digest)
		}
	}

	for _, input := range []string{"", "abc", strings.Repeat("z", 64), "../" + digest, digest + "00"} {
		if _, err := NormalizeDigest(input); !errors.Is(err, ErrInvalidDigest) {
			t.Errorf("NormalizeDigest(%q) error = %v, want ErrInvalidDigest", input, err)
		}
	}

	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.Path("sha256:" + digest)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(store.Root(), "sha256", digest[:2], digest[2:])
	if path != want {
		t.Fatalf("Path() = %q, want %q", path, want)
	}
	if _, err := store.Path("../../etc/passwd"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("unsafe Path() error = %v, want ErrInvalidDigest", err)
	}
	if !filepath.IsAbs(store.Root()) {
		t.Fatalf("Root() = %q, want absolute path", store.Root())
	}
}

func TestOpenRejectsFileRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root-file")
	if err := os.WriteFile(root, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("Open(file) succeeded, want error")
	}

	realRoot := filepath.Join(t.TempDir(), "real-root")
	if err := os.Mkdir(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedRoot := filepath.Join(t.TempDir(), "linked-root")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(linkedRoot); err == nil {
		t.Fatal("Open(symlink root) succeeded, want error")
	}
	linkedParent := filepath.Join(t.TempDir(), "linked-parent")
	if err := os.Symlink(realRoot, linkedParent); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(filepath.Join(linkedParent, "nested")); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("Open(path beneath symlink) = %v, want ErrStoreChanged", err)
	}
}

func TestPutReadVerifyHasAndOpenObject(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("immutable payload")
	digest := DigestBytes(payload)

	if present, err := store.Has(digest); err != nil || present {
		t.Fatalf("Has(missing) = %v, %v; want false, nil", present, err)
	}
	if _, err := store.Has("not-a-digest"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("Has(invalid) error = %v, want ErrInvalidDigest", err)
	}

	info, err := store.PutBytes(payload)
	if err != nil {
		t.Fatal(err)
	}
	if info.Digest != digest || info.Size != int64(len(payload)) {
		t.Fatalf("PutBytes() = %+v", info)
	}
	if present, err := store.Has("SHA256:" + strings.ToUpper(digest)); err != nil || !present {
		t.Fatalf("Has(stored) = %v, %v; want true, nil", present, err)
	}

	file, err := store.OpenObject(digest)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := io.ReadAll(file)
	closeErr := file.Close()
	if err != nil || closeErr != nil || !reflect.DeepEqual(opened, payload) {
		t.Fatalf("OpenObject() bytes = %q, read err %v, close err %v", opened, err, closeErr)
	}
	read, err := store.ReadBytes(digest, true)
	if err != nil || !reflect.DeepEqual(read, payload) {
		t.Fatalf("ReadBytes(verify) = %q, %v", read, err)
	}
	if err := store.Verify(digest); err != nil {
		t.Fatalf("Verify() = %v", err)
	}

	again, err := store.Put(strings.NewReader(string(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if again != info {
		t.Fatalf("idempotent Put() = %+v, want %+v", again, info)
	}
	entries, err := os.ReadDir(filepath.Join(store.Root(), ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary objects remain after commit: %v", entries)
	}
}

func TestPutFailureRollsBackTemporaryObject(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(&failingReader{data: []byte("partial")}); err == nil || !strings.Contains(err.Error(), "injected read failure") {
		t.Fatalf("Put(failing reader) error = %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(store.Root(), ".tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary objects remain after rollback: %v", entries)
	}
	var count int
	if err := store.Walk(func(ObjectInfo) error { count++; return nil }); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("Walk() found %d objects after failed Put, want 0", count)
	}
}

func TestPutDetectsExistingSizeConflict(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("expected payload")
	path, err := store.Path(DigestBytes(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(payload); err == nil || !strings.Contains(err.Error(), "size conflict") {
		t.Fatalf("PutBytes() conflict error = %v", err)
	}
}

func TestPutHashesSameSizeExistingObject(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("expected payload")
	path, err := store.Path(DigestBytes(payload))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", len(payload))), 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PutBytes(payload); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("PutBytes() same-size corruption error = %v, want ErrDigestMismatch", err)
	}
	if present, err := store.Has(DigestBytes(payload)); present || !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Has() same-size corruption = %v, %v; want false, ErrDigestMismatch", present, err)
	}
}

func TestStoreRejectsRootAndLayoutReplacement(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "cas")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.PutBytes([]byte("root identity"))
	if err != nil {
		t.Fatal(err)
	}
	moved := root + "-moved"
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sha256"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, ".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Path(info.Digest); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("Path() after root replacement = %v, want ErrStoreChanged", err)
	}
	if _, err := store.PutBytes([]byte("must not escape")); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("PutBytes() after root replacement = %v, want ErrStoreChanged", err)
	}
	if _, err := store.ReadBytes(info.Digest, true); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("ReadBytes() after root replacement = %v, want ErrStoreChanged", err)
	}

	second, err := Open(filepath.Join(base, "second"))
	if err != nil {
		t.Fatal(err)
	}
	object, err := second.PutBytes([]byte("layout identity"))
	if err != nil {
		t.Fatal(err)
	}
	shaRoot := filepath.Join(second.Root(), "sha256")
	if err := os.Rename(shaRoot, shaRoot+"-moved"); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, shaRoot); err != nil {
		t.Fatal(err)
	}
	if _, err := second.ReadBytes(object.Digest, true); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("ReadBytes() through replacement layout = %v, want ErrStoreChanged", err)
	}
	if entries, err := os.ReadDir(external); err != nil || len(entries) != 0 {
		t.Fatalf("replacement layout target changed: entries=%v err=%v", entries, err)
	}
}

func TestStoreRejectsShardAndObjectSymlinks(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.PutBytes([]byte("shard identity"))
	if err != nil {
		t.Fatal(err)
	}
	shard := filepath.Dir(object.Path)
	if err := os.Rename(shard, shard+"-moved"); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	if err := os.Symlink(external, shard); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Path(object.Digest); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("Path() through shard symlink = %v, want ErrStoreChanged", err)
	}
	if _, err := store.ReadBytes(object.Digest, true); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("ReadBytes() through shard symlink = %v, want ErrStoreChanged", err)
	}
	if _, err := store.PutBytes([]byte("shard identity")); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("PutBytes() through shard symlink = %v, want ErrStoreChanged", err)
	}
	if err := store.Walk(func(ObjectInfo) error { return nil }); !errors.Is(err, ErrStoreChanged) {
		t.Fatalf("Walk() through shard symlink = %v, want ErrStoreChanged", err)
	}

	other, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("object symlink")
	digest := DigestBytes(payload)
	path, err := other.Path(digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, err := other.ReadBytes(digest, true); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("ReadBytes(object symlink) = %v, want ErrUnsafeObject", err)
	}
	if _, err := other.PutBytes(payload); !errors.Is(err, ErrUnsafeObject) {
		t.Fatalf("PutBytes(object symlink) = %v, want ErrUnsafeObject", err)
	}
}

func TestReadBytesBounded(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("bounded object")
	object, err := store.PutBytes(payload)
	if err != nil {
		t.Fatal(err)
	}
	data, err := store.ReadBytesBounded(object.Digest, true, int64(len(payload)))
	if err != nil || !reflect.DeepEqual(data, payload) {
		t.Fatalf("ReadBytesBounded(exact) = %q, %v", data, err)
	}
	if _, err := store.ReadBytesBounded(object.Digest, true, int64(len(payload)-1)); !errors.Is(err, ErrObjectTooLarge) {
		t.Fatalf("ReadBytesBounded(small) = %v, want ErrObjectTooLarge", err)
	}
	if _, err := store.ReadBytesBounded(object.Digest, true, 0); err == nil {
		t.Fatal("ReadBytesBounded(zero) succeeded, want error")
	}
}

func TestDeleteExactObjectIsIdempotent(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	object, err := store.PutBytes([]byte("prunable object"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(object.Digest); err != nil {
		t.Fatal(err)
	}
	if present, err := store.Has(object.Digest); err != nil || present {
		t.Fatalf("Has(deleted) = %t, %v", present, err)
	}
	if err := store.Delete(object.Digest); err != nil {
		t.Fatalf("repeated Delete() = %v", err)
	}
	if err := store.Delete("not-a-digest"); !errors.Is(err, ErrInvalidDigest) {
		t.Fatalf("Delete(invalid) = %v, want ErrInvalidDigest", err)
	}
}

func TestCorruptionDetectionAndUnverifiedRead(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.PutBytes([]byte("original"))
	if err != nil {
		t.Fatal(err)
	}
	corrupt := []byte("tampered")
	if err := os.Chmod(info.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(info.Path, corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := store.ReadBytes(info.Digest, false)
	if err != nil || !reflect.DeepEqual(read, corrupt) {
		t.Fatalf("ReadBytes(unverified) = %q, %v", read, err)
	}
	if _, err := store.ReadBytes(info.Digest, true); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("ReadBytes(verified) error = %v, want ErrDigestMismatch", err)
	}
	if err := store.Verify(info.Digest); !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("Verify(corrupt) error = %v, want ErrDigestMismatch", err)
	}
	if _, err := store.OpenObject(DigestBytes([]byte("missing"))); err == nil {
		t.Fatal("OpenObject(missing) succeeded, want error")
	}
}

func TestConcurrentPutAndWalk(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(strings.Repeat("concurrent", 1024))
	const writers = 8
	infos := make(chan ObjectInfo, writers)
	errorsCh := make(chan error, writers)
	var wait sync.WaitGroup
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			info, putErr := store.PutBytes(payload)
			if putErr != nil {
				errorsCh <- putErr
				return
			}
			infos <- info
		}()
	}
	wait.Wait()
	close(errorsCh)
	close(infos)
	for err := range errorsCh {
		t.Errorf("concurrent PutBytes(): %v", err)
	}
	for info := range infos {
		if info.Digest != DigestBytes(payload) {
			t.Errorf("concurrent digest = %q", info.Digest)
		}
	}
	if err := store.Verify(DigestBytes(payload)); err != nil {
		t.Fatal(err)
	}

	other, err := store.PutBytes([]byte("another object"))
	if err != nil {
		t.Fatal(err)
	}
	invalid := filepath.Join(store.Root(), "sha256", "zz", "not-an-object")
	if err := os.MkdirAll(filepath.Dir(invalid), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(invalid, []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	var got []string
	if err := store.Walk(func(info ObjectInfo) error {
		got = append(got, info.Digest)
		if info.Path == "" || info.Size <= 0 {
			t.Errorf("Walk() info = %+v", info)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{DigestBytes(payload), other.Digest}
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Walk() digests = %v, want %v", got, want)
	}
	sentinel := errors.New("stop walk")
	if err := store.Walk(func(ObjectInfo) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("Walk(callback error) = %v, want sentinel", err)
	}
}

func TestSyncDirectoryMissingPath(t *testing.T) {
	if err := syncDirectory(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("syncDirectory(missing) succeeded, want error")
	}
}
