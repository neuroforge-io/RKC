package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const maximumFileCacheEntryBytes = int64(1 << 20)

// FileCache is a portable metadata cache for stage results. Large stage objects
// remain in the content-addressed store identified by Result.ObjectDigest; the
// cache stores only deterministic pointers and metadata.
type FileCache struct {
	Root         string
	rootIdentity os.FileInfo
}

type FileCacheEntry struct {
	Key          string    `json:"cache_key"`
	Result       Result    `json:"result"`
	SizeBytes    int64     `json:"size_bytes"`
	LastAccessed time.Time `json:"last_accessed"`
}

func OpenFileCache(root string) (*FileCache, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return nil, err
	}
	if err := rejectFileCacheSymlinks(absolute); err != nil {
		return nil, err
	}
	identity, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if identity.Mode()&os.ModeSymlink != 0 || !identity.IsDir() {
		return nil, errors.New("stage cache root is not a non-symlink directory")
	}
	return &FileCache{Root: absolute, rootIdentity: identity}, nil
}

func (cache *FileCache) Load(ctx context.Context, key string) (Result, bool, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, false, err
	}
	if err := cache.validateRoot(); err != nil {
		return Result{}, false, err
	}
	path, err := cache.path(key)
	if err != nil {
		return Result{}, false, err
	}
	file, identity, err := openStableCacheFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	defer file.Close()
	if identity.Size() > maximumFileCacheEntryBytes {
		return Result{}, false, fmt.Errorf(
			"stage cache entry exceeds %d bytes",
			maximumFileCacheEntryBytes,
		)
	}
	data, err := io.ReadAll(io.LimitReader(file, maximumFileCacheEntryBytes+1))
	if err != nil {
		return Result{}, false, err
	}
	if int64(len(data)) > maximumFileCacheEntryBytes {
		return Result{}, false, fmt.Errorf(
			"stage cache entry exceeds %d bytes",
			maximumFileCacheEntryBytes,
		)
	}
	if err := validateStableCacheRead(path, file, identity, int64(len(data))); err != nil {
		return Result{}, false, err
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return Result{}, false, fmt.Errorf("decode stage cache %s: %w", key, err)
	}
	return result, true, nil
}

func (cache *FileCache) Store(ctx context.Context, key string, result Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cache.validateRoot(); err != nil {
		return err
	}
	path, err := cache.path(key)
	if err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if int64(len(data)) > maximumFileCacheEntryBytes {
		return fmt.Errorf("stage cache entry exceeds %d bytes", maximumFileCacheEntryBytes)
	}
	shard, shardIdentity, err := cache.ensureShard(filepath.Base(filepath.Dir(path)))
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(shard, ".stage-")
	if err != nil {
		return err
	}
	name := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(name)
		}
	}()
	if err := temp.Chmod(0o644); err != nil {
		return err
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if current, err := os.Lstat(path); err == nil {
		if current.Mode()&os.ModeSymlink != 0 || !current.Mode().IsRegular() {
			return fmt.Errorf("refuse to replace unsafe stage cache entry %q", path)
		}
		if current.Size() > maximumFileCacheEntryBytes {
			return fmt.Errorf("existing stage cache entry exceeds %d bytes", maximumFileCacheEntryBytes)
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if bytes.Equal(existing, data) {
			return nil
		}
		return fmt.Errorf("immutable stage cache entry conflict for %s", key)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := cache.validateShard(shard, shardIdentity); err != nil {
		return err
	}
	if err := os.Link(name, path); err != nil {
		if current, statErr := os.Lstat(path); statErr == nil &&
			current.Mode().IsRegular() && current.Mode()&os.ModeSymlink == 0 {
			if current.Size() <= maximumFileCacheEntryBytes {
				existing, readErr := os.ReadFile(path)
				if readErr == nil && bytes.Equal(existing, data) {
					return nil
				}
			}
		}
		return fmt.Errorf("install immutable stage cache entry: %w", err)
	}
	if err := os.Remove(name); err != nil {
		return err
	}
	committed = true
	return syncStableCacheDirectory(shard, shardIdentity)
}

// Entries returns every structurally valid metadata entry in deterministic
// cache-key order. Unexpected files, symlinks, and corrupt JSON fail closed so
// administrative commands cannot silently overlook poisoned cache state.
func (cache *FileCache) Entries(ctx context.Context) ([]FileCacheEntry, error) {
	if cache == nil {
		return nil, errors.New("stage cache is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cache.validateRoot(); err != nil {
		return nil, err
	}
	var entries []FileCacheEntry
	err := filepath.WalkDir(cache.Root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == cache.Root {
			if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return errors.New("stage cache root is not a stable directory")
			}
			return nil
		}
		relative, err := filepath.Rel(cache.Root, path)
		if err != nil {
			return err
		}
		parts := strings.Split(filepath.ToSlash(relative), "/")
		if entry.IsDir() {
			if len(parts) != 1 || !isLowerHex(parts[0], 2) || entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("unexpected stage cache directory %q", relative)
			}
			return nil
		}
		if len(parts) != 2 || !isLowerHex(parts[0], 2) ||
			!strings.HasSuffix(parts[1], ".json") ||
			!isLowerHex(strings.TrimSuffix(parts[1], ".json"), 62) ||
			entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() {
			return fmt.Errorf("unexpected stage cache entry %q", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		file, identity, err := openStableCacheFile(path)
		if err != nil {
			return err
		}
		if identity.Size() > maximumFileCacheEntryBytes {
			_ = file.Close()
			return fmt.Errorf("stage cache entry %s exceeds %d bytes", relative, maximumFileCacheEntryBytes)
		}
		data, err := io.ReadAll(io.LimitReader(file, maximumFileCacheEntryBytes+1))
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if int64(len(data)) > maximumFileCacheEntryBytes {
			return fmt.Errorf("stage cache entry %s exceeds %d bytes", relative, maximumFileCacheEntryBytes)
		}
		if err := validateStableCachePath(path, identity, int64(len(data))); err != nil {
			return err
		}
		var result Result
		if err := json.Unmarshal(data, &result); err != nil {
			return fmt.Errorf("decode stage cache %s: %w", relative, err)
		}
		key := "stage:" + parts[0] + strings.TrimSuffix(parts[1], ".json")
		entries = append(entries, FileCacheEntry{
			Key: key, Result: result, SizeBytes: info.Size(), LastAccessed: info.ModTime().UTC(),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := cache.validateRoot(); err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Key < entries[j].Key })
	return entries, nil
}

// Delete removes one exact cache metadata entry. Missing entries are already
// pruned and therefore succeed.
func (cache *FileCache) Delete(ctx context.Context, key string) error {
	if cache == nil {
		return errors.New("stage cache is nil")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cache.validateRoot(); err != nil {
		return err
	}
	path, err := cache.path(key)
	if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("refuse to delete unsafe stage cache entry %q", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	shard := filepath.Dir(path)
	shardIdentity, err := os.Lstat(shard)
	if err != nil {
		return err
	}
	return syncStableCacheDirectory(shard, shardIdentity)
}

func (cache *FileCache) Invalidate(ctx context.Context, key string) error {
	return cache.Delete(ctx, key)
}

func (cache *FileCache) path(key string) (string, error) {
	if !strings.HasPrefix(key, "stage:") {
		return "", fmt.Errorf("invalid stage cache key %q", key)
	}
	digest := strings.TrimPrefix(key, "stage:")
	if !isLowerHex(digest, 64) || strings.ContainsAny(digest, `/\\`) {
		return "", fmt.Errorf("invalid stage cache digest %q", digest)
	}
	return filepath.Join(cache.Root, digest[:2], digest[2:]+".json"), nil
}

func isLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			if character < 'a' || character > 'f' {
				return false
			}
		}
	}
	return true
}

func (cache *FileCache) validateRoot() error {
	if cache == nil || cache.rootIdentity == nil {
		return errors.New("stage cache root identity is missing")
	}
	current, err := os.Lstat(cache.Root)
	if err != nil || current.Mode()&os.ModeSymlink != 0 ||
		!current.IsDir() || !os.SameFile(cache.rootIdentity, current) {
		return errors.New("stage cache root identity changed")
	}
	return nil
}

func (cache *FileCache) ensureShard(name string) (string, os.FileInfo, error) {
	if !isLowerHex(name, 2) {
		return "", nil, errors.New("invalid stage cache shard")
	}
	if err := cache.validateRoot(); err != nil {
		return "", nil, err
	}
	path := filepath.Join(cache.Root, name)
	identity, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", nil, err
		}
		identity, err = os.Lstat(path)
	}
	if err != nil {
		return "", nil, err
	}
	if identity.Mode()&os.ModeSymlink != 0 || !identity.IsDir() {
		return "", nil, errors.New("stage cache shard is not a non-symlink directory")
	}
	if err := cache.validateShard(path, identity); err != nil {
		return "", nil, err
	}
	return path, identity, nil
}

func (cache *FileCache) validateShard(path string, identity os.FileInfo) error {
	if err := cache.validateRoot(); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 ||
		!current.IsDir() || identity == nil || !os.SameFile(identity, current) {
		return errors.New("stage cache shard identity changed")
	}
	return nil
}

func openStableCacheFile(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, nil, errors.New("stage cache entry is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, nil, errors.New("stage cache entry identity changed")
	}
	return file, opened, nil
}

func validateStableCacheRead(
	path string,
	file *os.File,
	identity os.FileInfo,
	bytesRead int64,
) error {
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() ||
		!os.SameFile(identity, opened) || opened.Size() != bytesRead {
		return errors.New("stage cache entry changed while reading")
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(identity, current) {
		return errors.New("stage cache entry pathname changed while reading")
	}
	return nil
}

func validateStableCachePath(path string, identity os.FileInfo, size int64) error {
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() ||
		identity == nil || !os.SameFile(identity, current) || current.Size() != size {
		return errors.New("stage cache entry pathname changed while reading")
	}
	return nil
}

func syncStableCacheDirectory(path string, identity os.FileInfo) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil || identity == nil || !opened.IsDir() || !os.SameFile(identity, opened) {
		return errors.New("stage cache directory identity changed")
	}
	if err := directory.Sync(); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || !os.SameFile(identity, current) {
		return errors.New("stage cache directory identity changed")
	}
	return nil
}

func rejectFileCacheSymlinks(path string) error {
	clean := filepath.Clean(path)
	volume := filepath.VolumeName(clean)
	remainder := strings.TrimPrefix(clean, volume)
	current := volume
	if filepath.IsAbs(clean) {
		current += string(os.PathSeparator)
		remainder = strings.TrimLeft(remainder, string(os.PathSeparator))
	}
	for _, component := range strings.Split(remainder, string(os.PathSeparator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("stage cache path component %s is a symlink", current)
		}
	}
	return nil
}
