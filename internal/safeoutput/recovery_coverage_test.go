package safeoutput

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/privatepath"
)

func TestRecoveryRejectsTamperedJournalBoundOutputs(t *testing.T) {
	t.Run("published target", func(t *testing.T) {
		transaction, journal := exchangedRecoveryFixture(t)
		if err := os.WriteFile(filepath.Join(transaction.Target, "new.txt"), []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := recoverReplacement(filepath.Dir(transaction.Target), journal)
		if err == nil || !strings.Contains(err.Error(), "recovered target validation failed") {
			t.Fatalf("recoverReplacement(tampered target) = %v", err)
		}
		assertPathExists(t, transaction.Staging)
		assertPathExists(t, journal.root)
	})

	t.Run("prior staging", func(t *testing.T) {
		transaction, journal := exchangedRecoveryFixture(t)
		if err := os.WriteFile(filepath.Join(transaction.Staging, "old.txt"), []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverReplacement(filepath.Dir(transaction.Target), journal); err == nil {
			t.Fatal("recoverReplacement(tampered prior staging) succeeded")
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, transaction.Staging)
		assertPathExists(t, journal.root)
	})

	t.Run("prior quarantine payload", func(t *testing.T) {
		transaction, journal := exchangedRecoveryFixture(t)
		payload := filepath.Join(journal.root, "payload")
		if err := os.Rename(transaction.Staging, payload); err != nil {
			t.Fatal(err)
		}
		if err := journal.update("prior-quarantined"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(payload, "old.txt"), []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverReplacement(filepath.Dir(transaction.Target), journal); err == nil {
			t.Fatal("recoverReplacement(tampered prior payload) succeeded")
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, payload)
		assertPathExists(t, journal.root)
	})

	t.Run("prior quarantine payload while target is missing", func(t *testing.T) {
		transaction, journal, payload := portableRecoveryFixture(t)
		if err := os.WriteFile(filepath.Join(payload, "old.txt"), []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverReplacement(filepath.Dir(transaction.Target), journal); err == nil {
			t.Fatal("recoverReplacement(tampered portable prior payload) succeeded")
		}
		assertPathMissing(t, transaction.Target)
		assertPathExists(t, transaction.Staging)
		assertPathExists(t, payload)
		assertPathExists(t, journal.root)
	})

	t.Run("prior target before exchange", func(t *testing.T) {
		transaction, _, journal := newReplacementFixture(t)
		if err := os.WriteFile(filepath.Join(transaction.Target, "old.txt"), []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverReplacement(filepath.Dir(transaction.Target), journal); err == nil {
			t.Fatal("recoverReplacement(tampered prior target) succeeded")
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, transaction.Staging)
		assertPathExists(t, journal.root)
	})

	t.Run("new staging before exchange", func(t *testing.T) {
		transaction, _, journal := newReplacementFixture(t)
		if err := os.WriteFile(filepath.Join(transaction.Staging, "new.txt"), []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverReplacement(filepath.Dir(transaction.Target), journal); err == nil {
			t.Fatal("recoverReplacement(tampered new staging) succeeded")
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, transaction.Staging)
		assertPathExists(t, journal.root)
	})
}

func TestRecoveryRejectsAmbiguousPublicationIdentities(t *testing.T) {
	t.Run("both prior staging and payload", func(t *testing.T) {
		transaction, journal := exchangedRecoveryFixture(t)
		payload := filepath.Join(journal.root, "payload")
		if err := os.Mkdir(payload, 0o700); err != nil {
			t.Fatal(err)
		}
		err := recoverReplacement(filepath.Dir(transaction.Target), journal)
		if err == nil || !strings.Contains(err.Error(), "both staging and quarantine payload") {
			t.Fatalf("recoverReplacement(duplicate prior locations) = %v", err)
		}
		assertPathExists(t, transaction.Staging)
		assertPathExists(t, payload)
	})

	t.Run("unrelated staging at expected path", func(t *testing.T) {
		transaction, journal := exchangedRecoveryFixture(t)
		retainedPrior := filepath.Join(filepath.Dir(transaction.Target), "retained-prior")
		if err := os.Rename(transaction.Staging, retainedPrior); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(transaction.Staging, 0o700); err != nil {
			t.Fatal(err)
		}
		userData := filepath.Join(transaction.Staging, "user-data")
		if err := os.WriteFile(userData, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		err := recoverReplacement(filepath.Dir(transaction.Target), journal)
		if err == nil || !strings.Contains(err.Error(), "identity is ambiguous") {
			t.Fatalf("recoverReplacement(unrelated staging) = %v", err)
		}
		if data, err := os.ReadFile(userData); err != nil || string(data) != "preserve" {
			t.Fatalf("unrelated staging changed: %q, %v", data, err)
		}
		assertPathExists(t, retainedPrior)
		assertPathExists(t, journal.root)
	})

	t.Run("unexpected payload before exchange", func(t *testing.T) {
		transaction, _, journal := newReplacementFixture(t)
		payload := filepath.Join(journal.root, "payload")
		if err := os.Mkdir(payload, 0o700); err != nil {
			t.Fatal(err)
		}
		err := recoverReplacement(filepath.Dir(transaction.Target), journal)
		if err == nil || !strings.Contains(err.Error(), "identities are ambiguous") {
			t.Fatalf("recoverReplacement(unexpected payload) = %v", err)
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, transaction.Staging)
		assertPathExists(t, payload)
	})
}

func TestRecoveryCompletesOrRetainsFailClosedState(t *testing.T) {
	t.Run("published target with no prior path discards journal", func(t *testing.T) {
		transaction, journal := exchangedRecoveryFixture(t)
		retainedPrior := filepath.Join(filepath.Dir(transaction.Target), "retained-prior")
		if err := os.Rename(transaction.Staging, retainedPrior); err != nil {
			t.Fatal(err)
		}
		if err := recoverReplacement(filepath.Dir(transaction.Target), journal); err != nil {
			t.Fatal(err)
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, retainedPrior)
		assertPathMissing(t, journal.root)
	})

	t.Run("changed journal root after exchange", func(t *testing.T) {
		transaction, journal := exchangedRecoveryFixture(t)
		retainedJournal := replaceJournalRootIdentity(t, journal)
		err := recoverReplacement(filepath.Dir(transaction.Target), journal)
		if err == nil || !strings.Contains(err.Error(), "journal root identity changed") {
			t.Fatalf("recoverReplacement(changed journal after exchange) = %v", err)
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, filepath.Join(journal.root, "payload"))
		assertPathExists(t, retainedJournal)
	})

	t.Run("changed journal root before exchange", func(t *testing.T) {
		transaction, _, journal := newReplacementFixture(t)
		retainedJournal := replaceJournalRootIdentity(t, journal)
		err := recoverReplacement(filepath.Dir(transaction.Target), journal)
		if err == nil || !strings.Contains(err.Error(), "journal root identity changed") {
			t.Fatalf("recoverReplacement(changed journal before exchange) = %v", err)
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, filepath.Join(journal.root, "payload"))
		assertPathExists(t, retainedJournal)
	})

	t.Run("prior staging move after exchange refuses non-directory payload", func(t *testing.T) {
		transaction, journal := exchangedRecoveryFixture(t)
		payload := filepath.Join(journal.root, "payload")
		if err := os.WriteFile(payload, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverReplacement(filepath.Dir(transaction.Target), journal); err == nil {
			t.Fatal("recoverReplacement(non-directory payload after exchange) succeeded")
		}
		if data, err := os.ReadFile(payload); err != nil || string(data) != "preserve" {
			t.Fatalf("non-directory payload changed: %q, %v", data, err)
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, transaction.Staging)
	})

	t.Run("new staging move before exchange refuses non-directory payload", func(t *testing.T) {
		transaction, _, journal := newReplacementFixture(t)
		payload := filepath.Join(journal.root, "payload")
		if err := os.WriteFile(payload, []byte("preserve"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := recoverReplacement(filepath.Dir(transaction.Target), journal); err == nil {
			t.Fatal("recoverReplacement(non-directory payload before exchange) succeeded")
		}
		if data, err := os.ReadFile(payload); err != nil || string(data) != "preserve" {
			t.Fatalf("non-directory payload changed: %q, %v", data, err)
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, transaction.Staging)
	})

	t.Run("portable restore refuses pathname replacement", func(t *testing.T) {
		transaction, journal, payload := portableRecoveryFixture(t)
		originalRename := renameNoReplaceOperation
		renameNoReplaceOperation = func(_, _ string) error { return errors.New("injected no-replace refusal") }
		t.Cleanup(func() { renameNoReplaceOperation = originalRename })
		err := recoverReplacement(filepath.Dir(transaction.Target), journal)
		if err == nil || !strings.Contains(err.Error(), "injected no-replace refusal") {
			t.Fatalf("recoverReplacement(no-replace refusal) = %v", err)
		}
		assertPathMissing(t, transaction.Target)
		assertPathExists(t, transaction.Staging)
		assertPathExists(t, payload)
	})

	t.Run("portable restored target is revalidated", func(t *testing.T) {
		if !renameNoReplaceSupported() {
			t.Skip("atomic no-replace recovery is unavailable")
		}
		transaction, journal, _ := portableRecoveryFixture(t)
		originalRename := renameNoReplaceOperation
		renameNoReplaceOperation = func(source, target string) error {
			if err := originalRename(source, target); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(target, "old.txt"), []byte("tampered"), 0o600)
		}
		t.Cleanup(func() { renameNoReplaceOperation = originalRename })
		if err := recoverReplacement(filepath.Dir(transaction.Target), journal); err == nil {
			t.Fatal("recoverReplacement(tampered restored target) succeeded")
		}
		assertPathExists(t, transaction.Target)
		assertPathExists(t, transaction.Staging)
		assertPathExists(t, journal.root)
	})

	t.Run("portable new staging quarantine refusal retains both outputs", func(t *testing.T) {
		if !renameNoReplaceSupported() {
			t.Skip("atomic no-replace recovery is unavailable")
		}
		transaction, journal, payload := portableRecoveryFixture(t)
		originalRename := renameNoReplaceOperation
		renameNoReplaceOperation = func(source, target string) error {
			if err := originalRename(source, target); err != nil {
				return err
			}
			if err := os.Mkdir(payload, 0o700); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(payload, "blocker"), []byte("preserve"), 0o600)
		}
		t.Cleanup(func() { renameNoReplaceOperation = originalRename })
		if err := recoverReplacement(filepath.Dir(transaction.Target), journal); err == nil {
			t.Fatal("recoverReplacement(blocked staging quarantine) succeeded")
		}
		if data, err := os.ReadFile(filepath.Join(transaction.Target, "old.txt")); err != nil || string(data) != "old" {
			t.Fatalf("restored prior output = %q, %v", data, err)
		}
		assertPathExists(t, transaction.Staging)
		assertPathExists(t, payload)
		assertPathExists(t, journal.root)
	})
}

func TestOwnershipIdentityChecksFailClosed(t *testing.T) {
	t.Run("unmarked exact directory", func(t *testing.T) {
		root := t.TempDir()
		identity, err := privatepath.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateOwnedDirectory(root, identity, ErrTargetUnowned, "atlas"); !errors.Is(err, ErrTargetUnowned) || !strings.Contains(err.Error(), "unmarked") {
			t.Fatalf("validateOwnedDirectory(unmarked) = %v", err)
		}
	})

	t.Run("marker cannot replace complete ownership proof", func(t *testing.T) {
		root := t.TempDir()
		if err := writeMarker(root, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "atlas", SnapshotID: "snapshot"}); err != nil {
			t.Fatal(err)
		}
		identity, err := privatepath.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		err = validateOwnedDirectory(root, identity, ErrTargetUnowned, "atlas")
		if !errors.Is(err, ErrTargetUnowned) || !strings.Contains(err.Error(), "complete ownership validation failed") {
			t.Fatalf("validateOwnedDirectory(marker only) = %v", err)
		}
	})

	t.Run("published snapshot binding", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "atlas")
		if err := os.Mkdir(root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "payload"), []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		finalizeOwnedAtlasFixture(t, root, "snapshot")
		identity, err := privatepath.Lstat(root)
		if err != nil {
			t.Fatal(err)
		}
		if err := validatePublishedOutput(root, "atlas", identity, "other"); err == nil || !strings.Contains(err.Error(), "requested kind and snapshot") {
			t.Fatalf("validatePublishedOutput(wrong snapshot) = %v", err)
		}
	})

	t.Run("quarantine root identity", func(t *testing.T) {
		parent := t.TempDir()
		owned := filepath.Join(parent, "owned")
		if err := os.Mkdir(owned, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeMarker(owned, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "staging"}); err != nil {
			t.Fatal(err)
		}
		identity, err := privatepath.Lstat(owned)
		if err != nil {
			t.Fatal(err)
		}
		quarantine, err := quarantineOwnedDirectory(owned, identity, ErrInvalidStaging, "staging")
		if err != nil {
			t.Fatal(err)
		}
		retained := quarantine.root + "-retained"
		if err := os.Rename(quarantine.root, retained); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(quarantine.root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := quarantine.validate(ErrInvalidStaging, "staging"); !errors.Is(err, ErrInvalidStaging) || !strings.Contains(err.Error(), "quarantine identity changed") {
			t.Fatalf("quarantine.validate(replaced root) = %v", err)
		}
		assertPathExists(t, filepath.Join(retained, "payload"))
	})

	t.Run("restore refuses existing target", func(t *testing.T) {
		parent := t.TempDir()
		owned := filepath.Join(parent, "owned")
		if err := os.Mkdir(owned, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeMarker(owned, Marker{SchemaVersion: markerVersion, Producer: producer, Kind: "staging"}); err != nil {
			t.Fatal(err)
		}
		identity, err := privatepath.Lstat(owned)
		if err != nil {
			t.Fatal(err)
		}
		quarantine, err := quarantineOwnedDirectory(owned, identity, ErrInvalidStaging, "staging")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(owned, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := quarantine.restore(owned); err == nil || !strings.Contains(err.Error(), "already exists") {
			t.Fatalf("quarantine.restore(existing target) = %v", err)
		}
		assertPathExists(t, quarantine.payload)
	})
}

func exchangedRecoveryFixture(t *testing.T) (*Transaction, *replacementJournal) {
	t.Helper()
	if !replacementHasNoMissingTargetWindow() {
		t.Skip("atomic exchange recovery state is unavailable")
	}
	transaction, _, journal := newReplacementFixture(t)
	if err := exchangePaths(transaction.Staging, transaction.Target); err != nil {
		t.Fatal(err)
	}
	return transaction, journal
}

func portableRecoveryFixture(t *testing.T) (*Transaction, *replacementJournal, string) {
	t.Helper()
	transaction, _, journal := newReplacementFixture(t)
	payload := filepath.Join(journal.root, "payload")
	if err := os.Rename(transaction.Target, payload); err != nil {
		t.Fatal(err)
	}
	if err := journal.update("prior-quarantined"); err != nil {
		t.Fatal(err)
	}
	return transaction, journal, payload
}

func replaceJournalRootIdentity(t *testing.T, journal *replacementJournal) string {
	t.Helper()
	retained := journal.root + "-retained"
	if err := os.Rename(journal.root, retained); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(journal.root, 0o700); err != nil {
		t.Fatal(err)
	}
	return retained
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := privatepath.Lstat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := privatepath.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be absent: %v", path, err)
	}
}
