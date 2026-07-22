package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FileCache is a portable metadata cache for stage results. Large stage objects
// remain in the content-addressed store identified by Result.ObjectDigest; the
// cache stores only deterministic pointers and metadata.
type FileCache struct{ Root string }

func OpenFileCache(root string) (*FileCache, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return nil, err
	}
	return &FileCache{Root: absolute}, nil
}

func (cache *FileCache) Load(ctx context.Context, key string) (Result, bool, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, false, err
	}
	path, err := cache.path(key)
	if err != nil {
		return Result{}, false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Result{}, false, nil
	}
	if err != nil {
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
	path, err := cache.path(key)
	if err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".stage-")
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
	if err := os.Rename(name, path); err != nil {
		return err
	}
	committed = true
	return nil
}

func (cache *FileCache) path(key string) (string, error) {
	if !strings.HasPrefix(key, "stage:") {
		return "", fmt.Errorf("invalid stage cache key %q", key)
	}
	digest := strings.TrimPrefix(key, "stage:")
	if len(digest) != 64 || strings.ContainsAny(digest, `/\\`) {
		return "", fmt.Errorf("invalid stage cache digest %q", digest)
	}
	return filepath.Join(cache.Root, digest[:2], digest[2:]+".json"), nil
}
