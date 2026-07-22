package sqlite

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

func finalRewriteStagedObject(
	t *testing.T,
	database *Database,
	build rkcstore.BuildID,
	family string,
	id string,
	mutate func(map[string]any),
) {
	t.Helper()
	var stored string
	if err := database.db.QueryRow(
		`SELECT canonical_record_json FROM staged_canonical_records
		 WHERE build_id = ? AND record_family = ? AND record_id = ?`,
		build, family, id,
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(stored), &object); err != nil {
		t.Fatal(err)
	}
	mutate(object)
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	productionExec(t, database,
		`UPDATE staged_canonical_records
		 SET canonical_record_json = ?, canonical_record_sha256 = ?
		 WHERE build_id = ? AND record_family = ? AND record_id = ?`,
		string(body), writerSHA256(body), build, family, id)
}

func finalAssertNoPublication(t *testing.T, database *Database) {
	t.Helper()
	if got := productionCount(t, database, "SELECT COUNT(*) FROM canonical_snapshots"); got != 0 {
		t.Fatalf("failed operation published %d canonical snapshots", got)
	}
}

func TestFinalWriterRejectsEveryLatePublicationHazard(t *testing.T) {
	ctx := context.Background()

	t.Run("nil and aborted writer", func(t *testing.T) {
		bundle := writerTestBundle("snapshot", "repository", "")
		var nilDatabase *Database
		productionRequireError(t, nilDatabase.Commit(ctx, "build", bundle.Snapshot), rkcstore.ErrConflict)

		database := writerTestOpen(t)
		build := writerTestStage(t, database, bundle, true)
		if err := database.Abort(ctx, build, errors.New("cancel publication")); err != nil {
			t.Fatal(err)
		}
		productionRequireError(t, database.Commit(ctx, build, bundle.Snapshot), rkcstore.ErrBuildClosed)
		finalAssertNoPublication(t, database)
	})

	t.Run("inconsistent coverage", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		build := writerTestStage(t, database, bundle, false)
		coverage := rkcmodel.BuildCoverage(bundle)
		coverage.NodesTotal++
		if err := database.PutCoverage(ctx, build, coverage); err != nil {
			t.Fatal(err)
		}
		productionRequireError(t, database.Commit(ctx, build, bundle.Snapshot), rkcstore.ErrCoverageMismatch)
		finalAssertNoPublication(t, database)
	})

	t.Run("semantic validation report", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		bundle.Edges[0].To = "missing-node"
		build := writerTestStage(t, database, bundle, true)
		err := database.Commit(ctx, build, bundle.Snapshot)
		if !errors.Is(err, rkcstore.ErrValidation) {
			t.Fatalf("invalid bundle commit = %v, want ErrValidation", err)
		}
		var failure *rkcstore.ValidationFailure
		if !errors.As(err, &failure) || !failure.Result.Report.HasErrors() {
			t.Fatalf("validation diagnostics were not preserved: %#v", err)
		}
		finalAssertNoPublication(t, database)
	})

	t.Run("duplicate snapshot across repositories", func(t *testing.T) {
		database := writerTestOpen(t)
		first := writerTestBundle("shared-snapshot", "repository-a", "")
		writerTestCommit(t, database, first)
		second := writerTestBundle("shared-snapshot", "repository-b", "")
		build := writerTestStage(t, database, second, true)
		productionRequireError(t, database.Commit(ctx, build, second.Snapshot), rkcstore.ErrConflict)
		if got := productionCount(t, database,
			"SELECT COUNT(*) FROM canonical_snapshots WHERE snapshot_id = 'shared-snapshot'"); got != 1 {
			t.Fatalf("duplicate identifier left %d canonical rows", got)
		}
	})

	identifierCases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"required identifier", func(object map[string]any) { object["id"] = "bad\nidentifier" }},
		{"optional identifier", func(object map[string]any) { object["logical_id"] = "bad\x00identifier" }},
	}
	for _, test := range identifierCases {
		t.Run(test.name, func(t *testing.T) {
			database := writerTestOpen(t)
			bundle := writerTestBundle("snapshot", "repository", "")
			build := writerTestStage(t, database, bundle, true)
			productionExec(t, database, "DROP TRIGGER staged_canonical_records_update_guard")
			if test.name == "required identifier" {
				productionExec(t, database,
					`UPDATE staged_canonical_records SET record_id = ?
					 WHERE build_id = ? AND record_family = 'artifact' AND record_id = 'artifact'`,
					"bad\nidentifier", build)
				finalRewriteStagedObject(t, database, build, "artifact", "bad\nidentifier", test.mutate)
			} else {
				finalRewriteStagedObject(t, database, build, "artifact", "artifact", test.mutate)
			}
			productionRequireError(t, database.Commit(ctx, build, bundle.Snapshot), rkcstore.ErrInvalidArgument)
			finalAssertNoPublication(t, database)
		})
	}

	t.Run("unknown staged family", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		build := writerTestStage(t, database, bundle, true)
		connection, err := database.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if _, err := connection.ExecContext(ctx, "PRAGMA ignore_check_constraints = ON"); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.ExecContext(ctx,
			`UPDATE staged_canonical_records SET record_family = 'unknown'
			 WHERE build_id = ? AND record_family = 'artifact'`, build); err != nil {
			t.Fatal(err)
		}
		productionRequireError(t, database.Commit(ctx, build, bundle.Snapshot), rkcstore.ErrConflict)
		finalAssertNoPublication(t, database)
	})

	t.Run("multiple coverage records", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		build := writerTestStage(t, database, bundle, true)
		coverage, err := json.Marshal(rkcmodel.BuildCoverage(bundle))
		if err != nil {
			t.Fatal(err)
		}
		productionExec(t, database,
			`INSERT INTO staged_canonical_records(
			 build_id, record_family, record_id, ordinal,
			 canonical_record_json, canonical_record_sha256
			 ) VALUES (?, 'coverage', 'second', 1, ?, ?)`,
			build, string(coverage), writerSHA256(coverage))
		productionRequireError(t, database.Commit(ctx, build, bundle.Snapshot), rkcstore.ErrConflict)
		finalAssertNoPublication(t, database)
	})

	t.Run("oversized durable staged record", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		build := writerTestStage(t, database, bundle, true)
		body := []byte(`{"id":"artifact","padding":"` +
			strings.Repeat("x", int(rkcstore.DefaultMemoryOptions().MaxRecordBytes)) + `"}`)
		productionExec(t, database, "DROP TRIGGER staged_canonical_records_update_guard")
		productionExec(t, database,
			`UPDATE staged_canonical_records
			 SET canonical_record_json = ?, canonical_record_sha256 = ?
			 WHERE build_id = ? AND record_family = 'artifact'`,
			string(body), writerSHA256(body), build)
		productionRequireError(t, database.Commit(ctx, build, bundle.Snapshot), rkcstore.ErrResourceExhausted)
		finalAssertNoPublication(t, database)
	})

	t.Run("metadata byte ceiling", func(t *testing.T) {
		database := writerTestOpen(t)
		_, err := database.BeginBuild(ctx, rkcstore.BuildOptions{
			RepositoryID: "repository",
			Metadata: map[string]string{
				"key": strings.Repeat("x", int(rkcstore.DefaultMemoryOptions().MaxMetadataBytes)),
			},
		})
		productionRequireError(t, err, rkcstore.ErrResourceExhausted)
	})
}

func TestFinalCanonicalRecordPreparationRejectsEveryUnserializableFamily(t *testing.T) {
	bad := map[string]any{"channel": make(chan int)}
	cases := []struct {
		name   string
		mutate func(*rkcmodel.Bundle)
	}{
		{"node", func(bundle *rkcmodel.Bundle) { bundle.Nodes[0].Attributes = bad }},
		{"edge", func(bundle *rkcmodel.Bundle) { bundle.Edges[0].Attributes = bad }},
		{"evidence", func(bundle *rkcmodel.Bundle) { bundle.Evidence[0].Attributes = bad }},
		{"diagnostic", func(bundle *rkcmodel.Bundle) { bundle.Diagnostics[0].Attributes = bad }},
		{"conflict", func(bundle *rkcmodel.Bundle) { bundle.Conflicts[0].Attributes = bad }},
		{"document", func(bundle *rkcmodel.Bundle) { bundle.Documents[0].Attributes = bad }},
		{"claim", func(bundle *rkcmodel.Bundle) { bundle.Claims[0].Attributes = bad }},
		{"execution path", func(bundle *rkcmodel.Bundle) { bundle.Paths[0].Attributes = bad }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			bundle := writerTestBundle("snapshot", "repository", "")
			test.mutate(&bundle)
			_, err := writerPrepareCanonicalRecords("publish", bundle, rkcmodel.Coverage{})
			productionRequireError(t, err, rkcstore.ErrInvalidArgument)
		})
	}

	for name, payload := range map[string][]byte{
		"unknown field":  []byte(`{"id":"node","unknown":true}`),
		"trailing value": []byte(`{"id":"node"}{}`),
		"wrong type":     []byte(`{"id":1}`),
	} {
		t.Run("strict decode "+name, func(t *testing.T) {
			var node rkcmodel.Node
			if err := writerDecodeJSON(payload, &node); err == nil {
				t.Fatalf("writer decoder accepted %s", name)
			}
		})
	}
	if err := writerValidIdentifier("write", "record_id", strings.Repeat("x", rkcstore.MaxIdentifierSize+1)); !errors.Is(err, rkcstore.ErrInvalidArgument) {
		t.Fatalf("oversized identifier = %v", err)
	}
}

func finalRewriteCanonicalSnapshot(
	t *testing.T,
	database *Database,
	mutate func(map[string]any),
) {
	t.Helper()
	var stored string
	if err := database.db.QueryRow(
		"SELECT canonical_snapshot_json FROM canonical_snapshots WHERE snapshot_id = 'snapshot'",
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal([]byte(stored), &object); err != nil {
		t.Fatal(err)
	}
	mutate(object)
	body, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	productionExec(t, database, "DROP TRIGGER canonical_snapshots_update_guard")
	productionExec(t, database,
		"UPDATE canonical_snapshots SET canonical_snapshot_json = ? WHERE snapshot_id = 'snapshot'",
		string(body))
}

func finalAuthenticatedCursor(t *testing.T, key []byte, payload string) rkcstore.Cursor {
	t.Helper()
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(payload))
	return rkcstore.Cursor(base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)))
}

func TestFinalReaderPointListAndCursorIntegrity(t *testing.T) {
	ctx := context.Background()

	t.Run("snapshot identity", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		connection, err := database.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		if _, err := connection.ExecContext(ctx, "PRAGMA ignore_check_constraints = ON"); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.ExecContext(ctx, "DROP TRIGGER canonical_snapshots_update_guard"); err != nil {
			t.Fatal(err)
		}
		var stored string
		if err := connection.QueryRowContext(ctx,
			"SELECT canonical_snapshot_json FROM canonical_snapshots WHERE snapshot_id = 'snapshot'",
		).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		var snapshot rkcmodel.Snapshot
		if err := json.Unmarshal([]byte(stored), &snapshot); err != nil {
			t.Fatal(err)
		}
		snapshot.ID = "other"
		body, err := json.Marshal(snapshot)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := connection.ExecContext(ctx,
			"UPDATE canonical_snapshots SET canonical_snapshot_json = ? WHERE snapshot_id = 'snapshot'",
			string(body)); err != nil {
			t.Fatal(err)
		}
		_, err = database.Snapshot(ctx, "snapshot")
		productionRequireError(t, err, rkcstore.ErrValidation)
	})

	t.Run("point record size", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		body := []byte(`{"id":"artifact","padding":"` + strings.Repeat("x", readerMaxObjectJSONBytes) + `"}`)
		productionReplaceCanonicalRecord(t, database, "artifact", "artifact", body)
		_, err := database.Artifact(ctx, "snapshot", "artifact")
		productionRequireError(t, err, rkcstore.ErrResourceExhausted)
	})

	t.Run("point strict JSON", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		body := []byte(`{"id":"artifact","unknown":true}`)
		productionReplaceCanonicalRecord(t, database, "artifact", "artifact", body)
		_, err := database.Artifact(ctx, "snapshot", "artifact")
		productionRequireError(t, err, rkcstore.ErrValidation)
	})

	t.Run("point row identity", func(t *testing.T) {
		database, bundle := productionReaderFixture(t)
		artifact := bundle.Artifacts[0]
		artifact.ID = "other"
		body, err := json.Marshal(artifact)
		if err != nil {
			t.Fatal(err)
		}
		productionReplaceCanonicalRecord(t, database, "artifact", "artifact", body)
		_, err = database.Artifact(ctx, "snapshot", "artifact")
		productionRequireError(t, err, rkcstore.ErrValidation)
	})

	t.Run("lookup identifier ceiling", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		_, err := database.Evidence(ctx, "snapshot", strings.Repeat("x", rkcstore.MaxIdentifierSize+1))
		productionRequireError(t, err, rkcstore.ErrInvalidArgument)
	})

	t.Run("repository without current snapshot", func(t *testing.T) {
		database := writerTestOpen(t)
		productionExec(t, database,
			`INSERT INTO repositories(repository_id, display_name, created_at, metadata_json)
			 VALUES ('empty-repository', 'empty-repository', ?, '{}')`, writerTimestamp(time.Unix(1, 0)))
		_, err := database.Current(ctx, "empty-repository")
		productionRequireError(t, err, rkcstore.ErrSnapshotNotFound)
	})

	t.Run("digest representations", func(t *testing.T) {
		for name, digest := range map[string]string{
			"short":     "00",
			"uppercase": strings.Repeat("A", sha256.Size*2),
			"nonhex":    strings.Repeat("z", sha256.Size*2),
		} {
			t.Run(name, func(t *testing.T) {
				err := readerVerifyDigest("read", "snapshot", "record", `{}`, digest)
				productionRequireError(t, err, rkcstore.ErrValidation)
			})
		}
	})

	t.Run("non UTF-8 stored JSON", func(t *testing.T) {
		_, err := readerDecodeObject[rkcmodel.Node](
			"read", "snapshot", "node", string([]byte{0xff, 0xfe}), readerMaxObjectJSONBytes,
		)
		productionRequireError(t, err, rkcstore.ErrValidation)
	})

	t.Run("absent read-only cursor key", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		path := database.path
		productionExec(t, database, "DELETE FROM schema_meta WHERE key = ?", readerCursorKeyName)
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		options := testOptions(path)
		options.ReadOnly = true
		readOnly, err := Open(ctx, options)
		if err != nil {
			t.Fatal(err)
		}
		defer readOnly.Close()
		_, err = readOnly.ListSnapshots(ctx, rkcstore.SnapshotQuery{})
		productionRequireError(t, err, rkcstore.ErrValidation)
	})

	t.Run("corrupt cursor key", func(t *testing.T) {
		for name, key := range map[string]string{
			"short":     "aa",
			"uppercase": strings.Repeat("A", sha256.Size*2),
			"nonhex":    strings.Repeat("z", sha256.Size*2),
		} {
			t.Run(name, func(t *testing.T) {
				database, _ := productionReaderFixture(t)
				productionExec(t, database, "UPDATE schema_meta SET value = ? WHERE key = ?", key, readerCursorKeyName)
				_, err := database.ListSnapshots(ctx, rkcstore.SnapshotQuery{})
				productionRequireError(t, err, rkcstore.ErrValidation)
			})
		}
	})

	t.Run("authenticated cursor payload shape", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		connection, err := database.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		key, err := readerCursorKey(ctx, connection, "query nodes", true)
		if err != nil {
			t.Fatal(err)
		}
		connection.Close()
		scope := readerScopeFingerprint("snapshot", "", "", "", "")
		payloads := []string{
			`{"v":1,"k":"nodes","s":"` + scope + `","i":"node-a","unknown":true}`,
			`{"v":1,"k":"nodes","s":"` + scope + `","i":"node-a"}{}`,
			`{"k":"nodes","s":"` + scope + `","i":"node-a"}`,
			`{"v":1,"k":"nodes","s":"` + scope + `","i":""}`,
		}
		for _, payload := range payloads {
			cursor := finalAuthenticatedCursor(t, key, payload)
			_, err := database.QueryNodes(ctx, rkcstore.NodeQuery{
				SnapshotID: "snapshot", PageRequest: rkcstore.PageRequest{Cursor: cursor},
			})
			productionRequireError(t, err, rkcstore.ErrInvalidCursor)
		}
	})

	t.Run("record cursor sort key", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		connection, err := database.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		key, err := readerCursorKey(ctx, connection, "query nodes", true)
		connection.Close()
		if err != nil {
			t.Fatal(err)
		}
		cursor, err := readerSealCursor(
			"query nodes", key, "nodes", readerScopeFingerprint("snapshot", "", "", "", ""),
			"unexpected-sort-key", "node-a",
		)
		if err != nil {
			t.Fatal(err)
		}
		_, err = database.QueryNodes(ctx, rkcstore.NodeQuery{
			SnapshotID: "snapshot", PageRequest: rkcstore.PageRequest{Cursor: cursor},
		})
		productionRequireError(t, err, rkcstore.ErrInvalidCursor)
	})

	t.Run("stale snapshot cursor", func(t *testing.T) {
		database, _ := productionReaderFixture(t)
		second := readerTestBundle("snapshot-2", "repository", "snapshot", time.Unix(200, 0).UTC())
		readerTestSeedSnapshot(t, database, second)
		page, err := database.ListSnapshots(ctx, rkcstore.SnapshotQuery{
			RepositoryID: "repository", PageRequest: rkcstore.PageRequest{Limit: 1},
		})
		if err != nil || page.Next == "" {
			t.Fatalf("first snapshot page = %+v, %v", page, err)
		}
		productionExec(t, database, "DROP TRIGGER canonical_snapshots_update_guard")
		productionExec(t, database,
			"UPDATE canonical_snapshots SET published_at = ? WHERE snapshot_id = 'snapshot-2'",
			writerTimestamp(time.Unix(201, 0)))
		_, err = database.ListSnapshots(ctx, rkcstore.SnapshotQuery{
			RepositoryID: "repository", PageRequest: rkcstore.PageRequest{Cursor: page.Next},
		})
		productionRequireError(t, err, rkcstore.ErrInvalidCursor)
	})
}

func TestFinalLeaseAbortAndRecoveryStateMachines(t *testing.T) {
	ctx := context.Background()

	t.Run("lease manager lifecycle", func(t *testing.T) {
		var nilManager *writerLeaseManager
		if _, err := nilManager.acquire(false); !errors.Is(err, errWriterLeaseManagerGone) {
			t.Fatalf("nil acquire = %v", err)
		}
		if err := nilManager.attach("build", nil); !errors.Is(err, errWriterLeaseManagerGone) {
			t.Fatalf("nil attach = %v", err)
		}
		if _, owned, err := nilManager.lockBuild("build"); owned || !errors.Is(err, errWriterLeaseManagerGone) {
			t.Fatalf("nil lock = owned %t, %v", owned, err)
		}
		if err := nilManager.close(); err != nil {
			t.Fatalf("nil close = %v", err)
		}
		var nilLease *writerLease
		if err := nilLease.close(); err != nil {
			t.Fatalf("nil lease close = %v", err)
		}
		var nilGuard *writerLeaseGuard
		nilGuard.unlock()
		if err := nilGuard.release(); err != nil {
			t.Fatalf("nil guard release = %v", err)
		}

		directory := privateTempDir(t)
		manager, err := openWriterLeaseManager(filepath.Join(directory, "lease.db"))
		if err != nil {
			t.Fatal(err)
		}
		lease, err := manager.acquire(false)
		if err != nil {
			t.Fatal(err)
		}
		if err := manager.attach("build", lease); err != nil {
			t.Fatal(err)
		}
		if err := manager.attach("build", lease); err == nil {
			t.Fatal("duplicate lease attachment succeeded")
		}
		if guard, owned, err := manager.lockBuild("missing"); err != nil || owned || guard != nil {
			t.Fatalf("missing lease lock = %+v, %t, %v", guard, owned, err)
		}
		guard, owned, err := manager.lockBuild("build")
		if err != nil || !owned {
			t.Fatalf("owned lease lock = %+v, %t, %v", guard, owned, err)
		}
		guard.unlock()
		guard, owned, err = manager.lockBuild("build")
		if err != nil || !owned {
			t.Fatalf("second lease lock = %+v, %t, %v", guard, owned, err)
		}
		if err := guard.release(); err != nil {
			t.Fatal(err)
		}
		if err := lease.close(); err != nil {
			t.Fatalf("idempotent lease close = %v", err)
		}
		if err := manager.close(); err != nil {
			t.Fatal(err)
		}
		if err := manager.close(); err != nil {
			t.Fatalf("idempotent manager close = %v", err)
		}
		if _, err := manager.acquire(false); !errors.Is(err, errWriterLeaseManagerGone) {
			t.Fatalf("closed manager acquire = %v", err)
		}
		if err := manager.attach("late", &writerLease{}); !errors.Is(err, errWriterLeaseManagerGone) {
			t.Fatalf("closed manager attach = %v", err)
		}
		if _, _, err := manager.lockBuild("late"); !errors.Is(err, errWriterLeaseManagerGone) {
			t.Fatalf("closed manager lock = %v", err)
		}
	})

	t.Run("abort unsupported durable state", func(t *testing.T) {
		database := writerTestOpen(t)
		build, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"})
		if err != nil {
			t.Fatal(err)
		}
		connection, err := database.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close()
		for _, statement := range []string{
			"PRAGMA ignore_check_constraints = ON",
			"DROP TRIGGER builds_state_transition_guard",
		} {
			if _, err := connection.ExecContext(ctx, statement); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := connection.ExecContext(ctx, "UPDATE builds SET state = 'unknown' WHERE build_id = ?", build); err != nil {
			t.Fatal(err)
		}
		productionRequireError(t, database.Abort(ctx, build, nil), rkcstore.ErrBuildClosed)
	})

	t.Run("abort canonical snapshot conflict", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		build, err := database.BeginBuild(ctx, rkcstore.BuildOptions{RepositoryID: "repository"})
		if err != nil {
			t.Fatal(err)
		}
		snapshotJSON, _ := json.Marshal(bundle.Snapshot)
		bundleJSON, _ := rkcmodel.CanonicalJSON(bundle)
		productionExec(t, database,
			`INSERT INTO canonical_snapshots(
			 snapshot_id, repository_id, build_id, schema_version,
			 canonical_snapshot_json, canonical_bundle_json, canonical_digest, published_at
			 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			bundle.Snapshot.ID, bundle.Snapshot.RepositoryID, build, bundle.Snapshot.SchemaVersion,
			string(snapshotJSON), string(bundleJSON), writerSHA256(bundleJSON), writerTimestamp(bundle.Snapshot.CreatedAt))
		productionRequireError(t, database.Abort(ctx, build, nil), rkcstore.ErrConflict)
	})

	t.Run("recover canonical snapshot conflict", func(t *testing.T) {
		database := writerTestOpen(t)
		productionInsertOpenBuild(t, database, "orphan")
		bundle := writerTestBundle("snapshot", "repository", "")
		snapshotJSON, _ := json.Marshal(bundle.Snapshot)
		bundleJSON, _ := rkcmodel.CanonicalJSON(bundle)
		productionExec(t, database,
			`INSERT INTO canonical_snapshots(
			 snapshot_id, repository_id, build_id, schema_version,
			 canonical_snapshot_json, canonical_bundle_json, canonical_digest, published_at
			 ) VALUES (?, ?, 'orphan', ?, ?, ?, ?, ?)`,
			bundle.Snapshot.ID, bundle.Snapshot.RepositoryID, bundle.Snapshot.SchemaVersion,
			string(snapshotJSON), string(bundleJSON), writerSHA256(bundleJSON), writerTimestamp(bundle.Snapshot.CreatedAt))
		_, err := database.Recover(ctx)
		productionRequireError(t, err, rkcstore.ErrConflict)
	})

	t.Run("recover compare and swap", func(t *testing.T) {
		database := writerTestOpen(t)
		productionInsertOpenBuild(t, database, "orphan")
		productionExec(t, database,
			`CREATE TRIGGER final_recovery_cas_blocker
			 BEFORE UPDATE OF state ON builds WHEN NEW.state = 'aborted'
			 BEGIN SELECT RAISE(IGNORE); END`)
		_, err := database.Recover(ctx)
		productionRequireError(t, err, rkcstore.ErrConflict)
		if got := productionCount(t, database,
			"SELECT COUNT(*) FROM builds WHERE build_id = 'orphan' AND state = 'open'"); got != 1 {
			t.Fatalf("recovery CAS failure changed %d rows", got)
		}
	})
}

func TestFinalProjectionRepresentationAndFTSFailures(t *testing.T) {
	ctx := context.Background()

	t.Run("incompatible logical identity", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		bundle.Nodes[0].LogicalID = bundle.Artifacts[0].LogicalID
		build := writerTestStage(t, database, bundle, true)
		productionRequireError(t, database.Commit(ctx, build, bundle.Snapshot), rkcstore.ErrValidation)
		finalAssertNoPublication(t, database)
	})

	t.Run("duplicate section in one document", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		duplicate := bundle.Documents[0].Sections[0]
		duplicate.Ordinal++
		bundle.Documents[0].Sections = append(bundle.Documents[0].Sections, duplicate)
		build := writerTestStage(t, database, bundle, true)
		err := database.Commit(ctx, build, bundle.Snapshot)
		var failure *rkcstore.ValidationFailure
		if !errors.Is(err, rkcstore.ErrValidation) || !errors.As(err, &failure) || !failure.Result.Report.HasErrors() {
			t.Fatalf("duplicate section validation = %#v", err)
		}
		finalAssertNoPublication(t, database)
	})

	t.Run("unrepresentable document parent", func(t *testing.T) {
		database := writerTestOpen(t)
		bundle := writerTestBundle("snapshot", "repository", "")
		bundle.Documents[0].Sections[0].ParentID = "missing-parent"
		build := writerTestStage(t, database, bundle, true)
		err := database.Commit(ctx, build, bundle.Snapshot)
		var failure *rkcstore.ValidationFailure
		if !errors.Is(err, rkcstore.ErrValidation) || !errors.As(err, &failure) || !failure.Result.Report.HasErrors() {
			t.Fatalf("missing parent validation = %#v", err)
		}
		finalAssertNoPublication(t, database)
	})

	ftsCases := []struct {
		name   string
		bundle rkcmodel.Bundle
	}{
		{"artifact", rkcmodel.Bundle{Snapshot: writerTestBundle("snapshot", "repository", "").Snapshot, Artifacts: []rkcmodel.Artifact{{ID: "artifact"}}}},
		{"node", rkcmodel.Bundle{Snapshot: writerTestBundle("snapshot", "repository", "").Snapshot, Nodes: []rkcmodel.Node{{ID: "node"}}}},
		{"document section", rkcmodel.Bundle{Snapshot: writerTestBundle("snapshot", "repository", "").Snapshot, Documents: []rkcmodel.Document{{ID: "document", Sections: []rkcmodel.DocumentSection{{ID: "section"}}}}}},
		{"document", rkcmodel.Bundle{Snapshot: writerTestBundle("snapshot", "repository", "").Snapshot, Documents: []rkcmodel.Document{{ID: "document"}}}},
		{"claim", rkcmodel.Bundle{Snapshot: writerTestBundle("snapshot", "repository", "").Snapshot, Claims: []rkcmodel.Claim{{ID: "claim"}}}},
	}
	for _, test := range ftsCases {
		t.Run("FTS "+test.name, func(t *testing.T) {
			database := writerTestOpen(t)
			productionExec(t, database, "DROP TABLE search_fts")
			transaction, err := database.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer transaction.Rollback()
			err = writerProjectFTS(ctx, transaction, "project", writerBuildRecord{id: "build"}, test.bundle)
			productionRequireError(t, err, rkcstore.ErrInternal)
		})
	}
}

func TestFinalUpgradeAndStorageClassifierBoundaries(t *testing.T) {
	ctx := context.Background()
	if err := CheckV2UpgradeEligibility(ctx, nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("nil v2 database = %v", err)
	}
	if err := readerStorageError("read", "snapshot", "database", ErrBusy); !errors.Is(err, rkcstore.ErrInternal) {
		t.Fatalf("bare sentinel reader classification = %v", err)
	}
	if got := writerLimitedReason(errors.New("reason"), 0); got != "" {
		t.Fatalf("zero reason limit = %q", got)
	}
	if got := writerLimitedReason(nil, 10); got != "" {
		t.Fatalf("nil reason = %q", got)
	}

	closed, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := enableWAL(ctx, closed, "closed", time.Millisecond); err == nil {
		t.Fatal("enableWAL accepted a closed database")
	}

	path := filepath.Join(privateTempDir(t), "full-options.db")
	options := testOptions(path)
	options.Synchronous = "full"
	options.MMapBytes = 4096
	options.CacheKiB = 2048
	normalized, err := normalizeOptions(options)
	if err != nil || normalized.Synchronous != "FULL" || normalized.MMapBytes != 4096 || normalized.CacheKiB != 2048 {
		t.Fatalf("normalized explicit options = %+v, %v", normalized, err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
}
