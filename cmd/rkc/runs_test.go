package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/scheduler"
)

func TestScanCreatesInspectableRunJournal(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	writeTestFile(t, filepath.Join(repository, "main.go"),
		"package fixture\n\nfunc Run() bool { return true }\n")
	output := filepath.Join(root, "atlas")
	state := filepath.Join(root, "snapshots")
	runs := filepath.Join(root, "runs")
	stdout, err := captureStdout(t, func() error {
		return runScanContext(context.Background(), []string{
			"--out", output,
			"--state-dir", state,
			"--runs-dir", runs,
			"--no-cache",
			"--no-plugins",
			"--no-frameworks",
			"--no-secret-scan",
			"--no-static-site",
			"--no-jsonl-graph",
			"--no-search-index",
			"--no-integrations",
			"--json",
			repository,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var summary map[string]any
	if err := json.Unmarshal([]byte(stdout), &summary); err != nil {
		t.Fatalf("decode scan summary %q: %v", stdout, err)
	}
	runID, runIDOK := summary["run_id"].(string)
	journalPath, pathOK := summary["run_journal"].(string)
	if !runIDOK || !pathOK || scheduler.ValidateRunID(runID) != nil ||
		journalPath != filepath.Join(runs, runID+".jsonl") {
		t.Fatalf("scan journal summary = %#v", summary)
	}
	replay, err := scheduler.ReadFileJournal(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if replay.RunID != runID || !replay.Terminal || replay.Interrupted ||
		scheduler.JournalState(replay.State) != scheduler.JournalStateSucceeded {
		t.Fatalf("scan journal replay = %+v", replay)
	}

	listJSON, err := captureStdout(t, func() error {
		return runRuns([]string{"list", "--runs-dir", runs, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var listed runListReport
	if err := json.Unmarshal([]byte(listJSON), &listed); err != nil {
		t.Fatal(err)
	}
	if listed.Count != 1 || len(listed.Runs) != 1 ||
		listed.Runs[0].RunID != runID ||
		listed.Runs[0].State != scheduler.JournalStateSucceeded {
		t.Fatalf("runs list = %+v", listed)
	}
	showJSON, err := captureStdout(t, func() error {
		return runRuns([]string{"show", "--runs-dir", runs, "--json", runID})
	})
	if err != nil {
		t.Fatal(err)
	}
	var shown scheduler.JournalReport
	if err := json.Unmarshal([]byte(showJSON), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.RunID != runID || shown.LastSequence != replay.LastSequence ||
		len(shown.Records) != len(replay.Records) {
		t.Fatalf("runs show = %+v", shown)
	}
}

func TestScanJournalTracksCommandFailuresAfterDAGCompletion(t *testing.T) {
	t.Run("publication failure", func(t *testing.T) {
		root := t.TempDir()
		repository := filepath.Join(root, "repository")
		writeTestFile(t, filepath.Join(repository, "main.go"), "package fixture\n")
		output := filepath.Join(root, "occupied-output")
		if err := os.Mkdir(output, 0o755); err != nil {
			t.Fatal(err)
		}
		runs := filepath.Join(root, "runs")
		err := runScanContext(context.Background(), []string{
			"--out", output,
			"--runs-dir", runs,
			"--no-cache",
			"--no-plugins",
			"--no-frameworks",
			"--no-secret-scan",
			repository,
		})
		if err == nil {
			t.Fatal("scan unexpectedly published over an existing output")
		}
		assertSingleRunJournalState(t, runs, scheduler.JournalStateFailed, "output directory already exists")
	})

	t.Run("diagnostic policy failure", func(t *testing.T) {
		root := t.TempDir()
		repository := filepath.Join(root, "repository")
		writeTestFile(t, filepath.Join(repository, "openapi.json"), "{")
		runs := filepath.Join(root, "runs")
		err := runScanContext(context.Background(), []string{
			"--out", filepath.Join(root, "atlas"),
			"--runs-dir", runs,
			"--no-cache",
			"--no-plugins",
			"--no-secret-scan",
			"--fail-on-errors",
			repository,
		})
		if err == nil || !strings.Contains(err.Error(), "error diagnostic") {
			t.Fatalf("scan diagnostic policy failure = %v", err)
		}
		assertSingleRunJournalState(t, runs, scheduler.JournalStateFailed, "error diagnostic")
	})
}

func TestRunsInspectionTextOrderingAndStrictFailures(t *testing.T) {
	root := filepath.Join(t.TempDir(), "runs")
	first := "0123456789abcdef0123456789abcdef"
	second := "fedcba9876543210fedcba9876543210"
	createSuccessfulRunJournal(t, root, first)
	createSuccessfulRunJournal(t, root, second)

	text, err := captureStdout(t, func() error {
		return runRunsList([]string{"--runs-dir", root, "--limit", "1"})
	})
	if err != nil || !strings.Contains(text, "1 run(s) shown") {
		t.Fatalf("runs list text=%q error=%v", text, err)
	}
	text, err = captureStdout(t, func() error {
		return runRunsShow([]string{"--runs-dir", root, first})
	})
	if err != nil || !strings.Contains(text, "State: succeeded") ||
		!strings.Contains(text, "Records:") {
		t.Fatalf("runs show text=%q error=%v", text, err)
	}

	if err := runRuns(nil); err == nil {
		t.Fatal("runs without subcommand succeeded")
	}
	if err := runRuns([]string{"unknown"}); err == nil {
		t.Fatal("unknown runs subcommand succeeded")
	}
	if err := runRunsList([]string{"--runs-dir", root, "--limit", "-1"}); err == nil {
		t.Fatal("runs list accepted a negative limit")
	}
	if err := runRunsList([]string{"--runs-dir", root, "--limit", "0"}); err == nil {
		t.Fatal("runs list accepted an unbounded zero limit")
	}
	if err := runRunsShow([]string{"--runs-dir", root, "unsafe"}); err == nil {
		t.Fatal("runs show accepted an invalid run ID")
	}

	corruptID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := os.WriteFile(
		filepath.Join(root, corruptID+".jsonl"),
		[]byte("{}\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	output, err := captureStdout(t, func() error {
		return runRunsList([]string{"--runs-dir", root, "--limit", "50", "--json"})
	})
	if err != nil {
		t.Fatalf("runs list corrupt journal = %v", err)
	}
	var report runListReport
	if err := json.Unmarshal([]byte(output), &report); err != nil ||
		report.Healthy || len(report.Issues) != 1 ||
		report.Issues[0].Name != corruptID {
		t.Fatalf("runs list corrupt report=%+v output=%q err=%v", report, output, err)
	}
	if err := runRunsShow([]string{"--runs-dir", root, corruptID}); err == nil ||
		!strings.Contains(err.Error(), "strictly replay") {
		t.Fatalf("runs show corrupt journal = %v", err)
	}

	if err := os.WriteFile(filepath.Join(root, ".DS_Store"), []byte("noise"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := captureStdout(t, func() error {
		return runRunsShow([]string{"--runs-dir", root, first})
	}); err != nil {
		t.Fatalf("unrelated directory entry blocked runs show: %v", err)
	}
}

func TestRunsInspectionDirectorySafetyAndMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	output, err := captureStdout(t, func() error {
		return runRunsList([]string{"--runs-dir", missing, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var report runListReport
	if err := json.Unmarshal([]byte(output), &report); err != nil ||
		report.Count != 0 || len(report.Runs) != 0 {
		t.Fatalf("missing runs report=%+v output=%q err=%v", report, output, err)
	}

	insecure := filepath.Join(t.TempDir(), "insecure")
	if err := os.Mkdir(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(insecure, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runRunsList([]string{"--runs-dir", insecure}); err == nil ||
		!strings.Contains(err.Error(), "only by its owner") {
		t.Fatalf("runs list insecure root = %v", err)
	}

	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(target, link); err == nil {
		if err := runRunsList([]string{"--runs-dir", link}); err == nil ||
			!strings.Contains(err.Error(), "non-symlink") {
			t.Fatalf("runs list symlink root = %v", err)
		}
	}

	repository := filepath.Join(t.TempDir(), "repository")
	writeTestFile(t, filepath.Join(repository, "main.go"), "package fixture\n")
	if err := runScanContext(context.Background(), []string{
		"--out", filepath.Join(t.TempDir(), "atlas"),
		"--runs-dir", filepath.Join(repository, ".rkc-runs"),
		"--no-cache",
		repository,
	}); err == nil || !strings.Contains(err.Error(), "outside the scanned repository") {
		t.Fatalf("scan with repository-local runs directory = %v", err)
	}

	aliasRoot := t.TempDir()
	runs := filepath.Join(aliasRoot, "runs")
	if err := os.Mkdir(runs, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(aliasRoot, "runs-alias")
	if err := os.Symlink(runs, alias); err == nil {
		err := runScanContext(context.Background(), []string{
			"--out", filepath.Join(t.TempDir(), "atlas"),
			"--runs-dir", runs,
			"--database", filepath.Join(alias, "state.sqlite"),
			"--no-cache",
			repository,
		})
		if err == nil || !strings.Contains(err.Error(), "cannot be stored inside") {
			t.Fatalf("scan accepted aliased SQLite path inside runs: %v", err)
		}
	}
}

func TestExplicitRunsDirectoryDoesNotDependOnUserCacheDiscovery(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	root := filepath.Join(t.TempDir(), "runs")
	output, err := captureStdout(t, func() error {
		return runRunsList([]string{"--runs-dir", root, "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var report runListReport
	if err := json.Unmarshal([]byte(output), &report); err != nil ||
		report.RunsDirectory != root || report.Count != 0 {
		t.Fatalf("explicit runs directory report=%+v output=%q err=%v", report, output, err)
	}
}

func createSuccessfulRunJournal(t *testing.T, root, runID string) {
	t.Helper()
	journal, err := scheduler.OpenFileJournal(root, runID)
	if err != nil {
		t.Fatal(err)
	}
	_, executeErr := scheduler.Execute(context.Background(), []scheduler.Stage{{
		ID: "stage", Version: "v1",
		Run: func(context.Context, scheduler.Inputs) (scheduler.Result, error) {
			return scheduler.Result{ObjectDigest: strings.Repeat("b", 64)}, nil
		},
	}}, scheduler.Options{RunID: runID, Journal: journal})
	closeErr := journal.Close()
	if executeErr != nil || closeErr != nil {
		t.Fatalf("create run journal: execute=%v close=%v", executeErr, closeErr)
	}
}

func assertSingleRunJournalState(
	t *testing.T,
	root string,
	state scheduler.JournalState,
	errorSubstring string,
) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("run journal entries = %d, want 1", len(entries))
	}
	report, err := scheduler.ReadFileJournal(filepath.Join(root, entries[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Terminal || report.State != state {
		t.Fatalf("run journal report = %+v, want terminal %s", report, state)
	}
	last := report.Records[len(report.Records)-1]
	if !strings.Contains(last.Error, errorSubstring) {
		t.Fatalf("terminal journal error = %q, want %q", last.Error, errorSubstring)
	}
}
