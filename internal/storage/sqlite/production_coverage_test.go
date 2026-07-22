package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

// These tests deliberately corrupt private temporary databases after dropping
// the relevant immutability guard. They model on-disk damage and old/hostile
// writers, and prove that the public API fails closed without partial
// publication. No production database or repository fixture is reused.

func productionRequireError(t *testing.T, err error, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("error = %v, want %v", err, target)
	}
	var operation *rkcstore.OperationError
	if !errors.As(err, &operation) {
		t.Fatalf("error = %#v, want *rkcstore.OperationError", err)
	}
}

func productionExec(t *testing.T, database *Database, statement string, arguments ...any) {
	t.Helper()
	if _, err := database.db.Exec(statement, arguments...); err != nil {
		t.Fatalf("exec %q: %v", statement, err)
	}
}

func productionCount(t *testing.T, database *Database, statement string, arguments ...any) int {
	t.Helper()
	var count int
	if err := database.db.QueryRow(statement, arguments...).Scan(&count); err != nil {
		t.Fatalf("count %q: %v", statement, err)
	}
	return count
}

func productionReaderFixture(t *testing.T) (*Database, rkcmodel.Bundle) {
	t.Helper()
	database, _ := readerTestOpen(t)
	bundle := readerTestBundle("snapshot", "repository", "", time.Unix(100, 0).UTC())
	readerTestSeedSnapshot(t, database, bundle)
	return database, bundle
}

func productionRewriteStagedID(t *testing.T, database *Database, build rkcstore.BuildID, family, id string) {
	t.Helper()
	var body string
	if err := database.db.QueryRow(
		`SELECT canonical_record_json FROM staged_canonical_records
		 WHERE build_id = ? AND record_family = ? AND record_id = ?`,
		build, family, id,
	).Scan(&body); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(body), &object); err != nil {
		t.Fatal(err)
	}
	object["id"] = "different-id"
	rewritten, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	productionExec(
		t,
		database,
		`UPDATE staged_canonical_records
		 SET canonical_record_json = ?, canonical_record_sha256 = ?
		 WHERE build_id = ? AND record_family = ? AND record_id = ?`,
		string(rewritten), writerSHA256(rewritten), build, family, id,
	)
}

func productionReplaceCanonicalRecord(
	t *testing.T,
	database *Database,
	family string,
	id string,
	body []byte,
) {
	t.Helper()
	productionExec(t, database, "DROP TRIGGER canonical_snapshot_records_update_guard")
	productionExec(
		t,
		database,
		`UPDATE canonical_snapshot_records
		 SET canonical_record_json = ?, canonical_record_sha256 = ?
		 WHERE snapshot_id = 'snapshot' AND record_family = ? AND record_id = ?`,
		string(body), writerSHA256(body), family, id,
	)
}

func productionInsertOpenBuild(t *testing.T, database *Database, id string) {
	t.Helper()
	now := writerTimestamp(time.Unix(100, 0))
	productionExec(
		t,
		database,
		`INSERT INTO repositories(repository_id, display_name, created_at, metadata_json)
		 VALUES ('repository', 'repository', ?, '{}')
		 ON CONFLICT(repository_id) DO NOTHING`,
		now,
	)
	productionExec(
		t,
		database,
		`INSERT INTO builds(
		   build_id, repository_id, expected_schema, state, created_at, updated_at
		 ) VALUES (?, 'repository', ?, 'open', ?, ?)`,
		id, rkcmodel.SchemaVersion, now, now,
	)
}

func TestProductionBeginBuildAndCommitContracts(t *testing.T) {
	ctx := context.Background()

	t.Run("begin validation", func(t *testing.T) {
		database := writerTestOpen(t)
		metadata := make(map[string]string, rkcstore.DefaultMemoryOptions().MaxMetadataKeys+1)
		for index := 0; index <= rkcstore.DefaultMemoryOptions().MaxMetadataKeys; index++ {
			metadata[fmt.Sprintf("key-%03d", index)] = "value"
		}
		cases := []struct {
			name    string
			options rkcstore.BuildOptions
			want    error
		}{
			{"missing repository", rkcstore.BuildOptions{}, rkcstore.ErrInvalidArgument},
			{"invalid parent", rkcstore.BuildOptions{RepositoryID: "repository", ParentSnapshotID: "bad\nparent"}, rkcstore.ErrInvalidArgument},
			{"unsupported schema", rkcstore.BuildOptions{RepositoryID: "repository", ExpectedSchema: "999"}, rkcstore.ErrInvalidArgument},
			{"metadata NUL", rkcstore.BuildOptions{RepositoryID: "repository", Metadata: map[string]string{"key": "bad\x00value"}}, rkcstore.ErrInvalidArgument},
			{"metadata count", rkcstore.BuildOptions{RepositoryID: "repository", Metadata: metadata}, rkcstore.ErrResourceExhausted},
		}
		for _, test := range cases {
			t.Run(test.name, func(t *testing.T) {
				_, err := database.BeginBuild(ctx, test.options)
				productionRequireError(t, err, test.want)
			})
		}
		var nilDatabase *Database
		_, err := nilDatabase.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"})
		productionRequireError(t, err, rkcstore.ErrConflict)
	})

	t.Run("parent compare and swap", func(t *testing.T) {
		database := writerTestOpen(t)
		base := writerTestBundle("base", "repository", "")
		writerTestCommit(t, database, base)
		_, err := database.BeginBuild(ctx, rkcstore.BuildOptions{
			RepositoryID: "repository", ParentSnapshotID: "wrong",
		})
		productionRequireError(t, err, rkcstore.ErrConflict)
	})

	t.Run("open build ceiling", func(t *testing.T) {
		database := writerTestOpen(t)
		limit := rkcstore.DefaultMemoryOptions().MaxOpenBuilds
		for index := 0; index < limit; index++ {
			if _, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"}); err != nil {
				t.Fatal(err)
			}
		}
		_, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"})
		productionRequireError(t, err, rkcstore.ErrResourceExhausted)
		if got := productionCount(t, database, "SELECT COUNT(*) FROM builds WHERE state = 'open'"); got != limit {
			t.Fatalf("open builds = %d, want %d", got, limit)
		}
	})

	t.Run("commit state validation", func(t *testing.T) {
		mutations := []struct {
			name   string
			mutate func(*rkcmodel.Snapshot)
			want   error
		}{
			{"repository", func(value *rkcmodel.Snapshot) { value.RepositoryID = "other" }, rkcstore.ErrInvalidArgument},
			{"parent", func(value *rkcmodel.Snapshot) { value.ParentSnapshotID = "other" }, rkcstore.ErrInvalidArgument},
			{"schema", func(value *rkcmodel.Snapshot) { value.SchemaVersion = "other" }, rkcstore.ErrInvalidArgument},
			{"status", func(value *rkcmodel.Snapshot) { value.Status = "draft" }, rkcstore.ErrInvalidArgument},
		}
		for _, test := range mutations {
			t.Run(test.name, func(t *testing.T) {
				database := writerTestOpen(t)
				bundle := writerTestBundle("snapshot", "repository", "")
				build := writerTestStage(t, database, bundle, true)
				test.mutate(&bundle.Snapshot)
				productionRequireError(t, database.Commit(ctx, build, bundle.Snapshot), test.want)
				if got := productionCount(t, database, "SELECT COUNT(*) FROM canonical_snapshots"); got != 0 {
					t.Fatalf("invalid commit published %d snapshots", got)
				}
			})
		}
	})

	t.Run("commit lifecycle and coverage", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		productionRequireError(t, database.Commit(canceled, "missing", bundle.Snapshot), rkcstore.ErrCanceled)
		bundle.Snapshot.ID = ""
		productionRequireError(t, database.Commit(ctx, "missing", bundle.Snapshot), rkcstore.ErrInvalidArgument)

		bundle = writerTestBundle("snapshot", "repository", "")
		productionRequireError(t, database.Commit(ctx, "missing", bundle.Snapshot), rkcstore.ErrBuildNotFound)
		build := writerTestStage(t, database, bundle, false)
		productionRequireError(t, database.Commit(ctx, build, bundle.Snapshot), rkcstore.ErrCoverageMismatch)
		if got := productionCount(t, database, "SELECT COUNT(*) FROM canonical_snapshots"); got != 0 {
			t.Fatalf("coverage failure published %d snapshots", got)
		}
	})

	t.Run("oversized snapshot envelope", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		bundle.Snapshot.Metadata = map[string]string{
			"padding": strings.Repeat("x", int(rkcstore.DefaultMemoryOptions().MaxRecordBytes)+1),
		}
		productionRequireError(t, database.Commit(ctx, "missing", bundle.Snapshot), rkcstore.ErrResourceExhausted)
	})
}

func TestProductionStagedCorruptionFailsClosed(t *testing.T) {
	identities := []struct{ family, id string }{
		{"artifact", "artifact"},
		{"node", "node-a"},
		{"edge", "edge"},
		{"evidence", "evidence"},
		{"diagnostic", "diagnostic"},
		{"conflict", "conflict"},
		{"document", "document"},
		{"claim", "claim"},
		{"execution_path", "path"},
	}
	for _, test := range identities {
		t.Run(test.family+" identity", func(t *testing.T) {
			database := writerTestOpen(t)
			bundle := writerTestBundle("snapshot", "repository", "")
			build := writerTestStage(t, database, bundle, true)
			productionExec(t, database, "DROP TRIGGER staged_canonical_records_update_guard")
			productionRewriteStagedID(t, database, build, test.family, test.id)
			productionRequireError(t, database.Commit(context.Background(), build, bundle.Snapshot), rkcstore.ErrConflict)
			if got := productionCount(t, database, "SELECT COUNT(*) FROM canonical_snapshots"); got != 0 {
				t.Fatalf("corrupt %s published %d snapshots", test.family, got)
			}
		})
	}

	t.Run("noncontiguous ordinal", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		build := writerTestStage(t, database, bundle, true)
		productionExec(t, database, "DROP TRIGGER staged_canonical_records_update_guard")
		productionExec(t, database,
			`UPDATE staged_canonical_records SET ordinal = 9
			 WHERE build_id = ? AND record_family = 'node' AND record_id = 'node-b'`, build)
		productionRequireError(t, database.Commit(context.Background(), build, bundle.Snapshot), rkcstore.ErrConflict)
	})

	t.Run("digest mismatch", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		build := writerTestStage(t, database, bundle, true)
		productionExec(t, database, "DROP TRIGGER staged_canonical_records_update_guard")
		productionExec(t, database,
			`UPDATE staged_canonical_records SET canonical_record_sha256 = ?
			 WHERE build_id = ? AND record_family = 'artifact'`, strings.Repeat("0", 64), build)
		productionRequireError(t, database.Commit(context.Background(), build, bundle.Snapshot), rkcstore.ErrConflict)
	})

	t.Run("strict JSON", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		build := writerTestStage(t, database, bundle, true)
		body := []byte(`{"id":"artifact","unknown":true}`)
		productionExec(t, database, "DROP TRIGGER staged_canonical_records_update_guard")
		productionExec(t, database,
			`UPDATE staged_canonical_records
			 SET canonical_record_json = ?, canonical_record_sha256 = ?
			 WHERE build_id = ? AND record_family = 'artifact'`,
			string(body), writerSHA256(body), build)
		productionRequireError(t, database.Commit(context.Background(), build, bundle.Snapshot), rkcstore.ErrConflict)
	})

	t.Run("misidentified coverage", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		build := writerTestStage(t, database, bundle, true)
		productionExec(t, database, "DROP TRIGGER staged_canonical_records_update_guard")
		productionExec(t, database,
			`UPDATE staged_canonical_records SET record_id = 'other'
			 WHERE build_id = ? AND record_family = 'coverage'`, build)
		productionRequireError(t, database.Commit(context.Background(), build, bundle.Snapshot), rkcstore.ErrConflict)
	})
}

func TestProductionAbortAndRecoveryContracts(t *testing.T) {
	ctx := context.Background()

	t.Run("abort typed lifecycle failures", func(t *testing.T) {
		database := writerTestOpen(t)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		productionRequireError(t, database.Abort(canceled, "build", nil), rkcstore.ErrCanceled)
		productionRequireError(t, database.Abort(nil, "build", nil), rkcstore.ErrInvalidArgument)
		productionRequireError(t, database.Abort(ctx, "", nil), rkcstore.ErrInvalidArgument)
		productionRequireError(t, database.Abort(ctx, "missing", nil), rkcstore.ErrBuildNotFound)

		bundle := writerTestBundle("snapshot", "repository", "")
		committed := writerTestCommit(t, database, bundle)
		productionRequireError(t, database.Abort(ctx, committed, nil), rkcstore.ErrBuildCommitted)

		var nilDatabase *Database
		productionRequireError(t, nilDatabase.Abort(ctx, "build", nil), rkcstore.ErrConflict)
	})

	t.Run("abort rolls back when staging deletion fails", func(t *testing.T) {
		database := writerTestOpen(t)
		build, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"})
		if err != nil {
			t.Fatal(err)
		}
		if err := database.PutNodes(ctx, build, []rkcmodel.Node{{ID: "node"}}); err != nil {
			t.Fatal(err)
		}
		productionExec(t, database,
			`CREATE TRIGGER production_abort_delete_blocker
			 BEFORE DELETE ON staged_canonical_records
			 BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
		productionRequireError(t, database.Abort(ctx, build, errors.New("stop")), rkcstore.ErrInternal)
		if got := productionCount(t, database,
			"SELECT COUNT(*) FROM builds WHERE build_id = ? AND state = 'open'", build); got != 1 {
			t.Fatalf("abort failure left %d open build rows, want 1", got)
		}
		if got := productionCount(t, database,
			"SELECT COUNT(*) FROM staged_canonical_records WHERE build_id = ?", build); got != 1 {
			t.Fatalf("abort failure retained %d staged rows, want 1", got)
		}
	})

	t.Run("abort compare and swap failure", func(t *testing.T) {
		database := writerTestOpen(t)
		build, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"})
		if err != nil {
			t.Fatal(err)
		}
		productionExec(t, database,
			`CREATE TRIGGER production_abort_cas_blocker
			 BEFORE UPDATE OF state ON builds WHEN NEW.state = 'aborted'
			 BEGIN SELECT RAISE(IGNORE); END`)
		productionRequireError(t, database.Abort(ctx, build, nil), rkcstore.ErrConflict)
	})

	t.Run("recover validation and catalogue ceiling", func(t *testing.T) {
		database := writerTestOpen(t)
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		_, err := database.Recover(canceled)
		productionRequireError(t, err, rkcstore.ErrCanceled)
		var nilDatabase *Database
		_, err = nilDatabase.Recover(ctx)
		productionRequireError(t, err, rkcstore.ErrConflict)

		for index := 0; index <= rkcstore.DefaultMemoryOptions().MaxOpenBuilds; index++ {
			productionInsertOpenBuild(t, database, fmt.Sprintf("orphan-%d", index))
		}
		result, err := database.Recover(ctx)
		productionRequireError(t, err, rkcstore.ErrResourceExhausted)
		if len(result.AbortedBuilds) != 0 {
			t.Fatalf("failed recovery leaked an uncommitted result: %v", result.AbortedBuilds)
		}
		if got := productionCount(t, database, "SELECT COUNT(*) FROM builds WHERE state = 'open'"); got != rkcstore.DefaultMemoryOptions().MaxOpenBuilds+1 {
			t.Fatalf("failed recovery mutated open build count to %d", got)
		}
	})

	t.Run("recover rolls back failed deletion", func(t *testing.T) {
		database := writerTestOpen(t)
		productionInsertOpenBuild(t, database, "orphan")
		body := []byte(`{"id":"node"}`)
		productionExec(t, database,
			`INSERT INTO staged_canonical_records(
			 build_id, record_family, record_id, ordinal,
			 canonical_record_json, canonical_record_sha256
			 ) VALUES ('orphan', 'node', 'node', 0, ?, ?)`, string(body), writerSHA256(body))
		productionExec(t, database,
			`CREATE TRIGGER production_recover_delete_blocker
			 BEFORE DELETE ON staged_canonical_records
			 BEGIN SELECT RAISE(ABORT, 'blocked'); END`)
		_, err := database.Recover(ctx)
		productionRequireError(t, err, rkcstore.ErrInternal)
		if got := productionCount(t, database,
			"SELECT COUNT(*) FROM builds WHERE build_id = 'orphan' AND state = 'open'"); got != 1 {
			t.Fatalf("failed recovery left %d open orphan rows", got)
		}
	})
}

func TestProductionCommitProjectionFailuresRollbackAtomically(t *testing.T) {
	projectionTables := []string{
		"snapshots",
		"logical_entities",
		"artifacts",
		"evidence",
		"nodes",
		"node_evidence",
		"edges",
		"edge_evidence",
		"documents",
		"document_sections",
		"chunks",
		"section_evidence",
		"diagnostics",
		"conflicts",
		"claims",
		"execution_paths",
		"coverage_records",
	}
	for _, table := range projectionTables {
		t.Run(table, func(t *testing.T) {
			database := writerTestOpen(t)
			bundle := writerTestBundle("snapshot", "repository", "")
			build := writerTestStage(t, database, bundle, true)
			productionExec(t, database, fmt.Sprintf(
				`CREATE TRIGGER production_projection_blocker
				 BEFORE INSERT ON %s
				 BEGIN SELECT RAISE(ABORT, 'blocked'); END`, table,
			))
			productionRequireError(
				t,
				database.Commit(context.Background(), build, bundle.Snapshot),
				rkcstore.ErrValidation,
			)
			if got := productionCount(t, database, "SELECT COUNT(*) FROM canonical_snapshots"); got != 0 {
				t.Fatalf("blocked %s projection published %d snapshots", table, got)
			}
			if got := productionCount(t, database,
				"SELECT COUNT(*) FROM staged_canonical_records WHERE build_id = ?", build); got == 0 {
				t.Fatalf("blocked %s projection destroyed staging", table)
			}
		})
	}

	tailFailures := []struct {
		name      string
		statement string
		want      error
	}{
		{
			"canonical snapshot insert",
			`CREATE TRIGGER production_tail_blocker BEFORE INSERT ON canonical_snapshots
			 BEGIN SELECT RAISE(ABORT, 'blocked'); END`,
			rkcstore.ErrInternal,
		},
		{
			"canonical record insert",
			`CREATE TRIGGER production_tail_blocker BEFORE INSERT ON canonical_snapshot_records
			 BEGIN SELECT RAISE(ABORT, 'blocked'); END`,
			rkcstore.ErrInternal,
		},
		{
			"staging deletion",
			`CREATE TRIGGER production_tail_blocker BEFORE DELETE ON staged_canonical_records
			 BEGIN SELECT RAISE(ABORT, 'blocked'); END`,
			rkcstore.ErrInternal,
		},
		{
			"build close",
			`CREATE TRIGGER production_tail_blocker BEFORE UPDATE OF state ON builds
			 WHEN NEW.state = 'committed' BEGIN SELECT RAISE(ABORT, 'blocked'); END`,
			rkcstore.ErrConflict,
		},
		{
			"repository publish",
			`CREATE TRIGGER production_tail_blocker BEFORE UPDATE OF current_snapshot_id ON repositories
			 WHEN NEW.current_snapshot_id IS NOT NULL BEGIN SELECT RAISE(ABORT, 'blocked'); END`,
			rkcstore.ErrConflict,
		},
	}
	for _, test := range tailFailures {
		t.Run(test.name, func(t *testing.T) {
			database := writerTestOpen(t)
			bundle := writerTestBundle("snapshot", "repository", "")
			build := writerTestStage(t, database, bundle, true)
			productionExec(t, database, test.statement)
			productionRequireError(t, database.Commit(context.Background(), build, bundle.Snapshot), test.want)
			if got := productionCount(t, database, "SELECT COUNT(*) FROM canonical_snapshots"); got != 0 {
				t.Fatalf("failed publication retained %d canonical snapshots", got)
			}
		})
	}
}

func TestProductionCanonicalReaderCorruptionFailsClosed(t *testing.T) {
	ctx := context.Background()

	t.Run("bundle snapshot strict JSON", func(t *testing.T) {
		database, bundle := productionReaderFixture(t)
		body, err := json.Marshal(bundle.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(body, &object); err != nil {
			t.Fatal(err)
		}
		object["unknown"] = true
		body, err = json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		productionExec(t, database, "DROP TRIGGER canonical_snapshots_update_guard")
		productionExec(t, database,
			"UPDATE canonical_snapshots SET canonical_snapshot_json = ? WHERE snapshot_id = 'snapshot'",
			string(body))
		_, err = database.Bundle(ctx, "snapshot")
		productionRequireError(t, err, rkcstore.ErrValidation)
	})

	t.Run("bundle snapshot size ceiling", func(t *testing.T) {
		database, bundle := productionReaderFixture(t)
		body, err := json.Marshal(map[string]any{
			"schema_version": bundle.Snapshot.SchemaVersion,
			"id":             bundle.Snapshot.ID,
			"repository_id":  bundle.Snapshot.RepositoryID,
			"status":         "committed",
			"padding":        strings.Repeat("x", readerMaxObjectJSONBytes),
		})
		if err != nil {
			t.Fatal(err)
		}
		productionExec(t, database, "DROP TRIGGER canonical_snapshots_update_guard")
		productionExec(t, database,
			"UPDATE canonical_snapshots SET canonical_snapshot_json = ? WHERE snapshot_id = 'snapshot'",
			string(body))
		_, err = database.Bundle(ctx, "snapshot")
		productionRequireError(t, err, rkcstore.ErrResourceExhausted)
	})

	t.Run("bundle strict JSON", func(t *testing.T) {
		database, bundle := productionReaderFixture(t)
		body, err := rkcmodel.CanonicalJSON(bundle)
		if err != nil {
			t.Fatal(err)
		}
		var object map[string]any
		if err := json.Unmarshal(body, &object); err != nil {
			t.Fatal(err)
		}
		object["unknown"] = true
		body, err = json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		productionExec(t, database, "DROP TRIGGER canonical_snapshots_update_guard")
		productionExec(t, database,
			`UPDATE canonical_snapshots
			 SET canonical_bundle_json = ?, canonical_digest = ?
			 WHERE snapshot_id = 'snapshot'`, string(body), writerSHA256(body))
		_, err = database.Bundle(ctx, "snapshot")
		productionRequireError(t, err, rkcstore.ErrValidation)
	})

	t.Run("bundle canonical form", func(t *testing.T) {
		database, bundle := productionReaderFixture(t)
		bundle.Nodes[0], bundle.Nodes[1] = bundle.Nodes[1], bundle.Nodes[0]
		body, err := json.Marshal(bundle)
		if err != nil {
			t.Fatal(err)
		}
		productionExec(t, database, "DROP TRIGGER canonical_snapshots_update_guard")
		productionExec(t, database,
			`UPDATE canonical_snapshots
			 SET canonical_bundle_json = ?, canonical_digest = ?
			 WHERE snapshot_id = 'snapshot'`, string(body), writerSHA256(body))
		_, err = database.Bundle(ctx, "snapshot")
		productionRequireError(t, err, rkcstore.ErrValidation)
	})

	t.Run("bundle model validation", func(t *testing.T) {
		database, bundle := productionReaderFixture(t)
		bundle.Edges[0].To = "missing-node"
		body, err := rkcmodel.CanonicalJSON(bundle)
		if err != nil {
			t.Fatal(err)
		}
		productionExec(t, database, "DROP TRIGGER canonical_snapshots_update_guard")
		productionExec(t, database,
			`UPDATE canonical_snapshots
			 SET canonical_bundle_json = ?, canonical_digest = ?
			 WHERE snapshot_id = 'snapshot'`, string(body), writerSHA256(body))
		_, err = database.Bundle(ctx, "snapshot")
		productionRequireError(t, err, rkcstore.ErrValidation)
	})

	coverageCases := []struct {
		name   string
		mutate func(*testing.T, *Database, rkcmodel.Bundle)
		want   error
	}{
		{
			"missing",
			func(t *testing.T, database *Database, _ rkcmodel.Bundle) {
				productionExec(t, database, "DROP TRIGGER canonical_snapshot_records_delete_guard")
				productionExec(t, database,
					"DELETE FROM canonical_snapshot_records WHERE snapshot_id = 'snapshot' AND record_family = 'coverage'")
			},
			rkcstore.ErrRecordNotFound,
		},
		{
			"identity",
			func(t *testing.T, database *Database, _ rkcmodel.Bundle) {
				productionExec(t, database, "DROP TRIGGER canonical_snapshot_records_update_guard")
				productionExec(t, database,
					`UPDATE canonical_snapshot_records SET record_id = 'other'
					 WHERE snapshot_id = 'snapshot' AND record_family = 'coverage'`)
			},
			rkcstore.ErrValidation,
		},
		{
			"digest",
			func(t *testing.T, database *Database, _ rkcmodel.Bundle) {
				productionExec(t, database, "DROP TRIGGER canonical_snapshot_records_update_guard")
				productionExec(t, database,
					`UPDATE canonical_snapshot_records SET canonical_record_sha256 = ?
					 WHERE snapshot_id = 'snapshot' AND record_family = 'coverage'`, strings.Repeat("0", 64))
			},
			rkcstore.ErrValidation,
		},
		{
			"strict JSON",
			func(t *testing.T, database *Database, _ rkcmodel.Bundle) {
				body := []byte(`{"snapshot_id":"snapshot","unknown":true}`)
				productionReplaceCanonicalRecord(t, database, "coverage", "coverage", body)
			},
			rkcstore.ErrValidation,
		},
		{
			"snapshot identity",
			func(t *testing.T, database *Database, bundle rkcmodel.Bundle) {
				coverage := rkcmodel.BuildCoverage(bundle)
				coverage.SnapshotID = "other"
				body, err := json.Marshal(coverage)
				if err != nil {
					t.Fatal(err)
				}
				productionReplaceCanonicalRecord(t, database, "coverage", "coverage", body)
			},
			rkcstore.ErrValidation,
		},
	}
	for _, test := range coverageCases {
		t.Run("coverage "+test.name, func(t *testing.T) {
			database, bundle := productionReaderFixture(t)
			test.mutate(t, database, bundle)
			_, err := database.Coverage(ctx, "snapshot")
			productionRequireError(t, err, test.want)
		})
	}

	t.Run("multiple coverage records", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		var body, digest string
		if err := database.db.QueryRow(
			`SELECT canonical_record_json, canonical_record_sha256
			 FROM canonical_snapshot_records
			 WHERE snapshot_id = 'snapshot' AND record_family = 'coverage'`,
		).Scan(&body, &digest); err != nil {
			t.Fatal(err)
		}
		productionExec(t, database, "DROP TRIGGER canonical_snapshot_records_insert_guard")
		productionExec(t, database,
			`INSERT INTO canonical_snapshot_records(
			 snapshot_id, record_family, record_id, ordinal,
			 canonical_record_json, canonical_record_sha256
			 ) VALUES ('snapshot', 'coverage', 'other', 1, ?, ?)`, body, digest)
		_, err := database.Coverage(ctx, "snapshot")
		productionRequireError(t, err, rkcstore.ErrValidation)
	})

	queryCases := []struct {
		name string
		body func(rkcmodel.Bundle) []byte
		want error
	}{
		{
			"oversized",
			func(rkcmodel.Bundle) []byte {
				return []byte(`{"id":"node-a","padding":"` + strings.Repeat("x", readerMaxObjectJSONBytes) + `"}`)
			},
			rkcstore.ErrResourceExhausted,
		},
		{
			"strict JSON",
			func(rkcmodel.Bundle) []byte { return []byte(`{"id":"node-a","unknown":true}`) },
			rkcstore.ErrValidation,
		},
		{
			"row identity",
			func(bundle rkcmodel.Bundle) []byte {
				value := bundle.Nodes[0]
				value.ID = "different"
				body, _ := json.Marshal(value)
				return body
			},
			rkcstore.ErrValidation,
		},
	}
	for _, test := range queryCases {
		t.Run("query "+test.name, func(t *testing.T) {
			database, bundle := productionReaderFixture(t)
			productionReplaceCanonicalRecord(t, database, "node", "node-a", test.body(bundle))
			_, err := database.QueryNodes(ctx, rkcstore.NodeQuery{SnapshotID: "snapshot"})
			productionRequireError(t, err, test.want)
		})
	}

	t.Run("stale record cursor", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		page, err := database.QueryNodes(ctx, rkcstore.NodeQuery{
			SnapshotID: "snapshot", PageRequest: rkcstore.PageRequest{Limit: 1},
		})
		if err != nil || page.Next == "" {
			t.Fatalf("first page = %+v, %v", page, err)
		}
		productionExec(t, database, "DROP TRIGGER canonical_snapshot_records_delete_guard")
		productionExec(t, database,
			`DELETE FROM canonical_snapshot_records
			 WHERE snapshot_id = 'snapshot' AND record_family = 'node' AND record_id = 'node-a'`)
		_, err = database.QueryNodes(ctx, rkcstore.NodeQuery{
			SnapshotID: "snapshot", PageRequest: rkcstore.PageRequest{Cursor: page.Next},
		})
		productionRequireError(t, err, rkcstore.ErrInvalidCursor)
	})

	t.Run("invalid publication time", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		productionExec(t, database, "DROP TRIGGER canonical_snapshots_update_guard")
		productionExec(t, database,
			"UPDATE canonical_snapshots SET published_at = 'not-a-time' WHERE snapshot_id = 'snapshot'")
		_, err := database.ListSnapshots(ctx, rkcstore.SnapshotQuery{RepositoryID: "repository"})
		productionRequireError(t, err, rkcstore.ErrValidation)
	})
}

func TestProductionPrivateErrorClassifiersRemainTyped(t *testing.T) {
	typed := writerConflict("operation", "build", "snapshot", "conflict")
	if got := readerStorageOrTypedError("read", "snapshot", "field", typed); got != typed {
		t.Fatalf("typed error was replaced: %v", got)
	}
	productionRequireError(
		t,
		readerStorageOrTypedError("read", "snapshot", "field", errors.New("I/O failure")),
		rkcstore.ErrInternal,
	)
	productionRequireError(
		t,
		writerStoredIDError("load", "build", "node", "expected", "observed", nil),
		rkcstore.ErrConflict,
	)
	decodeErr := writerConflict("load", "build", "", "decode")
	if got := writerStoredIDError("load", "build", "node", "expected", "", decodeErr); got != decodeErr {
		t.Fatalf("decode error was replaced: %v", got)
	}
	productionRequireError(t, writerResource("write", "build", "field", "bounded"), rkcstore.ErrResourceExhausted)
}
