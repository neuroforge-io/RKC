package safeoutput

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestResolveTargetRejectsRootsRepositoryAncestorsAndEmpty(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "workspace", "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"", "   ", string(filepath.Separator), repository, filepath.Dir(repository), base} {
		if _, err := ResolveTarget(target, repository); !errors.Is(err, ErrUnsafeTarget) {
			t.Errorf("ResolveTarget(%q) = %v, want ErrUnsafeTarget", target, err)
		}
	}
	descendant := filepath.Join(repository, "generated", "atlas")
	resolved, err := ResolveTarget(descendant, repository)
	if err != nil {
		t.Fatalf("ResolveTarget(repository descendant): %v", err)
	}
	if resolved != descendant {
		t.Fatalf("ResolveTarget(descendant) = %q, want %q", resolved, descendant)
	}
}

func TestResolveTargetRejectsFinalSymlinkAndParentAlias(t *testing.T) {
	base := t.TempDir()
	repository := filepath.Join(base, "repository")
	if err := os.MkdirAll(repository, 0o755); err != nil {
		t.Fatal(err)
	}
	finalLink := filepath.Join(base, "output-link")
	if err := os.Symlink(filepath.Join(base, "real-output"), finalLink); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTarget(finalLink, repository); !errors.Is(err, ErrUnsafeTarget) || !strings.Contains(err.Error(), "cannot be a symlink") {
		t.Fatalf("ResolveTarget(final symlink) = %v", err)
	}

	parentAlias := filepath.Join(base, "parent-alias")
	if err := os.Symlink(base, parentAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTarget(filepath.Join(parentAlias, "repository"), repository); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("ResolveTarget(parent alias to repository) = %v", err)
	}

	protectedAlias := filepath.Join(base, "repository-alias")
	if err := os.Symlink(repository, protectedAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTarget(repository, protectedAlias); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("ResolveTarget(real repository, protected symlink) = %v", err)
	}
}

func TestResolveTargetCanonicalizesExistingSymlinkParentAndMissingSuffix(t *testing.T) {
	base := t.TempDir()
	realParent := filepath.Join(base, "real-parent")
	if err := os.MkdirAll(realParent, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(realParent, alias); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(alias, "missing", "nested", "output")
	resolved, err := ResolveTarget(target, "")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(realParent, "missing", "nested", "output")
	if resolved != want {
		t.Fatalf("ResolveTarget() = %q, want %q", resolved, want)
	}

	danglingParent := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(base, "missing-target"), danglingParent); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTarget(filepath.Join(danglingParent, "output"), ""); err == nil || !strings.Contains(err.Error(), "resolve output parent") {
		t.Fatalf("ResolveTarget(dangling parent) = %v", err)
	}
}

func TestResolveTargetFailsClosedWhenProtectedRootCannotResolve(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "output")
	missing := filepath.Join(base, "missing-protected-root")
	if _, err := ResolveTarget(target, missing); !errors.Is(err, ErrUnsafeTarget) || !strings.Contains(err.Error(), "protected root symlinks") {
		t.Fatalf("ResolveTarget(missing protected root) = %v", err)
	}
	dangling := filepath.Join(base, "dangling-protected-root")
	if err := os.Symlink(missing, dangling); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveTarget(target, dangling); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("ResolveTarget(dangling protected root) = %v", err)
	}
}

func TestBeginCreatesSiblingStagingWithOwnershipMarker(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "missing", "atlas")
	transaction, err := Begin(target, "", false, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if transaction.Target != target || filepath.Dir(transaction.Staging) != filepath.Dir(target) || transaction.kind != "atlas" || transaction.force {
		t.Fatalf("Begin() transaction = %+v", transaction)
	}
	if !strings.HasPrefix(filepath.Base(transaction.Staging), ".rkc-build-") {
		t.Fatalf("staging name = %q", transaction.Staging)
	}
	marker, err := ReadMarker(transaction.Staging)
	if err != nil {
		t.Fatal(err)
	}
	if marker != (Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "staging"}) {
		t.Fatalf("staging marker = %+v", marker)
	}
	if !IsGenerated(transaction.Staging) {
		t.Fatal("staging directory was not detected as generated")
	}
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestBeginRejectsInvalidKindAndExistingWithoutForce(t *testing.T) {
	for _, kind := range []string{"", " ", "staging", "other"} {
		if _, err := Begin(filepath.Join(t.TempDir(), "output"), "", false, kind); !errors.Is(err, ErrUnsafeTarget) {
			t.Errorf("Begin(kind=%q) = %v, want ErrUnsafeTarget", kind, err)
		}
	}
	target := filepath.Join(t.TempDir(), "output")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(target, "", false, "atlas"); !errors.Is(err, ErrTargetExists) {
		t.Fatalf("Begin(existing, no force) = %v, want ErrTargetExists", err)
	}
}

func TestForceRejectsUnownedAndMismatchedTargets(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, target string)
	}{
		{name: "regular file", prepare: func(t *testing.T, target string) {
			if err := os.WriteFile(target, []byte("user data"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "empty directory", prepare: func(t *testing.T, target string) {
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "corrupt marker", prepare: func(t *testing.T, target string) {
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, MarkerName), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "wrong marker kind", prepare: func(t *testing.T, target string) {
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeMarker(target, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "synthesis"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "forged exact-kind marker without manifest", prepare: func(t *testing.T, target string) {
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := writeMarker(target, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas", SnapshotID: "forged"}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "partial legacy", prepare: func(t *testing.T, target string) {
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(target, "bundle.json"), []byte("{}"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "output")
			test.prepare(t, target)
			if _, err := Begin(target, "", true, "atlas"); !errors.Is(err, ErrTargetUnowned) {
				t.Fatalf("Begin(force unowned) = %v, want ErrTargetUnowned", err)
			}
		})
	}

	real := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(real), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(link, "", true, "atlas"); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("Begin(force symlink) = %v, want ErrUnsafeTarget", err)
	}
}

func TestReadMarkerOwnershipAndGeneratedDetection(t *testing.T) {
	root := t.TempDir()
	if IsGenerated(root) {
		t.Fatal("unmarked directory detected as generated")
	}
	if _, err := ReadMarker(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ReadMarker(missing) = %v", err)
	}
	valid := Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas", SnapshotID: "snapshot"}
	if err := writeMarker(root, valid); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadMarker(root)
	if err != nil || loaded != valid || !IsGenerated(root) {
		t.Fatalf("valid marker = %+v, %v generated=%v", loaded, err, IsGenerated(root))
	}
	data, err := os.ReadFile(filepath.Join(root, MarkerName))
	if err != nil || !strings.HasSuffix(string(data), "\n") {
		t.Fatalf("marker encoding = %q, %v", data, err)
	}

	invalidMarkers := []Marker{
		{SchemaVersion: "0", Producer: producer, Kind: "atlas"},
		{SchemaVersion: markerVersion, Producer: "someone-else", Kind: "atlas"},
		{SchemaVersion: markerVersion, Producer: producer, Kind: " "},
	}
	for index, marker := range invalidMarkers {
		if err := writeMarker(root, marker); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadMarker(root); err == nil || IsGenerated(root) {
			t.Errorf("invalid marker %d accepted: %v", index, err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, MarkerName), []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMarker(root); err == nil || !strings.Contains(err.Error(), "decode RKC output marker") || IsGenerated(root) {
		t.Fatalf("malformed marker = %v generated=%v", err, IsGenerated(root))
	}
	for _, body := range []string{
		`{"schema_version":"1.0","producer":"rkc","kind":"atlas","unexpected":true}`,
		`{"schema_version":"1.0","producer":"rkc","kind":"atlas"} {}`,
	} {
		if err := os.WriteFile(filepath.Join(root, MarkerName), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadMarker(root); err == nil || !strings.Contains(err.Error(), "decode RKC output marker") {
			t.Fatalf("non-exact marker %q accepted: %v", body, err)
		}
	}

	externalMarker := filepath.Join(t.TempDir(), "external-marker")
	if err := os.WriteFile(externalMarker, []byte(`{"schema_version":"1.0","producer":"rkc","kind":"atlas"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, MarkerName)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(externalMarker, filepath.Join(root, MarkerName)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadMarker(root); err == nil || !strings.Contains(err.Error(), "not a regular file") || IsGenerated(root) {
		t.Fatalf("symlink marker = %v generated=%v", err, IsGenerated(root))
	}

	file := filepath.Join(t.TempDir(), "plain-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if IsGenerated(file) {
		t.Fatal("regular file detected as generated")
	}
}

func TestCompleteLegacyOutputsDoNotGrantForceOwnership(t *testing.T) {
	tests := []struct {
		kind     string
		required []string
	}{
		{kind: "atlas", required: []string{"rkc.manifest.json", "bundle.json", "rkc-export-manifest.json"}},
		{kind: "synthesis", required: []string{"manifest.json", "claims.jsonl", "records.jsonl"}},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "legacy")
			if err := os.Mkdir(target, 0o755); err != nil {
				t.Fatal(err)
			}
			for _, name := range test.required {
				if err := os.WriteFile(filepath.Join(target, name), []byte("legacy"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Begin(target, "", true, test.kind); !errors.Is(err, ErrTargetUnowned) {
				t.Fatalf("Begin(force complete legacy output) = %v, want ErrTargetUnowned", err)
			}
			for _, name := range test.required {
				if data, err := os.ReadFile(filepath.Join(target, name)); err != nil || string(data) != "legacy" {
					t.Errorf("legacy file %q was changed: %q, %v", name, data, err)
				}
			}
		})
	}
}

func TestForceRequiresUntamperedCompleteManifest(t *testing.T) {
	makeTarget := func(t *testing.T) string {
		t.Helper()
		target := filepath.Join(t.TempDir(), "atlas")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, "bundle.json"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		finalizeOwnedAtlasFixture(t, target, "snapshot")
		return target
	}

	valid := makeTarget(t)
	transaction, err := Begin(valid, "", true, "atlas")
	if err != nil {
		t.Fatalf("Begin(valid owned output) = %v", err)
	}
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}

	tampered := makeTarget(t)
	if err := os.WriteFile(filepath.Join(tampered, "bundle.json"), []byte("[]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(tampered, "", true, "atlas"); !errors.Is(err, ErrTargetUnowned) || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("Begin(tampered output) = %v, want digest refusal", err)
	}

	extra := makeTarget(t)
	if err := os.WriteFile(filepath.Join(extra, "user-data"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(extra, "", true, "atlas"); !errors.Is(err, ErrTargetUnowned) || !strings.Contains(err.Error(), "unmanifested") {
		t.Fatalf("Begin(output with unmanifested data) = %v, want ownership refusal", err)
	}
	if data, err := os.ReadFile(filepath.Join(extra, "user-data")); err != nil || string(data) != "preserve" {
		t.Fatalf("ownership refusal changed extra file: %q, %v", data, err)
	}

	synthesis := filepath.Join(t.TempDir(), "synthesis")
	if err := os.Mkdir(synthesis, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(synthesis, "records.jsonl"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalizeOwnedSynthesisFixture(t, synthesis, "snapshot")
	synthesisTransaction, err := Begin(synthesis, "", true, "synthesis")
	if err != nil {
		t.Fatalf("Begin(valid synthesis output) = %v", err)
	}
	if err := synthesisTransaction.Abort(); err != nil {
		t.Fatal(err)
	}

	unknown := makeTarget(t)
	manifestPath := filepath.Join(unknown, "rkc-export-manifest.json")
	var manifest map[string]any
	data, err := os.ReadFile(manifestPath)
	if err != nil || json.Unmarshal(data, &manifest) != nil {
		t.Fatalf("read manifest fixture: %v", err)
	}
	manifest["unexpected"] = true
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Begin(unknown, "", true, "atlas"); !errors.Is(err, ErrTargetUnowned) || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("Begin(manifest with unknown field) = %v", err)
	}
}

func TestCommitAtomicallyReplacesOwnedOutput(t *testing.T) {
	if !renameNoReplaceSupported() {
		t.Skip("atomic no-replace publication is unavailable")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "atlas")
	if err := os.Mkdir(target, 0o755); err != nil {
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
	writeAtlasManifestFixture(t, transaction.Staging, "new-snapshot")
	if err := transaction.Commit("new-snapshot"); err != nil {
		t.Fatal(err)
	}
	if !transaction.committed || transaction.Staging == target {
		t.Fatalf("committed transaction state = %+v", transaction)
	}
	if _, err := os.Stat(filepath.Join(target, "old.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old output remains after commit: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "new.txt")); err != nil || string(data) != "new" {
		t.Fatalf("new output = %q, %v", data, err)
	}
	marker, err := ReadMarker(target)
	if err != nil || marker.Kind != "atlas" || marker.SnapshotID != "new-snapshot" {
		t.Fatalf("final marker = %+v, %v", marker, err)
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rkc-backup-") || strings.HasPrefix(entry.Name(), ".rkc-build-") || strings.HasPrefix(entry.Name(), ".rkc-quarantine-") {
			t.Fatalf("commit left transactional path %q", entry.Name())
		}
	}
	if err := transaction.Commit("again"); !errors.Is(err, ErrInvalidStaging) {
		t.Fatalf("second Commit() = %v, want ErrInvalidStaging", err)
	}
	if err := transaction.Abort(); err != nil {
		t.Fatalf("Abort(committed) = %v", err)
	}
}

func TestCommitExchangeFailurePreservesPriorAndStaging(t *testing.T) {
	if !replacementHasNoMissingTargetWindow() {
		t.Skip("Linux atomic-exchange behavior")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "atlas")
	if err := os.Mkdir(target, 0o755); err != nil {
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
	originalExchange := exchangeOperation
	exchangeOperation = func(_, _ string) error { return errors.New("injected exchange refusal") }
	t.Cleanup(func() { exchangeOperation = originalExchange })
	if err := transaction.Commit("new"); err == nil || !strings.Contains(err.Error(), "injected exchange refusal") {
		t.Fatalf("Commit(exchange failure) = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "old.txt")); err != nil || string(data) != "old" {
		t.Fatalf("prior output changed: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(transaction.Staging, "new.txt")); err != nil || string(data) != "new" {
		t.Fatalf("staging output changed: %q, %v", data, err)
	}
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitRejectsTargetSwapAfterAtomicExchange(t *testing.T) {
	if !replacementHasNoMissingTargetWindow() {
		t.Skip("Linux atomic-exchange behavior")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "atlas")
	if err := os.Mkdir(target, 0o755); err != nil {
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
	originalExchange := exchangeOperation
	retainedPublished := filepath.Join(parent, "retained-published")
	exchangeOperation = func(first, second string) error {
		if err := exchangePaths(first, second); err != nil {
			return err
		}
		if err := os.Rename(second, retainedPublished); err != nil {
			return err
		}
		if err := os.Mkdir(second, 0o755); err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(second, "user-data"), []byte("preserve"), 0o600)
	}
	t.Cleanup(func() { exchangeOperation = originalExchange })
	if err := transaction.Commit("new"); !errors.Is(err, ErrInvalidStaging) || !strings.Contains(err.Error(), "exact rollback identities unavailable") {
		t.Fatalf("Commit(post-exchange target swap) = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "user-data")); err != nil || string(data) != "preserve" {
		t.Fatalf("post-exchange replacement changed: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(retainedPublished, "new.txt")); err != nil || string(data) != "new" {
		t.Fatalf("exact staged inode was not retained: %q, %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(transaction.Staging, "old.txt")); err != nil || string(data) != "old" {
		t.Fatalf("prior output was not retained: %q, %v", data, err)
	}
}

func TestRecoveryCompletesCrashImmediatelyAfterExchange(t *testing.T) {
	if !replacementHasNoMissingTargetWindow() {
		t.Skip("Linux atomic-exchange behavior")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "atlas")
	if err := os.Mkdir(target, 0o755); err != nil {
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
	if err := exchangePaths(transaction.Staging, transaction.Target); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedReplacements(parent, filepath.Base(target)); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "new.txt")); err != nil || string(data) != "new" {
		t.Fatalf("recovered target = %q, %v", data, err)
	}
	if _, err := os.Stat(journal.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery retained completed journal: %v", err)
	}
}

func TestRecoveryCompletesCrashAfterPriorQuarantine(t *testing.T) {
	if !replacementHasNoMissingTargetWindow() {
		t.Skip("Linux atomic-exchange behavior")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "atlas")
	if err := os.Mkdir(target, 0o755); err != nil {
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
	if err := exchangePaths(transaction.Staging, transaction.Target); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(transaction.Staging, filepath.Join(journal.root, "payload")); err != nil {
		t.Fatal(err)
	}
	if err := journal.update("prior-quarantined"); err != nil {
		t.Fatal(err)
	}
	if err := recoverInterruptedReplacements(parent, filepath.Base(target)); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "new.txt")); err != nil || string(data) != "new" {
		t.Fatalf("recovered target = %q, %v", data, err)
	}
	if _, err := os.Stat(journal.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recovery retained completed journal: %v", err)
	}
}

func TestCommitNeverOverwritesRacedNewTarget(t *testing.T) {
	if !renameNoReplaceSupported() {
		t.Skip("atomic no-replace publication is unavailable")
	}
	parent := t.TempDir()
	target := filepath.Join(parent, "atlas")
	transaction, err := Begin(target, "", false, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(transaction.Staging, "new.txt"), []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAtlasManifestFixture(t, transaction.Staging, "new")
	originalRename := renameNoReplaceOperation
	renameNoReplaceOperation = func(first, second string) error {
		if err := os.Mkdir(second, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(second, "user-data"), []byte("preserve"), 0o600); err != nil {
			return err
		}
		return originalRename(first, second)
	}
	t.Cleanup(func() { renameNoReplaceOperation = originalRename })
	if err := transaction.Commit("new"); err == nil {
		t.Fatal("Commit(target creation race) succeeded")
	}
	if data, err := os.ReadFile(filepath.Join(target, "user-data")); err != nil || string(data) != "preserve" {
		t.Fatalf("raced target changed: %q, %v", data, err)
	}
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitRechecksOwnershipAndFailedCommitRemainsAbortable(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "atlas")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "bundle.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalizeOwnedAtlasFixture(t, target, "old")
	transaction, err := Begin(target, "", true, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	staging := transaction.Staging
	if err := os.RemoveAll(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "user-data"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit("snapshot"); !errors.Is(err, ErrTargetUnowned) {
		t.Fatalf("Commit(after ownership swap) = %v, want ErrTargetUnowned", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "user-data")); err != nil || string(data) != "preserve" {
		t.Fatalf("failed Commit damaged user target: %q, %v", data, err)
	}
	if err := transaction.Abort(); err != nil {
		t.Fatalf("Abort(after failed Commit) = %v", err)
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Abort left failed staging path: %v", err)
	}
}

func TestCommitRejectsInvalidStagedManifestBeforeQuarantine(t *testing.T) {
	parent := t.TempDir()
	target := filepath.Join(parent, "atlas")
	if err := os.Mkdir(target, 0o755); err != nil {
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
	if err := transaction.Commit("new"); !errors.Is(err, ErrInvalidStaging) || !strings.Contains(err.Error(), "manifest") {
		t.Fatalf("Commit(invalid staged manifest) = %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(target, "old.txt")); err != nil || string(data) != "old" {
		t.Fatalf("invalid staged output changed prior target: %q, %v", data, err)
	}
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}
}

func TestCommitRejectsForgedStagingAndAbortIsFailClosed(t *testing.T) {
	var nilTransaction *Transaction
	if err := nilTransaction.Commit("snapshot"); !errors.Is(err, ErrInvalidStaging) {
		t.Fatalf("nil Commit() = %v", err)
	}
	if err := nilTransaction.Abort(); err != nil {
		t.Fatalf("nil Abort() = %v", err)
	}
	if err := (&Transaction{}).Abort(); err != nil {
		t.Fatalf("empty Abort() = %v", err)
	}

	target := filepath.Join(t.TempDir(), "target")
	transaction, err := Begin(target, "", false, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	originalStaging := transaction.Staging
	transaction.Staging = filepath.Join(t.TempDir(), "not-a-sibling")
	if err := transaction.Commit("snapshot"); !errors.Is(err, ErrInvalidStaging) {
		t.Fatalf("Commit(non-sibling staging) = %v", err)
	}
	transaction.Staging = originalStaging
	if err := transaction.Abort(); err != nil {
		t.Fatal(err)
	}

	transaction, err = Begin(filepath.Join(t.TempDir(), "target"), "", false, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	staging := transaction.Staging
	if err := os.Remove(filepath.Join(staging, MarkerName)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "user-data"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Abort(); !errors.Is(err, ErrInvalidStaging) {
		t.Fatalf("Abort(unmarked staging) = %v, want ErrInvalidStaging", err)
	}
	if data, err := os.ReadFile(filepath.Join(staging, "user-data")); err != nil || string(data) != "keep" {
		t.Fatalf("Abort removed unmarked path: %q, %v", data, err)
	}
}

func TestStagingIdentityAndMarkerTamperingAreRejected(t *testing.T) {
	parent := t.TempDir()
	transaction, err := Begin(filepath.Join(parent, "target-one"), "", false, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	staging := transaction.Staging
	markerBytes, err := os.ReadFile(filepath.Join(staging, MarkerName))
	if err != nil {
		t.Fatal(err)
	}
	retainedOriginal := filepath.Join(parent, "retained-original-staging")
	if err := os.Rename(staging, retainedOriginal); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, MarkerName), markerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit("snapshot"); !errors.Is(err, ErrInvalidStaging) {
		t.Fatalf("Commit(replaced staging inode) = %v", err)
	}
	if err := transaction.Abort(); !errors.Is(err, ErrInvalidStaging) {
		t.Fatalf("Abort(replaced staging inode) = %v", err)
	}

	transaction, err = Begin(filepath.Join(parent, "target-two"), "", false, "atlas")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(transaction.Staging, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit("snapshot"); !errors.Is(err, ErrInvalidStaging) || !strings.Contains(err.Error(), "marker kind changed") {
		t.Fatalf("Commit(changed staging marker) = %v", err)
	}
	if err := transaction.Abort(); !errors.Is(err, ErrInvalidStaging) {
		t.Fatalf("Abort(changed staging marker) = %v", err)
	}

	if err := (&Transaction{Staging: staging}).validateStaging("staging"); !errors.Is(err, ErrInvalidStaging) {
		t.Fatalf("validateStaging(no identity) = %v", err)
	}
	file := filepath.Join(parent, "staging-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := (&Transaction{Staging: file, identity: identity}).validateStaging("staging"); !errors.Is(err, ErrInvalidStaging) {
		t.Fatalf("validateStaging(regular file) = %v", err)
	}
}

func TestRecursiveRemovalRejectsDirectoryAndMarkerReplacement(t *testing.T) {
	parent := t.TempDir()
	original := filepath.Join(parent, "owned")
	if err := os.Mkdir(original, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(original, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "staging"}); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(original)
	if err != nil {
		t.Fatal(err)
	}
	retained := filepath.Join(parent, "retained-original")
	if err := os.Rename(original, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(original, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(original, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "staging"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(original, "user-data"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedDirectory(original, identity, ErrInvalidStaging, "staging"); !errors.Is(err, ErrInvalidStaging) {
		t.Fatalf("removeOwnedDirectory(replaced inode) = %v, want ErrInvalidStaging", err)
	}
	if data, err := os.ReadFile(filepath.Join(original, "user-data")); err != nil || string(data) != "preserve" {
		t.Fatalf("inode mismatch removed replacement data: %q, %v", data, err)
	}
	if _, err := os.Stat(retained); err != nil {
		t.Fatalf("inode mismatch removed retained original: %v", err)
	}

	markerChanged := filepath.Join(parent, "marker-changed")
	if err := os.Mkdir(markerChanged, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(markerChanged, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas"}); err != nil {
		t.Fatal(err)
	}
	markerIdentity, err := os.Lstat(markerChanged)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(markerChanged, "user-data"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(markerChanged, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "other"}); err != nil {
		t.Fatal(err)
	}
	if err := removeOwnedDirectory(markerChanged, markerIdentity, ErrTargetUnowned, "atlas"); !errors.Is(err, ErrTargetUnowned) {
		t.Fatalf("removeOwnedDirectory(changed marker) = %v, want ErrTargetUnowned", err)
	}
	if data, err := os.ReadFile(filepath.Join(markerChanged, "user-data")); err != nil || string(data) != "preserve" {
		t.Fatalf("marker mismatch removed replacement data: %q, %v", data, err)
	}
}

func TestQuarantineBindsRemovalToMovedIdentity(t *testing.T) {
	parent := t.TempDir()
	owned := filepath.Join(parent, "owned")
	if err := os.Mkdir(owned, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(owned, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "staging"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "generated"), []byte("remove"), 0o600); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(owned)
	if err != nil {
		t.Fatal(err)
	}
	quarantine, err := quarantineOwnedDirectory(owned, identity, ErrInvalidStaging, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(owned, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "user-data"), []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := quarantine.remove(ErrInvalidStaging, "staging"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(owned, "user-data")); err != nil || string(data) != "preserve" {
		t.Fatalf("quarantined removal touched replacement: %q, %v", data, err)
	}
	if _, err := os.Stat(quarantine.root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine remains after removal: %v", err)
	}

	restorable := filepath.Join(parent, "restorable")
	if err := os.Mkdir(restorable, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(restorable, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "staging"}); err != nil {
		t.Fatal(err)
	}
	restorableIdentity, err := os.Lstat(restorable)
	if err != nil {
		t.Fatal(err)
	}
	restoredQuarantine, err := quarantineOwnedDirectory(restorable, restorableIdentity, ErrInvalidStaging, "staging")
	if err != nil {
		t.Fatal(err)
	}
	if err := restoredQuarantine.restore(restorable); err != nil {
		t.Fatal(err)
	}
	restoredIdentity, err := os.Lstat(restorable)
	if err != nil || !os.SameFile(restorableIdentity, restoredIdentity) {
		t.Fatalf("restored directory identity changed: %v", err)
	}
}

func TestFilesystemShapeErrorsFailClosed(t *testing.T) {
	base := t.TempDir()
	parentFile := filepath.Join(base, "parent-file")
	if err := os.WriteFile(parentFile, []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(parentFile, "output")
	if _, err := ResolveTarget(target, ""); err == nil {
		t.Fatal("ResolveTarget(path below regular file) succeeded")
	}
	if err := checkExisting(target, true, "atlas"); err == nil || !strings.Contains(err.Error(), "inspect output target") {
		t.Fatalf("checkExisting(path below file) = %v", err)
	}
	if err := writeMarker(filepath.Join(base, "missing-root"), Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas"}); err == nil || !strings.Contains(err.Error(), "create RKC output marker") {
		t.Fatalf("writeMarker(missing root) = %v", err)
	}
}

func TestWriteMarkerRollbackAndPathHelpers(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, MarkerName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(root, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas"}); err == nil {
		t.Fatal("writeMarker(directory destination) succeeded")
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".rkc-marker-") {
			t.Fatalf("failed writeMarker left temporary %q", entry.Name())
		}
	}

	if err := syncDirectory(filepath.Join(root, "missing")); err == nil {
		t.Fatal("syncDirectory(missing) succeeded")
	}

	if !containsPath("/a", "/a") || !containsPath("/a", "/a/b") || containsPath("/a/b", "/a") || containsPath("/a", "/ab") {
		t.Fatal("containsPath returned incorrect containment")
	}
	if resolved, err := resolveExistingParent(filepath.Join(root, "one", "two")); err != nil || resolved != filepath.Join(root, "one", "two") {
		t.Fatalf("resolveExistingParent(missing suffix) = %q, %v", resolved, err)
	}
}

func TestMarkerJSONShapeIsStable(t *testing.T) {
	marker := Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas", SnapshotID: "snapshot"}
	data, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"schema_version": markerVersion, "producer": producer, "kind": "atlas", "snapshot_id": "snapshot"}
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("Marker JSON = %v, want %v", decoded, want)
	}
}

func finalizeOwnedAtlasFixture(t *testing.T, root, snapshotID string) {
	t.Helper()
	writeAtlasManifestFixture(t, root, snapshotID)
	if err := writeMarker(root, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas", SnapshotID: snapshotID}); err != nil {
		t.Fatal(err)
	}
}

func writeAtlasManifestFixture(t *testing.T, root, snapshotID string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var files []ownershipManifestFile
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == MarkerName || entry.Name() == "rkc-export-manifest.json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		size := int64(len(data))
		files = append(files, ownershipManifestFile{Path: entry.Name(), SHA256: hex.EncodeToString(digest[:]), SizeBytes: &size})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	manifest := ownershipManifest{SchemaVersion: outputManifestVersion, SnapshotID: snapshotID, Files: files}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(root, "rkc-export-manifest.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func finalizeOwnedSynthesisFixture(t *testing.T, root, snapshotID string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "records.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	size := int64(len(data))
	manifest := ownershipManifest{
		SchemaVersion: outputManifestVersion,
		SnapshotID:    snapshotID,
		Files: []ownershipManifestFile{{
			Path: "records.jsonl", SHA256: hex.EncodeToString(digest[:]), Bytes: &size,
		}},
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	encoded = append(encoded, '\n')
	if err := os.WriteFile(filepath.Join(root, "manifest.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(root, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "synthesis", SnapshotID: snapshotID}); err != nil {
		t.Fatal(err)
	}
}
