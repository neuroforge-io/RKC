package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

type ownershipMigrationInterleave struct {
	queryExecutor
	commit func()
	reads  int
}

func (view *ownershipMigrationInterleave) QueryRowContext(ctx context.Context, query string, arguments ...any) *sql.Row {
	if query == "PRAGMA user_version" && view.reads == 0 {
		// application_id has already been read and scanned by the preflight.
		// Commit the other connection's migration before the next version read.
		view.reads++
		view.commit()
	}
	return view.queryExecutor.QueryRowContext(ctx, query, arguments...)
}

func TestOwnershipPreflightKeepsOneSnapshotAcrossCommittedMigration(t *testing.T) {
	for _, readOnly := range []bool{false, true} {
		name := "writable"
		if readOnly {
			name = "read-only"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(privateTempDir(t), "ownership-interleave.db")
			writer := rawDatabaseAtVersion(t, path, 3)
			plan, err := embeddedMigrationPlan()
			if err != nil {
				t.Fatal(err)
			}
			options := testOptions(path)
			options.ReadOnly = readOnly
			reader, err := sql.Open("sqlite", sqliteURI(options))
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			reader.SetMaxOpenConns(1)
			interleaved := false
			err = withOwnershipReadSnapshot(ctx, reader, path, func(snapshot queryExecutor) error {
				view := &ownershipMigrationInterleave{queryExecutor: snapshot, commit: func() {
					if err := applyMigration(ctx, writer, path, plan[3]); err != nil {
						t.Fatalf("commit migration while ownership snapshot is open: %v", err)
					}
					interleaved = true
					// The migration is already committed and observable elsewhere;
					// reader success cannot be attributed to writer serialization.
					version, err := readSchemaVersion(ctx, writer)
					if err != nil || version != 4 {
						t.Fatalf("writer did not publish v4: version=%d error=%v", version, err)
					}
				}}
				if err := inspectOwnershipView(ctx, view, path, plan); err != nil {
					return err
				}
				var version int
				if err := snapshot.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
					return err
				}
				if version != 3 {
					t.Fatalf("ownership snapshot advanced after another commit: user_version=%d", version)
				}
				schemaVersion, err := readSchemaVersion(ctx, snapshot)
				if err != nil || schemaVersion != version {
					t.Fatalf("schema and pragma disagree inside snapshot: schema=%d pragma=%d error=%v", schemaVersion, version, err)
				}
				return checkMigrationJournal(ctx, snapshot, plan[:version])
			})
			if err != nil || !interleaved {
				t.Fatalf("coherent ownership preflight across committed migration: error=%v interleaved=%t", err, interleaved)
			}
			// A fresh preflight must observe the new version after rollback,
			// retaining strict catalogue/journal verification without stale reads.
			if err := inspectOwnership(ctx, reader, path, plan); err != nil {
				t.Fatalf("fresh ownership preflight after v4 commit: %v", err)
			}
			var version int
			if err := reader.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 4 {
				t.Fatalf("ownership snapshot leaked into pool: version=%d error=%v", version, err)
			}
		})
	}
}

func TestOwnershipSnapshotReleasesConnectionAfterCanceledInspection(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "ownership-canceled.db")
	database := rawDatabaseAtVersion(t, path, 3)
	database.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	err := withOwnershipReadSnapshot(ctx, database, path, func(view queryExecutor) error {
		var version int
		if err := view.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		cancel()
		return ctx.Err()
	})
	defer cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled ownership inspection error = %v", err)
	}
	plan, err := embeddedMigrationPlan()
	if err != nil {
		t.Fatal(err)
	}
	if err := inspectOwnership(context.Background(), database, path, plan); err != nil {
		t.Fatalf("canceled preflight retained a read transaction: %v", err)
	}
}
