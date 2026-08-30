package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/sourceorigin"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	sqliteassets "github.com/neuroforge-io/RKC/storage/sqlite"
)

const (
	embeddedManifestSHA256 = "1015fc01a1002875062e7468c137d4d573772542abcc461ac4995786b4dacd73"
	manifestPath           = "migrations/manifest.json"
	currentDatabaseVersion = 5
	currentSchemaVersion   = "0.5.0"
	applicationID          = 0x524B4344
)

var canonicalMigrationName = regexp.MustCompile(`^[a-z][a-z0-9]*(?:_[a-z0-9]+)*$`)

type manifestDocument struct {
	SchemaVersion         string              `json:"schema_version"`
	DatabaseSchemaVersion string              `json:"database_schema_version"`
	Migrations            []manifestMigration `json:"migrations"`
}

type manifestMigration struct {
	Version             int    `json:"version"`
	Name                string `json:"name"`
	TargetSchemaVersion string `json:"target_schema_version"`
	SHA256              string `json:"sha256"`
	MinimumRKC          string `json:"minimum_rkc"`
}

type migration struct {
	manifestMigration
	Filename string
	Body     string
}

var errSchemaCatalogDrift = errors.New("governed schema catalogue drift")

type schemaCatalogRow struct {
	Type      string `json:"type"`
	Name      string `json:"name"`
	TableName string `json:"table_name"`
	SQL       string `json:"sql"`
}

func embeddedMigrationPlan() ([]migration, error) {
	return loadMigrationPlan(sqliteassets.Migrations, embeddedManifestSHA256)
}

func loadMigrationPlan(assets fs.FS, expectedManifestDigest string) ([]migration, error) {
	manifestBytes, err := fs.ReadFile(assets, manifestPath)
	if err != nil {
		return nil, operationError("read migration manifest", "", ErrMigrationTampered, err)
	}
	manifestDigest := sha256.Sum256(manifestBytes)
	if hex.EncodeToString(manifestDigest[:]) != expectedManifestDigest {
		return nil, operationError(
			"verify migration manifest",
			"",
			ErrMigrationTampered,
			fmt.Errorf("digest mismatch"),
		)
	}

	decoder := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	decoder.DisallowUnknownFields()
	var document manifestDocument
	if err := decoder.Decode(&document); err != nil {
		return nil, operationError("decode migration manifest", "", ErrMigrationTampered, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, operationError("decode migration manifest", "", ErrMigrationTampered, err)
	}
	if document.SchemaVersion != "1.0" ||
		document.DatabaseSchemaVersion != currentSchemaVersion ||
		len(document.Migrations) != currentDatabaseVersion {
		return nil, operationError(
			"verify migration manifest",
			"",
			ErrMigrationTampered,
			fmt.Errorf("unsupported manifest contract"),
		)
	}

	entries, err := fs.ReadDir(assets, "migrations")
	if err != nil {
		return nil, operationError("list migration assets", "", ErrMigrationTampered, err)
	}
	expectedFiles := map[string]struct{}{"manifest.json": {}}
	plan := make([]migration, 0, len(document.Migrations))
	for index, entry := range document.Migrations {
		version := index + 1
		if entry.Version != version ||
			!canonicalMigrationName.MatchString(entry.Name) ||
			entry.TargetSchemaVersion != fmt.Sprintf("0.%d.0", version) ||
			entry.MinimumRKC == "" ||
			len(entry.SHA256) != sha256.Size*2 {
			return nil, operationError(
				"verify migration manifest",
				"",
				ErrMigrationTampered,
				fmt.Errorf("invalid migration entry %d", version),
			)
		}
		if _, err := hex.DecodeString(entry.SHA256); err != nil || entry.SHA256 != strings.ToLower(entry.SHA256) {
			return nil, operationError(
				"verify migration manifest",
				"",
				ErrMigrationTampered,
				fmt.Errorf("invalid migration digest %d", version),
			)
		}

		filename := fmt.Sprintf("%04d_%s.sql", version, entry.Name)
		expectedFiles[filename] = struct{}{}
		payload, err := fs.ReadFile(assets, "migrations/"+filename)
		if err != nil {
			return nil, operationError("read migration", filename, ErrMigrationTampered, err)
		}
		digest := sha256.Sum256(payload)
		if hex.EncodeToString(digest[:]) != entry.SHA256 {
			return nil, operationError(
				"verify migration",
				filename,
				ErrMigrationTampered,
				fmt.Errorf("digest mismatch"),
			)
		}
		body, err := migrationBody(string(payload))
		if err != nil {
			return nil, operationError("parse migration", filename, ErrMigrationTampered, err)
		}
		plan = append(plan, migration{
			manifestMigration: entry,
			Filename:          filename,
			Body:              body,
		})
	}

	observedFiles := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, operationError(
				"verify migration assets",
				entry.Name(),
				ErrMigrationTampered,
				fmt.Errorf("unexpected directory"),
			)
		}
		observedFiles = append(observedFiles, entry.Name())
		if _, ok := expectedFiles[entry.Name()]; !ok {
			return nil, operationError(
				"verify migration assets",
				entry.Name(),
				ErrMigrationTampered,
				fmt.Errorf("unexpected file"),
			)
		}
	}
	if len(observedFiles) != len(expectedFiles) {
		sort.Strings(observedFiles)
		return nil, operationError(
			"verify migration assets",
			"",
			ErrMigrationTampered,
			fmt.Errorf("asset set mismatch: %v", observedFiles),
		)
	}
	return plan, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra json.RawMessage
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("trailing JSON value")
	}
	return err
}

func migrationBody(payload string) (string, error) {
	if payload == "" || !strings.HasSuffix(payload, "\n") || strings.Contains(payload, "\r") {
		return "", fmt.Errorf("migration is not canonical UTF-8/LF text")
	}
	lines := strings.Split(payload, "\n")
	begin := -1
	commit := -1
	for index, line := range lines {
		switch strings.TrimSpace(line) {
		case "BEGIN IMMEDIATE;":
			if begin != -1 {
				return "", fmt.Errorf("multiple BEGIN IMMEDIATE statements")
			}
			begin = index
		case "COMMIT;":
			if commit != -1 {
				return "", fmt.Errorf("multiple COMMIT statements")
			}
			commit = index
		}
	}
	if begin == -1 || commit <= begin {
		return "", fmt.Errorf("migration does not have one runner-replaceable transaction")
	}
	for _, line := range lines[commit+1:] {
		if strings.TrimSpace(line) != "" {
			return "", fmt.Errorf("content follows COMMIT")
		}
	}
	body := strings.Join(lines[begin+1:commit], "\n") + "\n"
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("empty migration body")
	}
	return body, nil
}

type queryExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func migrate(ctx context.Context, database *sql.DB, path string, plan []migration) error {
	for _, item := range plan {
		if err := applyMigration(ctx, database, path, item); err != nil {
			return err
		}
	}
	return nil
}

func applyMigration(ctx context.Context, database *sql.DB, path string, item migration) (resultErr error) {
	connection, err := database.Conn(ctx)
	if err != nil {
		return operationError("acquire migration connection", path, classifyMigrationError(err), err)
	}
	defer connection.Close()

	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return operationError("begin migration", path, classifyMigrationError(err), err)
	}
	defer func() {
		if resultErr != nil {
			_, _ = connection.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	version, err := readSchemaVersion(ctx, connection)
	if err != nil {
		return operationError("read schema version", path, classifyMigrationError(err), err)
	}
	if version >= item.Version {
		if _, err := connection.ExecContext(ctx, "COMMIT"); err != nil {
			return operationError("commit migration check", path, classifyMigrationError(err), err)
		}
		return nil
	}
	if version != item.Version-1 {
		return operationError(
			"plan migration",
			path,
			ErrIncompatibleSchema,
			fmt.Errorf("schema version %d cannot apply migration %d", version, item.Version),
		)
	}
	if item.Version == 3 {
		if err := checkV2UpgradeEligibility(ctx, connection); err != nil {
			return operationError("check v0.2 upgrade eligibility", path, classifyMigrationError(err), err)
		}
	}
	if item.Version == 5 {
		if err := checkV4RepositoryAffinityEligibility(ctx, connection); err != nil {
			return operationError(
				"check v0.4 repository affinity upgrade eligibility",
				path,
				classifyMigrationError(err),
				err,
			)
		}
	}
	if _, err := connection.ExecContext(ctx, item.Body); err != nil {
		return operationError("execute migration", item.Filename, classifyMigrationError(err), err)
	}

	observed, err := readSchemaVersion(ctx, connection)
	if err != nil || observed != item.Version {
		if err == nil {
			err = fmt.Errorf("migration recorded schema version %d, want %d", observed, item.Version)
		}
		return operationError("verify migration", item.Filename, classifyMigrationError(err), err)
	}
	if item.Version >= 3 {
		if _, err := connection.ExecContext(
			ctx,
			`INSERT INTO schema_migrations(
               version, name, target_schema_version, sha256, applied_at
             ) VALUES (?, ?, ?, ?, ?)`,
			item.Version,
			item.Name,
			item.TargetSchemaVersion,
			item.SHA256,
			time.Now().UTC().Format(time.RFC3339Nano),
		); err != nil {
			return operationError("record migration", item.Filename, classifyMigrationError(err), err)
		}
		if _, err := connection.ExecContext(
			ctx,
			fmt.Sprintf("PRAGMA application_id = %d", applicationID),
		); err != nil {
			return operationError("set application id", path, classifyMigrationError(err), err)
		}
		if _, err := connection.ExecContext(
			ctx,
			fmt.Sprintf("PRAGMA user_version = %d", item.Version),
		); err != nil {
			return operationError("set user version", path, classifyMigrationError(err), err)
		}
	}
	if _, err := connection.ExecContext(ctx, "COMMIT"); err != nil {
		return operationError("commit migration", item.Filename, classifyMigrationError(err), err)
	}
	return nil
}

func classifyMigrationError(err error) error {
	if errors.Is(err, ErrBackfillRequired) {
		return ErrBackfillRequired
	}
	if errors.Is(err, ErrIncompatibleSchema) {
		return ErrIncompatibleSchema
	}
	classified := classifyDatabaseError(err)
	if classified != ErrCheckFailed {
		return classified
	}
	return ErrMigrationFailed
}

func readSchemaVersion(ctx context.Context, executor queryExecutor) (int, error) {
	var exists int
	if err := executor.QueryRowContext(
		ctx,
		`SELECT EXISTS(
           SELECT 1 FROM sqlite_schema
           WHERE type = 'table' AND name = 'schema_meta'
         )`,
	).Scan(&exists); err != nil {
		return 0, err
	}
	if exists == 0 {
		return 0, nil
	}
	var version string
	if err := executor.QueryRowContext(
		ctx,
		"SELECT value FROM schema_meta WHERE key = 'schema_version'",
	).Scan(&version); err != nil {
		return 0, err
	}
	for number := 1; number <= currentDatabaseVersion; number++ {
		if version == fmt.Sprintf("0.%d.0", number) {
			return number, nil
		}
	}
	return 0, operationError(
		"read schema version",
		"",
		ErrIncompatibleSchema,
		fmt.Errorf("unsupported schema version %q", version),
	)
}

func checkLegacyCatalog(
	ctx context.Context,
	database *sql.DB,
	version int,
	plan []migration,
) error {
	return checkGovernedSchemaCatalog(ctx, database, version, plan)
}

func checkGovernedSchemaCatalog(
	ctx context.Context,
	executor queryExecutor,
	version int,
	plan []migration,
) error {
	observed, err := schemaCatalogFingerprint(ctx, executor)
	if err != nil {
		return err
	}
	expected, err := governedSchemaCatalogFingerprint(ctx, version, plan)
	if err != nil {
		return err
	}
	if observed != expected {
		return fmt.Errorf(
			"%w: observed %s, governed %s",
			errSchemaCatalogDrift,
			observed,
			expected,
		)
	}
	return nil
}

func governedSchemaCatalogFingerprint(
	ctx context.Context,
	version int,
	plan []migration,
) (string, error) {
	if version < 1 || len(plan) < version {
		return "", operationError(
			"derive governed schema catalogue",
			"",
			ErrMigrationTampered,
			fmt.Errorf("unsupported legacy version %d", version),
		)
	}
	reference, err := sql.Open("sqlite", ":memory:?_dqs=0&_error_rc=1")
	if err != nil {
		return "", err
	}
	reference.SetMaxOpenConns(1)
	reference.SetMaxIdleConns(1)
	defer reference.Close()
	for index := 0; index < version; index++ {
		if err := applyMigration(ctx, reference, ":memory:", plan[index]); err != nil {
			return "", err
		}
	}
	return schemaCatalogFingerprint(ctx, reference)
}

func schemaCatalogFingerprint(ctx context.Context, executor queryExecutor) (string, error) {
	rows, err := executor.QueryContext(
		ctx,
		`SELECT type, name, tbl_name, COALESCE(sql, '')
         FROM sqlite_schema
         WHERE name NOT LIKE 'sqlite_%'
         ORDER BY type, name, tbl_name, sql`,
	)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	catalog := make([]schemaCatalogRow, 0, 64)
	for rows.Next() {
		var row schemaCatalogRow
		if err := rows.Scan(&row.Type, &row.Name, &row.TableName, &row.SQL); err != nil {
			return "", err
		}
		catalog = append(catalog, row)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(catalog)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

var legacyV2ContentTables = [...]string{
	"repositories",
	"snapshots",
	"artifacts",
	"logical_entities",
	"nodes",
	"evidence",
	"node_evidence",
	"edges",
	"edge_evidence",
	"documents",
	"document_sections",
	"section_evidence",
	"chunks",
	"search_fts",
	"embeddings",
	"diagnostics",
	"tool_runs",
	"jobs",
	"cache_entries",
	"audit_events",
	"conflicts",
	"claims",
	"execution_paths",
	"coverage_records",
}

// CheckV2UpgradeEligibility applies the explicit runtime policy for upgrading a
// legacy v0.2 database. Version 0.2 did not retain the lossless canonical
// bundle, so any runtime/content row requires a future deterministic backfill.
func CheckV2UpgradeEligibility(ctx context.Context, database *sql.DB) error {
	if database == nil {
		return operationError(
			"check v0.2 upgrade eligibility",
			"",
			ErrInvalidOptions,
			fmt.Errorf("nil database"),
		)
	}
	connection, err := database.Conn(ctx)
	if err != nil {
		return operationError(
			"check v0.2 upgrade eligibility",
			"",
			classifyDatabaseError(err),
			err,
		)
	}
	defer connection.Close()
	return checkV2UpgradeEligibility(ctx, connection)
}

func checkV2UpgradeEligibility(ctx context.Context, executor queryExecutor) error {
	version, err := readSchemaVersion(ctx, executor)
	if err != nil {
		return err
	}
	if version != 2 {
		return operationError(
			"check v0.2 upgrade eligibility",
			"",
			ErrIncompatibleSchema,
			fmt.Errorf("schema version is %d, want 2", version),
		)
	}

	populated := make([]string, 0)
	for _, table := range legacyV2ContentTables {
		var exists int
		if err := executor.QueryRowContext(
			ctx,
			"SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name = ? AND type IN ('table', 'view'))",
			table,
		).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return operationError(
				"check v0.2 upgrade eligibility",
				"",
				ErrIncompatibleSchema,
				fmt.Errorf("legacy table %s is missing", table),
			)
		}
		var hasRows int
		query := fmt.Sprintf("SELECT EXISTS(SELECT 1 FROM %q)", table)
		if err := executor.QueryRowContext(ctx, query).Scan(&hasRows); err != nil {
			return err
		}
		if hasRows != 0 {
			populated = append(populated, table)
		}
	}
	if len(populated) != 0 {
		return operationError(
			"check v0.2 upgrade eligibility",
			"",
			ErrBackfillRequired,
			fmt.Errorf("populated legacy tables: %s", strings.Join(populated, ", ")),
		)
	}
	return nil
}

// checkV4RepositoryAffinityEligibility prevents migration 5 from retaining a
// credential-bearing/noncanonical origin or provenance that disagrees with its
// public opaque RepositoryID. Errors never contain stored provenance text.
func checkV4RepositoryAffinityEligibility(ctx context.Context, executor queryExecutor) error {
	version, err := readSchemaVersion(ctx, executor)
	if err != nil {
		return err
	}
	if version != 4 {
		return operationError(
			"check v0.4 repository affinity upgrade eligibility",
			"",
			ErrIncompatibleSchema,
			fmt.Errorf("schema version is %d, want 4", version),
		)
	}

	repositories, err := executor.QueryContext(
		ctx,
		`SELECT repository_id, canonical_origin
		 FROM repositories
		 WHERE canonical_origin IS NOT NULL`,
	)
	if err != nil {
		return err
	}
	for repositories.Next() {
		var repositoryID, legacyValue string
		if err := repositories.Scan(&repositoryID, &legacyValue); err != nil {
			_ = repositories.Close()
			return err
		}
		if legacyValue != "" && legacyValue != repositoryID &&
			(!sourceorigin.IsCanonical(legacyValue) ||
				rkcmodel.StableID("repository", legacyValue) != repositoryID) {
			_ = repositories.Close()
			return repositoryAffinityBackfillRequired()
		}
	}
	if err := repositories.Err(); err != nil {
		_ = repositories.Close()
		return err
	}
	if err := repositories.Close(); err != nil {
		return err
	}

	snapshots, err := executor.QueryContext(
		ctx,
		`SELECT snapshot_id, repository_id, schema_version,
		        canonical_snapshot_json
		 FROM canonical_snapshots
		 ORDER BY snapshot_id`,
	)
	if err != nil {
		return err
	}
	defer snapshots.Close()
	for snapshots.Next() {
		var rowID, repositoryID, schemaVersion, payload string
		if err := snapshots.Scan(
			&rowID,
			&repositoryID,
			&schemaVersion,
			&payload,
		); err != nil {
			return err
		}
		if len(payload) > readerMaxObjectJSONBytes {
			return repositoryAffinityBackfillRequired()
		}
		snapshot, err := readerDecodeObject[rkcmodel.Snapshot](
			"check v0.4 repository affinity upgrade eligibility",
			"",
			"canonical_snapshot_json",
			payload,
			readerMaxObjectJSONBytes,
		)
		if err != nil {
			return repositoryAffinityBackfillRequired()
		}
		if snapshot.ID != rowID || snapshot.RepositoryID != repositoryID ||
			snapshot.SchemaVersion != schemaVersion || snapshot.Status != "committed" {
			return repositoryAffinityBackfillRequired()
		}
		validationBundle := rkcmodel.Bundle{Snapshot: snapshot}
		if snapshot.Git.Origin != "" {
			validationBundle.Nodes = []rkcmodel.Node{{
				ID:            snapshot.RepositoryID,
				LogicalID:     snapshot.RepositoryID,
				Kind:          "repository",
				Name:          "Repository",
				QualifiedName: snapshot.Git.Origin,
				Attributes:    map[string]any{"git_origin": snapshot.Git.Origin},
			}}
		}
		if report := rkcmodel.ValidateBundle(
			validationBundle,
			rkcmodel.ValidationOptions{StrictVocabulary: true},
		); report.HasErrors() {
			return repositoryAffinityBackfillRequired()
		}
		if snapshot.Git.Origin != "" &&
			(!sourceorigin.IsCanonical(snapshot.Git.Origin) ||
				rkcmodel.StableID("repository", snapshot.Git.Origin) != snapshot.RepositoryID) {
			return repositoryAffinityBackfillRequired()
		}
	}
	return snapshots.Err()
}

func repositoryAffinityBackfillRequired() error {
	return operationError(
		"check v0.4 repository affinity upgrade eligibility",
		"",
		ErrBackfillRequired,
		errors.New("stored repository provenance requires an explicit affinity backfill"),
	)
}
