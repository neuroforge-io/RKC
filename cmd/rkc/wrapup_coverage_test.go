package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWrapupSQLitePublicationLifecycleAndSelectors(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "rkc.sqlite")
	bundle := sqliteSnapshotFixtureBundle(
		"wrapup-snapshot-one",
		"wrapup-repository",
		"",
		time.Unix(1_700_000_000, 0).UTC(),
	)

	for _, path := range []string{"", " surrounded "} {
		if _, err := canonicalSQLitePath(path); err == nil {
			t.Fatalf("canonicalSQLitePath(%q) unexpectedly succeeded", path)
		}
	}
	relative, err := canonicalSQLitePath("wrapup.sqlite")
	if err != nil || !filepath.IsAbs(relative) {
		t.Fatalf("canonical relative path = %q, %v", relative, err)
	}

	blockedParent := filepath.Join(root, "regular-file")
	writeTestFile(t, blockedParent, "not a directory")
	if _, err := prepareSQLiteBundle(ctx, filepath.Join(blockedParent, "store.sqlite"), bundle, nil); err == nil {
		t.Fatal("prepareSQLiteBundle unexpectedly opened a database below a regular file")
	}

	publication, err := prepareSQLiteBundle(ctx, databasePath, bundle, map[string]string{"test": "wrapup"})
	if err != nil {
		t.Fatalf("prepare initial SQLite bundle: %v", err)
	}
	if publication.Noop || publication.Path != databasePath {
		t.Fatalf("initial publication = noop %t path %q", publication.Noop, publication.Path)
	}
	if err := publication.Commit(ctx); err != nil {
		t.Fatalf("commit initial SQLite bundle: %v", err)
	}
	if err := publication.Commit(ctx); err != nil {
		t.Fatalf("repeat commit must be idempotent: %v", err)
	}
	if err := publication.Close(nil); err != nil {
		t.Fatalf("close committed publication: %v", err)
	}
	if err := publication.Close(nil); err != nil {
		t.Fatalf("repeat close must be idempotent: %v", err)
	}
	if err := publication.Commit(ctx); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("commit after close error = %v", err)
	}
	var nilPublication *sqlitePublication
	if err := nilPublication.Commit(ctx); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("nil publication commit error = %v", err)
	}
	if err := nilPublication.Close(nil); err != nil {
		t.Fatalf("nil publication close: %v", err)
	}

	bySnapshot, err := loadSQLiteDataset(ctx, databasePath, bundle.Snapshot.ID, "")
	if err != nil {
		t.Fatalf("load SQLite snapshot: %v", err)
	}
	if bySnapshot.Manifest.ID != bundle.Snapshot.ID || bySnapshot.Root != databasePath {
		t.Fatalf("snapshot dataset identity = %q, %q", bySnapshot.Manifest.ID, bySnapshot.Root)
	}
	byRepository, err := loadSQLiteDataset(ctx, databasePath, "", bundle.Snapshot.RepositoryID)
	if err != nil {
		t.Fatalf("load SQLite repository current: %v", err)
	}
	if byRepository.Manifest.ID != bundle.Snapshot.ID {
		t.Fatalf("repository current = %q", byRepository.Manifest.ID)
	}

	selectorFailures := []struct {
		name       string
		path       string
		snapshotID string
		repository string
		contains   string
	}{
		{"no selector", databasePath, "", "", "exactly one"},
		{"both selectors", databasePath, bundle.Snapshot.ID, bundle.Snapshot.RepositoryID, "exactly one"},
		{"missing snapshot", databasePath, "missing-snapshot", "", "not found"},
		{"missing repository", databasePath, "", "missing-repository", "current snapshot"},
		{"missing database", filepath.Join(root, "missing.sqlite"), bundle.Snapshot.ID, "", "open"},
	}
	for _, test := range selectorFailures {
		t.Run(test.name, func(t *testing.T) {
			_, err := loadSQLiteDataset(ctx, test.path, test.snapshotID, test.repository)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.contains)) {
				t.Fatalf("loadSQLiteDataset error = %v, want substring %q", err, test.contains)
			}
		})
	}

	replay, err := prepareSQLiteBundle(ctx, databasePath, bundle, nil)
	if err != nil {
		t.Fatalf("prepare idempotent replay: %v", err)
	}
	if !replay.Noop {
		t.Fatal("identical snapshot replay was not classified as a no-op")
	}
	if err := replay.Commit(ctx); err != nil {
		t.Fatalf("commit no-op replay: %v", err)
	}
	if err := replay.Close(nil); err != nil {
		t.Fatalf("close no-op replay: %v", err)
	}

	changed := bundle
	changed.Nodes = append(changed.Nodes[:0:0], changed.Nodes...)
	changed.Nodes[0].Name = "DifferentCanonicalContent"
	if _, err := prepareSQLiteBundle(ctx, databasePath, changed, nil); err == nil || !strings.Contains(err.Error(), "different canonical content") {
		t.Fatalf("conflicting snapshot replay error = %v", err)
	}

	second := sqliteSnapshotFixtureBundle(
		"wrapup-snapshot-two",
		bundle.Snapshot.RepositoryID,
		"",
		time.Unix(1_700_000_100, 0).UTC(),
	)
	aborted, err := prepareSQLiteBundle(ctx, databasePath, second, nil)
	if err != nil {
		t.Fatalf("prepare successor: %v", err)
	}
	if aborted.Bundle.Snapshot.ParentSnapshotID != bundle.Snapshot.ID {
		t.Fatalf("successor parent = %q", aborted.Bundle.Snapshot.ParentSnapshotID)
	}
	if err := aborted.Close(errors.New("intentional test abort")); err != nil {
		t.Fatalf("abort successor publication: %v", err)
	}
	if _, err := loadSQLiteDataset(ctx, databasePath, second.Snapshot.ID, ""); err == nil {
		t.Fatal("aborted successor unexpectedly became loadable")
	}
	current, err := loadSQLiteDataset(ctx, databasePath, "", bundle.Snapshot.RepositoryID)
	if err != nil || current.Manifest.ID != bundle.Snapshot.ID {
		t.Fatalf("current after abort = %v, %v", current, err)
	}
}

func TestWrapupCLIParserAndSelectorFailures(t *testing.T) {
	tests := []struct {
		name     string
		call     func() error
		contains string
	}{
		{"snapshots requires command", func() error { return runSnapshots(nil) }, "requires"},
		{"snapshots rejects unknown command", func() error { return runSnapshots([]string{"unknown"}) }, "unknown snapshots command"},
		{"snapshots list rejects positional", func() error { return runSnapshots([]string{"list", "extra"}) }, "positional"},
		{"snapshots list repository needs database", func() error { return runSnapshots([]string{"list", "--repository", "repo"}) }, "requires --database"},
		{"snapshots list cursor needs database", func() error { return runSnapshots([]string{"list", "--cursor", "cursor"}) }, "require --database"},
		{"snapshots list database conflicts with state", func() error { return runSnapshots([]string{"list", "--database", "db", "--state-dir", "state"}) }, "mutually exclusive"},
		{"snapshots list rejects zero limit", func() error { return runSnapshots([]string{"list", "--database", "db", "--limit", "0"}) }, "--limit must be"},
		{"snapshots show rejects two IDs", func() error { return runSnapshots([]string{"show", "one", "two"}) }, "at most one"},
		{"snapshots show rejects current and ID", func() error { return runSnapshots([]string{"show", "--current", "one"}) }, "mutually exclusive"},
		{"snapshots show repository needs database", func() error { return runSnapshots([]string{"show", "--repository", "repo"}) }, "requires --database"},
		{"snapshots export rejects two IDs", func() error { return runSnapshots([]string{"export", "one", "two"}) }, "at most one"},
		{"snapshots export repository needs database", func() error { return runSnapshots([]string{"export", "--repository", "repo"}) }, "requires --database"},
		{"snapshots recover rejects positional", func() error { return runSnapshots([]string{"recover", "extra"}) }, "positional"},
		{"snapshots recover database conflicts with state", func() error { return runSnapshots([]string{"recover", "--database", "db", "--state-dir", "state"}) }, "mutually exclusive"},
		{"snapshots recover rejects older-than for SQLite", func() error { return runSnapshots([]string{"recover", "--database", "db", "--older-than", "1s"}) }, "not used"},
		{"serve rejects positional", func() error { return runServe([]string{"extra"}) }, "positional"},
		{"serve selector needs database", func() error { return runServe([]string{"--snapshot", "one"}) }, "require --database"},
		{"serve database conflicts with dir", func() error { return runServe([]string{"--database", "db", "--dir", "atlas", "--snapshot", "one"}) }, "mutually exclusive"},
		{"serve database requires selector", func() error { return runServe([]string{"--database", "db"}) }, "exactly one"},
		{"query requires text", func() error { return runQuery(nil) }, "query text"},
		{"query rejects mode whitespace", func() error { return runQuery([]string{"--mode", " lexical", "alpha"}) }, "surrounding whitespace"},
		{"query rejects unknown mode", func() error { return runQuery([]string{"--mode", "unknown", "alpha"}) }, "unsupported retrieval mode"},
		{"query rejects semantic flags in lexical mode", func() error { return runQuery([]string{"--vector-index", "index.json", "alpha"}) }, "require --mode"},
		{"query selector needs database", func() error { return runQuery([]string{"--repository", "repo", "alpha"}) }, "require --database"},
		{"query database conflicts with dir", func() error {
			return runQuery([]string{"--database", "db", "--dir", "atlas", "--snapshot", "one", "alpha"})
		}, "mutually exclusive"},
		{"synthesize rejects positional", func() error { return runSynthesizeContext(context.Background(), []string{"extra"}) }, "positional"},
		{"synthesize rejects high limit", func() error { return runSynthesizeContext(context.Background(), []string{"--limit", "10001"}) }, "limit must be"},
		{"synthesize rejects high context", func() error { return runSynthesizeContext(context.Background(), []string{"--context", "262145"}) }, "context must be"},
		{"synthesize rejects zero output", func() error { return runSynthesizeContext(context.Background(), []string{"--max-output", "0"}) }, "max-output"},
		{"synthesize rejects high RSS", func() error { return runSynthesizeContext(context.Background(), []string{"--max-rss-mib", "2561"}) }, "safety ceiling"},
		{"synthesize rejects negative threads", func() error { return runSynthesizeContext(context.Background(), []string{"--threads", "-1"}) }, "threads must be"},
		{"synthesize rejects high batch", func() error { return runSynthesizeContext(context.Background(), []string{"--batch-size", "4097"}) }, "batch-size must be"},
		{"synthesize selector needs database", func() error { return runSynthesizeContext(context.Background(), []string{"--snapshot", "one"}) }, "require --database"},
		{"synthesize database conflicts with dir", func() error {
			return runSynthesizeContext(context.Background(), []string{"--database", "db", "--dir", "atlas", "--snapshot", "one"})
		}, "mutually exclusive"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil || !strings.Contains(err.Error(), test.contains) {
				t.Fatalf("error = %v, want substring %q", err, test.contains)
			}
		})
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runSynthesizeContext(cancelled, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled synthesis error = %v", err)
	}
}

func TestWrapupSQLiteSnapshotErrorsAndTextOutput(t *testing.T) {
	ctx := context.Background()
	databasePath := createSQLiteSnapshotsFixture(t)

	output, err := captureStdout(t, func() error {
		return snapshotsListSQLite(ctx, databasePath, "", 1, "", false)
	})
	if err != nil || !strings.Contains(output, "repository=") || !strings.Contains(output, "Next cursor:") {
		t.Fatalf("SQLite text list output = %q, %v", output, err)
	}
	output, err = captureStdout(t, func() error {
		return snapshotsShowSQLite(ctx, databasePath, sqliteSnapshotRepositoryA, "", true, false)
	})
	if err != nil || !strings.Contains(output, "Snapshot: "+sqliteSnapshotA2) || !strings.Contains(output, "Artifacts:") {
		t.Fatalf("SQLite text show output = %q, %v", output, err)
	}
	output, err = captureStdout(t, func() error {
		return snapshotsRecoverSQLite(ctx, databasePath, false)
	})
	if err != nil || !strings.Contains(output, "Recovered 0 incomplete SQLite build") {
		t.Fatalf("SQLite text recover output = %q, %v", output, err)
	}

	failures := []struct {
		name     string
		call     func() error
		contains string
	}{
		{"current requires repository", func() error { return snapshotsShowSQLite(ctx, databasePath, "", "", true, false) }, "requires --repository"},
		{"show rejects mismatched repository", func() error {
			return snapshotsShowSQLite(ctx, databasePath, sqliteSnapshotRepositoryB, sqliteSnapshotA1, false, false)
		}, "belongs to repository"},
		{"show rejects missing snapshot", func() error { return snapshotsShowSQLite(ctx, databasePath, "", "missing", false, false) }, "not found"},
		{"export selection requires repository", func() error { return snapshotsExportSQLite(ctx, databasePath, "", "", "", false, false, 1024) }, "requires --repository"},
		{"export rejects mismatched repository", func() error {
			return snapshotsExportSQLite(ctx, databasePath, sqliteSnapshotRepositoryB, sqliteSnapshotA1, filepath.Join(t.TempDir(), "out"), false, false, 1024)
		}, "belongs to repository"},
		{"export rejects sources", func() error {
			return snapshotsExportSQLite(ctx, databasePath, "", sqliteSnapshotA1, filepath.Join(t.TempDir(), "out"), false, true, 1024)
		}, "omit --include-sources"},
		{"open rejects blank path", func() error { _, _, err := openSnapshotsSQLite(ctx, "", true); return err }, "path is required"},
		{"open requires existing database", func() error {
			_, _, err := openSnapshotsSQLite(ctx, filepath.Join(t.TempDir(), "missing.sqlite"), true)
			return err
		}, "exist"},
	}
	for _, test := range failures {
		t.Run(test.name, func(t *testing.T) {
			err := test.call()
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(test.contains)) {
				t.Fatalf("error = %v, want substring %q", err, test.contains)
			}
		})
	}
}

func TestWrapupServeReadyFilePublication(t *testing.T) {
	receipt := serveReadyReceipt{
		SchemaVersion: "1.0",
		Address:       "127.0.0.1:8787",
		URL:           "http://127.0.0.1:8787",
		SnapshotID:    "wrapup-snapshot",
	}
	if err := publishServeReadyFile("", receipt); err != nil {
		t.Fatalf("blank ready path: %v", err)
	}
	path := filepath.Join(t.TempDir(), "ready.json")
	if err := publishServeReadyFile(path, receipt); err != nil {
		t.Fatalf("publish readiness receipt: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var decoded serveReadyReceipt
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("decode readiness receipt: %v", err)
	}
	if decoded != receipt {
		t.Fatalf("readiness receipt = %#v", decoded)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("readiness mode = %v, %v", info, err)
	}
	if err := publishServeReadyFile(path, receipt); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate readiness publication error = %v", err)
	}
	missingParent := filepath.Join(t.TempDir(), "missing", "ready.json")
	if err := publishServeReadyFile(missingParent, receipt); err == nil || !strings.Contains(err.Error(), "staging file") {
		t.Fatalf("missing readiness parent error = %v", err)
	}
}
