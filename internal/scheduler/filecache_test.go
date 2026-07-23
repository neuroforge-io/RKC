package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func cacheKey(character string) string {
	return "stage:" + strings.Repeat(character, 64)
}

func TestOpenFileCacheAndPathValidation(t *testing.T) {
	cache, err := OpenFileCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(cache.Root) {
		t.Fatalf("cache root = %q, want absolute", cache.Root)
	}
	path, err := cache.path(cacheKey("a"))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cache.Root, "aa", strings.Repeat("a", 62)+".json")
	if path != want {
		t.Fatalf("cache path = %q, want %q", path, want)
	}
	for _, key := range []string{"", strings.Repeat("a", 64), "stage:short", "stage:" + strings.Repeat("A", 64), "stage:" + strings.Repeat("z", 64), "stage:" + strings.Repeat("a", 31) + "/" + strings.Repeat("a", 32), "stage:" + strings.Repeat("a", 31) + `\` + strings.Repeat("a", 32)} {
		if _, err := cache.path(key); err == nil {
			t.Errorf("path(%q) succeeded, want rejection", key)
		}
	}

	fileRoot := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileRoot, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenFileCache(fileRoot); err == nil {
		t.Fatal("OpenFileCache(file) succeeded, want error")
	}
}

func TestFileCacheStoreLoadImmutabilityAndMiss(t *testing.T) {
	cache, err := OpenFileCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := cacheKey("b")
	if result, ok, err := cache.Load(context.Background(), key); err != nil || ok || !reflect.DeepEqual(result, Result{}) {
		t.Fatalf("Load(miss) = %+v, %v, %v", result, ok, err)
	}
	first := Result{StageID: "first", CacheKey: key, ObjectDigest: "object-1", Metadata: map[string]any{"count": float64(1)}}
	if err := cache.Store(context.Background(), key, first); err != nil {
		t.Fatal(err)
	}
	path, _ := cache.path(key)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("cache entry is not newline terminated")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("cache entry mode = %v, %v", info.Mode(), err)
	}
	loaded, ok, err := cache.Load(context.Background(), key)
	if err != nil || !ok || !reflect.DeepEqual(loaded, first) {
		t.Fatalf("Load(stored) = %+v, %v, %v; want %+v", loaded, ok, err, first)
	}

	if err := cache.Store(context.Background(), key, first); err != nil {
		t.Fatalf("idempotent Store() = %v", err)
	}
	second := Result{StageID: "second", ObjectDigest: "object-2"}
	if err := cache.Store(context.Background(), key, second); err == nil ||
		!strings.Contains(err.Error(), "immutable") {
		t.Fatalf("conflicting Store() = %v", err)
	}
	loaded, ok, err = cache.Load(context.Background(), key)
	if err != nil || !ok || !reflect.DeepEqual(loaded, first) {
		t.Fatalf("Load(after conflict) = %+v, %v, %v; want %+v", loaded, ok, err, first)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".stage-") {
			t.Fatalf("committed cache left temporary file %q", entry.Name())
		}
	}
}

func TestFileCacheCancellationAndInvalidKeys(t *testing.T) {
	cache, err := OpenFileCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	key := cacheKey("c")
	if _, _, err := cache.Load(ctx, key); !errors.Is(err, context.Canceled) {
		t.Fatalf("Load(cancelled) = %v", err)
	}
	if err := cache.Store(ctx, key, Result{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Store(cancelled) = %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache.Root, "cc")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled Store created state: %v", err)
	}
	if _, _, err := cache.Load(context.Background(), "unsafe"); err == nil {
		t.Fatal("Load(invalid key) succeeded")
	}
	if err := cache.Store(context.Background(), "unsafe", Result{}); err == nil {
		t.Fatal("Store(invalid key) succeeded")
	}
}

func TestFileCacheCorruptionAndReadFailure(t *testing.T) {
	cache, err := OpenFileCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := cacheKey("d")
	path, _ := cache.path(key)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Load(context.Background(), key); err == nil || !strings.Contains(err.Error(), "decode stage cache") {
		t.Fatalf("Load(corrupt) = %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cache.Load(context.Background(), key); err == nil {
		t.Fatal("Load(directory entry) succeeded, want read error")
	}
}

func TestFileCacheMarshalFailurePreservesExistingEntry(t *testing.T) {
	cache, err := OpenFileCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key := cacheKey("e")
	original := Result{StageID: "kept", ObjectDigest: "original"}
	if err := cache.Store(context.Background(), key, original); err != nil {
		t.Fatal(err)
	}
	invalid := Result{Metadata: map[string]any{"unsupported": make(chan int)}}
	if err := cache.Store(context.Background(), key, invalid); err == nil {
		t.Fatal("Store(unmarshalable result) succeeded")
	}
	loaded, ok, err := cache.Load(context.Background(), key)
	if err != nil || !ok || !reflect.DeepEqual(loaded, original) {
		t.Fatalf("entry after failed replacement = %+v, %v, %v", loaded, ok, err)
	}
}

func TestFileCacheFilesystemFailuresRollBackTemporaryFiles(t *testing.T) {
	cache, err := OpenFileCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blockedKey := cacheKey("f")
	if err := os.WriteFile(filepath.Join(cache.Root, "ff"), []byte("block shard"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cache.Store(context.Background(), blockedKey, Result{}); err == nil {
		t.Fatal("Store() through file shard succeeded, want error")
	}

	key := cacheKey("1")
	path, _ := cache.path(key)
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := cache.Store(context.Background(), key, Result{StageID: "no"}); err == nil {
		t.Fatal("Store() over directory succeeded, want rename error")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".stage-") {
			t.Fatalf("failed Store left temporary file %q", entry.Name())
		}
	}
}

func TestFileCacheEntriesAndDelete(t *testing.T) {
	cache, err := OpenFileCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, character := range []string{"2", "1"} {
		key := cacheKey(character)
		if err := cache.Store(context.Background(), key, Result{
			StageID: character, CacheKey: key, ObjectDigest: strings.Repeat(character, 64),
		}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := cache.Entries(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Key != cacheKey("1") || entries[1].Key != cacheKey("2") {
		t.Fatalf("Entries() = %+v", entries)
	}
	if entries[0].SizeBytes <= 0 || entries[0].LastAccessed.IsZero() {
		t.Fatalf("entry metadata = %+v", entries[0])
	}
	if err := cache.Delete(context.Background(), cacheKey("1")); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.Load(context.Background(), cacheKey("1")); err != nil || ok {
		t.Fatalf("deleted Load() = ok %t, err %v", ok, err)
	}
	if err := cache.Delete(context.Background(), cacheKey("1")); err != nil {
		t.Fatalf("repeated Delete() = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Entries(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Entries(cancelled) = %v", err)
	}
	if err := cache.Delete(ctx, cacheKey("2")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete(cancelled) = %v", err)
	}
}

func TestFileCacheEntriesRejectUnexpectedState(t *testing.T) {
	cache, err := OpenFileCache(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cache.Root, "unexpected"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Entries(context.Background()); err == nil || !strings.Contains(err.Error(), "unexpected stage cache entry") {
		t.Fatalf("Entries(unexpected) = %v", err)
	}
}
