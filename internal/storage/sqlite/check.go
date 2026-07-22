package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var errIntegrityResult = errors.New("SQLite integrity_check reported corruption")

var requiredSchemaObjects = [...]struct {
	objectType string
	name       string
}{
	{"table", "schema_meta"},
	{"table", "repositories"},
	{"table", "snapshots"},
	{"table", "artifacts"},
	{"table", "logical_entities"},
	{"table", "nodes"},
	{"table", "evidence"},
	{"table", "node_evidence"},
	{"table", "edges"},
	{"table", "edge_evidence"},
	{"table", "documents"},
	{"table", "document_sections"},
	{"table", "section_evidence"},
	{"table", "chunks"},
	{"table", "search_fts"},
	{"table", "embeddings"},
	{"table", "diagnostics"},
	{"table", "tool_runs"},
	{"table", "jobs"},
	{"table", "cache_entries"},
	{"table", "audit_events"},
	{"table", "conflicts"},
	{"table", "claims"},
	{"table", "execution_paths"},
	{"table", "coverage_records"},
	{"table", "schema_migrations"},
	{"table", "builds"},
	{"table", "staged_canonical_records"},
	{"table", "canonical_snapshots"},
	{"table", "canonical_snapshot_records"},
	{"index", "idx_builds_recovery"},
	{"index", "idx_builds_repository_state"},
	{"index", "idx_canonical_snapshots_repository_published"},
	{"trigger", "builds_close_staging_guard"},
	{"trigger", "builds_canonical_snapshot_lineage_update_guard"},
	{"trigger", "builds_closed_delete_guard"},
	{"trigger", "builds_closed_update_guard"},
	{"trigger", "builds_commit_compare_and_swap_guard"},
	{"trigger", "builds_commit_snapshot_guard"},
	{"trigger", "builds_initial_state_guard"},
	{"trigger", "builds_state_transition_guard"},
	{"trigger", "canonical_snapshot_records_delete_guard"},
	{"trigger", "canonical_snapshot_records_insert_guard"},
	{"trigger", "canonical_snapshot_records_update_guard"},
	{"trigger", "canonical_snapshots_build_open_insert_guard"},
	{"trigger", "canonical_snapshots_build_lineage_insert_guard"},
	{"trigger", "canonical_snapshots_delete_guard"},
	{"trigger", "canonical_snapshots_update_guard"},
	{"trigger", "repositories_current_snapshot_committed_guard"},
	{"trigger", "repositories_current_snapshot_compare_and_swap_guard"},
	{"trigger", "repositories_current_snapshot_clear_guard"},
	{"trigger", "repositories_current_snapshot_insert_guard"},
	{"trigger", "repositories_current_snapshot_repository_guard"},
	{"trigger", "staged_canonical_records_delete_guard"},
	{"trigger", "staged_canonical_records_insert_guard"},
	{"trigger", "staged_canonical_records_update_guard"},
}

// Check validates ownership, versions, required schema objects, immutable
// migration history, SQLite integrity, and all foreign keys.
func (d *Database) Check(ctx context.Context) error {
	return d.check(ctx, true)
}

// check performs the fixed-size structural checks required on every open. The
// public full check additionally scans data pages and foreign keys; callers run
// that potentially long maintenance operation explicitly under their own
// context and resource policy.
func (d *Database) check(ctx context.Context, full bool) error {
	if d == nil {
		return operationError("check", "", ErrClosed, nil)
	}
	d.lifecycle.RLock()
	defer d.lifecycle.RUnlock()
	if err := d.requireOpen("check"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return operationError("check", d.path, ErrCanceled, err)
	}
	if d.binding != nil {
		if err := d.binding.verifyFilesystem(); err != nil {
			return err
		}
	}
	plan, err := embeddedMigrationPlan()
	if err != nil {
		return err
	}
	connection, err := d.db.Conn(ctx)
	if err != nil {
		return operationError("check", d.path, classifyDatabaseError(err), err)
	}
	defer connection.Close()
	if d.binding != nil {
		if err := d.binding.verify(ctx, connection); err != nil {
			return err
		}
	}

	if err := checkConnectionPolicy(ctx, connection, d.options); err != nil {
		return d.checkError("connection policy", err)
	}
	var appID int64
	if err := connection.QueryRowContext(ctx, "PRAGMA application_id").Scan(&appID); err != nil {
		return d.checkError("application id", err)
	}
	if appID != applicationID {
		return operationError(
			"check application id",
			d.path,
			ErrForeignDatabase,
			fmt.Errorf("got %#x, want %#x", appID, applicationID),
		)
	}
	var userVersion int
	if err := connection.QueryRowContext(ctx, "PRAGMA user_version").Scan(&userVersion); err != nil {
		return d.checkError("user version", err)
	}
	if userVersion != currentDatabaseVersion {
		return operationError(
			"check user version",
			d.path,
			ErrIncompatibleSchema,
			fmt.Errorf("got %d, want %d", userVersion, currentDatabaseVersion),
		)
	}
	var schemaVersion string
	if err := connection.QueryRowContext(
		ctx,
		"SELECT value FROM schema_meta WHERE key = 'schema_version'",
	).Scan(&schemaVersion); err != nil {
		return d.checkError("schema version", err)
	}
	if schemaVersion != currentSchemaVersion {
		return operationError(
			"check schema version",
			d.path,
			ErrIncompatibleSchema,
			fmt.Errorf("got %q, want %q", schemaVersion, currentSchemaVersion),
		)
	}
	if err := checkRequiredSchema(ctx, connection); err != nil {
		return d.checkError("schema", err)
	}
	if err := checkGovernedSchemaCatalog(ctx, connection, currentDatabaseVersion, plan); err != nil {
		return d.checkError("schema catalogue", err)
	}
	if err := checkMigrationJournal(ctx, connection, plan); err != nil {
		return d.checkError("migration journal", err)
	}
	if full {
		if err := checkIntegrity(ctx, connection); err != nil {
			return d.checkError("integrity", err)
		}
		if err := checkForeignKeys(ctx, connection); err != nil {
			return d.checkError("foreign keys", err)
		}
	}
	if d.binding != nil {
		if err := d.binding.verifyFilesystem(); err != nil {
			return err
		}
	}
	return nil
}

func (d *Database) checkError(subject string, cause error) error {
	kind := classifyDatabaseError(cause)
	switch {
	case errors.Is(cause, errIntegrityResult):
		kind = ErrCorruptDatabase
	case errors.Is(cause, errSchemaCatalogDrift):
		kind = ErrIncompatibleSchema
	case errors.Is(cause, ErrMigrationTampered):
		kind = ErrMigrationTampered
	case errors.Is(cause, ErrIncompatibleSchema):
		kind = ErrIncompatibleSchema
	}
	return operationError("check "+subject, d.path, kind, cause)
}

func checkConnectionPolicy(ctx context.Context, connection *sql.Conn, options Options) error {
	checks := []struct {
		pragma string
		want   int64
	}{
		{"foreign_keys", 1},
		{"busy_timeout", options.BusyTimeout.Milliseconds()},
		{"trusted_schema", 0},
		{"temp_store", 2},
		{"cell_size_check", 1},
		{"cache_size", int64(-options.CacheKiB)},
		{"mmap_size", options.MMapBytes},
	}
	synchronous := int64(1)
	if options.Synchronous == "FULL" {
		synchronous = 2
	}
	checks = append(checks, struct {
		pragma string
		want   int64
	}{"synchronous", synchronous})
	for _, check := range checks {
		var observed int64
		if err := connection.QueryRowContext(ctx, "PRAGMA "+check.pragma).Scan(&observed); err != nil {
			return err
		}
		if observed != check.want {
			return fmt.Errorf("PRAGMA %s is %d, want %d", check.pragma, observed, check.want)
		}
	}
	var journalMode string
	if err := connection.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journalMode); err != nil {
		return err
	}
	if !strings.EqualFold(journalMode, "wal") {
		return fmt.Errorf("PRAGMA journal_mode is %q, want WAL", journalMode)
	}
	return nil
}

func checkRequiredSchema(ctx context.Context, connection *sql.Conn) error {
	for _, object := range requiredSchemaObjects {
		var exists int
		if err := connection.QueryRowContext(
			ctx,
			"SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type = ? AND name = ? AND sql IS NOT NULL)",
			object.objectType,
			object.name,
		).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return fmt.Errorf("required %s %s is missing", object.objectType, object.name)
		}
	}
	return nil
}

func checkMigrationJournal(ctx context.Context, connection queryExecutor, plan []migration) error {
	rows, err := connection.QueryContext(
		ctx,
		`SELECT version, name, target_schema_version, sha256, applied_at
         FROM schema_migrations ORDER BY version`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	index := 0
	for rows.Next() {
		if index >= len(plan) {
			return fmt.Errorf("migration journal contains an unexpected row")
		}
		var (
			version   int
			name      string
			target    string
			digest    string
			appliedAt sql.NullString
		)
		if err := rows.Scan(&version, &name, &target, &digest, &appliedAt); err != nil {
			return err
		}
		expected := plan[index]
		if version != expected.Version || name != expected.Name ||
			target != expected.TargetSchemaVersion || digest != expected.SHA256 {
			return fmt.Errorf("migration journal row %d drifted", index+1)
		}
		if version < 3 {
			if appliedAt.Valid {
				return fmt.Errorf("legacy migration %d has a fabricated application time", version)
			}
		} else {
			if !appliedAt.Valid || appliedAt.String == "" {
				return fmt.Errorf("migration %d has no application time", version)
			}
			if _, err := time.Parse(time.RFC3339Nano, appliedAt.String); err != nil {
				return fmt.Errorf("migration %d application time: %w", version, err)
			}
		}
		index++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if index != len(plan) {
		return fmt.Errorf("migration journal has %d rows, want %d", index, len(plan))
	}
	return nil
}

func checkIntegrity(ctx context.Context, connection *sql.Conn) error {
	rows, err := connection.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	results := make([]string, 0, 1)
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return err
		}
		results = append(results, result)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(results) != 1 || results[0] != "ok" {
		return fmt.Errorf("%w: returned %q", errIntegrityResult, results)
	}
	return nil
}

func checkForeignKeys(ctx context.Context, connection *sql.Conn) error {
	rows, err := connection.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowID sql.NullInt64
		var foreignKey int64
		if err := rows.Scan(&table, &rowID, &parent, &foreignKey); err != nil {
			return err
		}
		return fmt.Errorf(
			"foreign key violation table=%s rowid=%v parent=%s constraint=%d",
			table,
			rowID,
			parent,
			foreignKey,
		)
	}
	return rows.Err()
}
