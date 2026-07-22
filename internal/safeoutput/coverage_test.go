package safeoutput

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type syntheticOutputFileInfo struct{ system any }

func (syntheticOutputFileInfo) Name() string       { return "synthetic" }
func (syntheticOutputFileInfo) Size() int64        { return 0 }
func (syntheticOutputFileInfo) Mode() os.FileMode  { return 0 }
func (syntheticOutputFileInfo) ModTime() time.Time { return time.Time{} }
func (syntheticOutputFileInfo) IsDir() bool        { return false }
func (info syntheticOutputFileInfo) Sys() any      { return info.system }

func TestLowLevelFileAndManifestSafetyHelpers(t *testing.T) {
	if ReplacementPlatformDescription() == "" {
		t.Fatal("ReplacementPlatformDescription() is empty")
	}
	zero, negative, tooLarge := int64(0), int64(-1), ownershipManifestMaxBytes+1
	atlas := ownershipManifestFile{SizeBytes: &zero}
	if size, err := manifestFileSize("atlas", atlas); err != nil || size != 0 {
		t.Fatalf("manifestFileSize(atlas) = %d, %v", size, err)
	}
	synthesis := ownershipManifestFile{Bytes: &zero}
	if size, err := manifestFileSize("synthesis", synthesis); err != nil || size != 0 {
		t.Fatalf("manifestFileSize(synthesis) = %d, %v", size, err)
	}
	for _, test := range []struct {
		kind string
		file ownershipManifestFile
	}{
		{"atlas", ownershipManifestFile{}},
		{"atlas", ownershipManifestFile{SizeBytes: &negative}},
		{"atlas", ownershipManifestFile{SizeBytes: &tooLarge}},
		{"atlas", ownershipManifestFile{SizeBytes: &zero, Bytes: &zero}},
		{"synthesis", ownershipManifestFile{Bytes: &zero, SizeBytes: &zero}},
		{"other", ownershipManifestFile{Bytes: &zero}},
	} {
		if _, err := manifestFileSize(test.kind, test.file); err == nil {
			t.Errorf("manifestFileSize(%q, %+v) succeeded", test.kind, test.file)
		}
	}

	for _, valid := range []string{"file", "nested/file.json", "a..b"} {
		if !canonicalManifestPath(valid) {
			t.Errorf("canonicalManifestPath(%q) = false", valid)
		}
	}
	for _, invalid := range []string{"", ".", "..", "../escape", "/absolute", "nested/../escape", "bad\x00name"} {
		if canonicalManifestPath(invalid) {
			t.Errorf("canonicalManifestPath(%q) = true", invalid)
		}
	}

	root := t.TempDir()
	path := filepath.Join(root, "payload")
	if err := os.WriteFile(path, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if digest, err := hashRegularFile(path, identity, 7); err != nil || len(digest) != 64 {
		t.Fatalf("hashRegularFile(valid) = %q, %v", digest, err)
	}
	if _, err := hashRegularFile(path, identity, 6); err == nil {
		t.Fatal("hashRegularFile(too small limit) succeeded")
	}
	rootIdentity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hashRegularFile(path, rootIdentity, 7); err == nil {
		t.Fatal("hashRegularFile(wrong identity) succeeded")
	}
	if _, err := hashRegularFile(filepath.Join(root, "missing"), identity, 7); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hashRegularFile(missing) = %v", err)
	}
	if data, err := readBoundedRegular(path, 7); err != nil || string(data) != "payload" {
		t.Fatalf("readBoundedRegular(valid) = %q, %v", data, err)
	}
	if _, err := readBoundedRegular(path, 6); err == nil {
		t.Fatal("readBoundedRegular(too small) succeeded")
	}
	if _, err := readBoundedRegular(root, 100); err == nil {
		t.Fatal("readBoundedRegular(directory) succeeded")
	}
	if _, err := readBoundedRegular(filepath.Join(root, "missing"), 100); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("readBoundedRegular(missing) = %v", err)
	}
	if matched, err := sameDirectoryIdentity(root, rootIdentity); err != nil || !matched {
		t.Fatalf("sameDirectoryIdentity(valid) = %v, %v", matched, err)
	}
	if matched, err := sameDirectoryIdentity(root, nil); err != nil || matched {
		t.Fatalf("sameDirectoryIdentity(nil) = %v, %v", matched, err)
	}
	if _, err := sameDirectoryIdentity(filepath.Join(root, "missing"), rootIdentity); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sameDirectoryIdentity(missing) = %v", err)
	}
}

func TestOversizedMarkerAndUnsupportedIdentityShapes(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, MarkerName), []byte(strings.Repeat("x", markerMaxSize+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMarker(root); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("ReadMarker(oversized) = %v", err)
	}
	if _, err := persistentIdentityToken(syntheticOutputFileInfo{system: "not a struct"}); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("persistentIdentityToken(non-struct) = %v", err)
	}
	if _, err := persistentIdentityToken(syntheticOutputFileInfo{system: struct{}{}}); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("persistentIdentityToken(missing fields) = %v", err)
	}
}

func TestOwnershipMetadataAndDirectoryFailureBoundaries(t *testing.T) {
	if _, _, err := ownershipMetadataDigests(t.TempDir(), "other"); err == nil {
		t.Fatal("ownershipMetadataDigests(unsupported) succeeded")
	}
	root := filepath.Join(t.TempDir(), "atlas")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalizeOwnedAtlasFixture(t, root, "snapshot")
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	markerDigest, manifestDigest, err := ownershipMetadataDigests(root, "atlas")
	if err != nil || len(markerDigest) != 64 || len(manifestDigest) != 64 {
		t.Fatalf("ownershipMetadataDigests(valid) = %q, %q, %v", markerDigest, manifestDigest, err)
	}
	if err := validateJournalBoundOutput(root, "atlas", identity, "snapshot", markerDigest, manifestDigest); err != nil {
		t.Fatal(err)
	}
	if err := validateJournalBoundOutput(root, "atlas", identity, "snapshot", strings.Repeat("0", 64), manifestDigest); err == nil {
		t.Fatal("validateJournalBoundOutput(digest mismatch) succeeded")
	}
	if err := validateOwnedDirectory(root, nil, ErrTargetUnowned, "atlas"); !errors.Is(err, ErrTargetUnowned) {
		t.Fatalf("validateOwnedDirectory(nil identity) = %v", err)
	}
	if err := validateOwnedDirectory(root, identity, ErrTargetUnowned, "synthesis"); !errors.Is(err, ErrTargetUnowned) {
		t.Fatalf("validateOwnedDirectory(wrong kind) = %v", err)
	}
	if err := validateOwnedDirectory(root, identity, ErrTargetUnowned, "atlas"); err != nil {
		t.Fatalf("validateOwnedDirectory(valid complete atlas) = %v", err)
	}
}

func TestOwnershipManifestRejectsAmbiguousPayloads(t *testing.T) {
	newFixture := func(t *testing.T) (string, os.FileInfo, ownershipManifest) {
		t.Helper()
		root := filepath.Join(t.TempDir(), "atlas")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		payload := []byte("payload")
		if err := os.WriteFile(filepath.Join(root, "payload.txt"), payload, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		size := int64(len(payload))
		manifest := ownershipManifest{
			SchemaVersion: outputManifestVersion,
			SnapshotID:    "snapshot",
			Files: []ownershipManifestFile{{
				Path: "payload.txt", SHA256: hex.EncodeToString(digest[:]), SizeBytes: &size,
			}},
		}
		if err := writeMarker(root, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas", SnapshotID: "snapshot"}); err != nil {
			t.Fatal(err)
		}
		identity, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		return root, identity, manifest
	}
	writeManifest := func(t *testing.T, root string, manifest ownershipManifest, suffix string) {
		t.Helper()
		data, err := json.Marshal(manifest)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "rkc-export-manifest.json"), append(data, suffix...), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	tests := []struct {
		name   string
		mutate func(*ownershipManifest)
		suffix string
		want   string
	}{
		{"trailing JSON", func(*ownershipManifest) {}, `{}`, "trailing JSON"},
		{"wrong schema", func(manifest *ownershipManifest) { manifest.SchemaVersion = "0" }, "", "does not match"},
		{"empty files", func(manifest *ownershipManifest) { manifest.Files = nil }, "", "file count"},
		{"unsafe path", func(manifest *ownershipManifest) { manifest.Files[0].Path = "../payload.txt" }, "", "unsafe path"},
		{"reserved marker path", func(manifest *ownershipManifest) { manifest.Files[0].Path = MarkerName }, "", "unsafe path"},
		{"duplicate path", func(manifest *ownershipManifest) { manifest.Files = append(manifest.Files, manifest.Files[0]) }, "", "repeats path"},
		{"bad digest", func(manifest *ownershipManifest) { manifest.Files[0].SHA256 = "bad" }, "", "invalid digest"},
		{"missing size", func(manifest *ownershipManifest) { manifest.Files[0].SizeBytes = nil }, "", "invalid size"},
		{"negative size", func(manifest *ownershipManifest) { value := int64(-1); manifest.Files[0].SizeBytes = &value }, "", "invalid size"},
		{"byte limit", func(manifest *ownershipManifest) {
			maximum := ownershipManifestMaxBytes
			manifest.Files[0].SizeBytes = &maximum
			second := manifest.Files[0]
			second.Path = "second.txt"
			manifest.Files = append(manifest.Files, second)
		}, "", "byte count"},
		{"size mismatch", func(manifest *ownershipManifest) { value := int64(99); manifest.Files[0].SizeBytes = &value }, "", "size mismatch"},
		{"digest mismatch", func(manifest *ownershipManifest) { manifest.Files[0].SHA256 = strings.Repeat("0", 64) }, "", "digest mismatch"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root, identity, manifest := newFixture(t)
			test.mutate(&manifest)
			writeManifest(t, root, manifest, test.suffix)
			if err := validateOwnedOutput(root, "atlas", identity); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("validateOwnedOutput() = %v, want %q", err, test.want)
			}
		})
	}

	t.Run("missing manifested file", func(t *testing.T) {
		root, identity, manifest := newFixture(t)
		writeManifest(t, root, manifest, "")
		if err := os.Remove(filepath.Join(root, "payload.txt")); err != nil {
			t.Fatal(err)
		}
		if err := validateOwnedOutput(root, "atlas", identity); err == nil || !strings.Contains(err.Error(), "references missing") {
			t.Fatalf("validateOwnedOutput(missing file) = %v", err)
		}
	})
	t.Run("symlinked payload", func(t *testing.T) {
		root, identity, manifest := newFixture(t)
		writeManifest(t, root, manifest, "")
		payload := filepath.Join(root, "payload.txt")
		if err := os.Remove(payload); err != nil {
			t.Fatal(err)
		}
		external := filepath.Join(t.TempDir(), "external")
		if err := os.WriteFile(external, []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, payload); err != nil {
			t.Fatal(err)
		}
		if err := validateOwnedOutput(root, "atlas", identity); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("validateOwnedOutput(symlink) = %v", err)
		}
	})
	t.Run("manifest directory", func(t *testing.T) {
		root, identity, _ := newFixture(t)
		if err := os.Mkdir(filepath.Join(root, "rkc-export-manifest.json"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := validateOwnedOutput(root, "atlas", identity); err == nil || !strings.Contains(err.Error(), "manifest identity") {
			t.Fatalf("validateOwnedOutput(manifest directory) = %v", err)
		}
	})
	t.Run("unsupported exact marker kind", func(t *testing.T) {
		root, identity, _ := newFixture(t)
		if err := writeMarker(root, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "other", SnapshotID: "snapshot"}); err != nil {
			t.Fatal(err)
		}
		if err := validateOwnedOutput(root, "other", identity); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("validateOwnedOutput(unsupported kind) = %v", err)
		}
	})
}

func TestReplacementJournalPersistenceLoadAndValidation(t *testing.T) {
	if err := (*replacementJournal)(nil).persist(); err == nil {
		t.Fatal("nil journal persist succeeded")
	}
	if err := (*replacementJournal)(nil).discard(); err != nil {
		t.Fatalf("nil journal discard = %v", err)
	}
	root := filepath.Join(t.TempDir(), ".rkc-quarantine-valid")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	rootIdentity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	record := validReplacementJournalRecord()
	journal := &replacementJournal{root: root, path: filepath.Join(root, journalName), rootIdentity: rootIdentity, record: record}
	if err := journal.persist(); err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"exchanged", "prior-quarantined", "prepared"} {
		if err := journal.update(phase); err != nil {
			t.Fatalf("update(%q) = %v", phase, err)
		}
	}
	if err := journal.update("invalid"); err == nil {
		t.Fatal("update(invalid) succeeded")
	}
	loaded, err := loadReplacementJournal(root)
	if err != nil || loaded.record.TargetName != record.TargetName {
		t.Fatalf("loadReplacementJournal(valid) = %+v, %v", loaded, err)
	}
	if err := loaded.discard(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discard retained root: %v", err)
	}

	if _, err := loadReplacementJournal(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loadReplacementJournal(missing) = %v", err)
	}
	plain := filepath.Join(t.TempDir(), "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReplacementJournal(plain); err == nil || !strings.Contains(err.Error(), "owner-only directory") {
		t.Fatalf("loadReplacementJournal(file) = %v", err)
	}
	insecure := filepath.Join(t.TempDir(), "insecure")
	if err := os.Mkdir(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := loadReplacementJournal(insecure); err == nil || !strings.Contains(err.Error(), "owner-only directory") {
		t.Fatalf("loadReplacementJournal(insecure root) = %v", err)
	}

	for _, test := range []struct {
		name string
		body string
	}{
		{"malformed", "{"},
		{"trailing", replacementJournalJSON(t, record) + "{}"},
		{"invalid fields", replacementJournalJSON(t, func() replacementJournalRecord { altered := record; altered.TargetName = "../target"; return altered }())},
		{"invalid digest", replacementJournalJSON(t, func() replacementJournalRecord { altered := record; altered.NewMarkerSHA256 = "bad"; return altered }())},
		{"invalid phase", replacementJournalJSON(t, func() replacementJournalRecord { altered := record; altered.Phase = "bad"; return altered }())},
	} {
		t.Run(test.name, func(t *testing.T) {
			journalRoot := filepath.Join(t.TempDir(), ".rkc-quarantine-test")
			if err := os.Mkdir(journalRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(journalRoot, journalName), []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadReplacementJournal(journalRoot); err == nil {
				t.Fatal("invalid replacement journal loaded")
			}
		})
	}
}

func validReplacementJournalRecord() replacementJournalRecord {
	digest := strings.Repeat("0", 64)
	return replacementJournalRecord{
		SchemaVersion: journalVersion, TargetName: "atlas", StagingName: ".rkc-build-fixture",
		Kind: "atlas", NewSnapshot: "new", PriorSnapshot: "old", NewIdentity: "1:2", PriorIdentity: "3:4",
		NewMarkerSHA256: digest, NewManifestSHA256: digest, PriorMarkerSHA256: digest, PriorManifestSHA256: digest,
		Phase: "prepared",
	}
}

func replacementJournalJSON(t *testing.T, record replacementJournalRecord) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), ".journal-encoder")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	journal := &replacementJournal{root: root, path: filepath.Join(root, journalName), rootIdentity: identity, record: record}
	if err := journal.persist(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestIdentityTokensRecoveryScanAndQuarantineFailures(t *testing.T) {
	if _, err := persistentIdentityToken(nil); err == nil {
		t.Fatal("persistentIdentityToken(nil) succeeded")
	}
	if _, ok := numericIdentityField(reflect.Value{}); ok {
		t.Fatal("numericIdentityField(invalid) succeeded")
	}
	if value, ok := numericIdentityField(reflect.ValueOf(uint32(7))); !ok || value != 7 {
		t.Fatalf("numericIdentityField(uint32) = %d, %v", value, ok)
	}
	if value, ok := numericIdentityField(reflect.ValueOf(int64(9))); !ok || value != 9 {
		t.Fatalf("numericIdentityField(int64) = %d, %v", value, ok)
	}
	if _, ok := numericIdentityField(reflect.ValueOf("not numeric")); ok {
		t.Fatal("numericIdentityField(string) succeeded")
	}
	if info, token := inspectIdentityToken(filepath.Join(t.TempDir(), "missing")); info != nil || token != "" {
		t.Fatalf("inspectIdentityToken(missing) = %v, %q", info, token)
	}
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if info, token := inspectIdentityToken(file); info != nil || token != "" {
		t.Fatalf("inspectIdentityToken(file) = %v, %q", info, token)
	}

	parent := t.TempDir()
	for index := 0; index <= journalScanLimit; index++ {
		path := filepath.Join(parent, fmt.Sprintf(".rkc-quarantine-%03d", index))
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := recoverInterruptedReplacements(parent, "atlas"); err == nil || !strings.Contains(err.Error(), "scan limit") {
		t.Fatalf("recoverInterruptedReplacements(scan limit) = %v", err)
	}

	var nilQuarantine *quarantinedDirectory
	if err := nilQuarantine.validate(ErrTargetUnowned); !errors.Is(err, ErrTargetUnowned) {
		t.Fatalf("nil quarantine validate = %v", err)
	}
	if err := nilQuarantine.restore(filepath.Join(t.TempDir(), "target")); err == nil {
		t.Fatal("nil quarantine restore succeeded")
	}
	invalid := &quarantinedDirectory{root: filepath.Join(t.TempDir(), "missing")}
	if err := removeEmptyQuarantine(invalid); err == nil {
		t.Fatal("removeEmptyQuarantine(invalid) succeeded")
	}
}

func TestNewPortableRollbackAndPreExchangePublicationPaths(t *testing.T) {
	if !renameNoReplaceSupported() {
		t.Skip("atomic no-replace publication is unavailable")
	}
	t.Run("new target commit", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "atlas")
		transaction, err := Begin(target, "", false, "atlas")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(transaction.Staging, "new.txt"), []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeAtlasManifestFixture(t, transaction.Staging, "new")
		if err := transaction.Commit("new"); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(filepath.Join(target, "new.txt")); err != nil || string(data) != "new" {
			t.Fatalf("published new target = %q, %v", data, err)
		}
	})

	t.Run("new target post-publication marker tamper rolls back", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "atlas")
		transaction, err := Begin(target, "", false, "atlas")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(transaction.Staging, "new.txt"), []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		writeAtlasManifestFixture(t, transaction.Staging, "new")
		originalRename := renameNoReplaceOperation
		calls := 0
		renameNoReplaceOperation = func(source, destination string) error {
			calls++
			if err := originalRename(source, destination); err != nil {
				return err
			}
			if calls == 1 {
				return writeMarker(destination, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas", SnapshotID: "tampered"})
			}
			return nil
		}
		t.Cleanup(func() { renameNoReplaceOperation = originalRename })
		if err := transaction.Commit("new"); !errors.Is(err, ErrInvalidStaging) {
			t.Fatalf("Commit(post-publication tamper) = %v", err)
		}
		if calls != 2 {
			t.Fatalf("rename calls = %d, want publish plus exact rollback", calls)
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("tampered target remained published: %v", err)
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("portable replacement", func(t *testing.T) {
		transaction, priorIdentity, journal := newReplacementFixture(t)
		if err := transaction.commitPortable(priorIdentity, "new", journal); err != nil {
			t.Fatal(err)
		}
		if !transaction.committed {
			t.Fatal("portable replacement did not commit")
		}
		if data, err := os.ReadFile(filepath.Join(transaction.Target, "new.txt")); err != nil || string(data) != "new" {
			t.Fatalf("portable target = %q, %v", data, err)
		}
		if _, err := os.Lstat(journal.root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("portable replacement retained journal: %v", err)
		}
	})

	t.Run("portable quarantine refusal", func(t *testing.T) {
		transaction, priorIdentity, journal := newReplacementFixture(t)
		payload := filepath.Join(journal.root, "payload")
		if err := os.Mkdir(payload, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(payload, "blocker"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := transaction.commitPortable(priorIdentity, "new", journal); err == nil || !strings.Contains(err.Error(), "quarantine prior") {
			t.Fatalf("commitPortable(blocked quarantine) = %v", err)
		}
		if data, err := os.ReadFile(filepath.Join(transaction.Target, "old.txt")); err != nil || string(data) != "old" {
			t.Fatalf("blocked portable target = %q, %v", data, err)
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("portable publish refusal restores prior", func(t *testing.T) {
		transaction, priorIdentity, journal := newReplacementFixture(t)
		originalRename := renameNoReplaceOperation
		calls := 0
		renameNoReplaceOperation = func(source, target string) error {
			calls++
			if calls == 1 {
				return errors.New("injected publication refusal")
			}
			return originalRename(source, target)
		}
		t.Cleanup(func() { renameNoReplaceOperation = originalRename })
		if err := transaction.commitPortable(priorIdentity, "new", journal); err == nil || !strings.Contains(err.Error(), "injected publication refusal") {
			t.Fatalf("commitPortable(publication refusal) = %v", err)
		}
		if calls != 2 {
			t.Fatalf("rename calls = %d, want publish plus restore", calls)
		}
		if data, err := os.ReadFile(filepath.Join(transaction.Target, "old.txt")); err != nil || string(data) != "old" {
			t.Fatalf("restored portable target = %q, %v", data, err)
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("atomic rollback", func(t *testing.T) {
		if !replacementHasNoMissingTargetWindow() {
			t.Skip("atomic exchange is unavailable")
		}
		transaction, priorIdentity, journal := newReplacementFixture(t)
		if err := exchangePaths(transaction.Staging, transaction.Target); err != nil {
			t.Fatal(err)
		}
		cause := errors.New("injected post-exchange validation failure")
		if err := transaction.rollbackExchange(priorIdentity, journal, cause); !errors.Is(err, cause) {
			t.Fatalf("rollbackExchange() = %v", err)
		}
		if data, err := os.ReadFile(filepath.Join(transaction.Target, "old.txt")); err != nil || string(data) != "old" {
			t.Fatalf("rolled-back target = %q, %v", data, err)
		}
		if _, err := os.Lstat(journal.root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rollback retained journal: %v", err)
		}
		if err := transaction.Abort(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("atomic rollback refusal retains journal", func(t *testing.T) {
		if !replacementHasNoMissingTargetWindow() {
			t.Skip("atomic exchange is unavailable")
		}
		transaction, priorIdentity, journal := newReplacementFixture(t)
		if err := exchangePaths(transaction.Staging, transaction.Target); err != nil {
			t.Fatal(err)
		}
		originalExchange := exchangeOperation
		exchangeOperation = func(_, _ string) error { return errors.New("injected rollback refusal") }
		t.Cleanup(func() { exchangeOperation = originalExchange })
		cause := errors.New("post-exchange failure")
		if err := transaction.rollbackExchange(priorIdentity, journal, cause); err == nil || !strings.Contains(err.Error(), "injected rollback refusal") {
			t.Fatalf("rollbackExchange(refusal) = %v", err)
		}
		if _, err := os.Lstat(journal.root); err != nil {
			t.Fatalf("failed rollback lost recovery journal: %v", err)
		}
	})

	t.Run("recover before exchange", func(t *testing.T) {
		transaction, _, journal := newReplacementFixture(t)
		if err := recoverInterruptedReplacements(filepath.Dir(transaction.Target), filepath.Base(transaction.Target)); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(filepath.Join(transaction.Target, "old.txt")); err != nil || string(data) != "old" {
			t.Fatalf("pre-exchange recovery target = %q, %v", data, err)
		}
		if _, err := os.Lstat(transaction.Staging); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pre-exchange recovery retained staging: %v", err)
		}
		if _, err := os.Lstat(journal.root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("pre-exchange recovery retained journal: %v", err)
		}
	})

	t.Run("recover portable quarantine before publish", func(t *testing.T) {
		transaction, _, journal := newReplacementFixture(t)
		payload := filepath.Join(journal.root, "payload")
		if err := os.Rename(transaction.Target, payload); err != nil {
			t.Fatal(err)
		}
		if err := journal.update("prior-quarantined"); err != nil {
			t.Fatal(err)
		}
		if err := recoverInterruptedReplacements(filepath.Dir(transaction.Target), filepath.Base(transaction.Target)); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(filepath.Join(transaction.Target, "old.txt")); err != nil || string(data) != "old" {
			t.Fatalf("portable recovery target = %q, %v", data, err)
		}
		if _, err := os.Lstat(transaction.Staging); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("portable recovery retained staging: %v", err)
		}
		if _, err := os.Lstat(journal.root); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("portable recovery retained journal: %v", err)
		}
	})
}

func newReplacementFixture(t *testing.T) (*Transaction, os.FileInfo, *replacementJournal) {
	t.Helper()
	parent := t.TempDir()
	target := filepath.Join(parent, "atlas")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalizeOwnedAtlasFixture(t, target, "old")
	transaction, err := Begin(target, "", true, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transaction.Staging, "new.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAtlasManifestFixture(t, transaction.Staging, "new")
	if err := writeMarker(transaction.Staging, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas", SnapshotID: "new"}); err != nil {
		t.Fatal(err)
	}
	priorIdentity, err := inspectExisting(target, true, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	journal, err := createReplacementJournal(transaction, priorIdentity, "new")
	if err != nil {
		t.Fatal(err)
	}
	return transaction, priorIdentity, journal
}
