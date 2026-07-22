package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFreshDatabaseIdentityAndJournal(t *testing.T) {
	database := openTestDatabase(t, filepath.Join(privateTempDir(t), "identity.db"))
	var appID, userVersion int
	if err := database.db.QueryRow("PRAGMA application_id").Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if err := database.db.QueryRow("PRAGMA user_version").Scan(&userVersion); err != nil {
		t.Fatal(err)
	}
	if appID != applicationID || userVersion != currentDatabaseVersion {
		t.Fatalf("identity = (%#x, %d)", appID, userVersion)
	}
	var rows, runtimeTimes int
	if err := database.db.QueryRow(
		`SELECT COUNT(*), SUM(CASE WHEN applied_at IS NOT NULL THEN 1 ELSE 0 END)
         FROM schema_migrations`,
	).Scan(&rows, &runtimeTimes); err != nil {
		t.Fatal(err)
	}
	if rows != currentDatabaseVersion || runtimeTimes != currentDatabaseVersion-2 {
		t.Fatalf("journal counts = rows:%d runtime-times:%d", rows, runtimeTimes)
	}
}

func TestCheckRejectsIdentitySchemaAndJournalDrift(t *testing.T) {
	t.Run("application id", func(t *testing.T) {
		database := openTestDatabase(t, filepath.Join(privateTempDir(t), "app-id.db"))
		if _, err := database.db.Exec("PRAGMA application_id = 1234"); err != nil {
			t.Fatal(err)
		}
		if err := database.Check(context.Background()); !errors.Is(err, ErrForeignDatabase) {
			t.Fatalf("Check(app id drift) = %v, want ErrForeignDatabase", err)
		}
	})

	t.Run("user version", func(t *testing.T) {
		database := openTestDatabase(t, filepath.Join(privateTempDir(t), "user-version.db"))
		if _, err := database.db.Exec("PRAGMA user_version = 2"); err != nil {
			t.Fatal(err)
		}
		if err := database.Check(context.Background()); !errors.Is(err, ErrIncompatibleSchema) {
			t.Fatalf("Check(user version drift) = %v, want ErrIncompatibleSchema", err)
		}
	})

	t.Run("required trigger", func(t *testing.T) {
		database := openTestDatabase(t, filepath.Join(privateTempDir(t), "trigger.db"))
		if _, err := database.db.Exec("DROP TRIGGER canonical_snapshots_update_guard"); err != nil {
			t.Fatal(err)
		}
		if err := database.Check(context.Background()); !errors.Is(err, ErrCheckFailed) {
			t.Fatalf("Check(trigger drift) = %v, want ErrCheckFailed", err)
		}
	})

	t.Run("same-name trigger DDL", func(t *testing.T) {
		database := openTestDatabase(t, filepath.Join(privateTempDir(t), "trigger-ddl.db"))
		if _, err := database.db.Exec(
			`DROP TRIGGER canonical_snapshots_update_guard;
             CREATE TRIGGER canonical_snapshots_update_guard
             BEFORE UPDATE ON canonical_snapshots
             BEGIN
               SELECT RAISE(ABORT, 'unauthorized replacement');
             END;`,
		); err != nil {
			t.Fatal(err)
		}
		if err := database.Check(context.Background()); !errors.Is(err, ErrIncompatibleSchema) {
			t.Fatalf("Check(same-name trigger drift) = %v, want ErrIncompatibleSchema", err)
		}
	})

	t.Run("required index", func(t *testing.T) {
		database := openTestDatabase(t, filepath.Join(privateTempDir(t), "index.db"))
		if _, err := database.db.Exec("DROP INDEX idx_builds_recovery"); err != nil {
			t.Fatal(err)
		}
		if err := database.Check(context.Background()); !errors.Is(err, ErrCheckFailed) {
			t.Fatalf("Check(index drift) = %v, want ErrCheckFailed", err)
		}
	})

	t.Run("journal", func(t *testing.T) {
		database := openTestDatabase(t, filepath.Join(privateTempDir(t), "journal.db"))
		if _, err := database.db.Exec("UPDATE schema_migrations SET sha256 = ? WHERE version = 3", strings.Repeat("b", 64)); err != nil {
			t.Fatal(err)
		}
		if err := database.Check(context.Background()); !errors.Is(err, ErrCheckFailed) {
			t.Fatalf("Check(journal drift) = %v, want ErrCheckFailed", err)
		}
	})

	journalCases := []struct {
		name string
		sql  string
	}{
		{
			"legacy fabricated time",
			"PRAGMA ignore_check_constraints = ON; UPDATE schema_migrations SET applied_at = '2026-01-01T00:00:00Z' WHERE version = 1",
		},
		{
			"runtime missing time",
			"PRAGMA ignore_check_constraints = ON; UPDATE schema_migrations SET applied_at = NULL WHERE version = 4",
		},
		{
			"runtime invalid time",
			"UPDATE schema_migrations SET applied_at = 'not-a-time' WHERE version = 4",
		},
		{"missing row", "DELETE FROM schema_migrations WHERE version = 4"},
		{
			"unexpected row",
			`INSERT INTO schema_migrations(version, name, target_schema_version, sha256, applied_at)
			 VALUES (5, 'unexpected', '0.5.0', '` + strings.Repeat("a", 64) + `', '2026-01-01T00:00:00Z')`,
		},
	}
	for _, fixture := range journalCases {
		t.Run("journal "+fixture.name, func(t *testing.T) {
			database := openTestDatabase(t, filepath.Join(privateTempDir(t), "journal-shape.db"))
			if _, err := database.db.Exec(fixture.sql); err != nil {
				t.Fatal(err)
			}
			if err := database.Check(context.Background()); !errors.Is(err, ErrCheckFailed) {
				t.Fatalf("Check(%s) = %v, want ErrCheckFailed", fixture.name, err)
			}
		})
	}
}

func TestCheckFindsWithoutRowIDForeignKeyViolation(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "foreign-key.db")
	database := openTestDatabase(t, path)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: path}).String()+"?_pragma=foreign_keys(OFF)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(
		`INSERT INTO artifacts(
           snapshot_id, artifact_id, path, kind, is_text, status
         ) VALUES ('missing', 'artifact', 'missing.go', 'source', 1, 'indexed')`,
	)
	if err != nil {
		_ = raw.Close()
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), testOptions(path))
	if err != nil {
		t.Fatalf("Open(foreign-key violation) = %v", err)
	}
	defer reopened.Close()
	if err := reopened.Check(context.Background()); !errors.Is(err, ErrCheckFailed) {
		t.Fatalf("Check(foreign-key violation) = %v, want ErrCheckFailed", err)
	}
}

func TestBusyClassificationUsesSQLiteResultCode(t *testing.T) {
	options := testOptions(filepath.Join(privateTempDir(t), "busy.db"))
	options.BusyTimeout = time.Millisecond
	database, err := Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	first, err := database.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := database.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if _, err := first.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer first.ExecContext(context.Background(), "ROLLBACK")
	if _, err := second.ExecContext(context.Background(), "BEGIN IMMEDIATE"); err == nil {
		_, _ = second.ExecContext(context.Background(), "ROLLBACK")
		t.Fatal("second BEGIN IMMEDIATE succeeded")
	} else if kind := classifyDatabaseError(err); kind != ErrBusy {
		t.Fatalf("busy classification = %v for %v", kind, err)
	}
}

func TestTypedErrorMatchesKindAndCause(t *testing.T) {
	cause := context.Canceled
	err := operationError("fixture", "/tmp/fixture.db", ErrCanceled, cause)
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("typed error does not unwrap: %v", err)
	}
	var typed *Error
	if !errors.As(err, &typed) || typed.Op != "fixture" {
		t.Fatalf("typed error = %#v", typed)
	}
	if (&Error{}).Error() == "" || (*Error)(nil).Error() != "<nil>" {
		t.Fatal("Error string contract failed")
	}
	if got := (&Error{Op: "kind", Path: "/tmp/db", Kind: ErrBusy}).Error(); !strings.Contains(got, "database busy") {
		t.Fatalf("kind-only Error() = %q", got)
	}
	if got := (&Error{Op: "bare"}).Error(); got != "sqlite: bare" {
		t.Fatalf("bare Error() = %q", got)
	}
	if unwrapped := (*Error)(nil).Unwrap(); unwrapped != nil {
		t.Fatalf("nil Error.Unwrap() = %v", unwrapped)
	}
}

func TestIntegrityResultClassifiesAsCorruption(t *testing.T) {
	database := &Database{path: "/tmp/fixture.db"}
	err := database.checkError(
		"integrity",
		fmt.Errorf("%w: page 2 is malformed", errIntegrityResult),
	)
	if !errors.Is(err, ErrCorruptDatabase) {
		t.Fatalf("integrity result classification = %v", err)
	}
}
