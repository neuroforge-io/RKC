// Package snapshot provides a crash-safe filesystem snapshot repository for
// local mode. It is deliberately independent of the static documentation
// exporter so canonical state and presentation artifacts can evolve separately.
package snapshot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/repository-knowledge-compiler/rkc/internal/cas"
	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

var (
	ErrAlreadyCommitted  = errors.New("snapshot transaction already committed")
	ErrTransactionClosed = errors.New("snapshot transaction is closed")
	ErrSnapshotNotFound  = errors.New("snapshot not found")
)

type Store struct {
	root string
	cas  *cas.Store
}

type Record struct {
	SnapshotID     string            `json:"snapshot_id"`
	Status         string            `json:"status"`
	BundleDigest   string            `json:"bundle_digest"`
	CoverageDigest string            `json:"coverage_digest,omitempty"`
	BundleObject   string            `json:"bundle_object"`
	CoverageObject string            `json:"coverage_object,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	CommittedAt    time.Time         `json:"committed_at,omitempty"`
	Metadata       map[string]string `json:"metadata,omitempty"`
}

type Transaction struct {
	store     *Store
	record    Record
	dir       string
	closed    bool
	committed bool
}

func Open(root string) (*Store, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot store: %w", err)
	}
	for _, name := range []string{"building", "snapshots", "objects"} {
		if err := os.MkdirAll(filepath.Join(absolute, name), 0o755); err != nil {
			return nil, fmt.Errorf("create snapshot store directory: %w", err)
		}
	}
	objectStore, err := cas.Open(filepath.Join(absolute, "objects"))
	if err != nil {
		return nil, err
	}
	return &Store{root: absolute, cas: objectStore}, nil
}

func (store *Store) Root() string    { return store.root }
func (store *Store) CAS() *cas.Store { return store.cas }

func (store *Store) Begin(snapshotID string, metadata map[string]string) (*Transaction, error) {
	if strings.TrimSpace(snapshotID) == "" || strings.ContainsAny(snapshotID, `/\\`) {
		return nil, fmt.Errorf("invalid snapshot id %q", snapshotID)
	}
	if _, err := os.Stat(filepath.Join(store.root, "snapshots", snapshotID)); err == nil {
		return nil, fmt.Errorf("snapshot %s already exists", snapshotID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	temp, err := os.MkdirTemp(filepath.Join(store.root, "building"), snapshotID+"-")
	if err != nil {
		return nil, fmt.Errorf("begin snapshot: %w", err)
	}
	record := Record{SnapshotID: snapshotID, Status: "building", CreatedAt: time.Now().UTC(), Metadata: cloneStrings(metadata)}
	transaction := &Transaction{store: store, record: record, dir: temp}
	if err := transaction.writeRecord(); err != nil {
		_ = os.RemoveAll(temp)
		return nil, err
	}
	return transaction, nil
}

func (transaction *Transaction) WriteBundle(bundle rkcmodel.Bundle) error {
	if err := transaction.ensureOpen(); err != nil {
		return err
	}
	if bundle.Snapshot.ID != "" && bundle.Snapshot.ID != transaction.record.SnapshotID {
		return fmt.Errorf("bundle snapshot %s does not match transaction %s", bundle.Snapshot.ID, transaction.record.SnapshotID)
	}
	data, err := rkcmodel.CanonicalJSON(bundle)
	if err != nil {
		return fmt.Errorf("canonicalize bundle: %w", err)
	}
	object, err := transaction.store.cas.PutBytes(data)
	if err != nil {
		return err
	}
	transaction.record.BundleDigest = object.Digest
	transaction.record.BundleObject = object.Digest
	return transaction.writeRecord()
}

func (transaction *Transaction) WriteCoverage(coverage rkcmodel.Coverage) error {
	if err := transaction.ensureOpen(); err != nil {
		return err
	}
	data, err := json.Marshal(coverage)
	if err != nil {
		return err
	}
	object, err := transaction.store.cas.PutBytes(data)
	if err != nil {
		return err
	}
	transaction.record.CoverageDigest = object.Digest
	transaction.record.CoverageObject = object.Digest
	return transaction.writeRecord()
}

func (transaction *Transaction) Commit() error {
	if transaction.committed {
		return ErrAlreadyCommitted
	}
	if err := transaction.ensureOpen(); err != nil {
		return err
	}
	if transaction.record.BundleObject == "" {
		return errors.New("cannot commit snapshot without bundle object")
	}
	transaction.record.Status = "committed"
	transaction.record.CommittedAt = time.Now().UTC()
	if err := transaction.writeRecord(); err != nil {
		return err
	}
	if err := syncDirectory(transaction.dir); err != nil {
		return err
	}
	finalPath := filepath.Join(transaction.store.root, "snapshots", transaction.record.SnapshotID)
	if err := os.Rename(transaction.dir, finalPath); err != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}
	if err := syncDirectory(filepath.Dir(finalPath)); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(transaction.store.root, "CURRENT"), []byte(transaction.record.SnapshotID+"\n"), 0o644); err != nil {
		return fmt.Errorf("update current snapshot: %w", err)
	}
	transaction.dir = finalPath
	transaction.committed = true
	transaction.closed = true
	return nil
}

func (transaction *Transaction) Abort(reason string) error {
	if transaction.closed {
		return nil
	}
	transaction.record.Status = "failed"
	if transaction.record.Metadata == nil {
		transaction.record.Metadata = map[string]string{}
	}
	transaction.record.Metadata["abort_reason"] = reason
	_ = transaction.writeRecord()
	transaction.closed = true
	return os.RemoveAll(transaction.dir)
}

func (transaction *Transaction) ensureOpen() error {
	if transaction.closed {
		return ErrTransactionClosed
	}
	return nil
}

func (transaction *Transaction) writeRecord() error {
	data, err := json.MarshalIndent(transaction.record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomic(filepath.Join(transaction.dir, "snapshot.json"), data, 0o644)
}

func (store *Store) CurrentID() (string, error) {
	data, err := os.ReadFile(filepath.Join(store.root, "CURRENT"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", ErrSnapshotNotFound
	}
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return "", ErrSnapshotNotFound
	}
	return value, nil
}

func (store *Store) LoadCurrent() (rkcmodel.Bundle, rkcmodel.Coverage, Record, error) {
	id, err := store.CurrentID()
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	return store.Load(id)
}

func (store *Store) Load(snapshotID string) (rkcmodel.Bundle, rkcmodel.Coverage, Record, error) {
	recordPath := filepath.Join(store.root, "snapshots", snapshotID, "snapshot.json")
	data, err := os.ReadFile(recordPath)
	if errors.Is(err, fs.ErrNotExist) {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, ErrSnapshotNotFound
	}
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	bundleBytes, err := store.cas.ReadBytes(record.BundleObject, true)
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	var bundle rkcmodel.Bundle
	if err := json.Unmarshal(bundleBytes, &bundle); err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	var coverage rkcmodel.Coverage
	if record.CoverageObject != "" {
		coverageBytes, err := store.cas.ReadBytes(record.CoverageObject, true)
		if err != nil {
			return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
		}
		if err := json.Unmarshal(coverageBytes, &coverage); err != nil {
			return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
		}
	} else {
		coverage = rkcmodel.BuildCoverage(bundle)
	}
	return bundle, coverage, record, nil
}

func (store *Store) List() ([]Record, error) {
	entries, err := os.ReadDir(filepath.Join(store.root, "snapshots"))
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(store.root, "snapshots", entry.Name(), "snapshot.json"))
		if err != nil {
			return nil, err
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CommittedAt.Equal(records[j].CommittedAt) {
			return records[i].SnapshotID < records[j].SnapshotID
		}
		return records[i].CommittedAt.After(records[j].CommittedAt)
	})
	return records, nil
}

// Recover removes abandoned building directories older than maxAge. A zero
// maxAge removes every abandoned build, suitable for a single-process CLI.
func (store *Store) Recover(maxAge time.Duration) ([]string, error) {
	root := filepath.Join(store.root, "building")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var removed []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return removed, err
		}
		if maxAge > 0 && now.Sub(info.ModTime()) < maxAge {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			return removed, err
		}
		removed = append(removed, entry.Name())
	}
	sort.Strings(removed)
	return removed, nil
}

func cloneStrings(input map[string]string) map[string]string {
	if input == nil {
		return nil
	}
	output := make(map[string]string, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".atomic-")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	committed := false
	defer func() {
		_ = temp.Close()
		if !committed {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(mode); err != nil {
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
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	committed = true
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
