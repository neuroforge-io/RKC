package workspace

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

// CreateGeneration creates an owner-private generation with a persistent reader
// lease and an ownership marker before any compiler output is written.
func CreateGeneration(parent, sourceID string) (string, error) {
	if !aliasPattern.MatchString(sourceID) {
		return "", errors.New("invalid generation source alias")
	}
	identity, err := privatepath.Lstat(parent)
	if err != nil {
		return "", err
	}
	if err := privatepath.CheckDir(parent, identity); err != nil {
		return "", err
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	name := sourceID + "-" + hex.EncodeToString(random[:])
	staging, err := privatepath.MkdirTemp(parent, ".building-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	if err := writeAtomic(staging, "generation.txt", []byte(SchemaVersion+"\n"+name+"\n")); err != nil {
		return "", err
	}
	if err := writeAtomic(staging, "readers.lock", nil); err != nil {
		return "", err
	}
	if err := privatepath.CheckDir(parent, identity); err != nil {
		return "", err
	}
	path := filepath.Join(parent, name)
	if err := privatepath.Rename(staging, path); err != nil {
		return "", err
	}
	return path, privatepath.SyncDirectoryStable(parent, identity)
}

// AcquireActive pins a generation while a reader verifies/loads atlas files.
// Release it after loading an independent in-memory dataset. Callers must still
// verify Active's snapshot and manifest bindings; this lease grants no trust.
func AcquireActive(active Active) (io.Closer, error) {
	root := filepath.Dir(active.AtlasPath)
	if filepath.Base(active.AtlasPath) != "atlas" || filepath.Base(root) != active.Generation {
		return nil, errors.New("active atlas is outside its generation")
	}
	return acquireGeneration(root, false)
}

func acquireGeneration(root string, exclusive bool) (*os.File, error) {
	if !generationPattern.MatchString(filepath.Base(root)) {
		return nil, errors.New("invalid generation directory")
	}
	if err := rejectSymlinks(root); err != nil {
		return nil, err
	}
	identity, err := privatepath.Lstat(root)
	if err != nil {
		return nil, err
	}
	if err := privatepath.CheckDir(root, identity); err != nil {
		return nil, err
	}
	marker, err := readPrivateFile(filepath.Join(root, "generation.txt"), 256)
	if err != nil {
		return nil, err
	}
	if string(marker) != SchemaVersion+"\n"+filepath.Base(root)+"\n" {
		return nil, errors.New("generation ownership marker mismatch")
	}
	path := filepath.Join(root, "readers.lock")
	before, err := privatepath.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := privatepath.CheckFile(path, before); err != nil {
		return nil, err
	}
	file, err := openLease(path)
	if err != nil {
		return nil, err
	}
	keep := false
	defer func() {
		if !keep {
			_ = file.Close()
		}
	}()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return nil, errors.New("generation lease identity changed")
	}
	if exclusive {
		err = lockExclusive(file)
	} else {
		err = lockShared(file)
	}
	if err != nil {
		return nil, err
	}
	if err := privatepath.CheckFile(path, before); err != nil {
		return nil, err
	}
	if err := privatepath.CheckDir(root, identity); err != nil {
		return nil, err
	}
	keep = true
	return file, nil
}

// Prune retains active and previous generations plus any older generation that
// a reader currently pins. It never follows symlinks, guesses ownership, or
// removes unmarked directories. Call after successful registry publication.
func (store *Store) Prune() error {
	if store.lock == nil {
		return errors.New("workspace writer is closed")
	}
	if err := privatepath.CheckDir(store.root, store.identity); err != nil {
		return err
	}
	root := filepath.Join(store.root, "generations")
	identity, err := privatepath.Lstat(root)
	if err != nil {
		return err
	}
	if err := privatepath.CheckDir(root, identity); err != nil {
		return err
	}
	directory, err := os.Open(root)
	if err != nil {
		return err
	}
	entries, err := directory.ReadDir(4097)
	closeErr := directory.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(entries) > 4096 {
		return errors.New("workspace generation cleanup exceeds directory bound; operator inspection required")
	}
	keep := map[string]bool{}
	for _, source := range store.Registry.Sources {
		for _, active := range []*Active{source.Active, source.Previous} {
			if active != nil {
				keep[active.Generation] = true
			}
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if keep[name] || !generationPattern.MatchString(name) {
			continue
		}
		path := filepath.Join(root, name)
		lease, err := acquireGeneration(path, true)
		if err != nil {
			continue
		} // Busy, replaced, unowned or unreadable: preserve.
		if err := privatepath.CheckDir(root, identity); err != nil {
			lease.Close()
			return err
		}
		// The generation is unreferenced in the serialized registry and cannot
		// acquire a new cooperative reader while this exclusive lease is held.
		err = os.RemoveAll(path)
		closeErr := lease.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return privatepath.SyncDirectoryStable(root, identity)
}
