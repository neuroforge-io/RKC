package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

func writerTestBundle(id string, repository rkcstore.RepositoryID, parent rkcstore.SnapshotID) rkcmodel.Bundle {
	artifact := rkcmodel.Artifact{
		ID: "artifact", LogicalID: "logical-artifact", Path: "main.go",
		Kind: "file", Language: "go", Status: "parsed", Text: true,
		Attributes: map[string]string{"owner": "writer-test"},
	}
	evidence := rkcmodel.Evidence{
		ID: "evidence", Kind: "declared", Method: "writer-test", Confidence: 1,
		Source: &rkcmodel.SourceRange{
			ArtifactID: artifact.ID, Path: artifact.Path, StartLine: 1, EndLine: 1,
		},
	}
	nodes := []rkcmodel.Node{
		{
			ID: "node-a", LogicalID: "logical-node-a", Kind: "function", Name: "Alpha",
			QualifiedName: "fixture.Alpha", Language: "go", Visibility: "public",
			ArtifactID: artifact.ID, EvidenceIDs: []string{evidence.ID},
		},
		{
			ID: "node-b", Kind: "function", Name: "Beta", Language: "go",
			Visibility: "private", ArtifactID: artifact.ID, EvidenceIDs: []string{evidence.ID},
		},
	}
	edge := rkcmodel.Edge{
		ID: "edge", Kind: "calls", From: nodes[0].ID, To: nodes[1].ID,
		Resolution: "declared", Confidence: 1, EvidenceIDs: []string{evidence.ID},
	}
	claim := rkcmodel.Claim{
		ID: "claim", SubjectID: nodes[0].ID, Text: "Alpha calls Beta.",
		Certainty: "supported", Generator: "writer-test", Validation: "accepted",
		EvidenceIDs: []string{evidence.ID},
	}
	return rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{
			SchemaVersion: rkcmodel.SchemaVersion, ID: id,
			RepositoryID: string(repository), ParentSnapshotID: string(parent),
			CreatedAt: time.Unix(100, 0).UTC(), Status: "committed",
			RootName: string(repository), RootPath: "/private/repository",
			ContentDigest: strings.Repeat("a", 64),
			Tool:          rkcmodel.ToolInfo{Name: "writer-test", Version: "1"},
			Metadata:      map[string]string{"snapshot": id},
		},
		Artifacts: []rkcmodel.Artifact{artifact},
		Nodes:     nodes,
		Edges:     []rkcmodel.Edge{edge},
		Evidence:  []rkcmodel.Evidence{evidence},
		Diagnostics: []rkcmodel.Diagnostic{{
			ID: "diagnostic", Severity: "warning", Code: "TEST-001",
			Message: "fixture", Stage: "test",
		}},
		Conflicts: []rkcmodel.Conflict{{
			ID: "conflict", Kind: "test", SubjectID: nodes[0].ID,
			CandidateIDs: []string{nodes[0].ID, nodes[1].ID}, PreferredID: nodes[0].ID,
			EvidenceIDs: []string{evidence.ID},
		}},
		Documents: []rkcmodel.Document{{
			ID: "document", Kind: "reference", Title: "Fixture", Generator: "writer-test",
			Status: "validated", SubjectIDs: []string{nodes[0].ID},
			Sections: []rkcmodel.DocumentSection{{
				ID: "section", Ordinal: 0, Heading: "Fixture", Markdown: "Alpha calls Beta.",
				ClaimIDs: []string{claim.ID}, EvidenceIDs: []string{evidence.ID},
			}},
		}},
		Claims: []rkcmodel.Claim{claim},
		Paths: []rkcmodel.ExecutionPath{{
			ID: "path", Name: "fixture", EntryNodeID: nodes[0].ID, ExitNodeID: nodes[1].ID,
			NodeIDs: []string{nodes[0].ID, nodes[1].ID}, EdgeIDs: []string{edge.ID},
			EvidenceIDs: []string{evidence.ID},
		}},
	}
}

func writerTestOpen(t *testing.T) *Database {
	t.Helper()
	database := openTestDatabase(t, filepath.Join(privateTempDir(t), "writer.db"))
	return database
}

func writerTestStage(t *testing.T, database *Database, bundle rkcmodel.Bundle, coverage bool) rkcstore.BuildID {
	t.Helper()
	ctx := context.Background()
	build, err := database.BeginBuild(ctx, rkcstore.BuildOptions{
		RepositoryID:     rkcstore.RepositoryID(bundle.Snapshot.RepositoryID),
		ParentSnapshotID: rkcstore.SnapshotID(bundle.Snapshot.ParentSnapshotID),
		ExpectedSchema:   bundle.Snapshot.SchemaVersion,
		Metadata:         map[string]string{"snapshot": bundle.Snapshot.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	writes := []error{
		database.PutArtifacts(ctx, build, bundle.Artifacts),
		database.PutEvidence(ctx, build, bundle.Evidence),
		database.PutNodes(ctx, build, bundle.Nodes),
		database.PutEdges(ctx, build, bundle.Edges),
		database.PutDiagnostics(ctx, build, bundle.Diagnostics),
		database.PutConflicts(ctx, build, bundle.Conflicts),
		database.PutDocuments(ctx, build, bundle.Documents),
		database.PutClaims(ctx, build, bundle.Claims),
		database.PutPaths(ctx, build, bundle.Paths),
	}
	for _, err := range writes {
		if err != nil {
			t.Fatal(err)
		}
	}
	if coverage {
		if err := database.PutCoverage(ctx, build, rkcmodel.BuildCoverage(bundle)); err != nil {
			t.Fatal(err)
		}
	}
	return build
}

func writerTestCommit(t *testing.T, database *Database, bundle rkcmodel.Bundle) rkcstore.BuildID {
	t.Helper()
	build := writerTestStage(t, database, bundle, true)
	if err := database.Commit(context.Background(), build, bundle.Snapshot); err != nil {
		t.Fatal(err)
	}
	return build
}

func TestSQLiteWriterPublishesCanonicalRelationalAndFTSAtomically(t *testing.T) {
	database := writerTestOpen(t)
	bundle := writerTestBundle("snapshot-1", "repository", "")
	build := writerTestStage(t, database, bundle, true)
	if !strings.HasPrefix(string(build), "build_") || len(build) != len("build_")+writerBuildIDBytes*2 {
		t.Fatalf("build id %q is not a 128-bit hexadecimal identifier", build)
	}
	validation, err := database.Validate(context.Background(), build)
	if err != nil || !validation.Valid() {
		t.Fatalf("Validate = %+v, %v", validation, err)
	}
	if err := database.Commit(context.Background(), build, bundle.Snapshot); err != nil {
		t.Fatal(err)
	}

	loaded, err := database.Bundle(context.Background(), "snapshot-1")
	if err != nil || !reflect.DeepEqual(loaded, bundle) {
		t.Fatalf("Bundle = %#v, %v", loaded, err)
	}
	current, err := database.Current(context.Background(), "repository")
	if err != nil || current.ID != "snapshot-1" || current.RootPath != bundle.Snapshot.RootPath {
		t.Fatalf("Current = %+v, %v", current, err)
	}
	checks := []struct {
		query string
		want  int
	}{
		{"SELECT COUNT(*) FROM canonical_snapshots", 1},
		{"SELECT COUNT(*) FROM canonical_snapshot_records", 11},
		{"SELECT COUNT(*) FROM staged_canonical_records", 0},
		{"SELECT COUNT(*) FROM snapshots WHERE status = 'complete'", 1},
		{"SELECT COUNT(*) FROM artifacts", 1},
		{"SELECT COUNT(*) FROM nodes", 2},
		{"SELECT COUNT(*) FROM edges", 1},
		{"SELECT COUNT(*) FROM evidence", 1},
		{"SELECT COUNT(*) FROM coverage_records", 1},
		{"SELECT COUNT(*) FROM search_fts", 6},
	}
	for _, check := range checks {
		var observed int
		if err := database.db.QueryRow(check.query).Scan(&observed); err != nil || observed != check.want {
			t.Errorf("%s = %d, %v; want %d", check.query, observed, err, check.want)
		}
	}
	if err := database.Commit(context.Background(), build, bundle.Snapshot); !errors.Is(err, rkcstore.ErrBuildCommitted) {
		t.Fatalf("second Commit = %v, want ErrBuildCommitted", err)
	}
}

func TestSQLiteWriterRegeneratesCanonicalRecordsFromNormalizedBundle(t *testing.T) {
	database := writerTestOpen(t)
	bundle := writerTestBundle("normalized", "repository", "")
	secondEvidence := bundle.Evidence[0]
	secondEvidence.ID = "z-evidence"
	bundle.Evidence = append(bundle.Evidence, secondEvidence)
	bundle.Nodes[0].EvidenceIDs = []string{secondEvidence.ID, bundle.Evidence[0].ID}
	bundle.Edges[0].Resolution = " Resolved "
	bundle.Edges[0].EvidenceIDs = []string{secondEvidence.ID, bundle.Evidence[0].ID}

	build := writerTestStage(t, database, bundle, true)
	var stagedJSON string
	if err := database.db.QueryRow(
		`SELECT canonical_record_json
		 FROM staged_canonical_records
		 WHERE build_id = ? AND record_family = 'edge' AND record_id = 'edge'`,
		build,
	).Scan(&stagedJSON); err != nil {
		t.Fatal(err)
	}
	if err := database.Commit(context.Background(), build, bundle.Snapshot); err != nil {
		t.Fatal(err)
	}

	var canonicalJSON, digest string
	if err := database.db.QueryRow(
		`SELECT canonical_record_json, canonical_record_sha256
		 FROM canonical_snapshot_records
		 WHERE snapshot_id = ? AND record_family = 'edge' AND record_id = 'edge'`,
		bundle.Snapshot.ID,
	).Scan(&canonicalJSON, &digest); err != nil {
		t.Fatal(err)
	}
	if canonicalJSON == stagedJSON {
		t.Fatal("canonical edge record was copied from unnormalized staging")
	}
	var edge rkcmodel.Edge
	if err := json.Unmarshal([]byte(canonicalJSON), &edge); err != nil {
		t.Fatal(err)
	}
	wantEvidence := []string{bundle.Evidence[0].ID, secondEvidence.ID}
	if edge.Resolution != rkcmodel.ResolutionCompilerResolved ||
		!reflect.DeepEqual(edge.EvidenceIDs, wantEvidence) {
		t.Fatalf("canonical edge = %+v, want normalized resolution and evidence %v", edge, wantEvidence)
	}
	if digest != writerSHA256([]byte(canonicalJSON)) {
		t.Fatalf("canonical record digest = %q, want digest of regenerated JSON", digest)
	}

	var nodeJSON string
	if err := database.db.QueryRow(
		`SELECT canonical_record_json
		 FROM canonical_snapshot_records
		 WHERE snapshot_id = ? AND record_family = 'node' AND record_id = 'node-a'`,
		bundle.Snapshot.ID,
	).Scan(&nodeJSON); err != nil {
		t.Fatal(err)
	}
	var node rkcmodel.Node
	if err := json.Unmarshal([]byte(nodeJSON), &node); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(node.EvidenceIDs, wantEvidence) {
		t.Fatalf("canonical node evidence = %v, want %v", node.EvidenceIDs, wantEvidence)
	}
}

func TestSQLiteWriterUsesDurableReaderBundleCeiling(t *testing.T) {
	if writerBundleExceedsDurableLimit(readerMaxBundleJSONBytes) {
		t.Fatal("writer rejects a bundle at the durable reader ceiling")
	}
	if !writerBundleExceedsDurableLimit(readerMaxBundleJSONBytes + 1) {
		t.Fatal("writer accepts a bundle above the durable reader ceiling")
	}
}

func TestSQLiteWriterListSnapshotsUsesCreatedAtOrdering(t *testing.T) {
	database := writerTestOpen(t)
	committedFirst := writerTestBundle("created-later", "repository", "")
	committedFirst.Snapshot.CreatedAt = time.Unix(200, 0).UTC()
	writerTestCommit(t, database, committedFirst)
	committedSecond := writerTestBundle("created-earlier", "repository", "created-later")
	committedSecond.Snapshot.CreatedAt = time.Unix(100, 0).UTC()
	writerTestCommit(t, database, committedSecond)

	page, err := database.ListSnapshots(context.Background(), rkcstore.SnapshotQuery{
		RepositoryID: "repository",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != "created-later" ||
		page.Items[1].ID != "created-earlier" {
		t.Fatalf("ListSnapshots = %+v, want descending CreatedAt order", page.Items)
	}
	for _, snapshot := range []rkcmodel.Snapshot{committedFirst.Snapshot, committedSecond.Snapshot} {
		var persisted string
		if err := database.db.QueryRow(
			"SELECT published_at FROM canonical_snapshots WHERE snapshot_id = ?",
			snapshot.ID,
		).Scan(&persisted); err != nil {
			t.Fatal(err)
		}
		if persisted != writerTimestamp(snapshot.CreatedAt) {
			t.Fatalf("snapshot %q ordering key = %q, want %q", snapshot.ID, persisted, writerTimestamp(snapshot.CreatedAt))
		}
	}
}

func TestSQLiteWriterDatabaseFailuresAreInternal(t *testing.T) {
	cause := errors.New("sqlite I/O failure")
	err := writerDatabaseError("write", "database", "build", "snapshot", cause)
	if !errors.Is(err, rkcstore.ErrInternal) || errors.Is(err, rkcstore.ErrConflict) {
		t.Fatalf("database error = %v, want ErrInternal and not ErrConflict", err)
	}
	var operation *rkcstore.OperationError
	if !errors.As(err, &operation) || operation.Code != rkcstore.CodeInternal ||
		operation.BuildID != "build" || operation.SnapshotID != "snapshot" {
		t.Fatalf("database operation error = %+v", operation)
	}
	if canceled := writerDatabaseError("write", "database", "", "", context.Canceled); !errors.Is(canceled, rkcstore.ErrCanceled) {
		t.Fatalf("canceled database error = %v, want ErrCanceled", canceled)
	}
}

func TestSQLiteWriterRejectsStaleConcurrentCommitWithoutPartialPublication(t *testing.T) {
	database := writerTestOpen(t)
	base := writerTestBundle("base", "repository", "")
	writerTestCommit(t, database, base)
	one := writerTestBundle("one", "repository", "base")
	two := writerTestBundle("two", "repository", "base")
	buildOne := writerTestStage(t, database, one, true)
	buildTwo := writerTestStage(t, database, two, true)
	if err := database.Commit(context.Background(), buildOne, one.Snapshot); err != nil {
		t.Fatal(err)
	}
	if err := database.Commit(context.Background(), buildTwo, two.Snapshot); !errors.Is(err, rkcstore.ErrConflict) {
		t.Fatalf("stale Commit = %v, want ErrConflict", err)
	}
	var snapshots int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM canonical_snapshots WHERE snapshot_id = 'two'").Scan(&snapshots); err != nil || snapshots != 0 {
		t.Fatalf("stale snapshot count = %d, %v", snapshots, err)
	}
	current, err := database.Current(context.Background(), "repository")
	if err != nil || current.ID != "one" {
		t.Fatalf("Current = %+v, %v", current, err)
	}
}

func TestSQLiteWriterLimitsCoverageAndBatchAtomicity(t *testing.T) {
	database := writerTestOpen(t)
	ctx := context.Background()
	build, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.PutNodes(ctx, build, []rkcmodel.Node{{ID: "same"}, {ID: "same"}}); !errors.Is(err, rkcstore.ErrConflict) {
		t.Fatalf("duplicate batch = %v", err)
	}
	var staged int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM staged_canonical_records WHERE build_id = ?", build).Scan(&staged); err != nil || staged != 0 {
		t.Fatalf("rejected batch staged %d rows, %v", staged, err)
	}
	badJSON := rkcmodel.Node{ID: "bad-json", Attributes: map[string]any{"channel": make(chan int)}}
	if err := database.PutNodes(ctx, build, []rkcmodel.Node{badJSON}); !errors.Is(err, rkcstore.ErrInvalidArgument) {
		t.Fatalf("non-JSON record = %v", err)
	}
	if err := database.PutNodes(ctx, build, []rkcmodel.Node{{ID: "line\nbreak"}}); !errors.Is(err, rkcstore.ErrInvalidArgument) {
		t.Fatalf("control-character id = %v", err)
	}
	coverage := rkcmodel.Coverage{SnapshotID: "future"}
	if err := database.PutCoverage(ctx, build, coverage); err != nil {
		t.Fatal(err)
	}
	if err := database.PutCoverage(ctx, build, coverage); !errors.Is(err, rkcstore.ErrConflict) {
		t.Fatalf("replacement coverage = %v, want ErrConflict", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := database.BeginBuild(canceled, rkcstore.BuildOptions{RepositoryID: "other"}); !errors.Is(err, rkcstore.ErrCanceled) {
		t.Fatalf("canceled BeginBuild = %v", err)
	}
	if _, err := database.BeginBuild(nil, rkcstore.BuildOptions{RepositoryID: "other"}); !errors.Is(err, rkcstore.ErrInvalidArgument) {
		t.Fatalf("nil-context BeginBuild = %v", err)
	}
}

func TestSQLiteWriterProjectionBlockerRollsBackCanonicalRows(t *testing.T) {
	database := writerTestOpen(t)
	bundle := writerTestBundle("stale", "repository", "")
	bundle.Documents[0].Status = "stale"
	build := writerTestStage(t, database, bundle, true)
	err := database.Commit(context.Background(), build, bundle.Snapshot)
	if !errors.Is(err, rkcstore.ErrValidation) {
		t.Fatalf("Commit = %v, want typed projection validation failure", err)
	}
	var canonical, staged int
	if err := database.db.QueryRow("SELECT COUNT(*) FROM canonical_snapshots").Scan(&canonical); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow("SELECT COUNT(*) FROM staged_canonical_records WHERE build_id = ?", build).Scan(&staged); err != nil {
		t.Fatal(err)
	}
	if canonical != 0 || staged == 0 {
		t.Fatalf("failed projection left canonical=%d staged=%d", canonical, staged)
	}
}

func TestSQLiteWriterAbortRecoveryAndClosedLifecycle(t *testing.T) {
	database := writerTestOpen(t)
	ctx := context.Background()
	abortID, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.PutNodes(ctx, abortID, []rkcmodel.Node{{ID: "node"}}); err != nil {
		t.Fatal(err)
	}
	if err := database.Abort(ctx, abortID, errors.New(strings.Repeat("é", 40_000))); err != nil {
		t.Fatal(err)
	}
	if err := database.Abort(ctx, abortID, nil); err != nil {
		t.Fatalf("idempotent Abort = %v", err)
	}
	if err := database.PutNodes(ctx, abortID, nil); !errors.Is(err, rkcstore.ErrBuildClosed) {
		t.Fatalf("write after Abort = %v", err)
	}
	recoverA, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"})
	if err != nil {
		t.Fatal(err)
	}
	recoverB, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"})
	if err != nil {
		t.Fatal(err)
	}
	path := database.Path()
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database = openTestDatabase(t, path)
	recovered, err := database.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []rkcstore.BuildID{recoverA, recoverB}
	if want[1] < want[0] {
		want[0], want[1] = want[1], want[0]
	}
	if !reflect.DeepEqual(recovered.AbortedBuilds, want) {
		t.Fatalf("Recover = %+v, want %v", recovered, want)
	}
	again, err := database.Recover(ctx)
	if err != nil || len(again.AbortedBuilds) != 0 {
		t.Fatalf("second Recover = %+v, %v", again, err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "closed"}); !errors.Is(err, rkcstore.ErrConflict) {
		t.Fatalf("BeginBuild after Close = %v, want typed conflict", err)
	}
}
