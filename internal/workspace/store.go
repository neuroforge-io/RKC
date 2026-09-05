package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

// Store serializes writers using a kernel lease. Readers use Load without a
// lock. A process exit releases the lease; stale lock files are never removed.
type Store struct {
	Path     string
	root     string
	identity os.FileInfo
	lock     *os.File
	Registry Registry
}

// Open initializes a new private workspace or acquires its exclusive writer
// lease. Existing directories are never adopted or permission-repaired.
func Open(path string) (*Store, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if filepath.Base(absolute) != "registry.json" {
		return nil, errors.New("workspace registry must be named registry.json")
	}
	if err := rejectSymlinks(absolute); err != nil {
		return nil, err
	}
	root := filepath.Dir(absolute)
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		if err := initialize(root); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	registry, err := Load(absolute)
	if err != nil {
		return nil, err
	}
	identity, err := privatepath.Lstat(root)
	if err != nil {
		return nil, err
	}
	lockPath := filepath.Join(root, "writer.lock")
	before, err := privatepath.Lstat(lockPath)
	if err != nil {
		return nil, err
	}
	if err := privatepath.CheckFile(lockPath, before); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(lockPath, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = lock.Close()
		}
	}()
	opened, err := lock.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("workspace writer lease identity changed")
	}
	if err := lockExclusive(lock); err != nil {
		return nil, fmt.Errorf("workspace writer is busy or cannot prove exclusive ownership: %w", err)
	}
	if err := privatepath.CheckFile(lockPath, before); err != nil {
		return nil, err
	}
	// Reload after taking the lease so a just-completed writer is not lost.
	registry, err = Load(absolute)
	if err != nil {
		return nil, err
	}
	keep = true
	return &Store{Path: absolute, root: root, identity: identity, lock: lock, Registry: registry}, nil
}

func initialize(root string) error {
	if err := os.MkdirAll(filepath.Dir(root), 0700); err != nil {
		return err
	}
	if err := rejectSymlinks(filepath.Dir(root)); err != nil {
		return err
	}
	staging, err := privatepath.MkdirTemp(filepath.Dir(root), ".rkc-workspace-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	registry := Registry{SchemaVersion: SchemaVersion, Generation: 1, Sources: []Source{}}
	data, err := json.MarshalIndent(registry, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(staging, "registry.json", append(data, '\n')); err != nil {
		return err
	}
	if err := writeAtomic(staging, "writer.lock", nil); err != nil {
		return err
	}
	if err := privateDirectory(staging, "generations"); err != nil {
		return err
	}
	// Staging is nonempty; a concurrent successful initialization cannot be
	// replaced by a directory rename on any supported platform.
	if err := privatepath.Rename(staging, root); err != nil {
		if _, loadErr := Load(filepath.Join(root, "registry.json")); loadErr == nil {
			return nil
		}
		return err
	}
	return privatepath.SyncDirectory(filepath.Dir(root))
}

func privateDirectory(root, name string) error {
	path, err := privatepath.MkdirTemp(root, ".directory-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(path)
	return privatepath.Rename(path, filepath.Join(root, name))
}

// Close releases the writer lease without deleting its stable lock inode.
func (store *Store) Close() error {
	if store == nil || store.lock == nil {
		return nil
	}
	file := store.lock
	store.lock = nil
	return file.Close()
}

// Add registers a source without scanning it or contacting a remote server.
func (store *Store) Add(source Source) error {
	if len(store.Registry.Sources) >= MaximumSources {
		return errors.New("workspace source limit reached")
	}
	for _, existing := range store.Registry.Sources {
		if existing.ID == source.ID {
			return errors.New("workspace source alias is already registered")
		}
	}
	if source.Active != nil || source.Previous != nil {
		return errors.New("new sources cannot supply active generations")
	}
	source.Freshness = Freshness{Status: "pending"}
	if err := validateSource(source); err != nil {
		return err
	}
	store.Registry.Sources = append(store.Registry.Sources, source)
	return store.save()
}

// Refresh calls one bounded, admitted producer. A nil result means its active
// generation still matches. The producer may only return a fully verified
// generation; failure leaves Active and Previous untouched.
func (store *Store) Refresh(ctx context.Context, id string, producer func(context.Context, Source, string) (*Active, error)) error {
	if ctx == nil || producer == nil {
		return errors.New("workspace refresh context and producer are required")
	}
	index := -1
	for i := range store.Registry.Sources {
		if store.Registry.Sources[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("workspace source alias is not registered")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	source := store.Registry.Sources[index]
	store.Registry.Sources[index].Freshness.Status = "stale"
	store.Registry.Sources[index].Freshness.ErrorCode = ""
	if err := store.save(); err != nil {
		return err
	}
	active, runErr := producer(ctx, source, filepath.Join(store.root, "generations"))
	if runErr == nil {
		runErr = ctx.Err()
	}
	now := time.Now().UTC()
	current := &store.Registry.Sources[index]
	current.Freshness.CheckedAt = now
	if runErr != nil {
		current.Freshness.Status = "error"
		current.Freshness.ErrorCode = "refresh_failed"
		var failure *RefreshError
		if errors.As(runErr, &failure) {
			current.Freshness.ErrorCode = failure.Code
		}
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			current.Freshness.ErrorCode = "canceled"
		}
		return errors.Join(runErr, store.save())
	}
	if active != nil {
		candidate := store.Registry
		candidate.Sources = append([]Source(nil), candidate.Sources...)
		candidate.Sources[index].Active = active
		if err := validate(candidate, store.root); err != nil {
			return err
		}
		current.Previous, current.Active = current.Active, active
		current.Freshness.UpdatedAt = now
	}
	if current.Active == nil {
		return errors.New("workspace producer returned no initial generation")
	}
	current.Freshness.Status = "current"
	current.Freshness.ErrorCode = ""
	if active != nil && active.SourceAdvanced {
		current.Freshness.Status = "stale"
		current.Freshness.ErrorCode = "source_changed"
	}
	return store.save()
}

// RefreshError carries only a fixed public error code and a private cause.
type RefreshError struct {
	Code  string
	Cause error
}

func (err *RefreshError) Error() string { return err.Cause.Error() }
func (err *RefreshError) Unwrap() error { return err.Cause }

func (store *Store) save() error {
	if store.lock == nil {
		return errors.New("workspace writer is closed")
	}
	if err := privatepath.CheckDir(store.root, store.identity); err != nil {
		return err
	}
	store.Registry.Generation++
	if err := validate(store.Registry, store.root); err != nil {
		return err
	}
	data, err := json.MarshalIndent(store.Registry, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > maximumRegistryBytes {
		return errors.New("workspace registry exceeds size limit")
	}
	return writeAtomic(store.root, "registry.json", append(data, '\n'))
}

func writeAtomic(root, name string, data []byte) error {
	identity, err := privatepath.Lstat(root)
	if err != nil {
		return err
	}
	if err := privatepath.CheckDir(root, identity); err != nil {
		return err
	}
	path := filepath.Join(root, name)
	if existing, err := privatepath.Lstat(path); err == nil {
		if err := privatepath.CheckFile(path, existing); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := privatepath.CreateTemp(root, ".registry-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := privatepath.CheckDir(root, identity); err != nil {
		return err
	}
	if err := privatepath.Rename(file.Name(), path); err != nil {
		return err
	}
	return privatepath.SyncDirectoryStable(root, identity)
}
