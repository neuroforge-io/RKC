// Package cas implements a filesystem content-addressed store used by local
// mode. Objects are immutable, verified on read when requested, and installed
// without replacing an existing pathname.
package cas

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

var (
	ErrInvalidDigest  = errors.New("invalid sha256 digest")
	ErrDigestMismatch = errors.New("object content does not match digest")
	ErrObjectTooLarge = errors.New("CAS object exceeds read limit")
	ErrStoreChanged   = errors.New("CAS store layout changed")
	ErrUnsafeObject   = errors.New("CAS object is not a stable regular file")
)

type Store struct {
	root          string
	shaRoot       string
	temporaryRoot string
	rootIdentity  os.FileInfo
	shaIdentity   os.FileInfo
	tempIdentity  os.FileInfo
}

type ObjectInfo struct {
	Digest string `json:"digest"`
	Path   string `json:"path"`
	Size   int64  `json:"size"`
}

func Open(root string) (*Store, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve CAS root: %w", err)
	}
	rootIdentity, err := ensureDirectory(absolute, 0o755)
	if err != nil {
		return nil, fmt.Errorf("create CAS root: %w", err)
	}
	shaRoot := filepath.Join(absolute, "sha256")
	shaIdentity, err := ensureDirectory(shaRoot, 0o755)
	if err != nil {
		return nil, fmt.Errorf("create CAS object layout: %w", err)
	}
	temporaryRoot := filepath.Join(absolute, ".tmp")
	tempIdentity, err := ensureDirectory(temporaryRoot, 0o700)
	if err != nil {
		return nil, fmt.Errorf("create CAS temporary layout: %w", err)
	}
	store := &Store{
		root: absolute, shaRoot: shaRoot, temporaryRoot: temporaryRoot,
		rootIdentity: rootIdentity, shaIdentity: shaIdentity, tempIdentity: tempIdentity,
	}
	if err := store.validateLayout(); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *Store) Root() string { return store.root }

func DigestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func NormalizeDigest(value string) (string, error) {
	value = strings.TrimSpace(strings.ToLower(value))
	value = strings.TrimPrefix(value, "sha256:")
	if len(value) != sha256.Size*2 {
		return "", ErrInvalidDigest
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return "", ErrInvalidDigest
	}
	return value, nil
}

func (store *Store) Path(digest string) (string, error) {
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return "", err
	}
	if err := store.validateLayout(); err != nil {
		return "", err
	}
	shardPath := filepath.Join(store.shaRoot, digest[:2])
	if info, err := os.Lstat(shardPath); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", fmt.Errorf("%w: digest shard is not a regular directory", ErrStoreChanged)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("inspect CAS shard: %w", err)
	}
	if err := store.validateLayout(); err != nil {
		return "", err
	}
	return filepath.Join(shardPath, digest[2:]), nil
}

func (store *Store) Has(digest string) (bool, error) {
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return false, err
	}
	_, err = store.verifyExisting(digest, -1)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func (store *Store) PutBytes(data []byte) (ObjectInfo, error) {
	return store.Put(bytes.NewReader(data))
}

func (store *Store) Put(reader io.Reader) (ObjectInfo, error) {
	if reader == nil {
		return ObjectInfo{}, errors.New("CAS object reader is nil")
	}
	if err := store.validateLayout(); err != nil {
		return ObjectInfo{}, err
	}
	temporary, err := os.CreateTemp(store.temporaryRoot, "object-")
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("create temporary CAS object: %w", err)
	}
	temporaryPath := temporary.Name()
	temporaryIdentity, statErr := temporary.Stat()
	defer func() {
		_ = temporary.Close()
		if temporaryIdentity != nil {
			_ = removeExactRegularFile(temporaryPath, temporaryIdentity)
		}
	}()
	if statErr != nil || !temporaryIdentity.Mode().IsRegular() {
		if statErr == nil {
			statErr = ErrUnsafeObject
		}
		return ObjectInfo{}, fmt.Errorf("inspect temporary CAS object: %w", statErr)
	}
	if err := store.validateTemporaryFile(temporaryPath, temporaryIdentity); err != nil {
		return ObjectInfo{}, err
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(temporary, hash), reader)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("write temporary CAS object: %w", err)
	}
	if err := temporary.Chmod(0o444); err != nil {
		return ObjectInfo{}, fmt.Errorf("protect temporary CAS object: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return ObjectInfo{}, fmt.Errorf("sync temporary CAS object: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return ObjectInfo{}, fmt.Errorf("close temporary CAS object: %w", err)
	}
	if err := store.validateTemporaryFile(temporaryPath, temporaryIdentity); err != nil {
		return ObjectInfo{}, err
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	shardPath, shardIdentity, err := store.ensureShard(digest[:2])
	if err != nil {
		return ObjectInfo{}, err
	}
	finalPath := filepath.Join(shardPath, digest[2:])
	if _, err := os.Lstat(finalPath); err == nil {
		return store.verifyExistingInShard(digest, size, shardPath, shardIdentity)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ObjectInfo{}, fmt.Errorf("inspect existing CAS object: %w", err)
	}
	if err := store.validateShard(shardPath, shardIdentity); err != nil {
		return ObjectInfo{}, err
	}
	if err := os.Link(temporaryPath, finalPath); err != nil {
		// A concurrent writer may have installed this digest. It is successful
		// only after hashing the complete, stable object; matching size alone is
		// not an integrity proof.
		if existing, verifyErr := store.verifyExistingInShard(digest, size, shardPath, shardIdentity); verifyErr == nil {
			return existing, nil
		} else {
			return ObjectInfo{}, fmt.Errorf("install CAS object: %w", errors.Join(err, verifyErr))
		}
	}
	linkedIdentity, err := os.Lstat(finalPath)
	if err != nil || !linkedIdentity.Mode().IsRegular() || !os.SameFile(temporaryIdentity, linkedIdentity) {
		if err == nil {
			err = ErrUnsafeObject
		}
		_ = removeExactRegularFile(finalPath, temporaryIdentity)
		return ObjectInfo{}, fmt.Errorf("validate installed CAS object: %w", err)
	}
	if err := store.validateShard(shardPath, shardIdentity); err != nil {
		_ = removeExactRegularFile(finalPath, temporaryIdentity)
		return ObjectInfo{}, err
	}
	if err := syncDirectoryStable(shardPath, shardIdentity); err != nil {
		return ObjectInfo{}, fmt.Errorf("sync CAS shard: %w", err)
	}
	if err := removeExactRegularFile(temporaryPath, temporaryIdentity); err != nil {
		return ObjectInfo{}, fmt.Errorf("remove committed CAS temporary link: %w", err)
	}
	temporaryIdentity = nil
	if err := syncDirectoryStable(store.temporaryRoot, store.tempIdentity); err != nil {
		return ObjectInfo{}, fmt.Errorf("sync CAS temporary layout: %w", err)
	}
	return ObjectInfo{Digest: digest, Path: finalPath, Size: size}, nil
}

func (store *Store) OpenObject(digest string) (*os.File, error) {
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return nil, err
	}
	file, _, _, _, err := store.openObject(digest)
	return file, err
}

func (store *Store) ReadBytes(digest string, verify bool) ([]byte, error) {
	return store.readBytes(digest, verify, 0)
}

// ReadBytesBounded reads at most maximum bytes and rejects objects whose
// stable size exceeds that limit before allocating their payload.
func (store *Store) ReadBytesBounded(digest string, verify bool, maximum int64) ([]byte, error) {
	if maximum <= 0 {
		return nil, errors.New("CAS read limit must be positive")
	}
	return store.readBytes(digest, verify, maximum)
}

func (store *Store) readBytes(digest string, verify bool, maximum int64) ([]byte, error) {
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return nil, err
	}
	file, path, objectIdentity, shardState, err := store.openObject(digest)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	if maximum > 0 && objectIdentity.Size() > maximum {
		return nil, fmt.Errorf("%w: maximum %d bytes", ErrObjectTooLarge, maximum)
	}
	reader := io.Reader(file)
	if maximum > 0 {
		reader = io.LimitReader(file, maximum+1)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		return nil, err
	}
	if maximum > 0 && int64(len(data)) > maximum {
		return nil, fmt.Errorf("%w: maximum %d bytes", ErrObjectTooLarge, maximum)
	}
	if err := store.validateRead(path, file, objectIdentity, shardState, int64(len(data))); err != nil {
		return nil, err
	}
	if verify && DigestBytes(data) != digest {
		return nil, ErrDigestMismatch
	}
	return data, nil
}

func (store *Store) Verify(digest string) error {
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return err
	}
	_, err = store.verifyExisting(digest, -1)
	return err
}

// Delete removes one exact immutable object. Missing objects are already
// deleted and succeed. The method verifies layout and file identity before
// unlinking so administrative pruning cannot follow attacker-controlled links.
func (store *Store) Delete(digest string) error {
	digest, err := NormalizeDigest(digest)
	if err != nil {
		return err
	}
	file, path, identity, shard, err := store.openObject(digest)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := store.validateShard(shard.path, shard.info); err != nil {
		return err
	}
	if err := removeExactRegularFile(path, identity); err != nil {
		return err
	}
	if err := syncDirectoryStable(shard.path, shard.info); err != nil {
		return err
	}
	return store.validateLayout()
}

func (store *Store) Walk(fn func(ObjectInfo) error) error {
	if fn == nil {
		return errors.New("CAS walk callback is nil")
	}
	if err := store.validateLayout(); err != nil {
		return err
	}
	err := filepath.WalkDir(store.shaRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == store.shaRoot {
			return nil
		}
		relative, err := filepath.Rel(store.shaRoot, path)
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 &&
			!strings.Contains(filepath.ToSlash(relative), "/") && validShardName(entry.Name()) {
			return fmt.Errorf("%w: digest shard is a symlink", ErrStoreChanged)
		}
		if entry.IsDir() {
			if strings.Contains(filepath.ToSlash(relative), "/") || !validShardName(entry.Name()) {
				return filepath.SkipDir
			}
			info, err := os.Lstat(path)
			if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("%w: invalid shard %s", ErrStoreChanged, relative)
			}
			return nil
		}
		digest := strings.ReplaceAll(filepath.ToSlash(relative), "/", "")
		if _, err := NormalizeDigest(digest); err != nil {
			return nil
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: %s", ErrUnsafeObject, relative)
		}
		return fn(ObjectInfo{Digest: digest, Path: path, Size: info.Size()})
	})
	if err != nil {
		return err
	}
	return store.validateLayout()
}

type shardIdentity struct {
	path string
	info os.FileInfo
}

func (store *Store) verifyExisting(digest string, expectedSize int64) (ObjectInfo, error) {
	if err := store.validateLayout(); err != nil {
		return ObjectInfo{}, err
	}
	shardPath := filepath.Join(store.shaRoot, digest[:2])
	shardInfo, err := os.Lstat(shardPath)
	if err != nil {
		return ObjectInfo{}, err
	}
	if shardInfo.Mode()&os.ModeSymlink != 0 || !shardInfo.IsDir() {
		return ObjectInfo{}, fmt.Errorf("%w: digest shard is not a regular directory", ErrStoreChanged)
	}
	return store.verifyExistingInShard(digest, expectedSize, shardPath, shardInfo)
}

func (store *Store) verifyExistingInShard(digest string, expectedSize int64, shardPath string, shardInfo os.FileInfo) (ObjectInfo, error) {
	if err := store.validateShard(shardPath, shardInfo); err != nil {
		return ObjectInfo{}, err
	}
	path := filepath.Join(shardPath, digest[2:])
	file, before, err := openStableRegular(path)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer file.Close()
	if expectedSize >= 0 && before.Size() != expectedSize {
		return ObjectInfo{}, fmt.Errorf("existing CAS object size conflict for %s: got %d, want %d", digest, before.Size(), expectedSize)
	}
	hash := sha256.New()
	reader := io.Reader(file)
	if expectedSize >= 0 {
		reader = io.LimitReader(file, expectedSize+1)
	}
	size, err := io.Copy(hash, reader)
	if err != nil {
		return ObjectInfo{}, err
	}
	if expectedSize >= 0 && size != expectedSize {
		return ObjectInfo{}, fmt.Errorf("existing CAS object size conflict for %s: got %d, want %d", digest, size, expectedSize)
	}
	if err := store.validateRead(path, file, before, shardIdentity{path: shardPath, info: shardInfo}, size); err != nil {
		return ObjectInfo{}, err
	}
	if hex.EncodeToString(hash.Sum(nil)) != digest {
		return ObjectInfo{}, ErrDigestMismatch
	}
	return ObjectInfo{Digest: digest, Path: path, Size: size}, nil
}

func (store *Store) openObject(digest string) (*os.File, string, os.FileInfo, shardIdentity, error) {
	if err := store.validateLayout(); err != nil {
		return nil, "", nil, shardIdentity{}, err
	}
	shardPath := filepath.Join(store.shaRoot, digest[:2])
	shardInfo, err := os.Lstat(shardPath)
	if err != nil {
		return nil, "", nil, shardIdentity{}, err
	}
	if shardInfo.Mode()&os.ModeSymlink != 0 || !shardInfo.IsDir() {
		return nil, "", nil, shardIdentity{}, fmt.Errorf("%w: digest shard is not a regular directory", ErrStoreChanged)
	}
	if err := store.validateShard(shardPath, shardInfo); err != nil {
		return nil, "", nil, shardIdentity{}, err
	}
	path := filepath.Join(shardPath, digest[2:])
	file, objectInfo, err := openStableRegular(path)
	if err != nil {
		return nil, "", nil, shardIdentity{}, fmt.Errorf("open CAS object %s: %w", digest, err)
	}
	if err := store.validateShard(shardPath, shardInfo); err != nil {
		_ = file.Close()
		return nil, "", nil, shardIdentity{}, err
	}
	return file, path, objectInfo, shardIdentity{path: shardPath, info: shardInfo}, nil
}

func (store *Store) validateRead(path string, file *os.File, before os.FileInfo, shard shardIdentity, bytesRead int64) error {
	after, err := file.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || after.Size() != bytesRead {
		return fmt.Errorf("%w: object changed while reading", ErrUnsafeObject)
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(before, current) {
		return fmt.Errorf("%w: object pathname changed while reading", ErrUnsafeObject)
	}
	return store.validateShard(shard.path, shard.info)
}

func (store *Store) ensureShard(name string) (string, os.FileInfo, error) {
	if !validShardName(name) {
		return "", nil, ErrInvalidDigest
	}
	if err := store.validateLayout(); err != nil {
		return "", nil, err
	}
	path := filepath.Join(store.shaRoot, name)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return "", nil, fmt.Errorf("create CAS shard: %w", err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return "", nil, fmt.Errorf("inspect CAS shard: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", nil, fmt.Errorf("%w: digest shard is not a regular directory", ErrStoreChanged)
	}
	if err := store.validateShard(path, info); err != nil {
		return "", nil, err
	}
	return path, info, nil
}

func (store *Store) validateLayout() error {
	if store == nil || store.rootIdentity == nil || store.shaIdentity == nil || store.tempIdentity == nil {
		return fmt.Errorf("%w: missing layout identity", ErrStoreChanged)
	}
	for _, item := range []struct {
		path string
		info os.FileInfo
	}{{store.root, store.rootIdentity}, {store.shaRoot, store.shaIdentity}, {store.temporaryRoot, store.tempIdentity}} {
		current, err := os.Lstat(item.path)
		if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(item.info, current) {
			return fmt.Errorf("%w: %s", ErrStoreChanged, item.path)
		}
	}
	return nil
}

func (store *Store) validateShard(path string, identity os.FileInfo) error {
	if err := store.validateLayout(); err != nil {
		return err
	}
	if identity == nil {
		return fmt.Errorf("%w: missing shard identity", ErrStoreChanged)
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(identity, current) {
		return fmt.Errorf("%w: digest shard identity changed", ErrStoreChanged)
	}
	return nil
}

func (store *Store) validateTemporaryFile(path string, identity os.FileInfo) error {
	if err := store.validateLayout(); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(identity, current) {
		return fmt.Errorf("%w: temporary object identity changed", ErrUnsafeObject)
	}
	return nil
}

func ensureDirectory(path string, mode fs.FileMode) (os.FileInfo, error) {
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(path, mode); err != nil {
			return nil, err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("path is not a non-symlink directory")
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	return info, nil
}

func rejectSymlinkComponents(path string) error {
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
			return fmt.Errorf("%w: path component %s is a symlink", ErrStoreChanged, current)
		}
	}
	return nil
}

func openStableRegular(path string) (*os.File, os.FileInfo, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, nil, ErrUnsafeObject
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		_ = file.Close()
		return nil, nil, ErrUnsafeObject
	}
	return file, opened, nil
}

func removeExactRegularFile(path string, identity os.FileInfo) error {
	if identity == nil {
		return errors.New("missing file identity")
	}
	current, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(identity, current) {
		return ErrUnsafeObject
	}
	return os.Remove(path)
}

func validShardName(value string) bool {
	if len(value) != 2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && value == strings.ToLower(value)
}

func syncDirectoryStable(path string, identity os.FileInfo) error {
	if identity == nil {
		return ErrStoreChanged
	}
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	opened, err := directory.Stat()
	if err != nil || !opened.IsDir() || !os.SameFile(identity, opened) {
		return ErrStoreChanged
	}
	if err := directory.Sync(); err != nil {
		return err
	}
	current, err := os.Lstat(path)
	if err != nil || !current.IsDir() || !os.SameFile(identity, current) {
		return ErrStoreChanged
	}
	return nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
