package safeoutput

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestGapSafeOutputSetupAndRecoveryFailuresPreserveInputs(t *testing.T) {
	t.Run("malformed recovery journal blocks begin", func(t *testing.T) {
		parent := t.TempDir()
		journalRoot := filepath.Join(parent, ".rkc-quarantine-malformed")
		if err := os.Mkdir(journalRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		journalPath := filepath.Join(journalRoot, journalName)
		if err := os.WriteFile(journalPath, []byte("{"), 0o600); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(parent, "atlas")
		if _, err := Begin(target, "", false, "atlas"); err == nil ||
			!strings.Contains(err.Error(), "recover interrupted output publication") {
			t.Fatalf("Begin(malformed recovery journal) = %v", err)
		}
		if data, err := os.ReadFile(journalPath); err != nil || string(data) != "{" {
			t.Fatalf("malformed recovery journal changed: %q, %v", data, err)
		}
		if _, err := os.Lstat(target); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("blocked Begin created target: %v", err)
		}
	})

	t.Run("regular protected root is rejected", func(t *testing.T) {
		parent := t.TempDir()
		protected := filepath.Join(parent, "protected-file")
		if err := os.WriteFile(protected, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveTarget(filepath.Join(parent, "atlas"), protected); !errors.Is(err, ErrUnsafeTarget) ||
			!strings.Contains(err.Error(), "not a resolved directory") {
			t.Fatalf("ResolveTarget(regular protected root) = %v", err)
		}
		if data, err := os.ReadFile(protected); err != nil || string(data) != "preserve" {
			t.Fatalf("protected file changed: %q, %v", data, err)
		}
	})

	t.Run("non-directory ancestor cannot be resolved as a parent", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows classifies a non-directory ancestor as a missing path")
		}
		parent := t.TempDir()
		blocker := filepath.Join(parent, "blocker")
		if err := os.WriteFile(blocker, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveExistingParent(filepath.Join(blocker, "child")); err == nil ||
			!strings.Contains(err.Error(), "inspect output parent") {
			t.Fatalf("resolveExistingParent(non-directory ancestor) = %v", err)
		}
	})

	missing := filepath.Join(t.TempDir(), "missing-parent")
	if err := recoverInterruptedReplacements(missing, "atlas"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recoverInterruptedReplacements(missing parent) = %v", err)
	}
}

func TestGapReplacementJournalCreationRejectsIncompleteOwnershipProofs(t *testing.T) {
	parent := t.TempDir()
	owned, ownedIdentity := newGapOwnedAtlas(t, parent, "owned", "old")

	t.Run("missing prior marker", func(t *testing.T) {
		transaction := &Transaction{
			Target: filepath.Join(parent, "missing"), Staging: owned,
			kind: "atlas", identity: ownedIdentity,
		}
		if _, err := createReplacementJournal(transaction, ownedIdentity, "new"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("createReplacementJournal(missing prior marker) = %v", err)
		}
	})

	t.Run("unsupported new identity", func(t *testing.T) {
		transaction := &Transaction{
			Target: owned, Staging: owned, kind: "atlas",
			identity: syntheticOutputFileInfo{system: "not a filesystem identity"},
		}
		if _, err := createReplacementJournal(transaction, ownedIdentity, "new"); err == nil ||
			!strings.Contains(err.Error(), "filesystem identity is unavailable") {
			t.Fatalf("createReplacementJournal(unsupported new identity) = %v", err)
		}
	})

	t.Run("unsupported prior identity", func(t *testing.T) {
		transaction := &Transaction{Target: owned, Staging: owned, kind: "atlas", identity: ownedIdentity}
		prior := syntheticOutputFileInfo{system: "not a filesystem identity"}
		if _, err := createReplacementJournal(transaction, prior, "new"); err == nil ||
			!strings.Contains(err.Error(), "filesystem identity is unavailable") {
			t.Fatalf("createReplacementJournal(unsupported prior identity) = %v", err)
		}
	})

	t.Run("staging metadata is required", func(t *testing.T) {
		staging := filepath.Join(parent, "empty-staging")
		if err := os.Mkdir(staging, 0o700); err != nil {
			t.Fatal(err)
		}
		stagingIdentity, err := os.Lstat(staging)
		if err != nil {
			t.Fatal(err)
		}
		transaction := &Transaction{Target: owned, Staging: staging, kind: "atlas", identity: stagingIdentity}
		if _, err := createReplacementJournal(transaction, ownedIdentity, "new"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("createReplacementJournal(missing staging metadata) = %v", err)
		}
	})

	t.Run("prior manifest is required", func(t *testing.T) {
		prior := filepath.Join(parent, "marker-only-prior")
		if err := os.Mkdir(prior, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeMarker(prior, Marker{
			SchemaVersion: markerVersion, Producer: producer, Kind: "atlas", SnapshotID: "old",
		}); err != nil {
			t.Fatal(err)
		}
		priorIdentity, err := os.Lstat(prior)
		if err != nil {
			t.Fatal(err)
		}
		transaction := &Transaction{Target: prior, Staging: owned, kind: "atlas", identity: ownedIdentity}
		if _, err := createReplacementJournal(transaction, priorIdentity, "new"); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("createReplacementJournal(missing prior manifest) = %v", err)
		}
	})

	otherIdentity, err := os.Lstat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := validateOwnedOutput(owned, "atlas", otherIdentity); err == nil ||
		!strings.Contains(err.Error(), "output directory changed") {
		t.Fatalf("validateOwnedOutput(wrong directory identity) = %v", err)
	}
}

func TestGapReplacementJournalRecoveryAndCleanupFailClosed(t *testing.T) {
	t.Run("unrelated valid journal is retained", func(t *testing.T) {
		parent := t.TempDir()
		journal := newGapPersistedJournal(t, parent, ".rkc-quarantine-unrelated")
		if err := recoverInterruptedReplacements(parent, "different-target"); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Lstat(journal.root); err != nil {
			t.Fatalf("unrelated journal was removed: %v", err)
		}
	})

	t.Run("matched ambiguous journal is retained", func(t *testing.T) {
		parent := t.TempDir()
		journal := newGapPersistedJournal(t, parent, ".rkc-quarantine-ambiguous")
		if err := recoverInterruptedReplacements(parent, journal.record.TargetName); err == nil ||
			!strings.Contains(err.Error(), "identities are ambiguous") {
			t.Fatalf("recoverInterruptedReplacements(ambiguous identities) = %v", err)
		}
		if _, err := os.Lstat(journal.root); err != nil {
			t.Fatalf("ambiguous journal was removed: %v", err)
		}
	})

	t.Run("journal must be a regular bounded file", func(t *testing.T) {
		parent := t.TempDir()
		directoryJournal := filepath.Join(parent, ".rkc-quarantine-directory")
		if err := os.Mkdir(directoryJournal, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(directoryJournal, journalName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := loadReplacementJournal(directoryJournal); err == nil ||
			!strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("loadReplacementJournal(directory journal) = %v", err)
		}

		oversizedRoot := filepath.Join(parent, ".rkc-quarantine-oversized")
		if err := os.Mkdir(oversizedRoot, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(
			filepath.Join(oversizedRoot, journalName),
			[]byte(strings.Repeat("x", journalMaxSize+1)),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := loadReplacementJournal(oversizedRoot); err == nil ||
			!strings.Contains(err.Error(), "bounded regular file") {
			t.Fatalf("loadReplacementJournal(oversized journal) = %v", err)
		}
	})

	t.Run("journal pathname replacement blocks discard", func(t *testing.T) {
		parent := t.TempDir()
		journal := newGapPersistedJournal(t, parent, ".rkc-quarantine-replaced-journal")
		retained := journal.path + ".retained"
		if err := os.Rename(journal.path, retained); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(journal.path, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := journal.discard(); err == nil || !strings.Contains(err.Error(), "journal identity changed") {
			t.Fatalf("discard(replaced journal) = %v", err)
		}
		for _, path := range []string{journal.path, retained} {
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("discard removed %s: %v", path, err)
			}
		}
	})

	t.Run("journal root replacement blocks discard", func(t *testing.T) {
		parent := t.TempDir()
		journal := newGapPersistedJournal(t, parent, ".rkc-quarantine-replaced-root")
		retained := replaceJournalRootIdentity(t, journal)
		journal.journalIdentity = nil
		if err := journal.discard(); err == nil || !strings.Contains(err.Error(), "root changed") {
			t.Fatalf("discard(replaced root) = %v", err)
		}
		if _, err := os.Lstat(retained); err != nil {
			t.Fatalf("discard removed retained root: %v", err)
		}
	})
}

func TestGapPortableAndExchangeFailuresRetainRecoveryState(t *testing.T) {
	t.Run("portable journal update failure", func(t *testing.T) {
		transaction, priorIdentity, journal := newReplacementFixture(t)
		retained := replaceJournalRootIdentity(t, journal)
		err := transaction.commitPortable(priorIdentity, "new", journal)
		if err == nil || !strings.Contains(err.Error(), "persist quarantine state") {
			t.Fatalf("commitPortable(replaced journal root) = %v", err)
		}
		if _, err := os.Lstat(filepath.Join(journal.root, "payload")); err != nil {
			t.Fatalf("prior output was not retained in quarantine: %v", err)
		}
		if _, err := os.Lstat(retained); err != nil {
			t.Fatalf("durable journal was not retained: %v", err)
		}
	})

	t.Run("portable publish and restore refusal", func(t *testing.T) {
		transaction, priorIdentity, journal := newReplacementFixture(t)
		refusal := errors.New("injected no-replace refusal")
		originalRename := renameNoReplaceOperation
		renameNoReplaceOperation = func(string, string) error { return refusal }
		t.Cleanup(func() { renameNoReplaceOperation = originalRename })
		err := transaction.commitPortable(priorIdentity, "new", journal)
		if !errors.Is(err, refusal) || !strings.Contains(err.Error(), "restore failed") {
			t.Fatalf("commitPortable(double refusal) = %v", err)
		}
		if _, err := os.Lstat(filepath.Join(journal.root, "payload")); err != nil {
			t.Fatalf("prior payload was not retained: %v", err)
		}
		if _, err := os.Lstat(transaction.Staging); err != nil {
			t.Fatalf("new staging was not retained: %v", err)
		}
	})

	t.Run("exchange detects tampered displaced prior", func(t *testing.T) {
		if !replacementHasNoMissingTargetWindow() {
			t.Skip("atomic exchange is unavailable")
		}
		transaction, priorIdentity, journal := newReplacementFixture(t)
		originalExchange := exchangeOperation
		exchangeOperation = func(first, second string) error {
			if err := originalExchange(first, second); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(first, "old.txt"), []byte("tampered"), 0o600)
		}
		t.Cleanup(func() { exchangeOperation = originalExchange })
		err := transaction.commitExchange(priorIdentity, "new", journal)
		if err == nil || !strings.Contains(err.Error(), "displaced prior output verification failed") ||
			!strings.Contains(err.Error(), "rolled-back prior output failed validation") {
			t.Fatalf("commitExchange(tampered prior) = %v", err)
		}
		if _, err := os.Lstat(journal.root); err != nil {
			t.Fatalf("tampered exchange lost recovery journal: %v", err)
		}
	})

	t.Run("exchange journal update failure", func(t *testing.T) {
		if !replacementHasNoMissingTargetWindow() {
			t.Skip("atomic exchange is unavailable")
		}
		transaction, priorIdentity, journal := newReplacementFixture(t)
		originalExchange := exchangeOperation
		retained := ""
		exchangeOperation = func(first, second string) error {
			if err := originalExchange(first, second); err != nil {
				return err
			}
			retained = journal.root + "-retained"
			if err := os.Rename(journal.root, retained); err != nil {
				return err
			}
			return os.Mkdir(journal.root, 0o700)
		}
		t.Cleanup(func() { exchangeOperation = originalExchange })
		err := transaction.commitExchange(priorIdentity, "new", journal)
		if err == nil || !strings.Contains(err.Error(), "persist exchanged publication state") {
			t.Fatalf("commitExchange(replaced journal root) = %v", err)
		}
		if retained == "" {
			t.Fatal("exchange did not retain the durable journal root")
		}
		if _, err := os.Lstat(retained); err != nil {
			t.Fatalf("durable exchange journal was not retained: %v", err)
		}
	})

	t.Run("rollback journal cleanup failure", func(t *testing.T) {
		if !replacementHasNoMissingTargetWindow() {
			t.Skip("atomic exchange is unavailable")
		}
		transaction, priorIdentity, journal := newReplacementFixture(t)
		if err := exchangePaths(transaction.Staging, transaction.Target); err != nil {
			t.Fatal(err)
		}
		originalExchange := exchangeOperation
		retainedJournal := journal.path + ".retained"
		exchangeOperation = func(first, second string) error {
			if err := originalExchange(first, second); err != nil {
				return err
			}
			if err := os.Rename(journal.path, retainedJournal); err != nil {
				return err
			}
			return os.WriteFile(journal.path, []byte("replacement"), 0o600)
		}
		t.Cleanup(func() { exchangeOperation = originalExchange })
		cause := errors.New("injected rollback cause")
		err := transaction.rollbackExchange(priorIdentity, journal, cause)
		if !errors.Is(err, cause) || !strings.Contains(err.Error(), "journal cleanup failed") {
			t.Fatalf("rollbackExchange(replaced journal) = %v", err)
		}
		for _, path := range []string{journal.path, retainedJournal} {
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("rollback removed %s: %v", path, err)
			}
		}
	})
}

func TestGapQuarantineRestoreAndCleanupNeverTouchReplacements(t *testing.T) {
	t.Run("restore operation refusal", func(t *testing.T) {
		quarantine, target := newGapStagingQuarantine(t)
		refusal := errors.New("injected restore refusal")
		originalRename := renameNoReplaceOperation
		renameNoReplaceOperation = func(string, string) error { return refusal }
		t.Cleanup(func() { renameNoReplaceOperation = originalRename })
		if err := quarantine.restore(target); !errors.Is(err, refusal) {
			t.Fatalf("quarantine.restore(refusal) = %v", err)
		}
		if _, err := os.Lstat(quarantine.payload); err != nil {
			t.Fatalf("restore refusal removed payload: %v", err)
		}
	})

	t.Run("restore rechecks final identity", func(t *testing.T) {
		quarantine, target := newGapStagingQuarantine(t)
		retained := target + "-retained"
		originalRename := renameNoReplaceOperation
		renameNoReplaceOperation = func(source, destination string) error {
			if err := originalRename(source, destination); err != nil {
				return err
			}
			if err := os.Rename(destination, retained); err != nil {
				return err
			}
			if err := os.Mkdir(destination, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(destination, "user-data"), []byte("preserve"), 0o600)
		}
		t.Cleanup(func() { renameNoReplaceOperation = originalRename })
		if err := quarantine.restore(target); err == nil || !strings.Contains(err.Error(), "restored directory identity changed") {
			t.Fatalf("quarantine.restore(replaced target) = %v", err)
		}
		if data, err := os.ReadFile(filepath.Join(target, "user-data")); err != nil || string(data) != "preserve" {
			t.Fatalf("restore changed replacement target: %q, %v", data, err)
		}
		if _, err := os.Lstat(retained); err != nil {
			t.Fatalf("restored owned inode was not retained: %v", err)
		}
	})

	var nilQuarantine *quarantinedDirectory
	if err := nilQuarantine.remove(ErrTargetUnowned); !errors.Is(err, ErrTargetUnowned) {
		t.Fatalf("nil quarantine remove = %v", err)
	}

	t.Run("changed journal identity blocks final removal", func(t *testing.T) {
		parent := t.TempDir()
		journal := newGapPersistedJournal(t, parent, ".rkc-quarantine-final-journal")
		quarantine := journal.quarantine(nil)
		retained := journal.path + ".retained"
		if err := os.Rename(journal.path, retained); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(journal.path, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := removeEmptyQuarantine(quarantine); err == nil ||
			!strings.Contains(err.Error(), "journal identity changed") {
			t.Fatalf("removeEmptyQuarantine(replaced journal) = %v", err)
		}
		for _, path := range []string{journal.path, retained} {
			if _, err := os.Lstat(path); err != nil {
				t.Fatalf("final removal changed %s: %v", path, err)
			}
		}
	})

	t.Run("nonempty quarantine root is retained", func(t *testing.T) {
		root := t.TempDir()
		identity, err := os.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		blocker := filepath.Join(root, "unrelated")
		if err := os.WriteFile(blocker, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		quarantine := &quarantinedDirectory{root: root, rootIdentity: identity}
		if err := removeEmptyQuarantine(quarantine); err == nil || !strings.Contains(err.Error(), "remove empty quarantine") {
			t.Fatalf("removeEmptyQuarantine(nonempty root) = %v", err)
		}
		if data, err := os.ReadFile(blocker); err != nil || string(data) != "preserve" {
			t.Fatalf("nonempty quarantine content changed: %q, %v", data, err)
		}
	})
}

func newGapOwnedAtlas(t *testing.T, parent, name, snapshotID string) (string, os.FileInfo) {
	t.Helper()
	root := filepath.Join(parent, name)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "payload.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	finalizeOwnedAtlasFixture(t, root, snapshotID)
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	return root, identity
}

func newGapPersistedJournal(t *testing.T, parent, name string) *replacementJournal {
	t.Helper()
	root := filepath.Join(parent, name)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(root)
	if err != nil {
		t.Fatal(err)
	}
	journal := &replacementJournal{
		root: root, path: filepath.Join(root, journalName), rootIdentity: identity,
		record: validReplacementJournalRecord(),
	}
	if err := journal.persist(); err != nil {
		t.Fatal(err)
	}
	return journal
}

func newGapStagingQuarantine(t *testing.T) (*quarantinedDirectory, string) {
	t.Helper()
	parent := t.TempDir()
	target := filepath.Join(parent, "owned")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeMarker(target, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "staging"}); err != nil {
		t.Fatal(err)
	}
	identity, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}
	quarantine, err := quarantineOwnedDirectory(target, identity, ErrInvalidStaging, "staging")
	if err != nil {
		t.Fatal(err)
	}
	return quarantine, target
}
