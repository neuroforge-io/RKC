package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

func TestSQLiteRepositoryAffinityBindsAndRoundTrips(t *testing.T) {
	database := writerTestOpen(t)
	bundle := originAffinityBundle("snapshot", "ssh://example.test/NeuroForge/RKC.git", "")
	writerTestCommit(t, database, bundle)

	var stored sql.NullString
	if err := database.db.QueryRow(
		"SELECT repository_affinity FROM repositories WHERE repository_id = ?",
		bundle.Snapshot.RepositoryID,
	).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !stored.Valid || stored.String != bundle.Snapshot.RepositoryID {
		t.Fatalf("stored affinity = %#v, want snapshot repository identity", stored)
	}

	ctx := context.Background()
	reads := []struct {
		name string
		read func() (string, error)
	}{
		{"snapshot", func() (string, error) {
			value, err := database.Snapshot(ctx, "snapshot")
			return value.Git.Origin, err
		}},
		{"bundle", func() (string, error) {
			value, err := database.Bundle(ctx, "snapshot")
			return value.Snapshot.Git.Origin, err
		}},
		{"current", func() (string, error) {
			value, err := database.Current(ctx, rkcstore.RepositoryID(bundle.Snapshot.RepositoryID))
			return value.Git.Origin, err
		}},
		{"list", func() (string, error) {
			value, err := database.ListSnapshots(ctx, rkcstore.SnapshotQuery{
				RepositoryID: rkcstore.RepositoryID(bundle.Snapshot.RepositoryID),
			})
			if err != nil || len(value.Items) != 1 {
				return "", err
			}
			return value.Items[0].Git.Origin, nil
		}},
	}
	for _, fixture := range reads {
		t.Run(fixture.name, func(t *testing.T) {
			observed, err := fixture.read()
			if err != nil || observed != bundle.Snapshot.Git.Origin {
				t.Fatalf("origin = %q, %v; want snapshot origin", observed, err)
			}
		})
	}

	if _, err := database.db.Exec(
		"UPDATE repositories SET repository_affinity = ? WHERE repository_id = ?",
		"different-affinity",
		bundle.Snapshot.RepositoryID,
	); err == nil {
		t.Fatal("bound repository affinity was mutable")
	}
	if _, err := database.db.Exec("DROP TRIGGER repositories_affinity_immutable_guard"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(
		"UPDATE repositories SET repository_affinity = ? WHERE repository_id = ?",
		"different-affinity",
		bundle.Snapshot.RepositoryID,
	); err == nil {
		t.Fatal("published snapshot accepted a different repository affinity")
	}
	if _, err := database.db.Exec("DROP TRIGGER repositories_current_snapshot_affinity_guard"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.db.Exec(
		"UPDATE repositories SET repository_affinity = NULL WHERE repository_id = ?",
		bundle.Snapshot.RepositoryID,
	); err == nil {
		t.Fatal("published repository accepted a NULL affinity")
	}
}

func TestSQLiteRepositoryAffinitySurvivesPrivacyModeTransitions(t *testing.T) {
	const origin = "https://example.test/NeuroForge/RKC.git"
	for _, fixture := range []struct {
		name          string
		firstRedacted bool
	}{
		{"canonical to redacted", false},
		{"redacted to canonical", true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			database := writerTestOpen(t)
			first := originAffinityBundle("first", origin, "")
			if fixture.firstRedacted {
				redactOriginProvenance(&first)
			}
			writerTestCommit(t, database, first)

			second := originAffinityBundle("second", origin, "first")
			if !fixture.firstRedacted {
				redactOriginProvenance(&second)
			}
			writerTestCommit(t, database, second)

			var affinity string
			if err := database.db.QueryRow(
				"SELECT repository_affinity FROM repositories WHERE repository_id = ?",
				first.Snapshot.RepositoryID,
			).Scan(&affinity); err != nil {
				t.Fatal(err)
			}
			if affinity != first.Snapshot.RepositoryID {
				t.Fatalf("repository affinity = %q, want opaque RepositoryID", affinity)
			}
			current, err := database.Current(
				context.Background(),
				rkcstore.RepositoryID(first.Snapshot.RepositoryID),
			)
			if err != nil || current.ID != second.Snapshot.ID ||
				current.Git.Origin != second.Snapshot.Git.Origin {
				t.Fatalf("Current after privacy transition = %+v, %v", current, err)
			}

			redactedID := first.Snapshot.ID
			if !fixture.firstRedacted {
				redactedID = second.Snapshot.ID
			}
			var snapshotJSON, bundleJSON string
			if err := database.db.QueryRow(
				`SELECT canonical_snapshot_json, canonical_bundle_json
				 FROM canonical_snapshots WHERE snapshot_id = ?`,
				redactedID,
			).Scan(&snapshotJSON, &bundleJSON); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(snapshotJSON, origin) || strings.Contains(bundleJSON, origin) ||
				strings.Contains(affinity, origin) {
				t.Fatal("redacted durable snapshot retained raw repository origin")
			}
		})
	}
}

func TestSQLiteRepositoryAffinityMismatchRollsBackPublication(t *testing.T) {
	database := writerTestOpen(t)
	first := originAffinityBundle("first", "https://example.test/repository.git", "")
	writerTestCommit(t, database, first)

	const corruptedAffinity = "private-affinity-sentinel"
	for _, trigger := range []string{
		"repositories_affinity_immutable_guard",
		"repositories_current_snapshot_affinity_required_guard",
		"repositories_current_snapshot_affinity_guard",
	} {
		if _, err := database.db.Exec("DROP TRIGGER " + trigger); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.db.Exec(
		"UPDATE repositories SET repository_affinity = ? WHERE repository_id = ?",
		corruptedAffinity,
		first.Snapshot.RepositoryID,
	); err != nil {
		t.Fatal(err)
	}

	second := originAffinityBundle("second", first.Snapshot.Git.Origin, "first")
	build := writerTestStage(t, database, second, true)
	err := database.Commit(context.Background(), build, second.Snapshot)
	if !errors.Is(err, rkcstore.ErrConflict) {
		t.Fatalf("mismatched Commit = %v, want ErrConflict", err)
	}
	for _, forbidden := range []string{first.Snapshot.Git.Origin, corruptedAffinity} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("mismatch error disclosed repository origin: %v", err)
		}
	}

	var current string
	var origin sql.NullString
	if err := database.db.QueryRow(
		`SELECT current_snapshot_id, repository_affinity
		 FROM repositories WHERE repository_id = ?`,
		first.Snapshot.RepositoryID,
	).Scan(&current, &origin); err != nil {
		t.Fatal(err)
	}
	if current != first.Snapshot.ID || !origin.Valid || origin.String != corruptedAffinity {
		t.Fatalf("repository after mismatch = current:%q origin:%#v", current, origin)
	}
	for _, query := range []string{
		"SELECT COUNT(*) FROM canonical_snapshots WHERE snapshot_id = 'second'",
		"SELECT COUNT(*) FROM canonical_snapshot_records WHERE snapshot_id = 'second'",
		"SELECT COUNT(*) FROM snapshots WHERE snapshot_id = 'second'",
		"SELECT COUNT(*) FROM artifacts WHERE snapshot_id = 'second'",
		"SELECT COUNT(*) FROM nodes WHERE snapshot_id = 'second'",
		"SELECT COUNT(*) FROM search_fts WHERE snapshot_id = 'second'",
	} {
		var count int
		if err := database.db.QueryRow(query).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s = %d, %v; want no published rows", query, count, err)
		}
	}
}

func TestSQLiteRepositoryAffinityBindingWaitsForBundleValidation(t *testing.T) {
	database := writerTestOpen(t)
	bundle := originAffinityBundle("invalid", "https://example.test/not-published-sentinel.git", "")
	bundle.Documents[0].Status = "stale"
	build := writerTestStage(t, database, bundle, true)
	if err := database.Commit(context.Background(), build, bundle.Snapshot); !errors.Is(err, rkcstore.ErrValidation) {
		t.Fatalf("invalid Commit = %v, want ErrValidation", err)
	}
	var origin, current sql.NullString
	if err := database.db.QueryRow(
		`SELECT repository_affinity, current_snapshot_id
		 FROM repositories WHERE repository_id = ?`,
		bundle.Snapshot.RepositoryID,
	).Scan(&origin, &current); err != nil {
		t.Fatal(err)
	}
	if origin.Valid || current.Valid {
		t.Fatalf("failed validation bound or published repository: origin=%#v current=%#v", origin, current)
	}
}

func TestSQLiteReadersRejectCorruptRepositoryAffinityWithoutDisclosure(t *testing.T) {
	for _, fixture := range []struct {
		name  string
		value any
	}{
		{"null", nil},
		{"different", "private-affinity-sentinel"},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			database := writerTestOpen(t)
			bundle := originAffinityBundle("snapshot", "https://example.test/repository.git", "")
			writerTestCommit(t, database, bundle)
			for _, trigger := range []string{
				"repositories_affinity_immutable_guard",
				"repositories_current_snapshot_affinity_required_guard",
				"repositories_current_snapshot_affinity_guard",
			} {
				if _, err := database.db.Exec("DROP TRIGGER " + trigger); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.db.Exec(
				"UPDATE repositories SET repository_affinity = ? WHERE repository_id = ?",
				fixture.value,
				bundle.Snapshot.RepositoryID,
			); err != nil {
				t.Fatal(err)
			}

			ctx := context.Background()
			calls := []struct {
				name string
				call func() error
			}{
				{"snapshot", func() error { _, err := database.Snapshot(ctx, "snapshot"); return err }},
				{"bundle", func() error { _, err := database.Bundle(ctx, "snapshot"); return err }},
				{"current", func() error {
					_, err := database.Current(ctx, rkcstore.RepositoryID(bundle.Snapshot.RepositoryID))
					return err
				}},
				{"list", func() error {
					_, err := database.ListSnapshots(ctx, rkcstore.SnapshotQuery{
						RepositoryID: rkcstore.RepositoryID(bundle.Snapshot.RepositoryID),
					})
					return err
				}},
				{"artifact", func() error {
					_, err := database.Artifact(ctx, "snapshot", "artifact")
					return err
				}},
				{"node", func() error { _, err := database.Node(ctx, "snapshot", "node-a"); return err }},
				{"evidence", func() error {
					_, err := database.Evidence(ctx, "snapshot", "evidence")
					return err
				}},
				{"coverage", func() error { _, err := database.Coverage(ctx, "snapshot"); return err }},
				{"query nodes", func() error {
					_, err := database.QueryNodes(ctx, rkcstore.NodeQuery{SnapshotID: "snapshot"})
					return err
				}},
				{"query edges", func() error {
					_, err := database.QueryEdges(ctx, rkcstore.EdgeQuery{SnapshotID: "snapshot"})
					return err
				}},
				{"query diagnostics", func() error {
					_, err := database.QueryDiagnostics(ctx, rkcstore.DiagnosticQuery{SnapshotID: "snapshot"})
					return err
				}},
				{"search", func() error {
					_, err := database.SearchFTS(ctx, "snapshot", search.Query{Text: "Alpha"})
					return err
				}},
				{"empty search", func() error {
					_, err := database.SearchFTS(ctx, "snapshot", search.Query{})
					return err
				}},
			}
			for _, call := range calls {
				t.Run(call.name, func(t *testing.T) {
					err := call.call()
					if !errors.Is(err, rkcstore.ErrValidation) {
						t.Fatalf("read = %v, want ErrValidation", err)
					}
					for _, forbidden := range []string{
						bundle.Snapshot.Git.Origin,
						"private-affinity-sentinel",
					} {
						if strings.Contains(err.Error(), forbidden) {
							t.Fatalf("reader error disclosed origin: %v", err)
						}
					}
				})
			}
		})
	}
}

func TestMigrationV5BackfillsPublishedRepositoryAffinity(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "v4-origin.db")
	raw := rawDatabaseAtVersion(t, path, 4)
	bundle := originAffinityBundle("snapshot", "https://example.test/repository.git", "")
	seedV4PublishedSnapshot(t, raw, bundle)
	if _, err := raw.Exec(
		"UPDATE repositories SET canonical_origin = ? WHERE repository_id = ?",
		bundle.Snapshot.Git.Origin,
		bundle.Snapshot.RepositoryID,
	); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	database, err := Open(context.Background(), testOptions(path))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var affinity sql.NullString
	if err := database.db.QueryRow(
		"SELECT repository_affinity FROM repositories WHERE repository_id = ?",
		bundle.Snapshot.RepositoryID,
	).Scan(&affinity); err != nil {
		t.Fatal(err)
	}
	if !affinity.Valid || affinity.String != bundle.Snapshot.RepositoryID {
		t.Fatalf("migrated affinity = %#v, want repository identity", affinity)
	}
	loaded, err := database.Snapshot(context.Background(), "snapshot")
	if err != nil || loaded.Git.Origin != bundle.Snapshot.Git.Origin {
		t.Fatalf("migrated Snapshot = %+v, %v", loaded, err)
	}
}

func TestMigrationV5RejectsNoncanonicalOriginWithoutDisclosure(t *testing.T) {
	const sentinel = "migration-origin-secret-sentinel"
	path := filepath.Join(privateTempDir(t), "v4-secret-origin.db")
	raw := rawDatabaseAtVersion(t, path, 4)
	bundle := writerTestBundle("snapshot", "repository", "")
	bundle.Snapshot.Git.Origin = "https://alice:" + sentinel + "@example.test/repository.git?token=" + sentinel
	seedV4PublishedSnapshot(t, raw, bundle)
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := Open(context.Background(), testOptions(path))
	if !errors.Is(err, ErrBackfillRequired) {
		t.Fatalf("Open(noncanonical v4 origin) = %v, want ErrBackfillRequired", err)
	}
	if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), "alice:") {
		t.Fatalf("migration error disclosed stored origin: %v", err)
	}

	inspected, err := sql.Open("sqlite", sqliteURI(testOptions(path)))
	if err != nil {
		t.Fatal(err)
	}
	defer inspected.Close()
	version, err := readSchemaVersion(context.Background(), inspected)
	if err != nil {
		t.Fatal(err)
	}
	if version != 4 {
		t.Fatalf("failed origin migration changed schema version to %d", version)
	}
}

func TestMigrationV5PreflightRejectsSnapshotRowIdentityDrift(t *testing.T) {
	const sentinel = "migration-row-identity-sentinel"
	bundle := writerTestBundle("snapshot", "repository", "")
	payload, err := json.Marshal(bundle.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range []struct {
		name             string
		rowID            string
		rowRepositoryID  string
		rowSchemaVersion string
	}{
		{"snapshot id", sentinel, bundle.Snapshot.RepositoryID, bundle.Snapshot.SchemaVersion},
		{"repository id", bundle.Snapshot.ID, sentinel, bundle.Snapshot.SchemaVersion},
		{"schema version", bundle.Snapshot.ID, bundle.Snapshot.RepositoryID, sentinel},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			database, err := sql.Open("sqlite", ":memory:")
			if err != nil {
				t.Fatal(err)
			}
			database.SetMaxOpenConns(1)
			defer database.Close()
			if _, err := database.Exec(
				`CREATE TABLE schema_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
				 INSERT INTO schema_meta(key, value) VALUES ('schema_version', '0.4.0');
				 CREATE TABLE repositories(repository_id TEXT PRIMARY KEY, canonical_origin TEXT);
				 INSERT INTO repositories(repository_id, canonical_origin) VALUES (?, NULL);
				 CREATE TABLE canonical_snapshots(
				   snapshot_id TEXT PRIMARY KEY,
				   repository_id TEXT NOT NULL,
				   schema_version TEXT NOT NULL,
				   canonical_snapshot_json TEXT NOT NULL
				 );
				 INSERT INTO canonical_snapshots(
				   snapshot_id, repository_id, schema_version, canonical_snapshot_json
				 ) VALUES (?, ?, ?, ?);`,
				fixture.rowRepositoryID,
				fixture.rowID,
				fixture.rowRepositoryID,
				fixture.rowSchemaVersion,
				string(payload),
			); err != nil {
				t.Fatal(err)
			}
			err = checkV4RepositoryAffinityEligibility(context.Background(), database)
			if !errors.Is(err, ErrBackfillRequired) {
				t.Fatalf("preflight(row drift) = %v, want ErrBackfillRequired", err)
			}
			if strings.Contains(err.Error(), sentinel) {
				t.Fatalf("preflight error disclosed drifted row value: %v", err)
			}
		})
	}
}

func originAffinityBundle(id, origin string, parent rkcstore.SnapshotID) rkcmodel.Bundle {
	repositoryID := rkcmodel.StableID("repository", origin)
	bundle := writerTestBundle(id, rkcstore.RepositoryID(repositoryID), parent)
	bundle.Snapshot.Git.Origin = origin
	bundle.Snapshot.Metadata["source_reference"] = origin
	bundle.Nodes = append(bundle.Nodes, rkcmodel.Node{
		ID:            repositoryID,
		LogicalID:     repositoryID,
		Kind:          "repository",
		Name:          "Repository",
		QualifiedName: origin,
		Attributes:    map[string]any{"git_origin": origin},
	})
	return bundle
}

func redactOriginProvenance(bundle *rkcmodel.Bundle) {
	if bundle == nil {
		return
	}
	origin := bundle.Snapshot.Git.Origin
	bundle.Snapshot.Git.Origin = ""
	delete(bundle.Snapshot.Metadata, "source_reference")
	for index := range bundle.Nodes {
		node := &bundle.Nodes[index]
		if node.Kind != "repository" || node.ID != bundle.Snapshot.RepositoryID {
			continue
		}
		node.QualifiedName = ""
		delete(node.Attributes, "git_origin")
		for key, value := range node.Attributes {
			if text, ok := value.(string); ok && text == origin {
				delete(node.Attributes, key)
			}
		}
	}
}

func seedV4PublishedSnapshot(t *testing.T, database *sql.DB, bundle rkcmodel.Bundle) {
	t.Helper()
	transaction, err := database.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer transaction.Rollback()
	now := writerTimestamp(time.Unix(100, 0))
	if _, err := transaction.Exec(
		`INSERT INTO repositories(repository_id, display_name, created_at, metadata_json)
		 VALUES (?, ?, ?, '{}')`,
		bundle.Snapshot.RepositoryID,
		bundle.Snapshot.RepositoryID,
		now,
	); err != nil {
		t.Fatal(err)
	}
	buildID := "migration-v5-build"
	if _, err := transaction.Exec(
		`INSERT INTO builds(
		   build_id, repository_id, expected_schema, state, created_at, updated_at
		 ) VALUES (?, ?, ?, 'open', ?, ?)`,
		buildID,
		bundle.Snapshot.RepositoryID,
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
	bundleJSON, err := rkcmodel.CanonicalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Exec(
		`INSERT INTO canonical_snapshots(
		   snapshot_id, repository_id, build_id, schema_version,
		   canonical_snapshot_json, canonical_bundle_json, canonical_digest,
		   published_at
		 ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		bundle.Snapshot.ID,
		bundle.Snapshot.RepositoryID,
		buildID,
		bundle.Snapshot.SchemaVersion,
		string(snapshotJSON),
		string(bundleJSON),
		rkcmodel.CanonicalDigest(bundle),
		now,
	); err != nil {
		t.Fatal(err)
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
		"UPDATE repositories SET current_snapshot_id = ? WHERE repository_id = ?",
		bundle.Snapshot.ID,
		bundle.Snapshot.RepositoryID,
	); err != nil {
		t.Fatal(err)
	}
	if err := transaction.Commit(); err != nil {
		t.Fatal(err)
	}
}
