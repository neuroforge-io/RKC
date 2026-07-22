package snapshot

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/cas"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func testBundle(id, marker string) rkcmodel.Bundle {
	return rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{
			SchemaVersion: rkcmodel.SchemaVersion,
			ID:            id,
			Status:        "committed",
			RootName:      marker,
			RootPath:      "/machine/local",
			ContentDigest: strings.Repeat("a", 64),
			Tool:          rkcmodel.ToolInfo{Name: "rkc-test", Version: "1"},
		},
		Artifacts: []rkcmodel.Artifact{{ID: "artifact:" + marker, Path: marker + ".go", Kind: "file", Status: "parsed", Text: true}},
		Nodes:     []rkcmodel.Node{{ID: "node:" + marker, Kind: "function", Name: marker, Visibility: "public", EvidenceIDs: []string{"evidence:" + marker}}},
		Evidence:  []rkcmodel.Evidence{{ID: "evidence:" + marker, Kind: "declared", Method: "test", Confidence: 1}},
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(store.Root()) || store.CAS() == nil {
		t.Fatalf("Open() returned invalid store: root=%q cas=%v", store.Root(), store.CAS())
	}
	return store
}

func TestOpenCreatesLayoutAndRejectsFileRoot(t *testing.T) {
	store := openTestStore(t)
	marker, err := readOwnershipMarker(store.Root(), storeMarkerName)
	if err != nil || marker.Kind != storeMarkerKind {
		t.Fatalf("store ownership marker = %+v, %v", marker, err)
	}
	if info, err := os.Lstat(filepath.Join(store.Root(), storeMarkerName)); err != nil || info.Size() > ownershipMarkerMax {
		t.Fatalf("bounded store marker: info=%v err=%v", info, err)
	}
	for _, directory := range []string{"building", "snapshots", "objects", filepath.Join("objects", "sha256")} {
		info, err := os.Stat(filepath.Join(store.Root(), directory))
		if err != nil || !info.IsDir() {
			t.Fatalf("layout %q: info=%v err=%v", directory, info, err)
		}
	}

	fileRoot := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileRoot, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(fileRoot); err == nil {
		t.Fatal("Open(file) succeeded, want error")
	}
}

func TestOpenRefusesUnownedNonemptyCorruptAndSymlinkRoots(t *testing.T) {
	unowned := filepath.Join(t.TempDir(), "unowned")
	if err := os.Mkdir(unowned, 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(unowned, "user-data")
	if err := os.WriteFile(userFile, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(unowned); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("Open(nonempty unowned) = %v, want ErrStoreUnowned", err)
	}
	if data, err := os.ReadFile(userFile); err != nil || string(data) != "preserve" {
		t.Fatalf("Open changed unowned data: %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(unowned, storeMarkerName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Open marked rejected directory: %v", err)
	}

	corrupt := filepath.Join(t.TempDir(), "corrupt")
	if err := os.Mkdir(corrupt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(corrupt, storeMarkerName), []byte(strings.Repeat("x", ownershipMarkerMax+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(corrupt); !errors.Is(err, ErrStoreUnowned) {
		t.Fatalf("Open(oversized marker) = %v, want ErrStoreUnowned", err)
	}

	real := t.TempDir()
	alias := filepath.Join(filepath.Dir(real), filepath.Base(real)+"-alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(alias); err == nil {
		t.Fatal("Open(symlink root) succeeded")
	}
}

func TestBeginRejectsUnsafeIDsAndClonesMetadata(t *testing.T) {
	store := openTestStore(t)
	for _, id := range []string{"", "   ", ".", "..", "../escape", `..\escape`, "nested/id"} {
		if _, err := store.Begin(id, nil); err == nil || !strings.Contains(err.Error(), "invalid snapshot id") {
			t.Errorf("Begin(%q) error = %v, want invalid ID", id, err)
		}
	}

	metadata := map[string]string{"source": "original"}
	transaction, err := store.Begin("safe-id", metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadata["source"] = "mutated"
	if transaction.record.Metadata["source"] != "original" {
		t.Fatalf("transaction metadata aliases caller: %v", transaction.record.Metadata)
	}
	if transaction.record.Status != "building" || transaction.record.CreatedAt.IsZero() {
		t.Fatalf("initial record = %+v", transaction.record)
	}
	data, err := os.ReadFile(filepath.Join(transaction.dir, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Fatal("initial snapshot record is not newline terminated")
	}
	marker, err := readOwnershipMarker(transaction.dir, buildingMarkerName)
	if err != nil || marker.SnapshotID != "safe-id" || marker.DirectoryName != filepath.Base(transaction.dir) {
		t.Fatalf("building ownership marker = %+v, %v", marker, err)
	}
	if err := transaction.Abort("test complete"); err != nil {
		t.Fatal(err)
	}
}

func TestBeginDoesNotRecursivelyCleanIncompleteOwnedBuild(t *testing.T) {
	store := openTestStore(t)
	_, err := store.Begin("oversized-record", map[string]string{"oversized": strings.Repeat("x", buildingRecordMaxSize)})
	if err == nil || !strings.Contains(err.Error(), "safety limit") {
		t.Fatalf("Begin(oversized record) = %v, want bounded-record error", err)
	}
	entries, readErr := os.ReadDir(filepath.Join(store.Root(), "building"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || !entries[0].IsDir() {
		t.Fatalf("incomplete build was recursively removed: %v", entries)
	}
	if _, markerErr := readOwnershipMarker(filepath.Join(store.Root(), "building", entries[0].Name()), buildingMarkerName); markerErr != nil {
		t.Fatalf("incomplete build lost ownership marker: %v", markerErr)
	}
}

func TestCommitLoadCurrentAndTransactionLifecycle(t *testing.T) {
	store := openTestStore(t)
	if _, err := store.CurrentID(); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("CurrentID() before commit = %v, want ErrSnapshotNotFound", err)
	}
	if _, _, _, err := store.LoadCurrent(); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("LoadCurrent() before commit = %v, want ErrSnapshotNotFound", err)
	}

	transaction, err := store.Begin("snapshot-1", map[string]string{"origin": "test"})
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err == nil || !strings.Contains(err.Error(), "without bundle") {
		t.Fatalf("Commit() without bundle = %v", err)
	}
	if err := transaction.WriteBundle(testBundle("different", "wrong")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("WriteBundle(mismatch) = %v", err)
	}
	bundle := testBundle("snapshot-1", "one")
	coverage := rkcmodel.BuildCoverage(bundle)
	if err := transaction.WriteBundle(bundle); err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteCoverage(coverage); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	if !transaction.committed || !transaction.closed || !strings.Contains(transaction.dir, filepath.Join("snapshots", "snapshot-1")) {
		t.Fatalf("committed transaction state = %+v", transaction)
	}

	id, err := store.CurrentID()
	if err != nil || id != "snapshot-1" {
		t.Fatalf("CurrentID() = %q, %v", id, err)
	}
	currentBytes, err := os.ReadFile(filepath.Join(store.Root(), "CURRENT"))
	if err != nil || string(currentBytes) != "snapshot-1\n" {
		t.Fatalf("CURRENT = %q, %v", currentBytes, err)
	}
	loadedBundle, loadedCoverage, record, err := store.LoadCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if loadedBundle.Snapshot.ID != bundle.Snapshot.ID || loadedBundle.Snapshot.RootPath != "" || loadedBundle.Nodes[0].Name != "one" {
		t.Fatalf("loaded bundle = %+v", loadedBundle)
	}
	if !reflect.DeepEqual(loadedCoverage, coverage) {
		t.Fatalf("loaded coverage = %+v, want %+v", loadedCoverage, coverage)
	}
	if record.Status != "committed" || record.BundleObject == "" || record.CoverageObject == "" || record.CommittedAt.IsZero() || record.Metadata["origin"] != "test" {
		t.Fatalf("loaded record = %+v", record)
	}
	if record.BundleDigest != record.BundleObject || record.CoverageDigest != record.CoverageObject {
		t.Fatalf("digest/object fields diverged: %+v", record)
	}

	if err := transaction.Commit(); !errors.Is(err, ErrAlreadyCommitted) {
		t.Fatalf("second Commit() = %v, want ErrAlreadyCommitted", err)
	}
	if err := transaction.WriteBundle(bundle); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("WriteBundle(closed) = %v, want ErrTransactionClosed", err)
	}
	if err := transaction.WriteCoverage(coverage); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("WriteCoverage(closed) = %v, want ErrTransactionClosed", err)
	}
	if err := transaction.Abort("ignored"); err != nil {
		t.Fatalf("Abort(committed) = %v", err)
	}
	if _, err := store.Begin("snapshot-1", nil); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("Begin(existing) = %v", err)
	}
}

func TestWriteBundleAndCoverageRequireSemanticBinding(t *testing.T) {
	store := openTestStore(t)
	transaction, err := store.Begin("semantic-write", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteCoverage(rkcmodel.Coverage{}); err == nil || !strings.Contains(err.Error(), "before a validated bundle") {
		t.Fatalf("WriteCoverage(before bundle) = %v", err)
	}
	if err := transaction.WriteBundle(testBundle("", "empty")); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("WriteBundle(empty snapshot id) = %v", err)
	}
	invalid := testBundle("semantic-write", "invalid")
	invalid.Snapshot.ContentDigest = "not-a-digest"
	if err := transaction.WriteBundle(invalid); err == nil || !strings.Contains(err.Error(), "bundle validation failed") {
		t.Fatalf("WriteBundle(invalid semantics) = %v", err)
	}

	bundle := testBundle("semantic-write", "valid")
	if err := transaction.WriteBundle(bundle); err != nil {
		t.Fatal(err)
	}
	wrong := rkcmodel.BuildCoverage(bundle)
	wrong.NodesTotal++
	if err := transaction.WriteCoverage(wrong); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("WriteCoverage(mismatched) = %v", err)
	}
	want := rkcmodel.BuildCoverage(bundle)
	if err := transaction.WriteCoverage(want); err != nil {
		t.Fatal(err)
	}
	if transaction.record.CoverageObject == "" {
		t.Fatal("WriteCoverage did not bind a coverage object")
	}

	rewritten := testBundle("semantic-write", "rewritten")
	if err := transaction.WriteBundle(rewritten); err != nil {
		t.Fatal(err)
	}
	if transaction.record.CoverageDigest != "" || transaction.record.CoverageObject != "" {
		t.Fatalf("rewritten bundle retained stale coverage binding: %+v", transaction.record)
	}
	if err := transaction.WriteCoverage(rkcmodel.BuildCoverage(rewritten)); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Abort("test complete"); err != nil {
		t.Fatal(err)
	}
}

func TestCommitCurrentFailureLeavesPublishedSnapshotRepairable(t *testing.T) {
	store := openTestStore(t)
	transaction, err := store.Begin("repair-current", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteBundle(testBundle("repair-current", "repair")); err != nil {
		t.Fatal(err)
	}
	currentPath := filepath.Join(store.Root(), "CURRENT")
	if err := os.Mkdir(currentPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); !errors.Is(err, ErrCurrentUpdate) {
		t.Fatalf("Commit() CURRENT failure = %v, want ErrCurrentUpdate", err)
	}
	if !transaction.committed || !transaction.closed || transaction.dir != filepath.Join(store.Root(), "snapshots", "repair-current") {
		t.Fatalf("post-publication transaction state = %+v", transaction)
	}
	if _, _, _, err := store.Load("repair-current"); err != nil {
		t.Fatalf("Load(published snapshot after CURRENT failure) = %v", err)
	}
	if err := transaction.Abort("must not remove published data"); err != nil {
		t.Fatalf("Abort(published transaction) = %v", err)
	}
	if _, err := os.Stat(transaction.dir); err != nil {
		t.Fatalf("Abort removed published snapshot: %v", err)
	}
	if err := transaction.Commit(); !errors.Is(err, ErrAlreadyCommitted) {
		t.Fatalf("second Commit() = %v, want ErrAlreadyCommitted", err)
	}
	if err := os.Remove(currentPath); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrent("repair-current"); err != nil {
		t.Fatalf("SetCurrent(repair) = %v", err)
	}
	loaded, _, _, err := store.LoadCurrent()
	if err != nil || loaded.Snapshot.ID != "repair-current" {
		t.Fatalf("LoadCurrent() after repair = %q, %v", loaded.Snapshot.ID, err)
	}
}

func TestLoadBuildsCoverageWhenObjectWasNotStored(t *testing.T) {
	store := openTestStore(t)
	bundle := testBundle("fallback", "fallback")
	transaction, err := store.Begin("fallback", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteBundle(bundle); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
	loaded, coverage, record, err := store.Load("fallback")
	if err != nil {
		t.Fatal(err)
	}
	want := rkcmodel.BuildCoverage(loaded)
	if !reflect.DeepEqual(coverage, want) {
		t.Fatalf("derived coverage = %+v, want %+v", coverage, want)
	}
	if record.CoverageObject != "" {
		t.Fatalf("record unexpectedly has coverage object: %+v", record)
	}
}

func TestAbortRemovesBuildAndClosesTransaction(t *testing.T) {
	store := openTestStore(t)
	transaction, err := store.Begin("abort-me", nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := transaction.dir
	if err := transaction.Abort("operator cancelled"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("aborted build still exists: %v", err)
	}
	if err := transaction.Commit(); !errors.Is(err, ErrTransactionClosed) {
		t.Fatalf("Commit(aborted) = %v, want ErrTransactionClosed", err)
	}
	if err := transaction.Abort("again"); err != nil {
		t.Fatalf("second Abort() = %v", err)
	}
}

func TestAbortRefusesReplacementDirectoryAndPreservesItsContents(t *testing.T) {
	store := openTestStore(t)
	transaction, err := store.Begin("abort-race", nil)
	if err != nil {
		t.Fatal(err)
	}
	original := transaction.dir
	moved := original + "-moved"
	if err := os.Rename(original, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(original, "user-data")
	if err := os.WriteFile(userFile, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Abort("race"); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("Abort(replaced directory) = %v, want ErrBuildingUnowned", err)
	}
	if data, err := os.ReadFile(userFile); err != nil || string(data) != "preserve" {
		t.Fatalf("Abort changed replacement data: %q, %v", data, err)
	}
}

func TestCompetingCommitDoesNotReplacePublishedSnapshot(t *testing.T) {
	store := openTestStore(t)
	first, err := store.Begin("same", nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Begin("same", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.WriteBundle(testBundle("same", "winner")); err != nil {
		t.Fatal(err)
	}
	if err := second.WriteBundle(testBundle("same", "loser")); err != nil {
		t.Fatal(err)
	}
	if err := first.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := second.Commit(); err == nil || !strings.Contains(err.Error(), "publish snapshot") {
		t.Fatalf("competing Commit() = %v, want publish error", err)
	}
	loaded, _, _, err := store.LoadCurrent()
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Snapshot.RootName != "winner" {
		t.Fatalf("competing transaction replaced winner: %+v", loaded.Snapshot)
	}
	if err := second.Abort("lost race"); err != nil {
		t.Fatal(err)
	}
}

func TestLoadReportsMissingAndCorruptState(t *testing.T) {
	store := openTestStore(t)
	if _, _, _, err := store.Load("missing"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("Load(missing) = %v, want ErrSnapshotNotFound", err)
	}

	badRecordDir := filepath.Join(store.Root(), "snapshots", "bad-record")
	if err := os.MkdirAll(badRecordDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badRecordDir, "snapshot.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Load("bad-record"); err == nil {
		t.Fatal("Load(corrupt record) succeeded, want error")
	}

	invalidJSON, err := store.CAS().PutBytes([]byte("not JSON"))
	if err != nil {
		t.Fatal(err)
	}
	writeRecordForTest(t, store, Record{SnapshotID: "bad-bundle", Status: "committed", BundleObject: invalidJSON.Digest})
	if _, _, _, err := store.Load("bad-bundle"); err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("Load(invalid bundle JSON) = %v", err)
	}

	bundleBytes, err := rkcmodel.CanonicalJSON(testBundle("bad-coverage", "coverage"))
	if err != nil {
		t.Fatal(err)
	}
	bundleObject, err := store.CAS().PutBytes(bundleBytes)
	if err != nil {
		t.Fatal(err)
	}
	writeRecordForTest(t, store, Record{SnapshotID: "bad-coverage", Status: "committed", BundleObject: bundleObject.Digest, CoverageObject: invalidJSON.Digest})
	if _, _, _, err := store.Load("bad-coverage"); err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Fatalf("Load(invalid coverage JSON) = %v", err)
	}

	corruptTransaction, err := store.Begin("corrupt-object", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := corruptTransaction.WriteBundle(testBundle("corrupt-object", "object")); err != nil {
		t.Fatal(err)
	}
	if err := corruptTransaction.Commit(); err != nil {
		t.Fatal(err)
	}
	_, _, corruptRecord, err := store.Load("corrupt-object")
	if err != nil {
		t.Fatal(err)
	}
	objectPath, err := store.CAS().Path(corruptRecord.BundleObject)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(objectPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectPath, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := store.Load("corrupt-object"); !errors.Is(err, cas.ErrDigestMismatch) {
		t.Fatalf("Load(corrupt CAS object) = %v, want ErrDigestMismatch", err)
	}
}

func TestLoadRejectsUnboundAndSemanticallyInvalidObjects(t *testing.T) {
	store := openTestStore(t)

	putBundle := func(bundle rkcmodel.Bundle) cas.ObjectInfo {
		t.Helper()
		data, err := rkcmodel.CanonicalJSON(bundle)
		if err != nil {
			t.Fatal(err)
		}
		object, err := store.CAS().PutBytes(data)
		if err != nil {
			t.Fatal(err)
		}
		return object
	}

	bound := putBundle(testBundle("record-binding", "record"))
	writeRecordForTest(t, store, Record{
		SnapshotID:   "record-binding",
		Status:       "committed",
		BundleDigest: strings.Repeat("b", 64),
		BundleObject: bound.Digest,
	})
	if _, _, _, err := store.Load("record-binding"); err == nil || !strings.Contains(err.Error(), "disagree") {
		t.Fatalf("Load(disagreeing record binding) = %v", err)
	}

	wrongID := putBundle(testBundle("other-snapshot", "other"))
	writeRecordForTest(t, store, Record{SnapshotID: "snapshot-binding", Status: "committed", BundleObject: wrongID.Digest})
	if _, _, _, err := store.Load("snapshot-binding"); err == nil || !strings.Contains(err.Error(), "snapshot id") {
		t.Fatalf("Load(bundle snapshot mismatch) = %v", err)
	}

	noncanonicalBundle := testBundle("noncanonical", "noncanonical")
	noncanonicalBytes, err := json.MarshalIndent(noncanonicalBundle, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	noncanonicalObject, err := store.CAS().PutBytes(noncanonicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	writeRecordForTest(t, store, Record{SnapshotID: "noncanonical", Status: "committed", BundleObject: noncanonicalObject.Digest})
	if _, _, _, err := store.Load("noncanonical"); err == nil || !strings.Contains(err.Error(), "not canonical") {
		t.Fatalf("Load(noncanonical bundle) = %v", err)
	}

	invalidBundle := testBundle("invalid-semantics", "invalid")
	invalidBundle.Snapshot.Tool.Version = ""
	invalidObject := putBundle(invalidBundle)
	writeRecordForTest(t, store, Record{SnapshotID: "invalid-semantics", Status: "committed", BundleObject: invalidObject.Digest})
	if _, _, _, err := store.Load("invalid-semantics"); err == nil || !strings.Contains(err.Error(), "bundle validation failed") {
		t.Fatalf("Load(semantically invalid bundle) = %v", err)
	}

	coverageBundle := testBundle("coverage-binding", "coverage")
	coverageObject := putBundle(coverageBundle)
	wrongCoverage := rkcmodel.BuildCoverage(coverageBundle)
	wrongCoverage.NodesTotal++
	coverageBytes, err := json.Marshal(wrongCoverage)
	if err != nil {
		t.Fatal(err)
	}
	wrongCoverageObject, err := store.CAS().PutBytes(coverageBytes)
	if err != nil {
		t.Fatal(err)
	}
	writeRecordForTest(t, store, Record{
		SnapshotID:     "coverage-binding",
		Status:         "committed",
		BundleObject:   coverageObject.Digest,
		CoverageObject: wrongCoverageObject.Digest,
	})
	if _, _, _, err := store.Load("coverage-binding"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("Load(mismatched coverage) = %v", err)
	}
}

func TestCurrentIDEmptyAndUnreadable(t *testing.T) {
	store := openTestStore(t)
	current := filepath.Join(store.Root(), "CURRENT")
	if err := os.WriteFile(current, []byte(" \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CurrentID(); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("CurrentID(empty) = %v", err)
	}
	if err := os.Remove(current); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(current, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CurrentID(); err == nil {
		t.Fatal("CurrentID(directory) succeeded, want error")
	}
}

func TestListUsesStableNewestFirstOrder(t *testing.T) {
	store := openTestStore(t)
	shared := time.Date(2026, 7, 22, 1, 2, 3, 0, time.UTC)
	writeRecordForTest(t, store, Record{SnapshotID: "beta", Status: "committed", CommittedAt: shared})
	writeRecordForTest(t, store, Record{SnapshotID: "alpha", Status: "committed", CommittedAt: shared})
	writeRecordForTest(t, store, Record{SnapshotID: "newest", Status: "committed", CommittedAt: shared.Add(time.Second)})
	if err := os.WriteFile(filepath.Join(store.Root(), "snapshots", "README"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	records, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(records))
	for index, record := range records {
		got[index] = record.SnapshotID
	}
	want := []string{"newest", "alpha", "beta"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List() IDs = %v, want %v", got, want)
	}

	badDir := filepath.Join(store.Root(), "snapshots", "zz-bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "snapshot.json"), []byte("invalid"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err == nil {
		t.Fatal("List() with corrupt record succeeded, want error")
	}
}

func TestRecoverRemovesOnlyExpiredBuildsInStableOrder(t *testing.T) {
	store := openTestStore(t)
	var oldNames []string
	for _, id := range []string{"old-b", "old-a"} {
		transaction, err := store.Begin(id, nil)
		if err != nil {
			t.Fatal(err)
		}
		oldNames = append(oldNames, filepath.Base(transaction.dir))
		old := time.Now().Add(-2 * time.Hour)
		if err := os.Chtimes(transaction.dir, old, old); err != nil {
			t.Fatal(err)
		}
		if err := transaction.closeLease(); err != nil {
			t.Fatal(err)
		}
	}
	fresh, err := store.Begin("fresh", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store.Root(), "building", "not-a-directory"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Recover(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(oldNames)
	if !reflect.DeepEqual(removed, oldNames) {
		t.Fatalf("Recover(maxAge) = %v, want %v", removed, oldNames)
	}
	if _, err := os.Stat(fresh.dir); err != nil {
		t.Fatalf("Recover removed fresh build: %v", err)
	}
	removed, err = store.Recover(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 0 {
		t.Fatalf("Recover(0) removed live transaction: %v", removed)
	}
	if _, err := os.Stat(fresh.dir); err != nil {
		t.Fatalf("Recover(0) removed live build: %v", err)
	}
	if err := fresh.closeLease(); err != nil {
		t.Fatal(err)
	}
	removed, err = store.Recover(0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{filepath.Base(fresh.dir)}) {
		t.Fatalf("Recover(0) after lease release = %v", removed)
	}
}

func TestRecoverRemovesInterruptedCommittedBuild(t *testing.T) {
	store := openTestStore(t)
	transaction, err := store.Begin("interrupted-commit", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteBundle(testBundle("interrupted-commit", "interrupted")); err != nil {
		t.Fatal(err)
	}
	transaction.record.Status = "committed"
	transaction.record.CommittedAt = time.Now().UTC()
	if err := transaction.writeRecord(); err != nil {
		t.Fatal(err)
	}
	directoryName := filepath.Base(transaction.dir)
	if err := transaction.closeLease(); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Recover(0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{directoryName}) {
		t.Fatalf("Recover(interrupted commit) = %v, want %q", removed, directoryName)
	}
	if _, err := os.Stat(transaction.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("interrupted committed build still exists: %v", err)
	}
}

func TestRecoverFailsClosedWhenLeaseIsMissing(t *testing.T) {
	store := openTestStore(t)
	transaction, err := store.Begin("missing-lease", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.closeLease(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(transaction.dir, buildingLeaseName)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recover(0); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("Recover(missing lease) = %v, want ErrBuildingUnowned", err)
	}
	if _, err := os.Stat(transaction.dir); err != nil {
		t.Fatalf("Recover removed transaction without a liveness proof: %v", err)
	}
}

func TestCommitRefusesPreexistingEmptyDestination(t *testing.T) {
	store := openTestStore(t)
	transaction, err := store.Begin("destination-race", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := transaction.WriteBundle(testBundle("destination-race", "source")); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(store.Root(), "snapshots", "destination-race")
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err == nil || !strings.Contains(err.Error(), "destination already exists") {
		t.Fatalf("Commit(existing empty destination) = %v", err)
	}
	if _, err := os.Stat(transaction.dir); err != nil {
		t.Fatalf("failed publication lost building transaction: %v", err)
	}
	if err := os.Remove(destination); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Abort("test complete"); err != nil {
		t.Fatal(err)
	}
}

func TestValidatePublishedDirectoryBindsInodeAndCommittedRecord(t *testing.T) {
	store := openTestStore(t)
	transaction, err := store.Begin("published-binding", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transaction.closeLease() })
	if err := transaction.WriteBundle(testBundle("published-binding", "source")); err != nil {
		t.Fatal(err)
	}
	transaction.record.Status = "committed"
	transaction.record.CommittedAt = time.Now().UTC()
	if err := transaction.writeRecord(); err != nil {
		t.Fatal(err)
	}
	if err := validatePublishedDirectory(transaction.dir, transaction.identity, transaction.marker, "published-binding"); err != nil {
		t.Fatalf("validatePublishedDirectory(valid) = %v", err)
	}
	original := transaction.dir + "-original"
	if err := os.Rename(transaction.dir, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(transaction.dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validatePublishedDirectory(transaction.dir, transaction.identity, transaction.marker, "published-binding"); err == nil {
		t.Fatal("validatePublishedDirectory accepted replacement inode")
	}
}

func TestRecoverRefusesUnownedAndInvalidBuildingDirectories(t *testing.T) {
	store := openTestStore(t)
	unowned := filepath.Join(store.Root(), "building", "unowned")
	if err := os.Mkdir(unowned, 0o755); err != nil {
		t.Fatal(err)
	}
	userFile := filepath.Join(unowned, "user-data")
	if err := os.WriteFile(userFile, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recover(0); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("Recover(unowned) = %v, want ErrBuildingUnowned", err)
	}
	if data, err := os.ReadFile(userFile); err != nil || string(data) != "preserve" {
		t.Fatalf("Recover changed unowned data: %q, %v", data, err)
	}

	if err := os.RemoveAll(unowned); err != nil {
		t.Fatal(err)
	}
	transaction, err := store.Begin("oversized-building-record", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transaction.dir, "snapshot.json"), []byte(strings.Repeat("x", buildingRecordMaxSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Recover(0); !errors.Is(err, ErrBuildingUnowned) {
		t.Fatalf("Recover(oversized record) = %v, want ErrBuildingUnowned", err)
	}
	if _, err := os.Stat(transaction.dir); err != nil {
		t.Fatalf("Recover removed invalid owned directory: %v", err)
	}
}

func TestRecoverSkipsSymlinkEntries(t *testing.T) {
	store := openTestStore(t)
	external := t.TempDir()
	userFile := filepath.Join(external, "user-data")
	if err := os.WriteFile(userFile, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(store.Root(), "building", "external-link")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}
	removed, err := store.Recover(0)
	if err != nil || len(removed) != 0 {
		t.Fatalf("Recover(symlink) = %v, %v", removed, err)
	}
	if data, err := os.ReadFile(userFile); err != nil || string(data) != "preserve" {
		t.Fatalf("Recover followed symlink: %q, %v", data, err)
	}
}

func TestAtomicWriteReplaceAndRollback(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "record")
	if err := writeAtomic(path, []byte("first"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(path, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "second" {
		t.Fatalf("atomic replacement = %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("atomic replacement mode = %v, %v", info.Mode(), err)
	}

	targetDirectory := filepath.Join(directory, "cannot-replace")
	if err := os.Mkdir(targetDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAtomic(targetDirectory, []byte("no"), 0o600); err == nil {
		t.Fatal("writeAtomic(directory target) succeeded, want error")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".atomic-") {
			t.Fatalf("writeAtomic rollback left temporary file %q", entry.Name())
		}
	}
	if err := syncDirectory(filepath.Join(directory, "missing")); err == nil {
		t.Fatal("syncDirectory(missing) succeeded, want error")
	}
}

func TestCloneStrings(t *testing.T) {
	if cloneStrings(nil) != nil {
		t.Fatal("cloneStrings(nil) should return nil")
	}
	original := map[string]string{"a": "b"}
	cloned := cloneStrings(original)
	cloned["a"] = "changed"
	if original["a"] != "b" {
		t.Fatal("cloneStrings returned an alias")
	}
}

func writeRecordForTest(t *testing.T, store *Store, record Record) {
	t.Helper()
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Date(2026, 7, 21, 23, 59, 59, 0, time.UTC)
	}
	if record.Status == "committed" && record.CommittedAt.IsZero() {
		record.CommittedAt = time.Date(2026, 7, 22, 0, 0, 0, 0, time.UTC)
	}
	if record.Status == "committed" && record.BundleObject == "" {
		record.BundleObject = strings.Repeat("0", 64)
	}
	if record.BundleObject != "" && record.BundleDigest == "" {
		record.BundleDigest = record.BundleObject
	}
	if record.CoverageObject != "" && record.CoverageDigest == "" {
		record.CoverageDigest = record.CoverageObject
	}
	directory := filepath.Join(store.Root(), "snapshots", record.SnapshotID)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "snapshot.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}
