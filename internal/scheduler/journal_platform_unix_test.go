//go:build !windows

package scheduler

import (
	"context"
	"os"
	"testing"
)

func TestFileJournalRejectsUnixPrivacyDrift(t *testing.T) {
	t.Run("root", func(t *testing.T) {
		root := t.TempDir()
		journal := openTestJournal(t, root, testRunID)
		if err := os.Chmod(root, 0o750); err != nil {
			_ = journal.Close()
			t.Fatal(err)
		}
		err := journal.Append(context.Background(), JournalRecord{
			RunID: testRunID,
			Kind:  JournalKindRun,
			State: JournalStateRunning,
			Plan:  testJournalPlan("stage"),
		})
		if err == nil {
			t.Fatal("Append accepted a group-accessible journal root")
		}
		_ = journal.Close()
	})

	t.Run("file", func(t *testing.T) {
		journal := openTestJournal(t, t.TempDir(), testRunID)
		if err := os.Chmod(journal.Path(), 0o640); err != nil {
			_ = journal.Close()
			t.Fatal(err)
		}
		err := journal.Append(context.Background(), JournalRecord{
			RunID: testRunID,
			Kind:  JournalKindRun,
			State: JournalStateRunning,
			Plan:  testJournalPlan("stage"),
		})
		if err == nil {
			t.Fatal("Append accepted a group-readable journal file")
		}
		_ = journal.Close()
	})
}
