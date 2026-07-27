package scheduler

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testRunID      = "0123456789abcdef0123456789abcdef"
	testOtherRunID = "fedcba9876543210fedcba9876543210"
)

func TestJournalRunIDGenerationAndValidation(t *testing.T) {
	runID, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := hex.DecodeString(runID)
	if err != nil || len(decoded) != 16 {
		t.Fatalf("NewRunID() = %q, decoded %d bytes, err %v", runID, len(decoded), err)
	}
	if runID != strings.ToLower(runID) {
		t.Fatalf("NewRunID() = %q, want canonical lowercase", runID)
	}
	if err := ValidateRunID(runID); err != nil {
		t.Fatalf("ValidateRunID(NewRunID()) = %v", err)
	}

	for _, invalid := range []string{
		"",
		strings.Repeat("a", 31),
		strings.Repeat("a", 33),
		"0123456789ABCDEF0123456789ABCDEF",
		"g123456789abcdef0123456789abcdef",
		"0123456789abcde/0123456789abcdef",
		"0123456789abcdef0123456789abcdeé",
	} {
		if err := ValidateRunID(invalid); err == nil {
			t.Errorf("ValidateRunID(%q) succeeded", invalid)
		}
	}
}

func TestFileJournalDurableSuccessfulReplay(t *testing.T) {
	journal := openTestJournal(t, t.TempDir(), testRunID)
	plan := testJournalPlan("stage")
	appendTestRecord(t, journal, JournalRecord{
		RunID: testRunID,
		Kind:  JournalKindRun,
		State: JournalStateRunning,
		Plan:  plan,
	})
	appendTestRecord(t, journal, JournalRecord{
		RunID:        testRunID,
		Kind:         JournalKindStage,
		State:        JournalStateRunning,
		StageID:      "stage",
		StageVersion: "v1",
		Attempt:      1,
		Resources:    plan[0].Resources,
	})
	appendTestRecord(t, journal, successfulStageRecord("stage", "v1", 1))
	appendTestRecord(t, journal, JournalRecord{
		RunID:    testRunID,
		Kind:     JournalKindRun,
		State:    JournalStateSucceeded,
		Duration: time.Millisecond,
	})
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := ReadFileJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Path != path || report.RunID != testRunID ||
		JournalState(report.State) != JournalStateSucceeded || !report.Terminal ||
		report.Interrupted || report.DiscardedTailBytes != 0 {
		t.Fatalf("ReadFileJournal() = %+v", report)
	}
	if report.LastSequence != 4 || len(report.Records) != 4 {
		t.Fatalf("journal replay sequence=%d records=%d", report.LastSequence, len(report.Records))
	}
	assertJournalChain(t, report.Records)
	for index, record := range report.Records {
		if record.Sequence != uint64(index+1) || record.OccurredAt.IsZero() {
			t.Errorf("record %d envelope = %+v", index, record)
		}
		if index > 0 && record.OccurredAt.Before(report.Records[index-1].OccurredAt) {
			t.Errorf("record %d time %s precedes record %d time %s",
				index, record.OccurredAt, index-1, report.Records[index-1].OccurredAt)
		}
	}
}

func TestFileJournalAllowsEmptyDAGRun(t *testing.T) {
	journal := openTestJournal(t, t.TempDir(), testRunID)
	appendTestRecord(t, journal, JournalRecord{
		RunID: testRunID,
		Kind:  JournalKindRun,
		State: JournalStateRunning,
		Plan:  []JournalStage{},
	})
	appendTestRecord(t, journal, JournalRecord{
		RunID: testRunID,
		Kind:  JournalKindRun,
		State: JournalStateSucceeded,
	})
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := ReadFileJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Terminal || JournalState(report.State) != JournalStateSucceeded ||
		len(report.Records) != 2 {
		t.Fatalf("empty-DAG replay = %+v", report)
	}
	assertJournalChain(t, report.Records)
}

func TestExecutePersistsExactJournalLifecycle(t *testing.T) {
	const outputDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	t.Run("succeeded", func(t *testing.T) {
		report := executeWithTestJournal(t, context.Background(), []Stage{{
			ID: "stage", Version: "v1",
			Run: func(context.Context, Inputs) (Result, error) {
				return Result{ObjectDigest: outputDigest}, nil
			},
		}}, Options{})
		assertJournalStates(t, report.Records,
			JournalStateRunning,
			JournalStateRunning,
			JournalStateSucceeded,
			JournalStateSucceeded,
		)
		if !report.Terminal || report.State != JournalStateSucceeded {
			t.Fatalf("successful journal report = %+v", report)
		}
	})

	t.Run("cached", func(t *testing.T) {
		key := mustCacheKey(t, "stage", "v1", nil, nil)
		cache := &memoryCache{values: map[string]Result{
			key: {ObjectDigest: outputDigest},
		}}
		report := executeWithTestJournal(t, context.Background(), []Stage{{
			ID: "stage", Version: "v1",
			Run: func(context.Context, Inputs) (Result, error) {
				return Result{}, errors.New("cached stage runner was called")
			},
		}}, Options{Cache: cache})
		assertJournalStates(t, report.Records,
			JournalStateRunning,
			JournalStateRunning,
			JournalStateCached,
			JournalStateSucceeded,
		)
	})

	t.Run("failed", func(t *testing.T) {
		report := executeWithTestJournal(t, context.Background(), []Stage{{
			ID: "stage", Version: "v1",
			Run: func(context.Context, Inputs) (Result, error) {
				return Result{}, errors.New("stage failed")
			},
		}}, Options{})
		assertJournalStates(t, report.Records,
			JournalStateRunning,
			JournalStateRunning,
			JournalStateFailed,
			JournalStateFailed,
		)
		if report.State != JournalStateFailed {
			t.Fatalf("failed journal state = %q", report.State)
		}
	})

	t.Run("cancelled", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		report := executeWithTestJournal(t, ctx, []Stage{{
			ID: "stage", Version: "v1",
			Run: func(context.Context, Inputs) (Result, error) {
				cancel()
				return Result{}, context.Canceled
			},
		}}, Options{})
		assertJournalStates(t, report.Records,
			JournalStateRunning,
			JournalStateRunning,
			JournalStateCancelled,
			JournalStateCancelled,
		)
		if report.State != JournalStateCancelled {
			t.Fatalf("cancelled journal state = %q", report.State)
		}
	})
}

func TestFileJournalRejectsInvalidStateTransitions(t *testing.T) {
	tests := []struct {
		name    string
		plan    []JournalStage
		records []JournalRecord
	}{
		{
			name: "unknown stage",
			plan: testJournalPlan("stage"),
			records: []JournalRecord{{
				RunID: testRunID, Kind: JournalKindStage, State: JournalStateRunning,
				StageID: "other", StageVersion: "v1", Attempt: 1,
			}},
		},
		{
			name: "mismatched stage version",
			plan: testJournalPlan("stage"),
			records: []JournalRecord{{
				RunID: testRunID, Kind: JournalKindStage, State: JournalStateRunning,
				StageID: "stage", StageVersion: "v2", Attempt: 1,
			}},
		},
		{
			name: "duplicate stage start",
			plan: testJournalPlan("stage"),
			records: []JournalRecord{
				{RunID: testRunID, Kind: JournalKindStage, State: JournalStateRunning,
					StageID: "stage", StageVersion: "v1", Attempt: 1},
				{RunID: testRunID, Kind: JournalKindStage, State: JournalStateRunning,
					StageID: "stage", StageVersion: "v1", Attempt: 1},
			},
		},
		{
			name: "unstarted stage completion",
			plan: testJournalPlan("stage"),
			records: []JournalRecord{
				successfulStageRecord("stage", "v1", 1),
			},
		},
		{
			name: "duplicate stage completion",
			plan: testJournalPlan("stage"),
			records: []JournalRecord{
				{RunID: testRunID, Kind: JournalKindStage, State: JournalStateRunning,
					StageID: "stage", StageVersion: "v1", Attempt: 1},
				successfulStageRecord("stage", "v1", 1),
				successfulStageRecord("stage", "v1", 1),
			},
		},
		{
			name: "incomplete successful run",
			plan: testJournalPlan("stage"),
			records: []JournalRecord{{
				RunID: testRunID, Kind: JournalKindRun, State: JournalStateSucceeded,
				Duration: time.Millisecond,
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			journal := openTestJournal(t, t.TempDir(), testRunID)
			appendTestRecord(t, journal, JournalRecord{
				RunID: testRunID, Kind: JournalKindRun, State: JournalStateRunning,
				Plan: test.plan,
			})
			for index, record := range test.records {
				err := journal.Append(context.Background(), record)
				if index == len(test.records)-1 {
					if err == nil {
						t.Fatalf("invalid record %d was accepted: %+v", index, record)
					}
					break
				}
				if err != nil {
					t.Fatalf("valid setup record %d failed: %v", index, err)
				}
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}

	t.Run("cyclic plan", func(t *testing.T) {
		journal := openTestJournal(t, t.TempDir(), testRunID)
		cyclic := []JournalStage{
			{ID: "a", Version: "v1", Dependencies: []string{"b"}},
			{ID: "b", Version: "v1", Dependencies: []string{"a"}},
		}
		err := journal.Append(context.Background(), JournalRecord{
			RunID: testRunID, Kind: JournalKindRun, State: JournalStateRunning,
			Plan: cyclic,
		})
		if err == nil {
			t.Fatal("cyclic journal plan was accepted")
		}
		if closeErr := journal.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
	})
}

func TestFileJournalTruncatedTailRecoveryAndCorruption(t *testing.T) {
	t.Run("incomplete final tail returns validated prefix", func(t *testing.T) {
		journal := openTestJournal(t, t.TempDir(), testRunID)
		appendTestRecord(t, journal, JournalRecord{
			RunID: testRunID, Kind: JournalKindRun, State: JournalStateRunning,
			Plan: testJournalPlan("stage"),
		})
		path := journal.Path()
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		tail := []byte(`{"schema_version":"rkc.scheduler-journal.v1"`)
		appendRawJournalBytes(t, path, tail)

		report, err := ReadFileJournal(path)
		if err != nil {
			t.Fatal(err)
		}
		if !report.Interrupted || report.Terminal ||
			report.DiscardedTailBytes != int64(len(tail)) ||
			report.LastSequence != 1 || len(report.Records) != 1 {
			t.Fatalf("truncated journal replay = %+v", report)
		}
		assertJournalChain(t, report.Records)
	})

	t.Run("complete malformed tail fails closed", func(t *testing.T) {
		journal := openTestJournal(t, t.TempDir(), testRunID)
		appendTestRecord(t, journal, JournalRecord{
			RunID: testRunID, Kind: JournalKindRun, State: JournalStateRunning,
			Plan: testJournalPlan("stage"),
		})
		path := journal.Path()
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		appendRawJournalBytes(t, path, []byte("{}\n"))
		if _, err := ReadFileJournal(path); err == nil {
			t.Fatal("complete malformed journal record was accepted")
		}
	})

	t.Run("earlier checksum corruption fails closed", func(t *testing.T) {
		journal := openTestJournal(t, t.TempDir(), testRunID)
		appendTestRecord(t, journal, JournalRecord{
			RunID: testRunID, Kind: JournalKindRun, State: JournalStateRunning,
			Plan: testJournalPlan("stage"),
		})
		appendTestRecord(t, journal, JournalRecord{
			RunID: testRunID, Kind: JournalKindStage, State: JournalStateRunning,
			StageID: "stage", StageVersion: "v1", Attempt: 1,
		})
		path := journal.Path()
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		const marker = `"record_digest":"`
		offset := bytes.Index(data, []byte(marker))
		if offset < 0 {
			t.Fatal("journal record has no record_digest")
		}
		offset += len(marker)
		if data[offset] == '0' {
			data[offset] = '1'
		} else {
			data[offset] = '0'
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadFileJournal(path); err == nil {
			t.Fatal("checksum-corrupt journal was accepted")
		}
	})
}

func TestFileJournalRunIDBinding(t *testing.T) {
	root := t.TempDir()
	journal := openTestJournal(t, root, testRunID)
	before, err := os.Stat(journal.Path())
	if err != nil {
		t.Fatal(err)
	}
	err = journal.Append(context.Background(), JournalRecord{
		RunID: testOtherRunID, Kind: JournalKindRun, State: JournalStateRunning,
		Plan: testJournalPlan("stage"),
	})
	if err == nil {
		t.Fatal("record with a different run ID was accepted")
	}
	after, statErr := os.Stat(journal.Path())
	if statErr != nil {
		t.Fatal(statErr)
	}
	if after.Size() != before.Size() {
		t.Fatalf("run-ID mismatch changed journal size from %d to %d", before.Size(), after.Size())
	}
	if closeErr := journal.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}

	executionJournal := openTestJournal(t, root, testOtherRunID)
	var ran bool
	_, err = Execute(context.Background(), []Stage{{
		ID: "stage", Version: "v1",
		Run: func(context.Context, Inputs) (Result, error) {
			ran = true
			return Result{}, nil
		},
	}}, Options{RunID: testRunID, Journal: executionJournal})
	if err == nil || ran {
		t.Fatalf("Execute mismatched journal = ran %t, err %v", ran, err)
	}
	info, statErr := os.Stat(executionJournal.Path())
	if statErr != nil {
		t.Fatal(statErr)
	}
	if info.Size() != 0 {
		t.Fatalf("Execute run-ID mismatch wrote %d bytes", info.Size())
	}
	if closeErr := executionJournal.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestOpenFileJournalSecurityAndPermissions(t *testing.T) {
	t.Run("owner-only creation", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "journal")
		journal := openTestJournal(t, root, testRunID)
		rootInfo, err := os.Stat(root)
		if err != nil {
			t.Fatal(err)
		}
		fileInfo, err := os.Stat(journal.Path())
		if err != nil {
			t.Fatal(err)
		}
		if got := rootInfo.Mode().Perm(); got != 0o700 {
			t.Fatalf("journal root mode = %#o, want 0700", got)
		}
		if got := fileInfo.Mode().Perm(); got != 0o600 {
			t.Fatalf("journal file mode = %#o, want 0600", got)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("existing run is immutable", func(t *testing.T) {
		root := t.TempDir()
		journal := openTestJournal(t, root, testRunID)
		path := journal.Path()
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := OpenFileJournal(root, testRunID); err == nil {
			t.Fatal("OpenFileJournal replaced an existing run")
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatal("existing journal changed after conflicting open")
		}
	})

	t.Run("symlink root is rejected", func(t *testing.T) {
		parent := t.TempDir()
		target := filepath.Join(parent, "target")
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(parent, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := OpenFileJournal(link, testRunID); err == nil {
			t.Fatal("OpenFileJournal accepted a symlink root")
		}
	})

	t.Run("symlink run file is rejected", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.WriteFile(target, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, testRunID+".jsonl")
		if err := os.Symlink(target, path); err != nil {
			t.Skipf("symlink unavailable: %v", err)
		}
		if _, err := OpenFileJournal(root, testRunID); err == nil {
			t.Fatal("OpenFileJournal accepted a symlink run file")
		}
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "keep" {
			t.Fatalf("symlink target = %q, %v", data, err)
		}
	})

	t.Run("insecure existing root is rejected", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "insecure")
		if err := os.Mkdir(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(root, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenFileJournal(root, testRunID); err == nil {
			t.Fatal("OpenFileJournal accepted a group/world-accessible root")
		}
	})
}

func TestFileJournalCloseSemantics(t *testing.T) {
	var nilJournal *FileJournal
	if nilJournal.Path() != "" || nilJournal.RunID() != "" {
		t.Fatal("nil journal exposed a path or run ID")
	}
	if err := nilJournal.Close(); err != nil {
		t.Fatalf("nil Close() = %v", err)
	}

	journal := openTestJournal(t, t.TempDir(), testRunID)
	appendTestRecord(t, journal, JournalRecord{
		RunID: testRunID, Kind: JournalKindRun, State: JournalStateRunning,
		Plan: testJournalPlan("stage"),
	})
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatalf("second Close() = %v", err)
	}
	if err := journal.Append(context.Background(), JournalRecord{}); err == nil {
		t.Fatal("Append after Close succeeded")
	}
	report, err := ReadFileJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if report.Terminal || report.Interrupted || len(report.Records) != 1 {
		t.Fatalf("closed nonterminal replay = %+v", report)
	}
}

func TestFileJournalConcurrentAppendOrdering(t *testing.T) {
	const stageCount = 8
	plan := make([]JournalStage, 0, stageCount)
	for index := 0; index < stageCount; index++ {
		plan = append(plan, JournalStage{ID: stageName(index), Version: "v1"})
	}
	journal := openTestJournal(t, t.TempDir(), testRunID)
	appendTestRecord(t, journal, JournalRecord{
		RunID: testRunID, Kind: JournalKindRun, State: JournalStateRunning,
		Plan: plan,
	})

	start := make(chan struct{})
	errs := make(chan error, stageCount)
	var wait sync.WaitGroup
	for index := 0; index < stageCount; index++ {
		stageID := stageName(index)
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if err := journal.Append(context.Background(), JournalRecord{
				RunID: testRunID, Kind: JournalKindStage, State: JournalStateRunning,
				StageID: stageID, StageVersion: "v1", Attempt: 1,
			}); err != nil {
				errs <- err
				return
			}
			if err := journal.Append(
				context.Background(),
				successfulStageRecord(stageID, "v1", 1),
			); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent Append() = %v", err)
	}
	if t.Failed() {
		_ = journal.Close()
		return
	}
	appendTestRecord(t, journal, JournalRecord{
		RunID: testRunID, Kind: JournalKindRun, State: JournalStateSucceeded,
		Duration: time.Millisecond,
	})
	path := journal.Path()
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	report, err := ReadFileJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Records) != 2+2*stageCount || !report.Terminal {
		t.Fatalf("concurrent replay = %+v", report)
	}
	assertJournalChain(t, report.Records)
	started := map[string]int{}
	finished := map[string]int{}
	for index, record := range report.Records {
		if record.Kind != JournalKindStage {
			continue
		}
		switch record.State {
		case JournalStateRunning:
			started[record.StageID] = index
		case JournalStateSucceeded:
			finished[record.StageID] = index
		}
	}
	for _, stage := range plan {
		startIndex, startedOK := started[stage.ID]
		finishIndex, finishedOK := finished[stage.ID]
		if !startedOK || !finishedOK || startIndex >= finishIndex {
			t.Errorf("stage %s lifecycle indexes start=%d/%t finish=%d/%t",
				stage.ID, startIndex, startedOK, finishIndex, finishedOK)
		}
	}
}

func openTestJournal(t *testing.T, root, runID string) *FileJournal {
	t.Helper()
	if err := os.Chmod(root, 0o700); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	journal, err := OpenFileJournal(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Path() == "" || !filepath.IsAbs(journal.Path()) || journal.RunID() != runID {
		_ = journal.Close()
		t.Fatalf("OpenFileJournal() = path %q run ID %q", journal.Path(), journal.RunID())
	}
	return journal
}

func appendTestRecord(t *testing.T, journal *FileJournal, record JournalRecord) {
	t.Helper()
	if err := journal.Append(context.Background(), record); err != nil {
		_ = journal.Close()
		t.Fatal(err)
	}
}

func testJournalPlan(stageIDs ...string) []JournalStage {
	plan := make([]JournalStage, 0, len(stageIDs))
	for _, stageID := range stageIDs {
		plan = append(plan, JournalStage{
			ID: stageID, Version: "v1",
		})
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].ID < plan[j].ID })
	return plan
}

func successfulStageRecord(stageID, version string, attempt uint32) JournalRecord {
	return JournalRecord{
		RunID: testRunID, Kind: JournalKindStage, State: JournalStateSucceeded,
		StageID: stageID, StageVersion: version, Attempt: attempt,
		CacheKey:     "stage:" + strings.Repeat("a", 64),
		OutputDigest: strings.Repeat("b", 64),
		Duration:     time.Microsecond,
	}
}

func assertJournalChain(t *testing.T, records []JournalRecord) {
	t.Helper()
	if len(records) == 0 {
		t.Fatal("journal chain is empty")
	}
	planDigest := records[0].PlanDigest
	if !isLowerHex(planDigest, 64) {
		t.Fatalf("plan digest = %q", planDigest)
	}
	previous := strings.Repeat("0", 64)
	for index, record := range records {
		if record.PlanDigest != planDigest {
			t.Errorf("record %d plan digest = %q, want %q",
				index, record.PlanDigest, planDigest)
		}
		if record.PreviousRecordDigest != previous {
			t.Errorf("record %d previous digest = %q, want %q",
				index, record.PreviousRecordDigest, previous)
		}
		if !isLowerHex(record.RecordDigest, 64) {
			t.Errorf("record %d digest = %q", index, record.RecordDigest)
		}
		previous = record.RecordDigest
	}
}

func appendRawJournalBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func executeWithTestJournal(
	t *testing.T,
	ctx context.Context,
	stages []Stage,
	options Options,
) JournalReport {
	t.Helper()
	journal := openTestJournal(t, t.TempDir(), testRunID)
	options.RunID = testRunID
	options.Journal = journal
	_, executeErr := Execute(ctx, stages, options)
	path := journal.Path()
	if closeErr := journal.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	report, replayErr := ReadFileJournal(path)
	if replayErr != nil {
		t.Fatal(replayErr)
	}
	if report.State == JournalStateSucceeded && executeErr != nil {
		t.Fatalf("successful Execute() returned %v", executeErr)
	}
	if report.State != JournalStateSucceeded && executeErr == nil {
		t.Fatalf("%s Execute() returned no error", report.State)
	}
	return report
}

func assertJournalStates(t *testing.T, records []JournalRecord, want ...JournalState) {
	t.Helper()
	if len(records) != len(want) {
		t.Fatalf("journal states have %d records, want %d: %+v", len(records), len(want), records)
	}
	for index, state := range want {
		if records[index].State != state {
			t.Errorf("journal record %d state = %q, want %q", index, records[index].State, state)
		}
	}
}

func stageName(index int) string {
	const names = "abcdefgh"
	if index < 0 || index >= len(names) {
		panic(errors.New("test stage index outside fixture range"))
	}
	return "stage-" + string(names[index])
}
