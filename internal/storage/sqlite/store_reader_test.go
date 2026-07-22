package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

type readerTestRecord struct {
	family  string
	id      string
	ordinal int
	value   any
}

func readerTestBundle(
	id string,
	repository string,
	parent string,
	created time.Time,
) rkcmodel.Bundle {
	artifact := rkcmodel.Artifact{
		ID: "artifact", Path: "main.go", Kind: "file", Language: "go",
		Status: "parsed", Text: true, Attributes: map[string]string{"owner": "reader"},
	}
	evidence := rkcmodel.Evidence{
		ID: "evidence", Kind: "declared", Method: "reader-test", Confidence: 1,
		Attributes: map[string]any{"nested": map[string]any{"stable": true}},
	}
	nodes := []rkcmodel.Node{
		{
			ID: "node-a", Kind: "function", Name: "Alpha", Language: "go",
			Visibility: "public", ArtifactID: artifact.ID,
			EvidenceIDs: []string{evidence.ID}, Attributes: map[string]any{"rank": 1},
		},
		{
			ID: "node-b", Kind: "function", Name: "Beta", Language: "go",
			Visibility: "private", ArtifactID: artifact.ID,
			EvidenceIDs: []string{evidence.ID},
		},
	}
	edges := []rkcmodel.Edge{
		{
			ID: "edge-a", Kind: "calls", From: "node-a", To: "node-b",
			Resolution: "declared", Confidence: 1, EvidenceIDs: []string{evidence.ID},
		},
		{
			ID: "edge-b", Kind: "related_to", From: "node-b", To: "node-a",
			Resolution: "unresolved", EvidenceIDs: []string{evidence.ID},
		},
	}
	diagnostics := []rkcmodel.Diagnostic{
		{ID: "diagnostic-a", Severity: "warning", Code: "TEST-001", Message: "warning", Stage: "parse"},
		{ID: "diagnostic-b", Severity: "note", Code: "TEST-002", Message: "note", Stage: "inventory"},
	}
	claim := rkcmodel.Claim{
		ID: "claim", SubjectID: "node-a", Text: "Alpha calls Beta.",
		Certainty: "supported", Generator: "reader-test",
		EvidenceIDs: []string{evidence.ID}, Validation: "accepted",
	}
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{
			SchemaVersion: rkcmodel.SchemaVersion,
			ID:            id, RepositoryID: repository, ParentSnapshotID: parent,
			CreatedAt: created.UTC(), Status: "committed", RootName: repository,
			RootPath:      "/private/" + repository,
			ContentDigest: strings.Repeat("a", 64),
			Tool:          rkcmodel.ToolInfo{Name: "reader-test", Version: "1"},
			Metadata:      map[string]string{"snapshot": id},
		},
		Artifacts: []rkcmodel.Artifact{artifact},
		Nodes:     nodes, Edges: edges, Evidence: []rkcmodel.Evidence{evidence},
		Diagnostics: diagnostics,
		Conflicts: []rkcmodel.Conflict{{
			ID: "conflict", Kind: "test", SubjectID: "node-a",
			CandidateIDs: []string{"node-a", "node-b"}, PreferredID: "node-a",
			EvidenceIDs: []string{evidence.ID},
		}},
		Documents: []rkcmodel.Document{{
			ID: "document", Kind: "reference", Title: "Reader fixture",
			Generator: "reader-test", Status: "validated",
		}},
		Claims: []rkcmodel.Claim{claim},
		Paths: []rkcmodel.ExecutionPath{{
			ID: "path", Name: "reader fixture", EntryNodeID: "node-a",
			ExitNodeID: "node-b", NodeIDs: []string{"node-a", "node-b"},
			EdgeIDs: []string{"edge-a"}, EvidenceIDs: []string{evidence.ID},
		}},
	}
	rkcmodel.SortBundle(&bundle)
	return bundle
}

func readerTestOpen(t *testing.T) (*Database, string) {
	t.Helper()
	path := filepath.Join(privateTempDir(t), "reader.db")
	database, err := Open(context.Background(), testOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return database, path
}

func readerTestRecords(bundle rkcmodel.Bundle, coverage rkcmodel.Coverage) []readerTestRecord {
	records := make([]readerTestRecord, 0, 16)
	for ordinal, value := range bundle.Artifacts {
		records = append(records, readerTestRecord{"artifact", value.ID, ordinal, value})
	}
	for ordinal, value := range bundle.Nodes {
		records = append(records, readerTestRecord{"node", value.ID, ordinal, value})
	}
	for ordinal, value := range bundle.Edges {
		records = append(records, readerTestRecord{"edge", value.ID, ordinal, value})
	}
	for ordinal, value := range bundle.Evidence {
		records = append(records, readerTestRecord{"evidence", value.ID, ordinal, value})
	}
	for ordinal, value := range bundle.Diagnostics {
		records = append(records, readerTestRecord{"diagnostic", value.ID, ordinal, value})
	}
	for ordinal, value := range bundle.Conflicts {
		records = append(records, readerTestRecord{"conflict", value.ID, ordinal, value})
	}
	for ordinal, value := range bundle.Documents {
		records = append(records, readerTestRecord{"document", value.ID, ordinal, value})
	}
	for ordinal, value := range bundle.Claims {
		records = append(records, readerTestRecord{"claim", value.ID, ordinal, value})
	}
	for ordinal, value := range bundle.Paths {
		records = append(records, readerTestRecord{"execution_path", value.ID, ordinal, value})
	}
	records = append(records, readerTestRecord{"coverage", "coverage", 0, coverage})
	return records
}

func readerTestSeedSnapshot(t *testing.T, database *Database, bundle rkcmodel.Bundle) {
	t.Helper()
	coverage := rkcmodel.BuildCoverage(bundle)
	transaction, err := database.db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	now := bundle.Snapshot.CreatedAt.UTC().Format(time.RFC3339Nano)
	if _, err := transaction.Exec(
		`INSERT INTO repositories(repository_id, display_name, created_at, metadata_json)
		 VALUES (?, ?, ?, '{}')
		 ON CONFLICT(repository_id) DO NOTHING`,
		bundle.Snapshot.RepositoryID,
		bundle.Snapshot.RepositoryID,
		now,
	); err != nil {
		t.Fatal(err)
	}
	buildID := "reader-build-" + bundle.Snapshot.ID
	var base any
	if bundle.Snapshot.ParentSnapshotID != "" {
		base = bundle.Snapshot.ParentSnapshotID
	}
	if _, err := transaction.Exec(
		`INSERT INTO builds(
		   build_id, repository_id, base_current_snapshot_id, parent_snapshot_id,
		   expected_schema, state, created_at, updated_at
		 ) VALUES (?, ?, ?, ?, ?, 'open', ?, ?)`,
		buildID,
		bundle.Snapshot.RepositoryID,
		base,
		base,
		bundle.Snapshot.SchemaVersion,
		now,
		now,
	); err != nil {
		t.Fatal(err)
	}
	snapshotJSON, err := json.Marshal(bundle.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	canonicalBundle, err := rkcmodel.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(
		`INSERT INTO canonical_snapshots(
		   snapshot_id, repository_id, parent_snapshot_id, build_id,
		   schema_version, canonical_snapshot_json, canonical_bundle_json,
		   canonical_digest, published_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		bundle.Snapshot.ID,
		bundle.Snapshot.RepositoryID,
		base,
		buildID,
		bundle.Snapshot.SchemaVersion,
		string(snapshotJSON),
		string(canonicalBundle),
		rkcmodel.CanonicalDigest(bundle),
		now,
	); err != nil {
		t.Fatal(err)
	}
	for _, record := range readerTestRecords(bundle, coverage) {
		payload, err := json.Marshal(record.value)
		if err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(payload)
		if _, err := transaction.Exec(
			`INSERT INTO canonical_snapshot_records(
			   snapshot_id, record_family, record_id, ordinal,
			   canonical_record_json, canonical_record_sha256
			 ) VALUES (?, ?, ?, ?, ?, ?)`,
			bundle.Snapshot.ID,
			record.family,
			record.id,
			record.ordinal,
			string(payload),
			hex.EncodeToString(digest[:]),
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := transaction.Exec(
		`UPDATE builds
		 SET state = 'committed', committed_snapshot_id = ?,
		     updated_at = ?, finished_at = ?
		 WHERE build_id = ?`,
		bundle.Snapshot.ID,
		now,
		now,
		buildID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(
		`UPDATE repositories SET current_snapshot_id = ? WHERE repository_id = ?`,
		bundle.Snapshot.ID,
		bundle.Snapshot.RepositoryID,
	); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteReaderPointReadsBundleAndDefensiveValues(t *testing.T) {
	database, _ := readerTestOpen(t)
	bundle := readerTestBundle("snapshot", "repo", "", time.Unix(100, 123).UTC())
	readerTestSeedSnapshot(t, database, bundle)
	ctx := context.Background()

	snapshot, err := database.Snapshot(ctx, "snapshot")
	if err != nil || !reflect.DeepEqual(snapshot, bundle.Snapshot) {
		t.Fatalf("Snapshot = %+v, %v", snapshot, err)
	}
	current, err := database.Current(ctx, "repo")
	if err != nil || !reflect.DeepEqual(current, bundle.Snapshot) {
		t.Fatalf("Current = %+v, %v", current, err)
	}
	exported, err := database.Bundle(ctx, "snapshot")
	if err != nil || exported.Snapshot.RootPath != bundle.Snapshot.RootPath ||
		len(exported.Nodes) != 2 || len(exported.Edges) != 2 {
		t.Fatalf("Bundle = %+v, %v", exported, err)
	}
	artifact, err := database.Artifact(ctx, "snapshot", "artifact")
	if err != nil || artifact.Attributes["owner"] != "reader" {
		t.Fatalf("Artifact = %+v, %v", artifact, err)
	}
	node, err := database.Node(ctx, "snapshot", "node-a")
	if err != nil || node.Name != "Alpha" {
		t.Fatalf("Node = %+v, %v", node, err)
	}
	evidence, err := database.Evidence(ctx, "snapshot", "evidence")
	if err != nil || evidence.Method != "reader-test" {
		t.Fatalf("Evidence = %+v, %v", evidence, err)
	}
	coverage, err := database.Coverage(ctx, "snapshot")
	if err != nil || coverage.SnapshotID != "snapshot" || coverage.NodesTotal != 2 {
		t.Fatalf("Coverage = %+v, %v", coverage, err)
	}

	snapshot.Metadata["snapshot"] = "mutated"
	exported.Nodes[0].Attributes["rank"] = 99
	artifact.Attributes["owner"] = "mutated"
	evidence.Attributes["nested"].(map[string]any)["stable"] = false
	coverage.NodeKinds["function"] = 99
	again, _ := database.Snapshot(ctx, "snapshot")
	againBundle, _ := database.Bundle(ctx, "snapshot")
	againArtifact, _ := database.Artifact(ctx, "snapshot", "artifact")
	againEvidence, _ := database.Evidence(ctx, "snapshot", "evidence")
	againCoverage, _ := database.Coverage(ctx, "snapshot")
	if again.Metadata["snapshot"] != "snapshot" ||
		againBundle.Nodes[0].Attributes["rank"] != float64(1) ||
		againArtifact.Attributes["owner"] != "reader" ||
		!againEvidence.Attributes["nested"].(map[string]any)["stable"].(bool) ||
		againCoverage.NodeKinds["function"] != 2 {
		t.Fatal("SQLite reader values alias mutable state")
	}
}

func TestSQLiteReaderKeysetQueriesAndPersistentAuthenticatedCursors(t *testing.T) {
	database, path := readerTestOpen(t)
	first := readerTestBundle("s1", "repo", "", time.Unix(100, 0).UTC())
	second := readerTestBundle("s2", "repo", "s1", time.Unix(200, 0).UTC())
	readerTestSeedSnapshot(t, database, first)
	readerTestSeedSnapshot(t, database, second)
	ctx := context.Background()

	page1, err := database.ListSnapshots(ctx, rkcstore.SnapshotQuery{
		RepositoryID: "repo", PageRequest: rkcstore.PageRequest{Limit: 1},
	})
	if err != nil || len(page1.Items) != 1 || page1.Items[0].ID != "s2" || page1.Next == "" {
		t.Fatalf("snapshot page 1 = %+v, %v", page1, err)
	}
	var persistedKey string
	if err := database.db.QueryRow(
		"SELECT value FROM schema_meta WHERE key = ?",
		readerCursorKeyName,
	).Scan(&persistedKey); err != nil || len(persistedKey) != sha256.Size*2 {
		t.Fatalf("persisted cursor key = %q, %v", persistedKey, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, testOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	page2, err := reopened.ListSnapshots(ctx, rkcstore.SnapshotQuery{
		RepositoryID: "repo", PageRequest: rkcstore.PageRequest{Limit: 1, Cursor: page1.Next},
	})
	if err != nil || len(page2.Items) != 1 || page2.Items[0].ID != "s1" || page2.Next != "" {
		t.Fatalf("snapshot page 2 = %+v, %v", page2, err)
	}
	var reopenedKey string
	if err := reopened.db.QueryRow(
		"SELECT value FROM schema_meta WHERE key = ?",
		readerCursorKeyName,
	).Scan(&reopenedKey); err != nil || reopenedKey != persistedKey {
		t.Fatalf("reopened cursor key = %q, %v", reopenedKey, err)
	}
	if _, err := reopened.ListSnapshots(ctx, rkcstore.SnapshotQuery{
		RepositoryID: "other", PageRequest: rkcstore.PageRequest{Cursor: page1.Next},
	}); !errors.Is(err, rkcstore.ErrInvalidCursor) {
		t.Fatalf("cross-scope snapshot cursor = %v", err)
	}
	tampered := []byte(page1.Next)
	if tampered[len(tampered)-1] == 'A' {
		tampered[len(tampered)-1] = 'B'
	} else {
		tampered[len(tampered)-1] = 'A'
	}
	if _, err := reopened.ListSnapshots(ctx, rkcstore.SnapshotQuery{
		RepositoryID: "repo", PageRequest: rkcstore.PageRequest{Cursor: rkcstore.Cursor(tampered)},
	}); !errors.Is(err, rkcstore.ErrInvalidCursor) {
		t.Fatalf("tampered snapshot cursor = %v", err)
	}

	nodes1, err := reopened.QueryNodes(ctx, rkcstore.NodeQuery{
		SnapshotID: "s2", PageRequest: rkcstore.PageRequest{Limit: 1},
	})
	if err != nil || len(nodes1.Items) != 1 || nodes1.Items[0].ID != "node-a" || nodes1.Next == "" {
		t.Fatalf("node page 1 = %+v, %v", nodes1, err)
	}
	nodes2, err := reopened.QueryNodes(ctx, rkcstore.NodeQuery{
		SnapshotID: "s2", PageRequest: rkcstore.PageRequest{Limit: 1, Cursor: nodes1.Next},
	})
	if err != nil || len(nodes2.Items) != 1 || nodes2.Items[0].ID != "node-b" || nodes2.Next != "" {
		t.Fatalf("node page 2 = %+v, %v", nodes2, err)
	}
	filteredNodes, err := reopened.QueryNodes(ctx, rkcstore.NodeQuery{
		SnapshotID: "s2", Kind: "function", Language: "go",
		ArtifactID: "artifact", Visibility: "public",
	})
	if err != nil || len(filteredNodes.Items) != 1 || filteredNodes.Items[0].ID != "node-a" {
		t.Fatalf("filtered nodes = %+v, %v", filteredNodes, err)
	}
	if _, err := reopened.QueryNodes(ctx, rkcstore.NodeQuery{
		SnapshotID: "s2", Kind: "method",
		PageRequest: rkcstore.PageRequest{Cursor: nodes1.Next},
	}); !errors.Is(err, rkcstore.ErrInvalidCursor) {
		t.Fatalf("cross-scope node cursor = %v", err)
	}
	edges, err := reopened.QueryEdges(ctx, rkcstore.EdgeQuery{
		SnapshotID: "s2", Kind: "calls", From: "node-a", To: "node-b", Resolution: "declared",
	})
	if err != nil || len(edges.Items) != 1 || edges.Items[0].ID != "edge-a" {
		t.Fatalf("filtered edges = %+v, %v", edges, err)
	}
	if _, err := reopened.QueryEdges(ctx, rkcstore.EdgeQuery{
		SnapshotID: "s2", Resolution: "fictional",
	}); !errors.Is(err, rkcstore.ErrInvalidQuery) {
		t.Fatalf("invalid edge resolution = %v", err)
	}
	diagnostics, err := reopened.QueryDiagnostics(ctx, rkcstore.DiagnosticQuery{
		SnapshotID: "s2", Severity: "warning", Code: "TEST-001", Stage: "parse",
	})
	if err != nil || len(diagnostics.Items) != 1 || diagnostics.Items[0].ID != "diagnostic-a" {
		t.Fatalf("filtered diagnostics = %+v, %v", diagnostics, err)
	}
	for _, request := range []rkcstore.PageRequest{
		{Limit: -1},
		{Limit: rkcstore.MaxPageSize + 1},
		{Cursor: rkcstore.Cursor(strings.Repeat("x", readerMaxCursorBytes+1))},
	} {
		if _, err := reopened.ListSnapshots(ctx, rkcstore.SnapshotQuery{PageRequest: request}); err == nil {
			t.Fatalf("invalid page request %+v succeeded", request)
		}
	}
}

func TestSQLiteReaderTypedFailuresStrictJSONAndLifecycle(t *testing.T) {
	database, _ := readerTestOpen(t)
	bundle := readerTestBundle("snapshot", "repo", "", time.Unix(100, 0).UTC())
	readerTestSeedSnapshot(t, database, bundle)
	ctx := context.Background()

	invalidCalls := []func() error{
		func() error { _, err := database.Snapshot(ctx, ""); return err },
		func() error { _, err := database.Bundle(nil, "snapshot"); return err },
		func() error { _, err := database.Current(ctx, " "); return err },
		func() error { _, err := database.Artifact(ctx, "", "artifact"); return err },
		func() error { _, err := database.Node(ctx, "snapshot", ""); return err },
		func() error {
			_, err := database.QueryNodes(ctx, rkcstore.NodeQuery{SnapshotID: "snapshot", PageRequest: rkcstore.PageRequest{Limit: -1}})
			return err
		},
	}
	for _, call := range invalidCalls {
		if err := call(); !errors.Is(err, rkcstore.ErrInvalidArgument) &&
			!errors.Is(err, rkcstore.ErrInvalidQuery) {
			t.Fatalf("invalid reader call = %v", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := database.Snapshot(canceled, "snapshot"); !errors.Is(err, context.Canceled) || !errors.Is(err, rkcstore.ErrCanceled) {
		t.Fatalf("canceled Snapshot = %v", err)
	}

	missingCalls := []func() error{
		func() error { _, err := database.Snapshot(ctx, "missing"); return err },
		func() error { _, err := database.Bundle(ctx, "missing"); return err },
		func() error { _, err := database.Coverage(ctx, "missing"); return err },
		func() error {
			_, err := database.QueryNodes(ctx, rkcstore.NodeQuery{SnapshotID: "missing"})
			return err
		},
	}
	for _, call := range missingCalls {
		if err := call(); !errors.Is(err, rkcstore.ErrSnapshotNotFound) {
			t.Fatalf("missing snapshot call = %v", err)
		}
	}
	for _, call := range []func() error{
		func() error { _, err := database.Artifact(ctx, "snapshot", "missing"); return err },
		func() error { _, err := database.Node(ctx, "snapshot", "missing"); return err },
		func() error { _, err := database.Evidence(ctx, "snapshot", "missing"); return err },
	} {
		if err := call(); !errors.Is(err, rkcstore.ErrRecordNotFound) {
			t.Fatalf("missing record call = %v", err)
		}
	}

	validNode, err := json.Marshal(rkcmodel.Node{ID: "node"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readerDecodeObject[rkcmodel.Node](
		"decode", "snapshot", "node", string(validNode), readerMaxObjectJSONBytes,
	); err != nil {
		t.Fatalf("canonical node decode = %v", err)
	}
	for _, payload := range []string{
		"null",
		`{"id":"node","unknown":true}`,
		`{"id":1}`,
		`{"id":"node"} {}`,
		` {"id":"node"}`,
	} {
		if _, err := readerDecodeObject[rkcmodel.Node](
			"decode", "snapshot", "node", payload, readerMaxObjectJSONBytes,
		); !errors.Is(err, rkcstore.ErrValidation) {
			t.Fatalf("strict decode accepted %q: %v", payload, err)
		}
	}
	if _, err := readerDecodeObject[rkcmodel.Node](
		"decode",
		"snapshot",
		"node",
		"{}",
		1,
	); !errors.Is(err, rkcstore.ErrResourceExhausted) {
		t.Fatalf("oversized decode = %v", err)
	}

	if readerScopeFingerprint("a", "\x00b") == readerScopeFingerprint("a\x00", "b") {
		t.Fatal("reader cursor scope framing is ambiguous")
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Snapshot(ctx, "snapshot"); !errors.Is(err, ErrClosed) {
		t.Fatalf("Snapshot after Close = %v", err)
	} else {
		var operation *rkcstore.OperationError
		if !errors.As(err, &operation) || operation.Code != rkcstore.CodeConflict {
			t.Fatalf("closed Snapshot classification = %#v", err)
		}
	}
}

func TestSQLiteReaderSerializesCloseWithConcurrentReads(t *testing.T) {
	database, _ := readerTestOpen(t)
	readerTestSeedSnapshot(
		t,
		database,
		readerTestBundle("snapshot", "repo", "", time.Unix(100, 0).UTC()),
	)
	start := make(chan struct{})
	results := make(chan error, 12)
	var wait sync.WaitGroup
	for worker := 0; worker < cap(results); worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			_, err := database.Snapshot(context.Background(), "snapshot")
			results <- err
		}()
	}
	close(start)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, ErrClosed) {
			t.Fatalf("concurrent reader returned %v", err)
		}
	}
}

func TestReaderCursorRejectsMalformedAuthenticatedPayloads(t *testing.T) {
	key := []byte(strings.Repeat("k", sha256.Size))
	scope := readerScopeFingerprint("snapshot")
	valid, err := readerSealCursor("query", key, "nodes", scope, "", "node-a")
	if err != nil {
		t.Fatal(err)
	}
	opened, err := readerOpenCursor("query", key, valid, "nodes", scope)
	if err != nil || opened.ID != "node-a" {
		t.Fatalf("open valid cursor = %+v, %v", opened, err)
	}
	for _, cursor := range []rkcstore.Cursor{
		"malformed",
		"***.***",
		rkcstore.Cursor(strings.Repeat("x", readerMaxCursorBytes+1)),
	} {
		if _, err := readerOpenCursor("query", key, cursor, "nodes", scope); !errors.Is(err, rkcstore.ErrInvalidCursor) {
			t.Fatalf("malformed cursor %q = %v", cursor, err)
		}
	}
	if _, err := readerOpenCursor("query", key, valid, "edges", scope); !errors.Is(err, rkcstore.ErrInvalidCursor) {
		t.Fatalf("cross-kind cursor = %v", err)
	}
	if _, err := readerOpenCursor("query", key, valid, "nodes", "other"); !errors.Is(err, rkcstore.ErrInvalidCursor) {
		t.Fatalf("cross-scope cursor = %v", err)
	}
}

func TestSQLiteReaderRejectsDigestCorruption(t *testing.T) {
	t.Run("canonical bundle", func(t *testing.T) {
		database, _ := readerTestOpen(t)
		readerTestSeedSnapshot(
			t,
			database,
			readerTestBundle("snapshot", "repo", "", time.Unix(100, 0).UTC()),
		)
		if _, err := database.db.Exec("DROP TRIGGER canonical_snapshots_update_guard"); err != nil {
			t.Fatal(err)
		}
		if _, err := database.db.Exec(
			"UPDATE canonical_snapshots SET canonical_digest = ? WHERE snapshot_id = ?",
			strings.Repeat("0", sha256.Size*2),
			"snapshot",
		); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Bundle(context.Background(), "snapshot"); !errors.Is(err, rkcstore.ErrValidation) {
			t.Fatalf("Bundle accepted corrupt digest: %v", err)
		}
	})

	t.Run("canonical record", func(t *testing.T) {
		database, _ := readerTestOpen(t)
		readerTestSeedSnapshot(
			t,
			database,
			readerTestBundle("snapshot", "repo", "", time.Unix(100, 0).UTC()),
		)
		if _, err := database.db.Exec(
			"DROP TRIGGER canonical_snapshot_records_update_guard",
		); err != nil {
			t.Fatal(err)
		}
		if _, err := database.db.Exec(
			`UPDATE canonical_snapshot_records
			 SET canonical_record_sha256 = ?
			 WHERE snapshot_id = ? AND record_family = ? AND record_id = ?`,
			strings.Repeat("0", sha256.Size*2),
			"snapshot",
			"node",
			"node-a",
		); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Node(context.Background(), "snapshot", "node-a"); !errors.Is(err, rkcstore.ErrValidation) {
			t.Fatalf("Node accepted corrupt digest: %v", err)
		}
		if _, err := database.QueryNodes(
			context.Background(),
			rkcstore.NodeQuery{SnapshotID: "snapshot"},
		); !errors.Is(err, rkcstore.ErrValidation) {
			t.Fatalf("QueryNodes accepted corrupt digest: %v", err)
		}
	})
}

func TestSQLiteReaderClassifiesInfrastructureFailureAsInternal(t *testing.T) {
	err := readerStorageError(
		"read",
		"snapshot",
		"database",
		errors.New("disk input/output failure"),
	)
	if !errors.Is(err, rkcstore.ErrInternal) {
		t.Fatalf("infrastructure failure = %v, want ErrInternal", err)
	}
	var operation *rkcstore.OperationError
	if !errors.As(err, &operation) || operation.Code != rkcstore.CodeInternal {
		t.Fatalf("infrastructure failure classification = %#v", err)
	}
}
