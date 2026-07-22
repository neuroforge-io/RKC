// Package cas implements a filesystem content-addressed store used by local
// mode. Objects are immutable, verified on read when requested, and committed by
// atomic rename inside the store filesystem.
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
)

type Store struct {
	root string
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
	if err := os.MkdirAll(filepath.Join(absolute, "sha256"), 0o755); err != nil {
		return nil, fmt.Errorf("create CAS root: %w", err)
	}
	return &Store{root: absolute}, nil
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
	return filepath.Join(store.root, "sha256", digest[:2], digest[2:]), nil
}

func (store *Store) Has(digest string) (bool, error) {
	path, err := store.Path(digest)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
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
	tempDir := filepath.Join(store.root, ".tmp")
	if err := os.MkdirAll(tempDir, 0o755); err != nil {
		return ObjectInfo{}, err
	}
	temp, err := os.CreateTemp(tempDir, "object-")
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("create temporary CAS object: %w", err)
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()

	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(temp, hash), reader)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("write temporary CAS object: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return ObjectInfo{}, fmt.Errorf("sync temporary CAS object: %w", err)
	}
	if err := temp.Close(); err != nil {
		return ObjectInfo{}, fmt.Errorf("close temporary CAS object: %w", err)
	}

	digest := hex.EncodeToString(hash.Sum(nil))
	finalPath, err := store.Path(digest)
	if err != nil {
		return ObjectInfo{}, err
	}
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o755); err != nil {
		return ObjectInfo{}, fmt.Errorf("create CAS shard: %w", err)
	}
	if existing, err := os.Stat(finalPath); err == nil {
		if existing.Size() != size {
			return ObjectInfo{}, fmt.Errorf("existing CAS object size conflict for %s", digest)
		}
		committed = true
		_ = os.Remove(tempPath)
		return ObjectInfo{Digest: digest, Path: finalPath, Size: size}, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return ObjectInfo{}, err
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		// Another writer may have won the race. The content digest guarantees
		// equality, but verify the final object exists and has the expected size.
		if existing, statErr := os.Stat(finalPath); statErr == nil && existing.Size() == size {
			committed = true
			_ = os.Remove(tempPath)
			return ObjectInfo{Digest: digest, Path: finalPath, Size: size}, nil
		}
		return ObjectInfo{}, fmt.Errorf("commit CAS object: %w", err)
	}
	committed = true
	_ = syncDirectory(filepath.Dir(finalPath))
	return ObjectInfo{Digest: digest, Path: finalPath, Size: size}, nil
}

func (store *Store) OpenObject(digest string) (*os.File, error) {
	path, err := store.Path(digest)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open CAS object %s: %w", digest, err)
	}
	return file, nil
}

func (store *Store) ReadBytes(digest string, verify bool) ([]byte, error) {
	file, err := store.OpenObject(digest)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(file)
	if err != nil {
		return nil, err
	}
	if verify {
		normalized, _ := NormalizeDigest(digest)
		if DigestBytes(data) != normalized {
			return nil, ErrDigestMismatch
		}
	}
	return data, nil
}

func (store *Store) Verify(digest string) error {
	file, err := store.OpenObject(digest)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	normalized, err := NormalizeDigest(digest)
	if err != nil {
		return err
	}
	if hex.EncodeToString(hash.Sum(nil)) != normalized {
		return ErrDigestMismatch
	}
	return nil
}

func (store *Store) Walk(fn func(ObjectInfo) error) error {
	root := filepath.Join(store.root, "sha256")
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		digest := strings.ReplaceAll(filepath.ToSlash(relative), "/", "")
		if _, err := NormalizeDigest(digest); err != nil {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		return fn(ObjectInfo{Digest: digest, Path: path, Size: info.Size()})
	})
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
