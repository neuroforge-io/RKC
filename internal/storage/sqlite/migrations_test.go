package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	sqliteassets "github.com/neuroforge-io/RKC/storage/sqlite"
)

func cloneMigrationAssets(t *testing.T) fstest.MapFS {
	t.Helper()
	result := fstest.MapFS{}
	err := fs.WalkDir(sqliteassets.Migrations, ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		payload, err := fs.ReadFile(sqliteassets.Migrations, path)
		if err != nil {
			return err
		}
		result[path] = &fstest.MapFile{Data: payload, Mode: 0o444}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func mutateManifest(
	t *testing.T,
	assets fstest.MapFS,
	mutate func(map[string]any),
) string {
	t.Helper()
	var document map[string]any
	if err := json.Unmarshal(assets[manifestPath].Data, &document); err != nil {
		t.Fatal(err)
	}
	mutate(document)
	payload, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, '\n')
	assets[manifestPath].Data = payload
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func rawDatabaseAtVersion(t *testing.T, path string, version int) *sql.DB {
	t.Helper()
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	options := testOptions(path)
	database, err := sql.Open("sqlite", sqliteURI(options))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	if err := database.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := enableWAL(context.Background(), database, path, defaultBusyTimeout); err != nil {
		t.Fatal(err)
	}
	plan, err := embeddedMigrationPlan()
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < version; index++ {
		if err := applyMigration(context.Background(), database, path, plan[index]); err != nil {
			t.Fatalf("apply migration %d: %v", index+1, err)
		}
	}
	return database
}

func TestEmbeddedMigrationPlanIsStrictAndComplete(t *testing.T) {
	plan, err := embeddedMigrationPlan()
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != currentDatabaseVersion {
		t.Fatalf("migration count = %d", len(plan))
	}
	for index, item := range plan {
		if item.Version != index+1 || item.Body == "" || item.Filename == "" {
			t.Fatalf("migration %d = %#v", index, item)
		}
	}
}

func TestMigrationAssetsFailClosedOnTamper(t *testing.T) {
	assets := cloneMigrationAssets(t)
	migrationPath := "migrations/0001_initial.sql"
	assets[migrationPath].Data = append(append([]byte(nil), assets[migrationPath].Data...), []byte("-- drift\n")...)
	if _, err := loadMigrationPlan(assets, embeddedManifestSHA256); !errors.Is(err, ErrMigrationTampered) {
		t.Fatalf("tampered migration error = %v", err)
	}

	assets = cloneMigrationAssets(t)
	assets[manifestPath].Data = append(append([]byte(nil), assets[manifestPath].Data...), ' ')
	if _, err := loadMigrationPlan(assets, embeddedManifestSHA256); !errors.Is(err, ErrMigrationTampered) {
		t.Fatalf("tampered manifest error = %v", err)
	}

	assets = cloneMigrationAssets(t)
	assets["migrations/untracked.sql"] = &fstest.MapFile{Data: []byte("SELECT 1;\n")}
	if _, err := loadMigrationPlan(assets, embeddedManifestSHA256); !errors.Is(err, ErrMigrationTampered) {
		t.Fatalf("untracked migration error = %v", err)
	}

	assets = cloneMigrationAssets(t)
	delete(assets, "migrations/0002_claims_conflicts_paths.sql")
	if _, err := loadMigrationPlan(assets, embeddedManifestSHA256); !errors.Is(err, ErrMigrationTampered) {
		t.Fatalf("missing migration error = %v", err)
	}
}

func TestMigrationManifestStructureFailsClosed(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]any)
	}{
		{"unknown field", func(document map[string]any) { document["unknown"] = true }},
		{"unsupported contract", func(document map[string]any) { document["schema_version"] = "2.0" }},
		{"invalid name", func(document map[string]any) {
			document["migrations"].([]any)[0].(map[string]any)["name"] = "Not Canonical"
		}},
		{"invalid digest", func(document map[string]any) {
			document["migrations"].([]any)[0].(map[string]any)["sha256"] = strings.Repeat("A", 64)
		}},
	}
	for _, fixture := range cases {
		t.Run(fixture.name, func(t *testing.T) {
			assets := cloneMigrationAssets(t)
			digest := mutateManifest(t, assets, fixture.mutate)
			if _, err := loadMigrationPlan(assets, digest); !errors.Is(err, ErrMigrationTampered) {
				t.Fatalf("loadMigrationPlan(%s) = %v", fixture.name, err)
			}
		})
	}

	assets := cloneMigrationAssets(t)
	assets["migrations/nested/unexpected.sql"] = &fstest.MapFile{Data: []byte("SELECT 1;\n")}
	if _, err := loadMigrationPlan(assets, embeddedManifestSHA256); !errors.Is(err, ErrMigrationTampered) {
		t.Fatalf("nested migration asset = %v", err)
	}
}

func TestRequireJSONEOFRejectsTrailingAndMalformedValues(t *testing.T) {
	for _, payload := range []string{"{} {}", "{} {"} {
		decoder := json.NewDecoder(strings.NewReader(payload))
		var first map[string]any
		if err := decoder.Decode(&first); err != nil {
			t.Fatal(err)
		}
		if err := requireJSONEOF(decoder); err == nil {
			t.Fatalf("requireJSONEOF(%q) succeeded", payload)
		}
	}
}

func TestMigrationBodyRequiresRunnerOwnedTransactionShape(t *testing.T) {
	valid := "PRAGMA foreign_keys = ON;\nBEGIN IMMEDIATE;\nSELECT 1;\nCOMMIT;\n"
	body, err := migrationBody(valid)
	if err != nil || body != "SELECT 1;\n" {
		t.Fatalf("migrationBody(valid) = %q, %v", body, err)
	}
	invalid := []string{
		"",
		"BEGIN IMMEDIATE;\nCOMMIT;",
		"SELECT 1;\n",
		"BEGIN IMMEDIATE;\nSELECT 1;\nBEGIN IMMEDIATE;\nCOMMIT;\n",
		"BEGIN IMMEDIATE;\nSELECT 1;\nCOMMIT;\nSELECT 2;\n",
		"BEGIN IMMEDIATE;\r\nSELECT 1;\r\nCOMMIT;\r\n",
	}
	for _, payload := range invalid {
		if _, err := migrationBody(payload); err == nil {
			t.Errorf("migrationBody(%q) succeeded", payload)
		}
	}
}

func TestEmptyV2UpgradePassesAndPopulatedV2FailsClosed(t *testing.T) {
	if err := CheckV2UpgradeEligibility(context.Background(), nil); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("CheckV2UpgradeEligibility(nil) = %v, want ErrInvalidOptions", err)
	}
	emptyPath := filepath.Join(privateTempDir(t), "empty-v2.db")
	empty := rawDatabaseAtVersion(t, emptyPath, 2)
	if err := CheckV2UpgradeEligibility(context.Background(), empty); err != nil {
		t.Fatalf("empty v2 eligibility = %v", err)
	}
	if err := empty.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(context.Background(), testOptions(emptyPath))
	if err != nil {
		t.Fatalf("Open(empty v2) error = %v", err)
	}
	_ = upgraded.Close()

	for _, fixture := range []struct {
		name   string
		insert string
	}{
		{
			"repository",
			`INSERT INTO repositories(repository_id, display_name, created_at)
             VALUES ('legacy', 'legacy', '2026-01-01T00:00:00Z')`,
		},
		{
			"fts-only",
			`INSERT INTO search_fts(
               snapshot_id, object_type, object_id, title,
               qualified_name, signature, body
             ) VALUES ('legacy', 'document', 'legacy', 'legacy', '', '', 'legacy')`,
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "populated-v2.db")
			database := rawDatabaseAtVersion(t, path, 2)
			if _, err := database.Exec(fixture.insert); err != nil {
				t.Fatal(err)
			}
			if err := CheckV2UpgradeEligibility(context.Background(), database); !errors.Is(err, ErrBackfillRequired) {
				t.Fatalf("populated eligibility error = %v, want ErrBackfillRequired", err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(context.Background(), testOptions(path)); !errors.Is(err, ErrBackfillRequired) {
				t.Fatalf("Open(populated v2) error = %v, want ErrBackfillRequired", err)
			}
		})
	}
}

func TestLegacyCatalogueRejectsSameNameAndTypeDDLDrift(t *testing.T) {
	for _, fixture := range []struct {
		name string
		sql  string
	}{
		{"table column", "ALTER TABLE jobs ADD COLUMN unauthorized TEXT"},
		{
			"index definition",
			`DROP INDEX idx_jobs_claim;
             CREATE INDEX idx_jobs_claim ON jobs(kind)`,
		},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			path := filepath.Join(privateTempDir(t), "ddl-drift.db")
			database := rawDatabaseAtVersion(t, path, 2)
			if _, err := database.Exec(fixture.sql); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(context.Background(), testOptions(path)); !errors.Is(err, ErrForeignDatabase) {
				t.Fatalf("Open(DDL drift) = %v, want ErrForeignDatabase", err)
			}
		})
	}
}

func TestGovernedLegacyCatalogueMatchesEmbeddedMigrations(t *testing.T) {
	plan, err := embeddedMigrationPlan()
	if err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 2; version++ {
		path := filepath.Join(privateTempDir(t), fmt.Sprintf("v%d.db", version))
		database := rawDatabaseAtVersion(t, path, version)
		if err := checkLegacyCatalog(context.Background(), database, version, plan); err != nil {
			t.Fatalf("v%d catalogue = %v", version, err)
		}
	}
}

func TestOwnedV3DDLDriftIsRejectedBeforeV4Migration(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "v3-drift.db")
	database := rawDatabaseAtVersion(t, path, 3)
	if _, err := database.Exec(
		`DROP TRIGGER canonical_snapshots_update_guard;
         CREATE TRIGGER canonical_snapshots_update_guard
         BEFORE UPDATE ON canonical_snapshots
         BEGIN
           SELECT RAISE(ABORT, 'drift');
         END;`,
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), testOptions(path)); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Open(drifted v3) = %v, want ErrIncompatibleSchema", err)
	}
	raw, err := sql.Open("sqlite", sqliteURI(testOptions(path)))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	version, err := readSchemaVersion(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("drifted database was mutated to version %d", version)
	}
	var journalRows int
	if err := raw.QueryRow("SELECT COUNT(*) FROM schema_migrations").Scan(&journalRows); err != nil {
		t.Fatal(err)
	}
	if journalRows != 3 {
		t.Fatalf("drifted database journal rows = %d, want 3", journalRows)
	}
}

func TestOwnedV3JournalDriftIsRejectedBeforeV4Migration(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "v3-journal-drift.db")
	database := rawDatabaseAtVersion(t, path, 3)
	if _, err := database.Exec(
		"UPDATE schema_migrations SET sha256 = ? WHERE version = 3",
		strings.Repeat("b", 64),
	); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), testOptions(path)); !errors.Is(err, ErrIncompatibleSchema) {
		t.Fatalf("Open(drifted v3 journal) = %v, want ErrIncompatibleSchema", err)
	}
	raw, err := sql.Open("sqlite", sqliteURI(testOptions(path)))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	version, err := readSchemaVersion(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("drifted database was mutated to version %d", version)
	}
	var v4Rows int
	if err := raw.QueryRow("SELECT COUNT(*) FROM schema_migrations WHERE version = 4").Scan(&v4Rows); err != nil {
		t.Fatal(err)
	}
	if v4Rows != 0 {
		t.Fatalf("drifted database gained %d v4 journal row(s)", v4Rows)
	}
}

func TestFailedMigrationRollsBackBodyAndJournal(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "rollback.db")
	database := rawDatabaseAtVersion(t, path, 2)
	plan, err := embeddedMigrationPlan()
	if err != nil {
		t.Fatal(err)
	}
	broken := plan[2]
	broken.Body += "THIS IS NOT SQL;\n"
	if err := applyMigration(context.Background(), database, path, broken); !errors.Is(err, ErrMigrationFailed) {
		t.Fatalf("apply broken migration error = %v, want ErrMigrationFailed", err)
	}
	version, err := readSchemaVersion(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("schema version after rollback = %d, want 2", version)
	}
	var journalExists int
	if err := database.QueryRow(
		"SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name = 'schema_migrations')",
	).Scan(&journalExists); err != nil {
		t.Fatal(err)
	}
	if journalExists != 0 {
		t.Fatal("schema_migrations survived failed v3 transaction")
	}
}

func TestV3SQLGuardAlsoRejectsPopulatedV2(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "sql-guard.db")
	database := rawDatabaseAtVersion(t, path, 2)
	if _, err := database.Exec(
		`INSERT INTO repositories(repository_id, display_name, created_at)
         VALUES ('legacy', 'legacy', '2026-01-01T00:00:00Z')`,
	); err != nil {
		t.Fatal(err)
	}
	plan, err := embeddedMigrationPlan()
	if err != nil {
		t.Fatal(err)
	}
	connection, err := database.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), plan[2].Body); err == nil {
		_, _ = connection.ExecContext(context.Background(), "ROLLBACK")
		t.Fatal("v3 SQL body accepted populated v2 database")
	}
	if _, err := connection.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	version, err := readSchemaVersion(context.Background(), database)
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("schema version after SQL guard = %d, want 2", version)
	}
}

func TestManifestDigestConstantMatchesEmbeddedBytes(t *testing.T) {
	payload, err := fs.ReadFile(sqliteassets.Migrations, manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	if observed := hex.EncodeToString(digest[:]); observed != embeddedManifestSHA256 {
		t.Fatalf("manifest digest = %s, want %s", observed, embeddedManifestSHA256)
	}
}
