// Package snapshot provides a crash-safe filesystem snapshot repository for
// local mode. It is deliberately independent of the static documentation
// exporter so canonical state and presentation artifacts can evolve separately.
package snapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/cas"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	storeMarkerName        = ".rkc-snapshot-store.json"
	buildingMarkerName     = ".rkc-snapshot-building.json"
	buildingLeaseName      = ".rkc-snapshot-lease"
	deleteQuarantinePrefix = ".rkc-delete-"
	ownershipSchema        = "1.0"
	ownershipProducer      = "rkc"
	storeMarkerKind        = "snapshot-store"
	buildingMarkerKind     = "snapshot-building"
	ownershipMarkerMax     = 4 * 1024
	buildingRecordMaxSize  = 64 * 1024
	currentFileMaxSize     = 1024
	maximumBundleSize      = 256 * 1024 * 1024
	maximumCoverageSize    = 16 * 1024 * 1024
)

var (
	ErrAlreadyCommitted  = errors.New("snapshot transaction already committed")
	ErrTransactionClosed = errors.New("snapshot transaction is closed")
	ErrSnapshotNotFound  = errors.New("snapshot not found")
	ErrStoreUnowned      = errors.New("snapshot store directory is not owned by RKC")
	ErrBuildingUnowned   = errors.New("snapshot building directory is not owned by RKC")
	ErrSnapshotPublished = errors.New("snapshot was published but finalization failed")
	ErrCurrentUpdate     = errors.New("snapshot was committed but CURRENT was not updated")
	ErrPublicationRace   = errors.New("snapshot publication identity could not be proven")
	ErrTransactionLive   = errors.New("snapshot transaction is still live")
)

type Store struct {
	root              string
	cas               *cas.Store
	identity          os.FileInfo
	marker            ownershipMarker
	buildingIdentity  os.FileInfo
	snapshotsIdentity os.FileInfo
}

// ownershipMarker is deliberately small and independent of snapshot content.
// Its exact value, together with directory inode identity, is the authority for
// recursive transaction cleanup.
type ownershipMarker struct {
	SchemaVersion string `json:"schema_version"`
	Producer      string `json:"producer"`
	Kind          string `json:"kind"`
	SnapshotID    string `json:"snapshot_id,omitempty"`
	DirectoryName string `json:"directory_name,omitempty"`
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
	store            *Store
	record           Record
	dir              string
	identity         os.FileInfo
	marker           ownershipMarker
	lease            *transactionLease
	expectedCoverage rkcmodel.Coverage
	hasBundle        bool
	closed           bool
	committed        bool
}

func Open(root string) (*Store, error) {
	absolute, err := safeoutput.ResolveTarget(root, "")
	if err != nil {
		return nil, fmt.Errorf("resolve snapshot store: %w", err)
	}
	identity, marker, err := openOwnedStoreRoot(absolute)
	if err != nil {
		return nil, err
	}
	layout := make(map[string]os.FileInfo, 3)
	for _, name := range []string{"building", "snapshots", "objects"} {
		info, err := ensureOwnedLayoutDirectory(absolute, identity, marker, name)
		if err != nil {
			return nil, err
		}
		layout[name] = info
	}
	objectStore, err := cas.Open(filepath.Join(absolute, "objects"))
	if err != nil {
		return nil, err
	}
	store := &Store{
		root: absolute, cas: objectStore, identity: identity, marker: marker,
		buildingIdentity: layout["building"], snapshotsIdentity: layout["snapshots"],
	}
	if err := store.validateRoot(); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *Store) Root() string    { return store.root }
func (store *Store) CAS() *cas.Store { return store.cas }

func (store *Store) Begin(snapshotID string, metadata map[string]string) (*Transaction, error) {
	if !validSnapshotID(snapshotID) {
		return nil, fmt.Errorf("invalid snapshot id %q", snapshotID)
	}
	if err := store.validateBuildingRoot(); err != nil {
		return nil, err
	}
	if err := store.validateSnapshotsRoot(); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(filepath.Join(store.root, "snapshots", snapshotID)); err == nil {
		return nil, fmt.Errorf("snapshot %s already exists", snapshotID)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	temp, err := os.MkdirTemp(filepath.Join(store.root, "building"), snapshotID+"-")
	if err != nil {
		return nil, fmt.Errorf("begin snapshot: %w", err)
	}
	identity, err := os.Lstat(temp)
	if err != nil || !identity.IsDir() || identity.Mode()&os.ModeSymlink != 0 {
		if err == nil {
			err = errors.New("created transaction path is not a regular directory")
		}
		_ = os.Remove(temp)
		return nil, fmt.Errorf("inspect snapshot transaction: %w", err)
	}
	leasePath := filepath.Join(temp, buildingLeaseName)
	lease, err := createTransactionLease(leasePath)
	if err != nil {
		_ = os.Remove(temp)
		return nil, fmt.Errorf("lease snapshot transaction: %w", err)
	}
	marker := ownershipMarker{
		SchemaVersion: ownershipSchema, Producer: ownershipProducer, Kind: buildingMarkerKind,
		SnapshotID: snapshotID, DirectoryName: filepath.Base(temp),
	}
	if _, err := writeOwnershipMarker(temp, buildingMarkerName, marker); err != nil {
		// Recursive cleanup is deliberately unavailable until both the marker
		// and bounded building record have been written and verified.
		_ = lease.Close()
		_ = removeExactRegularFile(leasePath, lease.identity)
		if current, statErr := os.Lstat(temp); statErr == nil && current.IsDir() && os.SameFile(identity, current) {
			_ = os.Remove(temp)
		}
		return nil, fmt.Errorf("mark snapshot transaction: %w", err)
	}
	record := Record{SnapshotID: snapshotID, Status: "building", CreatedAt: time.Now().UTC(), Metadata: cloneStrings(metadata)}
	transaction := &Transaction{store: store, record: record, dir: temp, identity: identity, marker: marker, lease: lease}
	if err := transaction.writeRecord(); err != nil {
		// Leave a marked but incomplete directory for operator inspection. It
		// cannot be recursively removed because it lacks a valid record.
		_ = transaction.closeLease()
		return nil, err
	}
	if _, err := validateBuildingDirectory(temp, identity, marker, "building"); err != nil {
		return nil, err
	}
	return transaction, nil
}

func (transaction *Transaction) WriteBundle(bundle rkcmodel.Bundle) error {
	if err := transaction.ensureOpen(); err != nil {
		return err
	}
	if err := transaction.validateBuilding("building"); err != nil {
		return err
	}
	if bundle.Snapshot.ID != transaction.record.SnapshotID {
		return fmt.Errorf("bundle snapshot %s does not match transaction %s", bundle.Snapshot.ID, transaction.record.SnapshotID)
	}
	canonical := rkcmodel.CanonicalBundle(bundle)
	if err := validateBundleForSnapshot(canonical); err != nil {
		return err
	}
	data, err := rkcmodel.CanonicalJSON(canonical)
	if err != nil {
		return fmt.Errorf("canonicalize bundle: %w", err)
	}
	if len(data) > maximumBundleSize {
		return fmt.Errorf("canonical bundle exceeds %d-byte safety limit", maximumBundleSize)
	}
	object, err := transaction.store.cas.PutBytes(data)
	if err != nil {
		return err
	}
	transaction.record.BundleDigest = object.Digest
	transaction.record.BundleObject = object.Digest
	// A rewritten bundle invalidates any coverage previously associated with
	// the transaction. Coverage is always an exact derivation of this object.
	transaction.record.CoverageDigest = ""
	transaction.record.CoverageObject = ""
	transaction.expectedCoverage = rkcmodel.BuildCoverage(canonical)
	transaction.hasBundle = true
	return transaction.writeRecord()
}

func (transaction *Transaction) WriteCoverage(coverage rkcmodel.Coverage) error {
	if err := transaction.ensureOpen(); err != nil {
		return err
	}
	if err := transaction.validateBuilding("building"); err != nil {
		return err
	}
	if !transaction.hasBundle || transaction.record.BundleObject == "" {
		return errors.New("cannot write coverage before a validated bundle")
	}
	if !reflect.DeepEqual(coverage, transaction.expectedCoverage) {
		return errors.New("coverage does not match the canonical bundle")
	}
	data, err := json.Marshal(coverage)
	if err != nil {
		return err
	}
	if len(data) > maximumCoverageSize {
		return fmt.Errorf("coverage exceeds %d-byte safety limit", maximumCoverageSize)
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
	if err := transaction.validateBuilding("building"); err != nil {
		return err
	}
	if err := transaction.store.validateSnapshotsRoot(); err != nil {
		return err
	}
	if transaction.record.BundleObject == "" {
		return errors.New("cannot commit snapshot without bundle object")
	}
	if !transaction.hasBundle {
		return errors.New("cannot commit snapshot without a validated bundle")
	}
	if err := validateRecordObjectBindings(transaction.record, false); err != nil {
		return err
	}
	if err := transaction.store.cas.Verify(transaction.record.BundleObject); err != nil {
		return fmt.Errorf("verify bundle object before commit: %w", err)
	}
	if transaction.record.CoverageObject != "" {
		if err := transaction.store.cas.Verify(transaction.record.CoverageObject); err != nil {
			return fmt.Errorf("verify coverage object before commit: %w", err)
		}
	}
	transaction.record.Status = "committed"
	transaction.record.CommittedAt = time.Now().UTC()
	if err := transaction.writeRecord(); err != nil {
		return err
	}
	if err := syncDirectory(transaction.dir); err != nil {
		return err
	}
	if err := transaction.validateBuilding("committed"); err != nil {
		return err
	}
	finalPath := filepath.Join(transaction.store.root, "snapshots", transaction.record.SnapshotID)
	// os.Rename cannot provide portable no-replace semantics for directories.
	// The destination check handles cooperative races; the exact-inode and
	// committed-record check immediately after rename fails closed against a
	// source replacement. A same-UID process able to mutate the store remains a
	// residual trust boundary until every supported OS has rename-no-replace.
	if _, err := os.Lstat(finalPath); err == nil {
		return fmt.Errorf("publish snapshot: destination already exists: %s", finalPath)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspect snapshot destination: %w", err)
	}
	if err := os.Rename(transaction.dir, finalPath); err != nil {
		return fmt.Errorf("publish snapshot: %w", err)
	}
	// Rename is the publication point. From here onward Abort must never target
	// the old building pathname, even if durability or CURRENT finalization
	// fails. The published snapshot can be loaded and CURRENT can be repaired.
	transaction.dir = finalPath
	transaction.committed = true
	transaction.closed = true
	if err := validatePublishedDirectory(finalPath, transaction.identity, transaction.marker, transaction.record.SnapshotID); err != nil {
		_ = transaction.closeLease()
		return fmt.Errorf("%w for %s: %v", ErrPublicationRace, transaction.record.SnapshotID, err)
	}
	if err := transaction.lease.validate(filepath.Join(finalPath, buildingLeaseName)); err != nil {
		_ = transaction.closeLease()
		return fmt.Errorf("%w for %s: %v", ErrPublicationRace, transaction.record.SnapshotID, err)
	}
	if err := transaction.store.validateSnapshotsRoot(); err != nil {
		_ = transaction.closeLease()
		return fmt.Errorf("%w: validate published snapshot directory: %v", ErrSnapshotPublished, err)
	}
	if err := syncDirectory(filepath.Dir(finalPath)); err != nil {
		_ = transaction.closeLease()
		return fmt.Errorf("%w: sync published snapshot directory: %v", ErrSnapshotPublished, err)
	}
	if err := transaction.store.validateSnapshotsRoot(); err != nil {
		_ = transaction.closeLease()
		return fmt.Errorf("%w: published snapshot directory changed while syncing: %v", ErrSnapshotPublished, err)
	}
	if err := validatePublishedDirectory(finalPath, transaction.identity, transaction.marker, transaction.record.SnapshotID); err != nil {
		_ = transaction.closeLease()
		return fmt.Errorf("%w for %s after sync: %v", ErrPublicationRace, transaction.record.SnapshotID, err)
	}
	if err := transaction.store.writeCurrent(transaction.record.SnapshotID); err != nil {
		_ = transaction.closeLease()
		return fmt.Errorf("%w for %s: %v; call SetCurrent to repair", ErrCurrentUpdate, transaction.record.SnapshotID, err)
	}
	if err := transaction.closeLease(); err != nil {
		return fmt.Errorf("%w: release published transaction lease: %v", ErrSnapshotPublished, err)
	}
	return nil
}

func (transaction *Transaction) Abort(reason string) error {
	if transaction.closed {
		return nil
	}
	if err := transaction.validateBuilding("building", "failed", "committed"); err != nil {
		return err
	}
	transaction.record.Status = "failed"
	if transaction.record.Metadata == nil {
		transaction.record.Metadata = map[string]string{}
	}
	transaction.record.Metadata["abort_reason"] = reason
	if err := transaction.writeRecord(); err != nil {
		return err
	}
	// Keep validation immediately adjacent to recursive removal. Neither an
	// earlier marker read nor a matching directory name grants delete authority.
	if err := removeBuildingDirectory(transaction.dir, transaction.identity, transaction.marker, transaction.lease, "failed"); err != nil {
		_ = transaction.closeLease()
		return err
	}
	transaction.closed = true
	transaction.dir = ""
	if err := transaction.closeLease(); err != nil {
		return err
	}
	return nil
}

func (transaction *Transaction) closeLease() error {
	if transaction == nil || transaction.lease == nil {
		return nil
	}
	err := transaction.lease.Close()
	transaction.lease = nil
	return err
}

func (transaction *Transaction) ensureOpen() error {
	if transaction.closed {
		return ErrTransactionClosed
	}
	return nil
}

func (transaction *Transaction) writeRecord() error {
	if err := transaction.validateBuildingMarker(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(transaction.record, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > buildingRecordMaxSize {
		return fmt.Errorf("snapshot building record exceeds %d-byte safety limit", buildingRecordMaxSize)
	}
	return writeAtomic(filepath.Join(transaction.dir, "snapshot.json"), data, 0o644)
}

func (store *Store) CurrentID() (string, error) {
	if err := store.validateRoot(); err != nil {
		return "", err
	}
	data, err := readBoundedRegular(filepath.Join(store.root, "CURRENT"), currentFileMaxSize)
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
	if !validSnapshotID(value) {
		return "", errors.New("CURRENT contains an invalid snapshot id")
	}
	if err := store.validateRoot(); err != nil {
		return "", err
	}
	return value, nil
}

// SetCurrent atomically points CURRENT at an already-published, fully valid
// snapshot. It is also the explicit repair path when Commit published the
// snapshot but returned ErrCurrentUpdate.
func (store *Store) SetCurrent(snapshotID string) error {
	if !validSnapshotID(snapshotID) {
		return fmt.Errorf("invalid snapshot id %q", snapshotID)
	}
	if _, _, _, err := store.Load(snapshotID); err != nil {
		return fmt.Errorf("validate current snapshot %s: %w", snapshotID, err)
	}
	return store.writeCurrent(snapshotID)
}

func (store *Store) writeCurrent(snapshotID string) error {
	if err := store.validateRoot(); err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(store.root, "CURRENT"), []byte(snapshotID+"\n"), 0o644); err != nil {
		return err
	}
	return store.validateRoot()
}

func (store *Store) LoadCurrent() (rkcmodel.Bundle, rkcmodel.Coverage, Record, error) {
	id, err := store.CurrentID()
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	return store.Load(id)
}

func (store *Store) Load(snapshotID string) (rkcmodel.Bundle, rkcmodel.Coverage, Record, error) {
	if !validSnapshotID(snapshotID) {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, fmt.Errorf("invalid snapshot id %q", snapshotID)
	}
	if err := store.validateSnapshotsRoot(); err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	snapshotRoot := filepath.Join(store.root, "snapshots", snapshotID)
	snapshotInfo, err := os.Lstat(snapshotRoot)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, ErrSnapshotNotFound
		}
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	if snapshotInfo.Mode()&os.ModeSymlink != 0 || !snapshotInfo.IsDir() {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, errors.New("snapshot path is not a regular directory")
	}
	recordPath := filepath.Join(store.root, "snapshots", snapshotID, "snapshot.json")
	var record Record
	if err := readBoundedJSONRegular(recordPath, buildingRecordMaxSize, &record); err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	if err := validateCommittedRecord(record, snapshotID); err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	if current, err := os.Lstat(snapshotRoot); err != nil || !current.IsDir() || !os.SameFile(snapshotInfo, current) {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, errors.New("snapshot directory changed while loading")
	}
	bundleBytes, err := store.cas.ReadBytesBounded(record.BundleObject, true, maximumBundleSize)
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	var bundle rkcmodel.Bundle
	if err := decodeStrictJSON(bundleBytes, &bundle); err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	canonicalBundle, err := rkcmodel.CanonicalJSON(bundle)
	if err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, fmt.Errorf("canonicalize stored bundle: %w", err)
	}
	if !bytes.Equal(bundleBytes, canonicalBundle) {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, errors.New("stored bundle is not canonical JSON")
	}
	if bundle.Snapshot.ID != snapshotID {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, errors.New("stored bundle snapshot id does not match its record")
	}
	if err := validateBundleForSnapshot(bundle); err != nil {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
	}
	expectedCoverage := rkcmodel.BuildCoverage(bundle)
	coverage := expectedCoverage
	if record.CoverageObject != "" {
		coverageBytes, err := store.cas.ReadBytesBounded(record.CoverageObject, true, maximumCoverageSize)
		if err != nil {
			return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
		}
		if err := decodeStrictJSON(coverageBytes, &coverage); err != nil {
			return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
		}
		canonicalCoverage, err := json.Marshal(coverage)
		if err != nil {
			return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, err
		}
		if !bytes.Equal(coverageBytes, canonicalCoverage) {
			return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, errors.New("stored coverage is not canonical JSON")
		}
		if !reflect.DeepEqual(coverage, expectedCoverage) {
			return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, errors.New("stored coverage does not match the canonical bundle")
		}
	}
	if current, err := os.Lstat(snapshotRoot); err != nil || !current.IsDir() || !os.SameFile(snapshotInfo, current) {
		return rkcmodel.Bundle{}, rkcmodel.Coverage{}, Record{}, errors.New("snapshot directory changed while loading")
	}
	return bundle, coverage, record, nil
}

func (store *Store) List() ([]Record, error) {
	if err := store.validateSnapshotsRoot(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(store.root, "snapshots"))
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, entry := range entries {
		if !entry.IsDir() || !validSnapshotID(entry.Name()) {
			continue
		}
		snapshotPath := filepath.Join(store.root, "snapshots", entry.Name())
		snapshotInfo, err := os.Lstat(snapshotPath)
		if err != nil || snapshotInfo.Mode()&os.ModeSymlink != 0 || !snapshotInfo.IsDir() {
			return nil, errors.New("snapshot list entry is not a stable regular directory")
		}
		var record Record
		if err := readBoundedJSONRegular(filepath.Join(snapshotPath, "snapshot.json"), buildingRecordMaxSize, &record); err != nil {
			return nil, err
		}
		if err := validateCommittedRecord(record, entry.Name()); err != nil {
			return nil, fmt.Errorf("snapshot list contains an invalid committed record: %w", err)
		}
		if current, err := os.Lstat(snapshotPath); err != nil || !current.IsDir() || !os.SameFile(snapshotInfo, current) {
			return nil, errors.New("snapshot list entry changed while reading")
		}
		records = append(records, record)
	}
	if err := store.validateSnapshotsRoot(); err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CommittedAt.Equal(records[j].CommittedAt) {
			return records[i].SnapshotID < records[j].SnapshotID
		}
		return records[i].CommittedAt.After(records[j].CommittedAt)
	})
	return records, nil
}

// Recover removes unlocked abandoned building directories older than maxAge.
// A zero maxAge means "any age", never "ignore liveness": live transactions
// are always skipped because their advisory lease cannot be acquired.
func (store *Store) Recover(maxAge time.Duration) ([]string, error) {
	if err := store.validateBuildingRoot(); err != nil {
		return nil, err
	}
	root := filepath.Join(store.root, "building")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var removed []string
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		info, err := os.Lstat(path)
		if err != nil {
			return removed, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			continue
		}
		if strings.HasPrefix(entry.Name(), deleteQuarantinePrefix) {
			// A private quarantine is only transient during an in-process delete.
			// Never guess that an unmarked quarantine is safe after a crash.
			return removed, fmt.Errorf("%w: incomplete delete quarantine requires operator inspection: %s", ErrBuildingUnowned, path)
		}
		marker, err := readOwnershipMarker(path, buildingMarkerName)
		if err != nil {
			return removed, fmt.Errorf("%w: %s: %v", ErrBuildingUnowned, path, err)
		}
		if _, err := validateBuildingDirectory(path, info, marker, "building", "failed", "committed"); err != nil {
			return removed, err
		}
		lease, live, err := acquireAbandonedTransactionLease(filepath.Join(path, buildingLeaseName))
		if err != nil {
			return removed, err
		}
		if live {
			continue
		}
		if maxAge > 0 && now.Sub(info.ModTime()) < maxAge {
			_ = lease.Close()
			continue
		}
		if err := removeBuildingDirectory(path, info, marker, lease, "building", "failed", "committed"); err != nil {
			_ = lease.Close()
			return removed, err
		}
		if err := lease.Close(); err != nil {
			return removed, err
		}
		removed = append(removed, entry.Name())
	}
	sort.Strings(removed)
	return removed, nil
}

func openOwnedStoreRoot(root string) (os.FileInfo, ownershipMarker, error) {
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
			return nil, ownershipMarker{}, fmt.Errorf("create snapshot store parent: %w", err)
		}
		if err := os.Mkdir(root, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, ownershipMarker{}, fmt.Errorf("create snapshot store: %w", err)
		}
		info, err = os.Lstat(root)
	}
	if err != nil {
		return nil, ownershipMarker{}, fmt.Errorf("inspect snapshot store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, ownershipMarker{}, fmt.Errorf("%w: root is not a regular directory", ErrStoreUnowned)
	}

	want := ownershipMarker{SchemaVersion: ownershipSchema, Producer: ownershipProducer, Kind: storeMarkerKind}
	marker, markerErr := readOwnershipMarker(root, storeMarkerName)
	if errors.Is(markerErr, fs.ErrNotExist) {
		nonempty, readErr := directoryHasEntries(root)
		if readErr != nil {
			return nil, ownershipMarker{}, readErr
		}
		if nonempty {
			return nil, ownershipMarker{}, fmt.Errorf("%w: refusing to adopt nonempty directory %s", ErrStoreUnowned, root)
		}
		markerIdentity, writeErr := writeOwnershipMarker(root, storeMarkerName, want)
		if writeErr != nil {
			return nil, ownershipMarker{}, fmt.Errorf("initialize snapshot store ownership: %w", writeErr)
		}
		soleMarker, readErr := directoryContainsOnly(root, storeMarkerName)
		if readErr != nil || !soleMarker {
			// Roll back only the exact marker file created by this call. Never
			// remove other directory content discovered after the empty check.
			_ = removeExactRegularFile(filepath.Join(root, storeMarkerName), markerIdentity)
			if readErr != nil {
				return nil, ownershipMarker{}, readErr
			}
			return nil, ownershipMarker{}, fmt.Errorf("%w: directory changed while ownership was initialized", ErrStoreUnowned)
		}
		marker = want
	} else if markerErr != nil {
		return nil, ownershipMarker{}, fmt.Errorf("%w: invalid ownership marker: %v", ErrStoreUnowned, markerErr)
	}
	if marker != want {
		return nil, ownershipMarker{}, fmt.Errorf("%w: ownership marker does not identify an RKC snapshot store", ErrStoreUnowned)
	}
	if err := validateOwnedStoreDirectory(root, info, marker); err != nil {
		return nil, ownershipMarker{}, err
	}
	return info, marker, nil
}

func ensureOwnedLayoutDirectory(root string, rootIdentity os.FileInfo, marker ownershipMarker, name string) (os.FileInfo, error) {
	if err := validateOwnedStoreDirectory(root, rootIdentity, marker); err != nil {
		return nil, err
	}
	path := filepath.Join(root, name)
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(path, 0o755); err != nil {
			return nil, fmt.Errorf("create snapshot store directory %s: %w", name, err)
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return nil, fmt.Errorf("inspect snapshot store directory %s: %w", name, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%w: %s is not a regular directory", ErrStoreUnowned, path)
	}
	if err := validateOwnedStoreDirectory(root, rootIdentity, marker); err != nil {
		return nil, err
	}
	return info, nil
}

func (store *Store) validateRoot() error {
	if store == nil || store.identity == nil {
		return fmt.Errorf("%w: missing store identity", ErrStoreUnowned)
	}
	return validateOwnedStoreDirectory(store.root, store.identity, store.marker)
}

func (store *Store) validateBuildingRoot() error {
	if err := store.validateRoot(); err != nil {
		return err
	}
	return validateLayoutIdentity(filepath.Join(store.root, "building"), store.buildingIdentity)
}

func (store *Store) validateSnapshotsRoot() error {
	if err := store.validateRoot(); err != nil {
		return err
	}
	return validateLayoutIdentity(filepath.Join(store.root, "snapshots"), store.snapshotsIdentity)
}

func validateLayoutIdentity(path string, identity os.FileInfo) error {
	if identity == nil {
		return fmt.Errorf("%w: missing layout identity for %s", ErrStoreUnowned, path)
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(identity, current) {
		return fmt.Errorf("%w: layout directory identity changed for %s", ErrStoreUnowned, path)
	}
	return nil
}

func validateOwnedStoreDirectory(path string, identity os.FileInfo, expected ownershipMarker) error {
	if identity == nil {
		return fmt.Errorf("%w: missing directory identity", ErrStoreUnowned)
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(identity, current) {
		return fmt.Errorf("%w: store directory identity changed", ErrStoreUnowned)
	}
	marker, err := readOwnershipMarker(path, storeMarkerName)
	if err != nil || marker != expected {
		return fmt.Errorf("%w: store ownership marker changed", ErrStoreUnowned)
	}
	final, err := os.Lstat(path)
	if err != nil || !final.IsDir() || !os.SameFile(identity, final) {
		return fmt.Errorf("%w: store directory changed during validation", ErrStoreUnowned)
	}
	return nil
}

func (transaction *Transaction) validateBuildingMarker() error {
	if transaction == nil || transaction.identity == nil || transaction.dir == "" {
		return fmt.Errorf("%w: missing transaction identity", ErrBuildingUnowned)
	}
	current, err := os.Lstat(transaction.dir)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(transaction.identity, current) {
		return fmt.Errorf("%w: transaction directory identity changed", ErrBuildingUnowned)
	}
	marker, err := readOwnershipMarker(transaction.dir, buildingMarkerName)
	if err != nil || marker != transaction.marker || marker.DirectoryName != filepath.Base(transaction.dir) {
		return fmt.Errorf("%w: transaction ownership marker changed", ErrBuildingUnowned)
	}
	return nil
}

func (transaction *Transaction) validateBuilding(statuses ...string) error {
	if err := transaction.validateBuildingMarker(); err != nil {
		return err
	}
	_, err := validateBuildingDirectory(transaction.dir, transaction.identity, transaction.marker, statuses...)
	return err
}

func validateBuildingDirectory(path string, identity os.FileInfo, expected ownershipMarker, statuses ...string) (Record, error) {
	if identity == nil || expected != (ownershipMarker{
		SchemaVersion: ownershipSchema, Producer: ownershipProducer, Kind: buildingMarkerKind,
		SnapshotID: expected.SnapshotID, DirectoryName: filepath.Base(path),
	}) || strings.TrimSpace(expected.SnapshotID) == "" {
		return Record{}, fmt.Errorf("%w: invalid expected transaction marker", ErrBuildingUnowned)
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(identity, current) {
		return Record{}, fmt.Errorf("%w: transaction directory identity changed", ErrBuildingUnowned)
	}
	marker, err := readOwnershipMarker(path, buildingMarkerName)
	if err != nil || marker != expected {
		return Record{}, fmt.Errorf("%w: transaction ownership marker changed", ErrBuildingUnowned)
	}
	var record Record
	if err := readBoundedJSONRegular(filepath.Join(path, "snapshot.json"), buildingRecordMaxSize, &record); err != nil {
		return Record{}, fmt.Errorf("%w: invalid building record: %v", ErrBuildingUnowned, err)
	}
	if record.SnapshotID != expected.SnapshotID || record.CreatedAt.IsZero() || !containsString(statuses, record.Status) {
		return Record{}, fmt.Errorf("%w: building record does not match transaction ownership", ErrBuildingUnowned)
	}
	var bindingErr error
	if record.Status == "committed" {
		bindingErr = validateCommittedRecord(record, expected.SnapshotID)
	} else {
		bindingErr = validateRecordObjectBindings(record, false)
	}
	if bindingErr != nil {
		return Record{}, fmt.Errorf("%w: invalid building record object binding: %v", ErrBuildingUnowned, bindingErr)
	}
	final, err := os.Lstat(path)
	if err != nil || !final.IsDir() || !os.SameFile(identity, final) {
		return Record{}, fmt.Errorf("%w: transaction directory changed during validation", ErrBuildingUnowned)
	}
	return record, nil
}

func removeBuildingDirectory(path string, identity os.FileInfo, marker ownershipMarker, lease *transactionLease, statuses ...string) error {
	if _, err := validateBuildingDirectory(path, identity, marker, statuses...); err != nil {
		return err
	}
	if err := lease.validate(filepath.Join(path, buildingLeaseName)); err != nil {
		return err
	}
	quarantineRoot, err := os.MkdirTemp(filepath.Dir(path), deleteQuarantinePrefix)
	if err != nil {
		return fmt.Errorf("create private snapshot delete quarantine: %w", err)
	}
	rootIdentity, err := os.Lstat(quarantineRoot)
	if err != nil || !rootIdentity.IsDir() || rootIdentity.Mode().Perm()&0o077 != 0 {
		_ = os.Remove(quarantineRoot)
		return fmt.Errorf("%w: snapshot delete quarantine is not owner-only", ErrBuildingUnowned)
	}
	payload := filepath.Join(quarantineRoot, filepath.Base(path))
	if err := os.Rename(path, payload); err != nil {
		_ = os.Remove(quarantineRoot)
		return fmt.Errorf("quarantine snapshot transaction: %w", err)
	}
	if _, err := validateBuildingDirectory(payload, identity, marker, statuses...); err != nil {
		return fmt.Errorf("%w; mismatched quarantine retained at %s", err, payload)
	}
	if err := lease.validate(filepath.Join(payload, buildingLeaseName)); err != nil {
		return fmt.Errorf("%w; quarantine retained at %s", err, payload)
	}
	if err := syncDirectory(quarantineRoot); err != nil {
		return fmt.Errorf("sync snapshot delete quarantine: %w", err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync snapshot building directory after quarantine: %w", err)
	}
	// The rename above binds deletion to the validated inode and prevents a
	// pathname replacement in the public building directory from being deleted.
	// A malicious same-UID process can still mutate this 0700 quarantine; that is
	// the explicit residual trust boundary until deletion is fd-relative.
	if err := os.RemoveAll(payload); err != nil {
		return fmt.Errorf("remove quarantined snapshot transaction: %w; retained at %s", err, payload)
	}
	currentRoot, err := os.Lstat(quarantineRoot)
	if err != nil || !currentRoot.IsDir() || !os.SameFile(rootIdentity, currentRoot) {
		return fmt.Errorf("%w: delete quarantine identity changed", ErrBuildingUnowned)
	}
	if err := os.Remove(quarantineRoot); err != nil {
		return fmt.Errorf("remove empty snapshot delete quarantine: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func validatePublishedDirectory(path string, identity os.FileInfo, expected ownershipMarker, snapshotID string) error {
	if identity == nil || expected.Kind != buildingMarkerKind || expected.SnapshotID != snapshotID {
		return errors.New("invalid expected published snapshot identity")
	}
	current, err := os.Lstat(path)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !current.IsDir() || !os.SameFile(identity, current) {
		return errors.New("published snapshot directory identity changed")
	}
	marker, err := readOwnershipMarker(path, buildingMarkerName)
	if err != nil || marker != expected {
		return errors.New("published snapshot ownership marker changed")
	}
	var record Record
	if err := readBoundedJSONRegular(filepath.Join(path, "snapshot.json"), buildingRecordMaxSize, &record); err != nil {
		return fmt.Errorf("read published snapshot record: %w", err)
	}
	if err := validateCommittedRecord(record, snapshotID); err != nil {
		return fmt.Errorf("validate published snapshot record: %w", err)
	}
	final, err := os.Lstat(path)
	if err != nil || !final.IsDir() || !os.SameFile(identity, final) {
		return errors.New("published snapshot directory changed during validation")
	}
	return nil
}

func readOwnershipMarker(root, name string) (ownershipMarker, error) {
	var marker ownershipMarker
	if err := readBoundedJSONRegular(filepath.Join(root, name), ownershipMarkerMax, &marker); err != nil {
		return ownershipMarker{}, err
	}
	if marker.SchemaVersion != ownershipSchema || marker.Producer != ownershipProducer ||
		(marker.Kind != storeMarkerKind && marker.Kind != buildingMarkerKind) {
		return ownershipMarker{}, errors.New("invalid snapshot ownership marker")
	}
	return marker, nil
}

func readBoundedJSONRegular(path string, maximum int64, destination any) error {
	data, err := readBoundedRegular(path, maximum)
	if err != nil {
		return err
	}
	return decodeStrictJSON(data, destination)
}

func decodeStrictJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid trailing JSON content: %w", err)
	}
	return nil
}

func validateBundleForSnapshot(bundle rkcmodel.Bundle) error {
	report := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{
		StrictVocabulary: true,
		RequireEvidence:  true,
	})
	if !report.HasErrors() {
		return nil
	}
	errorCount := 0
	var first rkcmodel.Diagnostic
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Severity != "error" && diagnostic.Severity != "fatal" {
			continue
		}
		if errorCount == 0 {
			first = diagnostic
		}
		errorCount++
	}
	return fmt.Errorf("bundle validation failed with %d error(s); first %s: %s", errorCount, first.Code, first.Message)
}

func validateCommittedRecord(record Record, snapshotID string) error {
	if record.SnapshotID != snapshotID || record.Status != "committed" ||
		record.CreatedAt.IsZero() || record.CommittedAt.IsZero() || record.CommittedAt.Before(record.CreatedAt) {
		return errors.New("snapshot record is not a valid committed record")
	}
	return validateRecordObjectBindings(record, true)
}

func validateRecordObjectBindings(record Record, requireBundle bool) error {
	if err := validateDigestPair("bundle", record.BundleDigest, record.BundleObject, requireBundle); err != nil {
		return err
	}
	return validateDigestPair("coverage", record.CoverageDigest, record.CoverageObject, false)
}

func validateDigestPair(name, digest, object string, required bool) error {
	if digest == "" && object == "" {
		if required {
			return fmt.Errorf("%s object binding is required", name)
		}
		return nil
	}
	if digest == "" || object == "" {
		return fmt.Errorf("%s digest and object must both be present", name)
	}
	normalizedDigest, digestErr := cas.NormalizeDigest(digest)
	normalizedObject, objectErr := cas.NormalizeDigest(object)
	if digestErr != nil || objectErr != nil || normalizedDigest != digest || normalizedObject != object {
		return fmt.Errorf("%s object binding is not a canonical sha256 digest", name)
	}
	if digest != object {
		return fmt.Errorf("%s digest and object disagree", name)
	}
	return nil
}

func readBoundedRegular(path string, maximum int64) ([]byte, error) {
	if maximum <= 0 {
		return nil, errors.New("file safety limit must be positive")
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || before.Size() > maximum {
		return nil, errors.New("file is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) || opened.Size() > maximum {
		return nil, errors.New("file identity changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, errors.New("file exceeds safety limit")
	}
	after, err := file.Stat()
	if err != nil || !os.SameFile(opened, after) || after.Size() != int64(len(data)) {
		return nil, errors.New("file changed while reading")
	}
	return data, nil
}

func validSnapshotID(value string) bool {
	return value != "" && len(value) <= 255 && strings.TrimSpace(value) == value &&
		value != "." && value != ".." && filepath.Base(value) == value &&
		!strings.ContainsAny(value, "/\\\x00")
}

func writeOwnershipMarker(root, name string, marker ownershipMarker) (os.FileInfo, error) {
	data, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	if len(data) > ownershipMarkerMax {
		return nil, errors.New("snapshot ownership marker exceeds safety limit")
	}
	path := filepath.Join(root, name)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return nil, err
	}
	identity, statErr := file.Stat()
	committed := false
	defer func() {
		_ = file.Close()
		if !committed && identity != nil {
			_ = removeExactRegularFile(path, identity)
		}
	}()
	if statErr != nil {
		return nil, statErr
	}
	if _, err := file.Write(data); err != nil {
		return nil, err
	}
	if err := file.Sync(); err != nil {
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	if err := syncDirectory(root); err != nil {
		return nil, err
	}
	committed = true
	return identity, nil
}

func removeExactRegularFile(path string, identity os.FileInfo) error {
	if identity == nil {
		return errors.New("missing file identity")
	}
	current, err := os.Lstat(path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(identity, current) {
		return errors.New("refusing to remove changed file identity")
	}
	return os.Remove(path)
}

func directoryHasEntries(path string) (bool, error) {
	directory, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer directory.Close()
	names, err := directory.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return len(names) != 0, nil
}

func directoryContainsOnly(path, expected string) (bool, error) {
	directory, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer directory.Close()
	names, err := directory.Readdirnames(2)
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	return len(names) == 1 && names[0] == expected, nil
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
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
