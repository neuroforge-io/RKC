package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

// Abort closes one open build atomically after removing all mutable staging.
// Aborting an already-aborted build is idempotent; committed builds are never
// rewritten or deleted.
func (d *Database) Abort(ctx context.Context, buildID rkcstore.BuildID, reason error) error {
	const operation = "abort build"
	if err := writerCheckContext(ctx, operation); err != nil {
		return err
	}
	if err := writerValidIdentifier(operation, "build_id", string(buildID)); err != nil {
		return err
	}
	reasonText := writerLimitedReason(reason, rkcstore.DefaultMemoryOptions().MaxMetadataBytes)
	if d == nil {
		return writerOperationError(rkcstore.CodeConflict, operation, buildID, "", "database", ErrClosed)
	}
	d.lifecycle.RLock()
	defer d.lifecycle.RUnlock()
	if err := d.requireOpen(operation); err != nil {
		return writerOperationError(rkcstore.CodeConflict, operation, buildID, "", "database", err)
	}
	guard, owned, err := d.writerLeases.lockBuild(buildID)
	if err != nil {
		return writerLeaseRuntimeError(operation, buildID, err)
	}
	if owned {
		defer guard.unlock()
	}
	abortedOpenBuild := false
	err = d.writerTransactionLocked(ctx, operation, func(transaction *sql.Tx) error {
		var state string
		err := transaction.QueryRowContext(
			ctx,
			"SELECT state FROM builds WHERE build_id = ?",
			buildID,
		).Scan(&state)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return writerOperationError(rkcstore.CodeBuildNotFound, operation, buildID, "", "", nil)
		case err != nil:
			return writerDatabaseError(operation, "build_id", buildID, "", err)
		case state == "committed":
			return writerOperationError(rkcstore.CodeBuildCommitted, operation, buildID, "", "", nil)
		case state == "aborted":
			return nil
		case state != "open":
			return writerOperationError(
				rkcstore.CodeBuildClosed,
				operation,
				buildID,
				"",
				"state",
				fmt.Errorf("unsupported durable build state %q", state),
			)
		}
		if !owned {
			return writerLeaseOwnershipError(operation, buildID)
		}
		if err := writerEnsureNoCanonicalSnapshot(ctx, transaction, operation, buildID); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(
			ctx,
			"DELETE FROM staged_canonical_records WHERE build_id = ?",
			buildID,
		); err != nil {
			return writerDatabaseError(operation, "staging", buildID, "", err)
		}
		now := writerTimestamp(time.Now())
		result, err := transaction.ExecContext(
			ctx,
			`UPDATE builds
			 SET state = 'aborted', abort_reason = ?, updated_at = ?, finished_at = ?
			 WHERE build_id = ? AND state = 'open'`,
			writerNullableString(reasonText),
			now,
			now,
			buildID,
		)
		if err != nil {
			return writerDatabaseError(operation, "build_id", buildID, "", err)
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			if err == nil {
				err = fmt.Errorf("updated %d builds, want 1", affected)
			}
			return writerConflict(operation, buildID, "", "abort compare-and-swap failed: "+err.Error())
		}
		abortedOpenBuild = true
		return nil
	})
	if err != nil {
		return err
	}
	if abortedOpenBuild {
		if err := guard.release(); err != nil {
			return writerLeaseRuntimeError(operation, buildID, err)
		}
	}
	return nil
}

// Recover atomically aborts the bounded set of open builds. BeginBuild enforces
// MaxOpenBuilds, and Recover independently refuses an over-limit catalogue so
// corrupted state cannot turn recovery into an unbounded scan or mutation.
// Recover first takes a nonblocking exclusive OS lease. Any live build retains
// a shared lease, so recovery can mutate only after every owning process has
// committed, aborted, closed, or exited.
func (d *Database) Recover(ctx context.Context) (rkcstore.RecoveryResult, error) {
	const operation = "recover builds"
	result := rkcstore.RecoveryResult{AbortedBuilds: make([]rkcstore.BuildID, 0)}
	if err := writerCheckContext(ctx, operation); err != nil {
		return result, err
	}
	if d == nil {
		return result, writerOperationError(rkcstore.CodeConflict, operation, "", "", "database", ErrClosed)
	}
	d.lifecycle.RLock()
	defer d.lifecycle.RUnlock()
	if err := d.requireOpen(operation); err != nil {
		return result, writerOperationError(rkcstore.CodeConflict, operation, "", "", "database", err)
	}
	lease, err := d.writerLeases.acquire(true)
	if err != nil {
		return result, writerLeaseRuntimeError(operation, "", err)
	}
	defer lease.close()
	err = d.writerTransactionLocked(ctx, operation, func(transaction *sql.Tx) error {
		limit := rkcstore.DefaultMemoryOptions().MaxOpenBuilds
		rows, err := transaction.QueryContext(
			ctx,
			"SELECT build_id FROM builds WHERE state = 'open' ORDER BY build_id LIMIT ?",
			limit+1,
		)
		if err != nil {
			return writerDatabaseError(operation, "builds", "", "", err)
		}
		for rows.Next() {
			var buildID rkcstore.BuildID
			if err := rows.Scan(&buildID); err != nil {
				_ = rows.Close()
				return writerDatabaseError(operation, "builds", "", "", err)
			}
			result.AbortedBuilds = append(result.AbortedBuilds, buildID)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return writerDatabaseError(operation, "builds", "", "", err)
		}
		if err := rows.Close(); err != nil {
			return writerDatabaseError(operation, "builds", "", "", err)
		}
		if len(result.AbortedBuilds) > limit {
			return writerResource(operation, "", "builds", "open build catalogue exceeds MaxOpenBuilds")
		}
		sort.Slice(result.AbortedBuilds, func(left, right int) bool {
			return result.AbortedBuilds[left] < result.AbortedBuilds[right]
		})
		if len(result.AbortedBuilds) == 0 {
			return nil
		}
		owner, err := writerRandomID("recovery_", writerBuildIDBytes)
		if err != nil {
			return writerOperationError(rkcstore.CodeConflict, operation, "", "", "recovery_owner", err)
		}
		now := writerTimestamp(time.Now())
		recoveryJSON := `{"action":"abort_incomplete_build","version":1}`
		for _, buildID := range result.AbortedBuilds {
			if err := writerCheckContext(ctx, operation); err != nil {
				return err
			}
			if err := writerEnsureNoCanonicalSnapshot(ctx, transaction, operation, buildID); err != nil {
				return err
			}
			if _, err := transaction.ExecContext(
				ctx,
				"DELETE FROM staged_canonical_records WHERE build_id = ?",
				buildID,
			); err != nil {
				return writerDatabaseError(operation, "staging", buildID, "", err)
			}
			update, err := transaction.ExecContext(
				ctx,
				`UPDATE builds
				 SET state = 'aborted', recovery_state = 'recovered',
				     recovery_owner = ?, recovery_started_at = ?,
				     recovery_json = ?, abort_reason = ?,
				     updated_at = ?, finished_at = ?
				 WHERE build_id = ? AND state = 'open'`,
				owner,
				now,
				recoveryJSON,
				"recovered incomplete build",
				now,
				now,
				buildID,
			)
			if err != nil {
				return writerDatabaseError(operation, "build_id", buildID, "", err)
			}
			if affected, err := update.RowsAffected(); err != nil || affected != 1 {
				if err == nil {
					err = fmt.Errorf("updated %d builds, want 1", affected)
				}
				return writerConflict(operation, buildID, "", "recovery compare-and-swap failed: "+err.Error())
			}
		}
		return nil
	})
	if err != nil {
		return rkcstore.RecoveryResult{AbortedBuilds: make([]rkcstore.BuildID, 0)}, err
	}
	if err := lease.close(); err != nil {
		return rkcstore.RecoveryResult{AbortedBuilds: make([]rkcstore.BuildID, 0)},
			writerLeaseRuntimeError(operation, "", err)
	}
	return result, nil
}

func writerEnsureNoCanonicalSnapshot(
	ctx context.Context,
	transaction *sql.Tx,
	operation string,
	buildID rkcstore.BuildID,
) error {
	var exists int
	if err := transaction.QueryRowContext(
		ctx,
		"SELECT EXISTS(SELECT 1 FROM canonical_snapshots WHERE build_id = ?)",
		buildID,
	).Scan(&exists); err != nil {
		return writerDatabaseError(operation, "canonical_snapshot", buildID, "", err)
	}
	if exists != 0 {
		return writerConflict(operation, buildID, "", "open build already owns an immutable canonical snapshot")
	}
	return nil
}

func writerLimitedReason(reason error, maximum int64) string {
	if reason == nil || maximum <= 0 {
		return ""
	}
	message := reason.Error()
	if int64(len(message)) <= maximum {
		return message
	}
	message = message[:maximum]
	for !utf8.ValidString(message) && len(message) > 0 {
		message = message[:len(message)-1]
	}
	return message
}
