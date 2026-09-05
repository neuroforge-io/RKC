package snapshot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/cas"
	"github.com/neuroforge-io/RKC/internal/privatepath"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestTransactionLeaseProvesLivenessAndIdentity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lease")
	lease, err := createTransactionLease(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.validate(path); err != nil {
		t.Fatalf("validate(live lease) = %v", err)
	}
	if _, live, err := acquireAbandonedTransactionLease(path); err != nil || !live {
		t.Fatalf("acquireAbandonedTransactionLease(live) live=%v err=%v", live, err)
	}
	if _, err := createTransactionLease(path); err == nil {
		t.Fatal("createTransactionLease(existing) succeeded")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	abandoned, live, err := acquireAbandonedTransactionLease(path)
	if err != nil || live || abandoned == nil {
		t.Fatalf("acquire abandoned lease = %v, live=%v, err=%v", abandoned, live, err)
	}
	if err := abandoned.validate(path); err != nil {
		t.Fatal(err)
	}
	if err := abandoned.Close(); err != nil {
		t.Fatal(err)
	}

	if _, _, err := acquireAbandonedTransactionLease(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("acquire missing lease = %v", err)
	}
	directory := filepath.Join(t.TempDir(), "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := acquireAbandonedTransactionLease(directory); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("acquire directory lease = %v", err)
	}
	var nilLease *transactionLease
	if err := nilLease.validate(path); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("nil lease validate = %v", err)
	}
	if err := nilLease.Close(); err != nil {
		t.Fatalf("nil lease Close = %v", err)
	}

	replacement := filepath.Join(t.TempDir(), "replacement")
	if err := os.WriteFile(replacement, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	replacementInfo, err := privatepath.Lstat(replacement)
	if err != nil {
		t.Fatal(err)
	}
	closedLease := &transactionLease{file: mustOpenSnapshotFile(t, replacement), identity: replacementInfo}
	if err := os.Remove(replacement); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := closedLease.validate(replacement); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("validate(replaced path) = %v", err)
	}
	_ = closedLease.Close()
}

func TestTransactionLeaseRejectsUnreadableAndInvalidPaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "unreadable")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	if _, live, err := acquireAbandonedTransactionLease(path); err == nil || live {
		t.Fatalf("acquire unreadable lease = live=%v err=%v", live, err)
	}

	if _, _, err := acquireAbandonedTransactionLease("invalid\x00lease"); err == nil {
		t.Fatal("acquire invalid lease path succeeded")
	}

	validPath := filepath.Join(root, "valid")
	lease, err := createTransactionLease(validPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := lease.validate(validPath); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("validate closed lease = %v", err)
	}
	if err := os.Remove(validPath); err != nil {
		t.Fatal(err)
	}
	if err := lease.validate(validPath); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("validate missing path = %v", err)
	}
	lease.file = nil
	if err := lease.Close(); err != nil {
		t.Fatalf("close cleared lease = %v", err)
	}
}

func mustOpenSnapshotFile(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := openLeaseFile(path, false)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestSnapshotValidationPrimitives(t *testing.T) {
	var nilStore *Store
	if err := nilStore.validateRoot(); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("nil validateRoot() = %v", err)
	}
	if err := nilStore.validateBuildingRoot(); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("nil validateBuildingRoot() = %v", err)
	}
	if err := nilStore.validateSnapshotsRoot(); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("nil validateSnapshotsRoot() = %v", err)
	}
	if err := validateLayoutIdentity("missing", nil); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("validateLayoutIdentity(nil) = %v", err)
	}
	if err := validateOwnedStoreDirectory("missing", nil, ownershipMarker{}); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("validateOwnedStoreDirectory(nil) = %v", err)
	}
	var nilTransaction *Transaction
	if err := nilTransaction.closeLease(); err != nil {
		t.Fatalf("nil closeLease() = %v", err)
	}
	if err := (&Transaction{closed: true}).ensureOpen(); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("ensureOpen(closed) = %v", err)
	}
	if err := (&Transaction{}).ensureOpen(); err != nil {
		t.Fatalf("ensureOpen(open) = %v", err)
	}
	if err := (&Transaction{}).validateBuildingMarker(); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("validateBuildingMarker(empty) = %v", err)
	}
}

func TestStrictJSONDigestAndSnapshotIDValidation(t *testing.T) {
	var destination map[string]any
	if err := decodeStrictJSON([]byte(`{"ok":true}`), &destination); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{`{`, `{"ok":true}{"again":true}`, `{"ok":true} trailing`} {
		if err := decodeStrictJSON([]byte(input), &destination); err == nil {
			t.Errorf("decodeStrictJSON(%q) succeeded", input)
		}
	}

	digest := cas.DigestBytes([]byte("object"))
	tests := []struct {
		name                  string
		digest, object        string
		required, wantFailure bool
	}{
		{name: "optional absent"},
		{name: "required absent", required: true, wantFailure: true},
		{name: "only digest", digest: digest, wantFailure: true},
		{name: "noncanonical", digest: strings.ToUpper(digest), object: strings.ToUpper(digest), wantFailure: true},
		{name: "disagreement", digest: digest, object: cas.DigestBytes([]byte("other")), wantFailure: true},
		{name: "valid", digest: digest, object: digest, required: true},
	}
	for _, test := range tests {
		err := validateDigestPair(test.name, test.digest, test.object, test.required)
		if (err != nil) != test.wantFailure {
			t.Errorf("validateDigestPair(%s) = %v", test.name, err)
		}
	}
	if err := validateRecordObjectBindings(Record{BundleDigest: digest}, false); err == nil {
		t.Fatal("validateRecordObjectBindings accepted partial bundle binding")
	}
	now := time.Now().UTC()
	if err := validateCommittedRecord(Record{SnapshotID: "id", Status: "committed", CreatedAt: now, CommittedAt: now.Add(-time.Second), BundleDigest: digest, BundleObject: digest}, "id"); err == nil {
		t.Fatal("validateCommittedRecord accepted backwards timestamp")
	}

	for _, valid := range []string{"a", "snapshot-123", strings.Repeat("x", 255)} {
		if !validSnapshotID(valid) {
			t.Errorf("validSnapshotID(%q) = false", valid)
		}
	}
	for _, invalid := range []string{"", " ", ".", "..", " a", "a ", "a/b", `a\b`, strings.Repeat("x", 256)} {
		if validSnapshotID(invalid) {
			t.Errorf("validSnapshotID(%q) = true", invalid)
		}
	}
}

func TestBoundedFilesMarkersAndDirectoryHelpers(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "data.json")
	if err := os.WriteFile(file, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readBoundedRegular(file, 0); err == nil {
		t.Fatal("readBoundedRegular(nonpositive limit) succeeded")
	}
	if _, err := readBoundedRegular(file, 1); err == nil {
		t.Fatal("readBoundedRegular(too small) succeeded")
	}
	if data, err := readBoundedRegular(file, 2); err != nil || string(data) != "{}" {
		t.Fatalf("readBoundedRegular(valid) = %q, %v", data, err)
	}
	if _, err := readBoundedRegular(root, 10); err == nil {
		t.Fatal("readBoundedRegular(directory) succeeded")
	}
	if _, err := readBoundedRegular(filepath.Join(root, "missing"), 10); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readBoundedRegular(missing) = %v", err)
	}

	marker := ownershipMarker{SchemaVersion: ownershipSchema, Producer: ownershipProducer, Kind: storeMarkerKind}
	if _, err := writeOwnershipMarker(root, storeMarkerName, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := writeOwnershipMarker(root, storeMarkerName, marker); err == nil {
		t.Fatal("writeOwnershipMarker(existing) succeeded")
	}
	loaded, err := readOwnershipMarker(root, storeMarkerName)
	if err != nil || loaded != marker {
		t.Fatalf("readOwnershipMarker() = %+v, %v", loaded, err)
	}
	if err := os.WriteFile(filepath.Join(root, "invalid-marker"), []byte(`{"schema_version":"0","producer":"rkc","kind":"snapshot-store"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOwnershipMarker(root, "invalid-marker"); err == nil {
		t.Fatal("readOwnershipMarker(invalid) succeeded")
	}

	empty := filepath.Join(root, "empty")
	if err := os.Mkdir(empty, 0o700); err != nil {
		t.Fatal(err)
	}
	if present, err := directoryHasEntries(empty); err != nil || present {
		t.Fatalf("directoryHasEntries(empty) = %v, %v", present, err)
	}
	if only, err := directoryContainsOnly(empty, "wanted"); err != nil || only {
		t.Fatalf("directoryContainsOnly(empty) = %v, %v", only, err)
	}
	if err := os.WriteFile(filepath.Join(empty, "wanted"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if present, err := directoryHasEntries(empty); err != nil || !present {
		t.Fatalf("directoryHasEntries(nonempty) = %v, %v", present, err)
	}
	if only, err := directoryContainsOnly(empty, "wanted"); err != nil || !only {
		t.Fatalf("directoryContainsOnly(single) = %v, %v", only, err)
	}
	if _, err := directoryHasEntries(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directoryHasEntries(missing) = %v", err)
	}
	if _, err := directoryContainsOnly(filepath.Join(root, "missing"), "wanted"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directoryContainsOnly(missing) = %v", err)
	}
	if containsString([]string{"a", "b"}, "c") || !containsString([]string{"a", "b"}, "b") {
		t.Fatal("containsString mismatch")
	}
	if err := syncDirectory(filepath.Join(root, "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("syncDirectory(missing) = %v", err)
	}
}

func TestExactFileRemovalAndCurrentValidation(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := privatepath.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := removeExactRegularFile(path, nil); err == nil {
		t.Fatal("removeExactRegularFile(nil) succeeded")
	}
	if err := removeExactRegularFile(filepath.Join(root, "missing"), identity); err == nil {
		t.Fatal("removeExactRegularFile(missing) succeeded")
	}
	if err := removeExactRegularFile(root, identity); err == nil {
		t.Fatal("removeExactRegularFile(wrong identity) succeeded")
	}
	if err := removeExactRegularFile(path, identity); err != nil {
		t.Fatal(err)
	}

	store, err := Open(filepath.Join(root, "store"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrent("../unsafe"); err == nil {
		t.Fatal("SetCurrent(invalid) succeeded")
	}
	if err := store.SetCurrent("missing"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("SetCurrent(missing) = %v", err)
	}
	if _, _, _, err := store.Load("../unsafe"); err == nil {
		t.Fatal("Load(invalid) succeeded")
	}
}

func TestTransactionAndStoreAdversarialValidationBranches(t *testing.T) {
	store := openTestStore(t)
	transaction, err := store.Begin("coverage-errors", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteBundle(testBundle("different", "mismatch")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("WriteBundle(mismatched snapshot) = %v", err)
	}
	if err := transaction.WriteCoverage(transaction.expectedCoverage); err == nil || !strings.Contains(err.Error(), "before a validated bundle") {
		t.Fatalf("WriteCoverage(before bundle) = %v", err)
	}
	if err := transaction.Commit(); err == nil || !strings.Contains(err.Error(), "without bundle object") {
		t.Fatalf("Commit(no bundle object) = %v", err)
	}

	originalRecord := transaction.record
	digest := cas.DigestBytes([]byte("not actually stored"))
	transaction.record.BundleObject = digest
	transaction.record.BundleDigest = digest
	if err := transaction.Commit(); err == nil || !strings.Contains(err.Error(), "without a validated bundle") {
		t.Fatalf("Commit(unvalidated bundle) = %v", err)
	}
	transaction.hasBundle = true
	transaction.record.BundleDigest = ""
	if err := transaction.Commit(); err == nil || !strings.Contains(err.Error(), "both be present") {
		t.Fatalf("Commit(partial binding) = %v", err)
	}
	transaction.record.BundleDigest = digest
	if err := transaction.Commit(); err == nil || !strings.Contains(err.Error(), "verify bundle object") {
		t.Fatalf("Commit(missing bound object) = %v", err)
	}
	transaction.record = originalRecord
	transaction.hasBundle = false

	if _, err := validateBuildingDirectory(transaction.dir, nil, transaction.marker, "building"); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("validateBuildingDirectory(nil identity) = %v", err)
	}
	wrongMarker := transaction.marker
	wrongMarker.Kind = "wrong"
	if _, err := validateBuildingDirectory(transaction.dir, transaction.identity, wrongMarker, "building"); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("validateBuildingDirectory(wrong expected marker) = %v", err)
	}
	if _, err := validateBuildingDirectory(transaction.dir, transaction.identity, transaction.marker, "committed"); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("validateBuildingDirectory(wrong status) = %v", err)
	}
	if err := validatePublishedDirectory(transaction.dir, nil, transaction.marker, transaction.record.SnapshotID); err == nil {
		t.Fatal("validatePublishedDirectory(nil identity) succeeded")
	}
	if err := validatePublishedDirectory(transaction.dir, transaction.identity, transaction.marker, "different"); err == nil {
		t.Fatal("validatePublishedDirectory(wrong snapshot) succeeded")
	}
	if err := validatePublishedDirectory(transaction.dir, transaction.identity, transaction.marker, transaction.record.SnapshotID); err == nil || !strings.Contains(err.Error(), "committed") {
		t.Fatalf("validatePublishedDirectory(building record) = %v", err)
	}
	if err := transaction.Abort("coverage cleanup"); err != nil {
		t.Fatal(err)
	}

	coverageTransaction, err := store.Begin("coverage-object-errors", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := coverageTransaction.WriteBundle(testBundle("coverage-object-errors", "bundle")); err != nil {
		t.Fatal(err)
	}
	missingCoverage := cas.DigestBytes([]byte("missing coverage"))
	coverageTransaction.record.CoverageDigest = missingCoverage
	coverageTransaction.record.CoverageObject = missingCoverage
	if err := coverageTransaction.Commit(); err == nil || !strings.Contains(err.Error(), "verify coverage object") {
		t.Fatalf("Commit(missing coverage object) = %v", err)
	}
	if err := coverageTransaction.Abort("coverage cleanup"); err != nil {
		t.Fatal(err)
	}

	wrongStoreMarker := store.marker
	wrongStoreMarker.Producer = "other"
	if err := validateOwnedStoreDirectory(store.root, store.identity, wrongStoreMarker); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("validateOwnedStoreDirectory(wrong marker) = %v", err)
	}
	layoutFile := filepath.Join(store.root, "layout-file")
	if err := os.WriteFile(layoutFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureOwnedLayoutDirectory(store.root, store.identity, store.marker, "layout-file"); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("ensureOwnedLayoutDirectory(file) = %v", err)
	}

	quarantine := filepath.Join(store.root, "building", deleteQuarantinePrefix+"operator")
	if err := os.Mkdir(quarantine, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recover(0); !errors.Is(err, ErrBuildingUnowned) || !strings.Contains(err.Error(), "operator inspection") {
		t.Fatalf("Recover(interrupted quarantine) = %v", err)
	}
	if err := os.Remove(quarantine); err != nil {
		t.Fatal(err)
	}

	badSnapshot := filepath.Join(store.root, "snapshots", "not-a-directory")
	if err := os.WriteFile(badSnapshot, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Load("not-a-directory"); err == nil || !strings.Contains(err.Error(), "not a regular directory") {
		t.Fatalf("Load(snapshot file) = %v", err)
	}
	if err := os.WriteFile(filepath.Join(store.root, "CURRENT"), []byte("../invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CurrentID(); err == nil || !strings.Contains(err.Error(), "invalid snapshot id") {
		t.Fatalf("CurrentID(invalid content) = %v", err)
	}
}

func TestHighLevelMethodsFailClosedOnBrokenIdentities(t *testing.T) {
	store := openTestStore(t)
	broken := *store
	broken.identity = nil
	if _, err := broken.Begin("broken", nil); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("Begin(broken root) = %v", err)
	}
	if _, err := broken.CurrentID(); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("CurrentID(broken root) = %v", err)
	}
	if err := broken.writeCurrent("broken"); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("writeCurrent(broken root) = %v", err)
	}
	if _, _, _, err := broken.Load("broken"); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("Load(broken root) = %v", err)
	}
	if _, err := broken.List(); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("List(broken root) = %v", err)
	}
	if _, err := broken.Recover(0); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("Recover(broken root) = %v", err)
	}

	brokenBuilding := *store
	brokenBuilding.buildingIdentity = nil
	if _, err := brokenBuilding.Begin("broken-building", nil); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("Begin(broken building layout) = %v", err)
	}
	brokenSnapshots := *store
	brokenSnapshots.snapshotsIdentity = nil
	if _, err := brokenSnapshots.Begin("broken-snapshots", nil); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("Begin(broken snapshots layout) = %v", err)
	}

	invalidTransaction := &Transaction{store: store, record: Record{SnapshotID: "broken"}}
	if err := invalidTransaction.WriteBundle(testBundle("broken", "bundle")); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("WriteBundle(missing transaction identity) = %v", err)
	}
	if err := invalidTransaction.WriteCoverage(invalidTransaction.expectedCoverage); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("WriteCoverage(missing transaction identity) = %v", err)
	}
	if err := invalidTransaction.Commit(); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("Commit(missing transaction identity) = %v", err)
	}
	if err := invalidTransaction.writeRecord(); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("writeRecord(missing transaction identity) = %v", err)
	}
}

func TestOpenDetectsCorruptNestedLayouts(t *testing.T) {
	root := filepath.Join(t.TempDir(), "layout-file")
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	building := filepath.Join(root, "building")
	if err := os.Remove(building); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(building, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("Open(building file) = %v", err)
	}

	root = filepath.Join(t.TempDir(), "cas-layout-file")
	if _, err := Open(root); err != nil {
		t.Fatal(err)
	}
	shaRoot := filepath.Join(root, "objects", "sha256")
	if err := os.Remove(shaRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(shaRoot, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); err == nil {
		t.Fatal("Open(CAS sha256 file) succeeded")
	}

	underFile := filepath.Join(t.TempDir(), "parent-file")
	if err := os.WriteFile(underFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openOwnedStoreRoot(filepath.Join(underFile, "child")); err == nil {
		t.Fatal("openOwnedStoreRoot(path beneath file) succeeded")
	}

	marked := filepath.Join(t.TempDir(), "marked")
	if err := os.Mkdir(marked, 0o700); err != nil {
		t.Fatal(err)
	}
	extraMarker := ownershipMarker{SchemaVersion: ownershipSchema, Producer: ownershipProducer, Kind: storeMarkerKind, SnapshotID: "unexpected"}
	if _, err := writeOwnershipMarker(marked, storeMarkerName, extraMarker); err != nil {
		t.Fatal(err)
	}
	if _, _, err := openOwnedStoreRoot(marked); !errors.Is(err, ErrStoreUnowned) || !strings.Contains(err.Error(), "does not identify") {
		t.Fatalf("openOwnedStoreRoot(extra marker fields) = %v", err)
	}
	_ = store
}

func TestBuildingValidationRejectsMarkerRecordAndInodeDrift(t *testing.T) {
	t.Run("marker", func(t *testing.T) {
		store := openTestStore(t)
		transaction, err := store.Begin("marker-drift", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = transaction.closeLease() })
		markerPath := filepath.Join(transaction.dir, buildingMarkerName)
		if err := os.WriteFile(markerPath, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := transaction.validateBuildingMarker(); !errors.Is(err, ErrBuildingUnowned) {
			t.Fatalf("validateBuildingMarker(marker drift) = %v", err)
		}
	})

	t.Run("record", func(t *testing.T) {
		store := openTestStore(t)
		transaction, err := store.Begin("record-drift", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = transaction.closeLease() })
		recordPath := filepath.Join(transaction.dir, "snapshot.json")
		if err := os.WriteFile(recordPath, []byte("{\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := validateBuildingDirectory(
			transaction.dir,
			transaction.identity,
			transaction.marker,
			"building",
		); !errors.Is(err, ErrBuildingUnowned) || !strings.Contains(err.Error(), "invalid building record") {
			t.Fatalf("validateBuildingDirectory(record drift) = %v", err)
		}
	})

	t.Run("object-binding", func(t *testing.T) {
		store := openTestStore(t)
		transaction, err := store.Begin("binding-drift", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = transaction.closeLease() })
		transaction.record.BundleDigest = cas.DigestBytes([]byte("partial"))
		if err := transaction.writeRecord(); err != nil {
			t.Fatal(err)
		}
		if _, err := validateBuildingDirectory(
			transaction.dir,
			transaction.identity,
			transaction.marker,
			"building",
		); !errors.Is(err, ErrBuildingUnowned) || !strings.Contains(err.Error(), "object binding") {
			t.Fatalf("validateBuildingDirectory(binding drift) = %v", err)
		}
	})

	t.Run("inode", func(t *testing.T) {
		store := openTestStore(t)
		transaction, err := store.Begin("inode-drift", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = transaction.closeLease() })
		original := transaction.dir + "-original"
		if err := os.Rename(transaction.dir, original); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(transaction.dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := validateBuildingDirectory(
			transaction.dir,
			transaction.identity,
			transaction.marker,
			"building",
		); !errors.Is(err, ErrBuildingUnowned) {
			t.Fatalf("validateBuildingDirectory(inode drift) = %v", err)
		}
	})
}

func TestTransactionStoragePublicationAndLeaseErrorsFailClosed(t *testing.T) {
	t.Run("object-store", func(t *testing.T) {
		store := openTestStore(t)
		transaction, err := store.Begin("object-store-failure", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = transaction.closeLease() })
		shaRoot := filepath.Join(store.Root(), "objects", "sha256")
		if err := os.Rename(shaRoot, shaRoot+"-original"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(shaRoot, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := transaction.WriteBundle(
			testBundle(transaction.record.SnapshotID, "object-store"),
		); err == nil {
			t.Fatal("WriteBundle with replaced object-store layout succeeded")
		}
	})

	t.Run("snapshot-layout", func(t *testing.T) {
		store := openTestStore(t)
		transaction, err := store.Begin("snapshot-layout-failure", nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = transaction.closeLease() })
		if err := transaction.WriteBundle(
			testBundle(transaction.record.SnapshotID, "snapshot-layout"),
		); err != nil {
			t.Fatal(err)
		}
		snapshots := filepath.Join(store.Root(), "snapshots")
		if err := os.Rename(snapshots, snapshots+"-original"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(snapshots, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := transaction.Commit(); !errors.Is(err, ErrStoreUnowned) {
			t.Fatalf("Commit(replaced snapshot layout) = %v", err)
		}
	})

	t.Run("closed-lease", func(t *testing.T) {
		store := openTestStore(t)
		transaction, err := store.Begin("closed-lease-abort", nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := transaction.closeLease(); err != nil {
			t.Fatal(err)
		}
		if err := transaction.Abort("must retain without lease"); !errors.Is(err, ErrBuildingUnowned) {
			t.Fatalf("Abort(closed lease) = %v", err)
		}
		if _, err := privatepath.Lstat(transaction.dir); err != nil {
			t.Fatalf("Abort(closed lease) removed unproven transaction: %v", err)
		}
	})
}

func TestPublishedValidationRejectsMarkerAndRecordDrift(t *testing.T) {
	prepare := func(t *testing.T, id string) *Transaction {
		t.Helper()
		store := openTestStore(t)
		transaction, err := store.Begin(id, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = transaction.closeLease() })
		if err := transaction.WriteBundle(testBundle(id, id)); err != nil {
			t.Fatal(err)
		}
		transaction.record.Status = "committed"
		transaction.record.CommittedAt = time.Now().UTC()
		if err := transaction.writeRecord(); err != nil {
			t.Fatal(err)
		}
		return transaction
	}

	t.Run("marker", func(t *testing.T) {
		transaction := prepare(t, "published-marker-drift")
		if err := os.WriteFile(
			filepath.Join(transaction.dir, buildingMarkerName),
			[]byte("{}\n"),
			0o644,
		); err != nil {
			t.Fatal(err)
		}
		if err := validatePublishedDirectory(
			transaction.dir,
			transaction.identity,
			transaction.marker,
			transaction.record.SnapshotID,
		); err == nil || !strings.Contains(err.Error(), "ownership marker changed") {
			t.Fatalf("validatePublishedDirectory(marker drift) = %v", err)
		}
	})

	t.Run("record", func(t *testing.T) {
		transaction := prepare(t, "published-record-drift")
		if err := os.WriteFile(
			filepath.Join(transaction.dir, "snapshot.json"),
			[]byte("{\n"),
			0o644,
		); err != nil {
			t.Fatal(err)
		}
		if err := validatePublishedDirectory(
			transaction.dir,
			transaction.identity,
			transaction.marker,
			transaction.record.SnapshotID,
		); err == nil || !strings.Contains(err.Error(), "read published snapshot record") {
			t.Fatalf("validatePublishedDirectory(record drift) = %v", err)
		}
	})
}

func TestStoreAndLayoutIdentityDriftFailsClosed(t *testing.T) {
	t.Run("store-marker", func(t *testing.T) {
		store := openTestStore(t)
		if err := os.WriteFile(
			filepath.Join(store.Root(), storeMarkerName),
			[]byte("{}\n"),
			0o644,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := store.CurrentID(); !errors.Is(err, ErrStoreUnowned) {
			t.Fatalf("CurrentID(store marker drift) = %v", err)
		}
	})

	for _, name := range []string{"building", "snapshots"} {
		t.Run(name, func(t *testing.T) {
			store := openTestStore(t)
			path := filepath.Join(store.Root(), name)
			if err := os.Rename(path, path+"-original"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
			var err error
			if name == "building" {
				_, err = store.Recover(0)
			} else {
				_, _, _, err = store.Load("missing")
			}
			if !errors.Is(err, ErrStoreUnowned) {
				t.Fatalf("%s layout replacement = %v", name, err)
			}
		})
	}
}

func TestRecoverDefersFreshAbandonedTransaction(t *testing.T) {
	store := openTestStore(t)
	transaction, err := store.Begin("fresh-abandoned", nil)
	if err != nil {
		t.Fatal(err)
	}
	directoryName := filepath.Base(transaction.dir)
	if err := transaction.closeLease(); err != nil {
		t.Fatal(err)
	}

	removed, err := store.Recover(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("Recover(maxAge) removed fresh abandoned transaction: %v", removed)
	}
	if _, err := privatepath.Lstat(transaction.dir); err != nil {
		t.Fatalf("Recover(maxAge) changed fresh transaction: %v", err)
	}

	removed, err = store.Recover(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0] != directoryName {
		t.Fatalf("Recover(0) = %v, want %q", removed, directoryName)
	}
}

func TestLoadRejectsNonCanonicalCoverageAndUnknownRecordFields(t *testing.T) {
	store := openTestStore(t)
	bundle := testBundle("noncanonical-coverage", "coverage")
	bundleBytes, err := rkcmodel.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	bundleObject, err := store.CAS().PutBytes(bundleBytes)
	if err != nil {
		t.Fatal(err)
	}
	coverageBytes, err := json.MarshalIndent(rkcmodel.BuildCoverage(bundle), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	coverageObject, err := store.CAS().PutBytes(coverageBytes)
	if err != nil {
		t.Fatal(err)
	}
	writeRecordForTest(t, store, Record{
		SnapshotID:     bundle.Snapshot.ID,
		Status:         "committed",
		BundleObject:   bundleObject.Digest,
		CoverageObject: coverageObject.Digest,
	})
	if _, _, _, err := store.Load(bundle.Snapshot.ID); err == nil || !strings.Contains(err.Error(), "coverage is not canonical") {
		t.Fatalf("Load(noncanonical coverage) = %v", err)
	}

	unknownID := "unknown-record-field"
	unknownRoot := filepath.Join(store.Root(), "snapshots", snapshotDirectoryName(unknownID))
	if err := os.Mkdir(unknownRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(unknownRoot, "snapshot.json"),
		[]byte(`{"unknown":true}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Load(unknownID); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Load(unknown record field) = %v", err)
	}
}

func TestMarkerAndAtomicWriteSafetyLimits(t *testing.T) {
	root := t.TempDir()
	marker := ownershipMarker{
		SchemaVersion: ownershipSchema,
		Producer:      ownershipProducer,
		Kind:          buildingMarkerKind,
		SnapshotID:    strings.Repeat("x", ownershipMarkerMax),
		DirectoryName: "oversized",
	}
	if _, err := writeOwnershipMarker(root, "oversized-marker", marker); err == nil || !strings.Contains(err.Error(), "safety limit") {
		t.Fatalf("writeOwnershipMarker(oversized) = %v", err)
	}
	if _, err := privatepath.Lstat(filepath.Join(root, "oversized-marker")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversized ownership marker was published: %v", err)
	}

	parentFile := filepath.Join(root, "parent-file")
	if err := os.WriteFile(parentFile, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(filepath.Join(parentFile, "child"), []byte("data"), 0o600); err == nil {
		t.Fatal("writeAtomic beneath regular file succeeded")
	}
	if data, err := os.ReadFile(parentFile); err != nil || string(data) != "preserve" {
		t.Fatalf("writeAtomic changed parent file: %q, %v", data, err)
	}
}
