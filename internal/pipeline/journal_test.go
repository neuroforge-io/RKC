package pipeline

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/neuroforge-io/RKC/internal/scheduler"
)

type recordingJournal struct {
	mu      sync.Mutex
	runID   string
	records []scheduler.JournalRecord
}

func (journal *recordingJournal) RunID() string {
	return journal.runID
}

func (journal *recordingJournal) Append(
	ctx context.Context,
	record scheduler.JournalRecord,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	journal.records = append(journal.records, record)
	return nil
}

func (journal *recordingJournal) snapshot() []scheduler.JournalRecord {
	journal.mu.Lock()
	defer journal.mu.Unlock()
	return append([]scheduler.JournalRecord(nil), journal.records...)
}

func TestScanForwardsDurableSchedulerJournal(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(
		t,
		filepath.Join(root, "main.go"),
		"package fixture\n\nfunc Run() bool { return true }\n",
	)
	const runID = "0123456789abcdef0123456789abcdef"
	journal := &recordingJournal{runID: runID}
	bundle, coverage, err := Scan(context.Background(), Options{
		Root:              root,
		ToolVersion:       "journal-test",
		DisablePlugins:    true,
		DisableFrameworks: true,
		DisableSecretScan: true,
		RunID:             runID,
		Journal:           journal,
	})
	if err != nil {
		t.Fatal(err)
	}
	if bundle.Snapshot.ID == "" || coverage.SnapshotID != bundle.Snapshot.ID {
		t.Fatalf("journalled scan result = snapshot %q coverage %q",
			bundle.Snapshot.ID, coverage.SnapshotID)
	}

	records := journal.snapshot()
	if len(records) != 32 {
		t.Fatalf("journal record count = %d, want 32", len(records))
	}
	first, last := records[0], records[len(records)-1]
	if first.Kind != scheduler.JournalKindRun ||
		first.State != scheduler.JournalStateRunning ||
		first.RunID != runID || len(first.Plan) != 15 ||
		first.PlanDigest == "" {
		t.Fatalf("journal run start = %+v", first)
	}
	if last.Kind != scheduler.JournalKindRun ||
		last.State != scheduler.JournalStateSucceeded ||
		last.RunID != runID || last.PlanDigest != first.PlanDigest {
		t.Fatalf("journal run finish = %+v", last)
	}
	started := map[string]int{}
	finished := map[string]int{}
	for _, record := range records[1 : len(records)-1] {
		if record.Kind != scheduler.JournalKindStage ||
			record.RunID != runID ||
			record.PlanDigest != first.PlanDigest ||
			record.Attempt != 1 {
			t.Errorf("journal stage envelope = %+v", record)
			continue
		}
		switch record.State {
		case scheduler.JournalStateRunning:
			started[record.StageID]++
		case scheduler.JournalStateSucceeded:
			finished[record.StageID]++
		default:
			t.Errorf("unexpected journal stage state: %+v", record)
		}
	}
	if len(started) != 15 || len(finished) != 15 {
		t.Fatalf("stage lifecycle coverage: started=%v finished=%v", started, finished)
	}
	for stageID, count := range started {
		if count != 1 || finished[stageID] != 1 {
			t.Errorf("stage %s lifecycle counts start=%d finish=%d",
				stageID, count, finished[stageID])
		}
	}
}

func TestScanRequiresCompleteJournalBinding(t *testing.T) {
	root := t.TempDir()
	const runID = "0123456789abcdef0123456789abcdef"
	for _, test := range []struct {
		name    string
		options Options
	}{
		{
			name:    "run ID only",
			options: Options{Root: root, RunID: runID},
		},
		{
			name: "journal only",
			options: Options{
				Root:    root,
				Journal: &recordingJournal{runID: runID},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := Scan(context.Background(), test.options)
			if err == nil ||
				!strings.Contains(err.Error(), "run ID and journal must be supplied together") {
				t.Fatalf("Scan() binding error = %v", err)
			}
		})
	}

	_, _, err := Scan(context.Background(), Options{
		Root:    root,
		RunID:   runID,
		Journal: &recordingJournal{runID: "fedcba9876543210fedcba9876543210"},
	})
	if err == nil || !strings.Contains(err.Error(), "run ID does not match") {
		t.Fatalf("Scan() mismatched binding error = %v", err)
	}
}
