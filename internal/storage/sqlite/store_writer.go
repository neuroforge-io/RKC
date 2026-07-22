package sqlite

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
	moderncsqlite "modernc.org/sqlite"
)

const (
	writerCoverageRecordID = "coverage"
	writerBuildIDBytes     = 16
	writerBuildIDAttempts  = 8
	writerSQLiteConstraint = 19
)

var (
	_ rkcstore.SnapshotWriter = (*Database)(nil)
	_ rkcstore.BuildRecoverer = (*Database)(nil)
)

type writerBuildRecord struct {
	id             rkcstore.BuildID
	repositoryID   rkcstore.RepositoryID
	baseCurrent    rkcstore.SnapshotID
	parent         rkcstore.SnapshotID
	expectedSchema string
	metadataJSON   string
	state          string
}

type writerPreparedRecord struct {
	id     string
	body   []byte
	digest string
}

type writerPreparedBatch struct {
	records []writerPreparedRecord
	bytes   int64
}

type writerCanonicalRecord struct {
	family  string
	id      string
	ordinal int
	body    []byte
	digest  string
}

// BeginBuild opens a durable, isolated staging namespace. Repository creation,
// current-snapshot comparison, the open-build limit, and build insertion share
// one immediate transaction, so the recorded parent is an exact CAS token.
func (d *Database) BeginBuild(ctx context.Context, options rkcstore.BuildOptions) (rkcstore.BuildID, error) {
	const operation = "begin build"
	if err := writerCheckContext(ctx, operation); err != nil {
		return "", err
	}
	if err := writerValidIdentifier(operation, "repository_id", string(options.RepositoryID)); err != nil {
		return "", err
	}
	if options.ParentSnapshotID != "" {
		if err := writerValidIdentifier(operation, "parent_snapshot_id", string(options.ParentSnapshotID)); err != nil {
			return "", err
		}
	}
	if options.ExpectedSchema == "" {
		options.ExpectedSchema = rkcmodel.SchemaVersion
	}
	if options.ExpectedSchema != rkcmodel.SchemaVersion {
		return "", writerInvalid(operation, "expected_schema", "unsupported RKC schema version")
	}
	metadataJSON, err := writerPrepareMetadata(operation, options.Metadata)
	if err != nil {
		return "", err
	}
	if d == nil {
		return "", writerOperationError(rkcstore.CodeConflict, operation, "", "", "database", ErrClosed)
	}
	d.lifecycle.RLock()
	defer d.lifecycle.RUnlock()
	if err := d.requireOpen(operation); err != nil {
		return "", writerOperationError(rkcstore.CodeConflict, operation, "", "", "database", err)
	}
	lease, err := d.writerLeases.acquire(false)
	if err != nil {
		return "", writerLeaseRuntimeError(operation, "", err)
	}
	attached := false
	defer func() {
		if !attached {
			_ = lease.close()
		}
	}()

	var buildID rkcstore.BuildID
	err = d.writerTransactionLocked(ctx, operation, func(transaction *sql.Tx) error {
		now := writerTimestamp(time.Now())
		if _, err := transaction.ExecContext(
			ctx,
			`INSERT INTO repositories(repository_id, display_name, created_at, metadata_json)
			 VALUES (?, ?, ?, '{}')
			 ON CONFLICT(repository_id) DO NOTHING`,
			options.RepositoryID,
			options.RepositoryID,
			now,
		); err != nil {
			return writerDatabaseError(operation, "repository_id", "", "", err)
		}

		var current sql.NullString
		if err := transaction.QueryRowContext(
			ctx,
			"SELECT current_snapshot_id FROM repositories WHERE repository_id = ?",
			options.RepositoryID,
		).Scan(&current); err != nil {
			return writerDatabaseError(operation, "repository_id", "", "", err)
		}
		base := rkcstore.SnapshotID(writerNullString(current))
		if base != options.ParentSnapshotID {
			return writerConflict(
				operation,
				"",
				options.ParentSnapshotID,
				fmt.Sprintf("parent snapshot %q is not the current snapshot %q", options.ParentSnapshotID, base),
			)
		}
		if base != "" {
			var repository string
			err := transaction.QueryRowContext(
				ctx,
				"SELECT repository_id FROM canonical_snapshots WHERE snapshot_id = ?",
				base,
			).Scan(&repository)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				return writerOperationError(rkcstore.CodeSnapshotNotFound, operation, "", base, "parent_snapshot_id", nil)
			case err != nil:
				return writerDatabaseError(operation, "parent_snapshot_id", "", base, err)
			case repository != string(options.RepositoryID):
				return writerConflict(operation, "", base, "parent snapshot belongs to another repository")
			}
		}

		limits := rkcstore.DefaultMemoryOptions()
		var openBuilds int
		if err := transaction.QueryRowContext(ctx, "SELECT COUNT(*) FROM builds WHERE state = 'open'").Scan(&openBuilds); err != nil {
			return writerDatabaseError(operation, "builds", "", "", err)
		}
		if openBuilds >= limits.MaxOpenBuilds {
			return writerResource(operation, "", "builds", "store exceeds MaxOpenBuilds")
		}

		for attempt := 0; attempt < writerBuildIDAttempts; attempt++ {
			candidate, err := writerRandomID("build_", writerBuildIDBytes)
			if err != nil {
				return writerOperationError(rkcstore.CodeConflict, operation, "", "", "build_id", err)
			}
			_, err = transaction.ExecContext(
				ctx,
				`INSERT INTO builds(
				   build_id, repository_id, base_current_snapshot_id,
				   parent_snapshot_id, expected_schema, state, metadata_json,
				   created_at, updated_at
				 ) VALUES (?, ?, ?, ?, ?, 'open', ?, ?, ?)`,
				candidate,
				options.RepositoryID,
				writerNullableString(string(base)),
				writerNullableString(string(options.ParentSnapshotID)),
				options.ExpectedSchema,
				metadataJSON,
				now,
				now,
			)
			if err == nil {
				buildID = rkcstore.BuildID(candidate)
				return nil
			}
			if !writerIsConstraintError(err) {
				return writerDatabaseError(operation, "build_id", "", "", err)
			}
		}
		return writerConflict(operation, "", "", "could not allocate a unique cryptographic build identifier")
	})
	if err != nil {
		return "", err
	}
	if err := d.writerLeases.attach(buildID, lease); err != nil {
		return "", writerLeaseRuntimeError(operation, buildID, err)
	}
	attached = true
	return buildID, nil
}

func (d *Database) PutArtifacts(ctx context.Context, build rkcstore.BuildID, values []rkcmodel.Artifact) error {
	return writerPutBatch(d, ctx, "put artifacts", build, "artifact", values, func(value rkcmodel.Artifact) string { return value.ID })
}

func (d *Database) PutNodes(ctx context.Context, build rkcstore.BuildID, values []rkcmodel.Node) error {
	return writerPutBatch(d, ctx, "put nodes", build, "node", values, func(value rkcmodel.Node) string { return value.ID })
}

func (d *Database) PutEdges(ctx context.Context, build rkcstore.BuildID, values []rkcmodel.Edge) error {
	return writerPutBatch(d, ctx, "put edges", build, "edge", values, func(value rkcmodel.Edge) string { return value.ID })
}

func (d *Database) PutEvidence(ctx context.Context, build rkcstore.BuildID, values []rkcmodel.Evidence) error {
	return writerPutBatch(d, ctx, "put evidence", build, "evidence", values, func(value rkcmodel.Evidence) string { return value.ID })
}

func (d *Database) PutDiagnostics(ctx context.Context, build rkcstore.BuildID, values []rkcmodel.Diagnostic) error {
	return writerPutBatch(d, ctx, "put diagnostics", build, "diagnostic", values, func(value rkcmodel.Diagnostic) string { return value.ID })
}

func (d *Database) PutConflicts(ctx context.Context, build rkcstore.BuildID, values []rkcmodel.Conflict) error {
	return writerPutBatch(d, ctx, "put conflicts", build, "conflict", values, func(value rkcmodel.Conflict) string { return value.ID })
}

func (d *Database) PutDocuments(ctx context.Context, build rkcstore.BuildID, values []rkcmodel.Document) error {
	return writerPutBatch(d, ctx, "put documents", build, "document", values, func(value rkcmodel.Document) string { return value.ID })
}

func (d *Database) PutClaims(ctx context.Context, build rkcstore.BuildID, values []rkcmodel.Claim) error {
	return writerPutBatch(d, ctx, "put claims", build, "claim", values, func(value rkcmodel.Claim) string { return value.ID })
}

func (d *Database) PutPaths(ctx context.Context, build rkcstore.BuildID, values []rkcmodel.ExecutionPath) error {
	return writerPutBatch(d, ctx, "put paths", build, "execution_path", values, func(value rkcmodel.ExecutionPath) string { return value.ID })
}

// PutCoverage stages exactly one immutable coverage record. Replacing coverage
// would destroy the append-only audit trail, so a second call is a conflict.
func (d *Database) PutCoverage(ctx context.Context, build rkcstore.BuildID, coverage rkcmodel.Coverage) error {
	return writerPutBatch(
		d,
		ctx,
		"put coverage",
		build,
		"coverage",
		[]rkcmodel.Coverage{coverage},
		func(rkcmodel.Coverage) string { return writerCoverageRecordID },
	)
}

// Validate reconstructs the complete provisional bundle from authenticated
// staged bytes and applies the same strict vocabulary and coverage comparison
// used by MemoryStore. It never validates a caller-side partial view.
func (d *Database) Validate(ctx context.Context, buildID rkcstore.BuildID) (rkcstore.ValidationResult, error) {
	const operation = "validate build"
	var result rkcstore.ValidationResult
	err := d.writerOwnedTransaction(ctx, operation, buildID, false, func(transaction *sql.Tx) error {
		build, err := writerLoadOpenBuild(ctx, transaction, operation, buildID)
		if err != nil {
			return err
		}
		provisional := rkcmodel.Snapshot{
			SchemaVersion:    build.expectedSchema,
			ID:               string(build.id),
			RepositoryID:     string(build.repositoryID),
			ParentSnapshotID: string(build.parent),
			Status:           "validating",
			RootName:         string(build.repositoryID),
			ContentDigest:    strings.Repeat("0", 64),
			Tool:             rkcmodel.ToolInfo{Name: "rkcstore-sqlite", Version: "1"},
		}
		bundle, provided, err := writerLoadStagedBundle(ctx, transaction, operation, build, provisional)
		if err != nil {
			return err
		}
		if err := writerValidateAllIdentifiers(operation, buildID, rkcstore.SnapshotID(provisional.ID), bundle); err != nil {
			return err
		}
		result = writerValidateBundle(bundle, provided, false)
		if _, err := transaction.ExecContext(
			ctx,
			"UPDATE builds SET validated_at = ?, updated_at = ? WHERE build_id = ? AND state = 'open'",
			writerTimestamp(time.Now()),
			writerTimestamp(time.Now()),
			buildID,
		); err != nil {
			return writerDatabaseError(operation, "build_id", buildID, "", err)
		}
		return nil
	})
	return result, err
}

// Commit validates and publishes in one immediate transaction. Canonical rows,
// normalized relational rows, and FTS rows become visible together or not at
// all; the build and repository updates are guarded by the migration-v4 CAS.
func (d *Database) Commit(ctx context.Context, buildID rkcstore.BuildID, snapshot rkcmodel.Snapshot) error {
	const operation = "commit build"
	if err := writerCheckContext(ctx, operation); err != nil {
		return err
	}
	snapshotJSON, err := writerPrepareJSON(operation, "snapshot", snapshot)
	if err != nil {
		return err
	}
	if err := writerValidIdentifier(operation, "snapshot_id", snapshot.ID); err != nil {
		return err
	}

	return d.writerOwnedTransaction(ctx, operation, buildID, true, func(transaction *sql.Tx) error {
		build, err := writerLoadOpenBuild(ctx, transaction, operation, buildID)
		if err != nil {
			return err
		}
		snapshotID := rkcstore.SnapshotID(snapshot.ID)
		switch {
		case snapshot.RepositoryID != string(build.repositoryID):
			return writerInvalid(operation, "repository_id", "snapshot does not match build repository")
		case snapshot.ParentSnapshotID != string(build.parent):
			return writerInvalid(operation, "parent_snapshot_id", "snapshot does not match build parent")
		case snapshot.SchemaVersion != build.expectedSchema:
			return writerInvalid(operation, "schema_version", "snapshot does not match expected schema")
		case snapshot.Status != "committed":
			return writerInvalid(operation, "status", "committed snapshot must have status committed")
		}
		if err := writerValidateBundleIdentifiers(operation, snapshot); err != nil {
			return err
		}

		var current sql.NullString
		if err := transaction.QueryRowContext(
			ctx,
			"SELECT current_snapshot_id FROM repositories WHERE repository_id = ?",
			build.repositoryID,
		).Scan(&current); err != nil {
			return writerDatabaseError(operation, "repository_id", buildID, snapshotID, err)
		}
		if rkcstore.SnapshotID(writerNullString(current)) != build.baseCurrent {
			return writerConflict(operation, buildID, snapshotID, "repository current snapshot changed during build")
		}
		var exists int
		if err := transaction.QueryRowContext(
			ctx,
			"SELECT EXISTS(SELECT 1 FROM canonical_snapshots WHERE snapshot_id = ?)",
			snapshotID,
		).Scan(&exists); err != nil {
			return writerDatabaseError(operation, "snapshot_id", buildID, snapshotID, err)
		}
		if exists != 0 {
			return writerConflict(operation, buildID, snapshotID, "snapshot identifier already exists")
		}

		bundle, coverage, err := writerLoadStagedBundle(ctx, transaction, operation, build, snapshot)
		if err != nil {
			return err
		}
		if err := writerValidateAllIdentifiers(operation, buildID, snapshotID, bundle); err != nil {
			return err
		}
		validation := writerValidateBundle(bundle, coverage, true)
		if validation.Report.HasErrors() {
			return &rkcstore.ValidationFailure{Operation: operation, BuildID: buildID, Result: validation}
		}
		if !validation.CoveragePresent || !validation.CoverageConsistent {
			return writerOperationError(rkcstore.CodeCoverageMismatch, operation, buildID, snapshotID, "coverage", nil)
		}

		canonicalBundle := rkcmodel.CanonicalBundle(bundle)
		canonicalBundleJSON, err := json.Marshal(canonicalBundle)
		if err != nil {
			return writerInvalid(operation, "bundle", "bundle is not canonically serializable: "+err.Error())
		}
		canonicalDigest := writerSHA256(canonicalBundleJSON)
		if canonicalDigest != rkcmodel.CanonicalDigest(bundle) ||
			canonicalDigest != validation.ExpectedCoverage.DeterministicOutputDigest {
			return writerOperationError(
				rkcstore.CodeCoverageMismatch,
				operation,
				buildID,
				snapshotID,
				"deterministic_output_digest",
				errors.New("canonical bundle digest disagrees with exact coverage"),
			)
		}
		if writerBundleExceedsDurableLimit(len(canonicalBundleJSON)) {
			return writerResource(operation, buildID, "bundle", "canonical bundle exceeds bounded publication size")
		}
		canonicalRecords, err := writerPrepareCanonicalRecords(
			operation,
			canonicalBundle,
			validation.ExpectedCoverage,
		)
		if err != nil {
			return err
		}

		now := writerTimestamp(time.Now())
		publishedAt := writerTimestamp(snapshot.CreatedAt)
		if _, err := transaction.ExecContext(
			ctx,
			`INSERT INTO canonical_snapshots(
			   snapshot_id, repository_id, parent_snapshot_id, build_id,
			   schema_version, publication_status, legacy_projection_status,
			   canonical_snapshot_json, canonical_bundle_json, canonical_digest,
			   published_at, metadata_json
			 ) VALUES (?, ?, ?, ?, ?, 'committed', 'complete', ?, ?, ?, ?, ?)`,
			snapshotID,
			build.repositoryID,
			writerNullableString(string(build.parent)),
			buildID,
			build.expectedSchema,
			string(snapshotJSON),
			string(canonicalBundleJSON),
			canonicalDigest,
			publishedAt,
			build.metadataJSON,
		); err != nil {
			return writerDatabaseError(operation, "canonical_snapshot", buildID, snapshotID, err)
		}

		for _, record := range canonicalRecords {
			if err := writerCheckContext(ctx, operation); err != nil {
				return err
			}
			if _, err := transaction.ExecContext(
				ctx,
				`INSERT INTO canonical_snapshot_records(
				   snapshot_id, record_family, record_id, ordinal,
				   canonical_record_json, canonical_record_sha256
				 ) VALUES (?, ?, ?, ?, ?, ?)`,
				snapshotID,
				record.family,
				record.id,
				record.ordinal,
				string(record.body),
				record.digest,
			); err != nil {
				return writerDatabaseError(operation, "canonical_records", buildID, snapshotID, err)
			}
		}

		if err := writerProjectBundle(ctx, transaction, operation, build, bundle, validation.ExpectedCoverage); err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx, "DELETE FROM staged_canonical_records WHERE build_id = ?", buildID); err != nil {
			return writerDatabaseError(operation, "staging", buildID, snapshotID, err)
		}

		result, err := transaction.ExecContext(
			ctx,
			`UPDATE builds
			 SET state = 'committed', committed_snapshot_id = ?,
			     validated_at = ?, updated_at = ?, finished_at = ?
			 WHERE build_id = ? AND repository_id = ? AND state = 'open'
			   AND base_current_snapshot_id IS ? AND parent_snapshot_id IS ?
			   AND expected_schema = ?`,
			snapshotID,
			now,
			now,
			now,
			buildID,
			build.repositoryID,
			writerNullableString(string(build.baseCurrent)),
			writerNullableString(string(build.parent)),
			build.expectedSchema,
		)
		if err != nil {
			return writerConflict(operation, buildID, snapshotID, "build close compare-and-swap failed: "+err.Error())
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			if err == nil {
				err = fmt.Errorf("updated %d builds, want 1", affected)
			}
			return writerConflict(operation, buildID, snapshotID, "build close compare-and-swap failed: "+err.Error())
		}

		result, err = transaction.ExecContext(
			ctx,
			`UPDATE repositories SET current_snapshot_id = ?
			 WHERE repository_id = ? AND current_snapshot_id IS ?`,
			snapshotID,
			build.repositoryID,
			writerNullableString(string(build.baseCurrent)),
		)
		if err != nil {
			return writerConflict(operation, buildID, snapshotID, "repository publish compare-and-swap failed: "+err.Error())
		}
		if affected, err := result.RowsAffected(); err != nil || affected != 1 {
			if err == nil {
				err = fmt.Errorf("updated %d repositories, want 1", affected)
			}
			return writerConflict(operation, buildID, snapshotID, "repository publish compare-and-swap failed: "+err.Error())
		}
		return nil
	})
}

func writerBundleExceedsDurableLimit(size int) bool {
	return size > readerMaxBundleJSONBytes
}

func writerPrepareCanonicalRecords(
	operation string,
	bundle rkcmodel.Bundle,
	coverage rkcmodel.Coverage,
) ([]writerCanonicalRecord, error) {
	records := make([]writerCanonicalRecord, 0,
		len(bundle.Artifacts)+len(bundle.Nodes)+len(bundle.Edges)+
			len(bundle.Evidence)+len(bundle.Diagnostics)+len(bundle.Conflicts)+
			len(bundle.Documents)+len(bundle.Claims)+len(bundle.Paths)+1,
	)
	appendFamily := func(family string, id string, ordinal int, value any) error {
		body, err := json.Marshal(value)
		if err != nil {
			return writerInvalid(
				operation,
				"canonical_records",
				fmt.Sprintf("canonical %s record %q is not serializable: %v", family, id, err),
			)
		}
		records = append(records, writerCanonicalRecord{
			family: family, id: id, ordinal: ordinal,
			body: body, digest: writerSHA256(body),
		})
		return nil
	}
	for ordinal, value := range bundle.Artifacts {
		if err := appendFamily("artifact", value.ID, ordinal, value); err != nil {
			return nil, err
		}
	}
	for ordinal, value := range bundle.Nodes {
		if err := appendFamily("node", value.ID, ordinal, value); err != nil {
			return nil, err
		}
	}
	for ordinal, value := range bundle.Edges {
		if err := appendFamily("edge", value.ID, ordinal, value); err != nil {
			return nil, err
		}
	}
	for ordinal, value := range bundle.Evidence {
		if err := appendFamily("evidence", value.ID, ordinal, value); err != nil {
			return nil, err
		}
	}
	for ordinal, value := range bundle.Diagnostics {
		if err := appendFamily("diagnostic", value.ID, ordinal, value); err != nil {
			return nil, err
		}
	}
	for ordinal, value := range bundle.Conflicts {
		if err := appendFamily("conflict", value.ID, ordinal, value); err != nil {
			return nil, err
		}
	}
	for ordinal, value := range bundle.Documents {
		if err := appendFamily("document", value.ID, ordinal, value); err != nil {
			return nil, err
		}
	}
	for ordinal, value := range bundle.Claims {
		if err := appendFamily("claim", value.ID, ordinal, value); err != nil {
			return nil, err
		}
	}
	for ordinal, value := range bundle.Paths {
		if err := appendFamily("execution_path", value.ID, ordinal, value); err != nil {
			return nil, err
		}
	}
	if err := appendFamily("coverage", writerCoverageRecordID, 0, coverage); err != nil {
		return nil, err
	}
	return records, nil
}

func writerPutBatch[T any](
	database *Database,
	ctx context.Context,
	operation string,
	buildID rkcstore.BuildID,
	family string,
	values []T,
	identifier func(T) string,
) error {
	batch, err := writerPrepareBatch(ctx, operation, values, identifier)
	if err != nil {
		return err
	}
	return database.writerOwnedTransaction(ctx, operation, buildID, false, func(transaction *sql.Tx) error {
		if _, err := writerLoadOpenBuild(ctx, transaction, operation, buildID); err != nil {
			return err
		}
		limits := rkcstore.DefaultMemoryOptions()
		var count, bytes int64
		if err := transaction.QueryRowContext(
			ctx,
			`SELECT COUNT(*), COALESCE(SUM(length(CAST(canonical_record_json AS BLOB))), 0)
			 FROM staged_canonical_records WHERE build_id = ?`,
			buildID,
		).Scan(&count, &bytes); err != nil {
			return writerDatabaseError(operation, "values", buildID, "", err)
		}
		if int64(len(batch.records)) > limits.MaxBuildRecords-count {
			return writerResource(operation, buildID, "values", "build exceeds MaxBuildRecords")
		}
		if batch.bytes > limits.MaxBuildBytes-bytes {
			return writerResource(operation, buildID, "values", "build exceeds MaxBuildBytes")
		}

		var nextOrdinal int64
		if err := transaction.QueryRowContext(
			ctx,
			`SELECT COALESCE(MAX(ordinal), -1) + 1
			 FROM staged_canonical_records WHERE build_id = ? AND record_family = ?`,
			buildID,
			family,
		).Scan(&nextOrdinal); err != nil {
			return writerDatabaseError(operation, "ordinal", buildID, "", err)
		}
		for index, record := range batch.records {
			if err := writerCheckContext(ctx, operation); err != nil {
				return err
			}
			_, err := transaction.ExecContext(
				ctx,
				`INSERT INTO staged_canonical_records(
				   build_id, record_family, record_id, ordinal,
				   canonical_record_json, canonical_record_sha256
				 ) VALUES (?, ?, ?, ?, ?, ?)`,
				buildID,
				family,
				record.id,
				nextOrdinal+int64(index),
				string(record.body),
				record.digest,
			)
			if err != nil {
				if writerIsConstraintError(err) {
					return writerConflict(operation, buildID, "", fmt.Sprintf("record identifier %q already exists", record.id))
				}
				return writerDatabaseError(operation, "values", buildID, "", err)
			}
		}
		if _, err := transaction.ExecContext(
			ctx,
			"UPDATE builds SET updated_at = ? WHERE build_id = ? AND state = 'open'",
			writerTimestamp(time.Now()),
			buildID,
		); err != nil {
			return writerDatabaseError(operation, "build_id", buildID, "", err)
		}
		return nil
	})
}

func writerPrepareBatch[T any](
	ctx context.Context,
	operation string,
	values []T,
	identifier func(T) string,
) (writerPreparedBatch, error) {
	if err := writerCheckContext(ctx, operation); err != nil {
		return writerPreparedBatch{}, err
	}
	limits := rkcstore.DefaultMemoryOptions()
	if len(values) > rkcstore.MaxBatchSize {
		return writerPreparedBatch{}, writerResource(operation, "", "values", "batch exceeds MaxBatchSize")
	}
	if int64(len(values)) > limits.MaxBuildRecords {
		return writerPreparedBatch{}, writerResource(operation, "", "values", "batch exceeds MaxBuildRecords")
	}
	prepared := writerPreparedBatch{records: make([]writerPreparedRecord, 0, len(values))}
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		id := identifier(value)
		if err := writerValidIdentifier(operation, "record_id", id); err != nil {
			return writerPreparedBatch{}, err
		}
		if _, duplicate := seen[id]; duplicate {
			return writerPreparedBatch{}, writerConflict(operation, "", "", fmt.Sprintf("duplicate record identifier %q in batch", id))
		}
		seen[id] = struct{}{}
		body, err := json.Marshal(value)
		if err != nil {
			return writerPreparedBatch{}, writerInvalid(operation, "values", "record is not canonically serializable: "+err.Error())
		}
		size := int64(len(body))
		if size > limits.MaxRecordBytes {
			return writerPreparedBatch{}, writerResource(operation, "", "values", "record exceeds MaxRecordBytes")
		}
		if size > limits.MaxBatchBytes-prepared.bytes {
			return writerPreparedBatch{}, writerResource(operation, "", "values", "batch exceeds MaxBatchBytes")
		}
		prepared.records = append(prepared.records, writerPreparedRecord{id: id, body: body, digest: writerSHA256(body)})
		prepared.bytes += size
	}
	if err := writerCheckContext(ctx, operation); err != nil {
		return writerPreparedBatch{}, err
	}
	return prepared, nil
}

func writerLoadOpenBuild(
	ctx context.Context,
	transaction *sql.Tx,
	operation string,
	buildID rkcstore.BuildID,
) (writerBuildRecord, error) {
	if err := writerValidIdentifier(operation, "build_id", string(buildID)); err != nil {
		return writerBuildRecord{}, err
	}
	var build writerBuildRecord
	var base, parent sql.NullString
	err := transaction.QueryRowContext(
		ctx,
		`SELECT build_id, repository_id, base_current_snapshot_id,
		        parent_snapshot_id, expected_schema, metadata_json, state
		 FROM builds WHERE build_id = ?`,
		buildID,
	).Scan(
		&build.id,
		&build.repositoryID,
		&base,
		&parent,
		&build.expectedSchema,
		&build.metadataJSON,
		&build.state,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return writerBuildRecord{}, writerOperationError(rkcstore.CodeBuildNotFound, operation, buildID, "", "", nil)
	}
	if err != nil {
		return writerBuildRecord{}, writerDatabaseError(operation, "build_id", buildID, "", err)
	}
	build.baseCurrent = rkcstore.SnapshotID(writerNullString(base))
	build.parent = rkcstore.SnapshotID(writerNullString(parent))
	switch build.state {
	case "open":
		return build, nil
	case "committed":
		return writerBuildRecord{}, writerOperationError(rkcstore.CodeBuildCommitted, operation, buildID, "", "", nil)
	default:
		return writerBuildRecord{}, writerOperationError(rkcstore.CodeBuildClosed, operation, buildID, "", "", nil)
	}
}

func writerLoadStagedBundle(
	ctx context.Context,
	transaction *sql.Tx,
	operation string,
	build writerBuildRecord,
	snapshot rkcmodel.Snapshot,
) (rkcmodel.Bundle, *rkcmodel.Coverage, error) {
	bundle := rkcmodel.Bundle{Snapshot: snapshot}
	var coverage *rkcmodel.Coverage
	rows, err := transaction.QueryContext(
		ctx,
		`SELECT record_family, record_id, ordinal,
		        canonical_record_json, canonical_record_sha256
		 FROM staged_canonical_records
		 WHERE build_id = ?
		 ORDER BY record_family, ordinal`,
		build.id,
	)
	if err != nil {
		return bundle, nil, writerDatabaseError(operation, "staging", build.id, "", err)
	}
	defer rows.Close()

	limits := rkcstore.DefaultMemoryOptions()
	ordinals := make(map[string]int64)
	var records, bytes int64
	for rows.Next() {
		if err := writerCheckContext(ctx, operation); err != nil {
			return bundle, nil, err
		}
		var family, id, body, digest string
		var ordinal int64
		if err := rows.Scan(&family, &id, &ordinal, &body, &digest); err != nil {
			return bundle, nil, writerDatabaseError(operation, "staging", build.id, "", err)
		}
		if ordinal != ordinals[family] {
			return bundle, nil, writerConflict(operation, build.id, "", fmt.Sprintf("non-contiguous %s ordinal %d", family, ordinal))
		}
		ordinals[family]++
		records++
		bytes += int64(len(body))
		if records > limits.MaxBuildRecords || bytes > limits.MaxBuildBytes || int64(len(body)) > limits.MaxRecordBytes {
			return bundle, nil, writerResource(operation, build.id, "staging", "stored build exceeds configured limits")
		}
		if writerSHA256([]byte(body)) != digest {
			return bundle, nil, writerConflict(operation, build.id, "", fmt.Sprintf("stored %s record %q digest mismatch", family, id))
		}
		decode := func(target any) error {
			if err := writerDecodeJSON([]byte(body), target); err != nil {
				return writerConflict(operation, build.id, "", fmt.Sprintf("stored %s record %q is invalid: %v", family, id, err))
			}
			return nil
		}
		switch family {
		case "artifact":
			var value rkcmodel.Artifact
			if err := decode(&value); err != nil || value.ID != id {
				return bundle, nil, writerStoredIDError(operation, build.id, family, id, value.ID, err)
			}
			bundle.Artifacts = append(bundle.Artifacts, value)
		case "node":
			var value rkcmodel.Node
			if err := decode(&value); err != nil || value.ID != id {
				return bundle, nil, writerStoredIDError(operation, build.id, family, id, value.ID, err)
			}
			bundle.Nodes = append(bundle.Nodes, value)
		case "edge":
			var value rkcmodel.Edge
			if err := decode(&value); err != nil || value.ID != id {
				return bundle, nil, writerStoredIDError(operation, build.id, family, id, value.ID, err)
			}
			bundle.Edges = append(bundle.Edges, value)
		case "evidence":
			var value rkcmodel.Evidence
			if err := decode(&value); err != nil || value.ID != id {
				return bundle, nil, writerStoredIDError(operation, build.id, family, id, value.ID, err)
			}
			bundle.Evidence = append(bundle.Evidence, value)
		case "diagnostic":
			var value rkcmodel.Diagnostic
			if err := decode(&value); err != nil || value.ID != id {
				return bundle, nil, writerStoredIDError(operation, build.id, family, id, value.ID, err)
			}
			bundle.Diagnostics = append(bundle.Diagnostics, value)
		case "conflict":
			var value rkcmodel.Conflict
			if err := decode(&value); err != nil || value.ID != id {
				return bundle, nil, writerStoredIDError(operation, build.id, family, id, value.ID, err)
			}
			bundle.Conflicts = append(bundle.Conflicts, value)
		case "document":
			var value rkcmodel.Document
			if err := decode(&value); err != nil || value.ID != id {
				return bundle, nil, writerStoredIDError(operation, build.id, family, id, value.ID, err)
			}
			bundle.Documents = append(bundle.Documents, value)
		case "claim":
			var value rkcmodel.Claim
			if err := decode(&value); err != nil || value.ID != id {
				return bundle, nil, writerStoredIDError(operation, build.id, family, id, value.ID, err)
			}
			bundle.Claims = append(bundle.Claims, value)
		case "execution_path":
			var value rkcmodel.ExecutionPath
			if err := decode(&value); err != nil || value.ID != id {
				return bundle, nil, writerStoredIDError(operation, build.id, family, id, value.ID, err)
			}
			bundle.Paths = append(bundle.Paths, value)
		case "coverage":
			if id != writerCoverageRecordID || coverage != nil {
				return bundle, nil, writerConflict(operation, build.id, "", "build contains multiple or misidentified coverage records")
			}
			var value rkcmodel.Coverage
			if err := decode(&value); err != nil {
				return bundle, nil, err
			}
			coverage = &value
		default:
			return bundle, nil, writerConflict(operation, build.id, "", fmt.Sprintf("unknown staged record family %q", family))
		}
	}
	if err := rows.Err(); err != nil {
		return bundle, nil, writerDatabaseError(operation, "staging", build.id, "", err)
	}
	rkcmodel.SortBundle(&bundle)
	return bundle, coverage, nil
}

func writerValidateBundle(bundle rkcmodel.Bundle, provided *rkcmodel.Coverage, exact bool) rkcstore.ValidationResult {
	report := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: true})
	expected := rkcmodel.BuildCoverage(bundle)
	result := rkcstore.ValidationResult{
		Report:           report,
		ExpectedCoverage: expected,
		CoveragePresent:  provided != nil,
	}
	if provided != nil {
		left, right := *provided, expected
		if !exact {
			left.SnapshotID, right.SnapshotID = "", ""
			left.DeterministicOutputDigest, right.DeterministicOutputDigest = "", ""
		}
		result.CoverageConsistent = reflect.DeepEqual(left, right)
	}
	return result
}

func writerPrepareMetadata(operation string, values map[string]string) (string, error) {
	limits := rkcstore.DefaultMemoryOptions()
	if len(values) > limits.MaxMetadataKeys {
		return "", writerResource(operation, "", "metadata", "metadata exceeds MaxMetadataKeys")
	}
	var total int64
	for key, value := range values {
		if !utf8.ValidString(key) || !utf8.ValidString(value) || strings.IndexByte(key, 0) >= 0 || strings.IndexByte(value, 0) >= 0 {
			return "", writerInvalid(operation, "metadata", "metadata must be valid UTF-8 without NUL bytes")
		}
		size := int64(len(key)) + int64(len(value))
		if size > limits.MaxMetadataBytes-total {
			return "", writerResource(operation, "", "metadata", "metadata exceeds MaxMetadataBytes")
		}
		total += size
	}
	if values == nil {
		values = map[string]string{}
	}
	body, err := json.Marshal(values)
	if err != nil {
		return "", writerInvalid(operation, "metadata", "metadata is not canonically serializable: "+err.Error())
	}
	return string(body), nil
}

func writerPrepareJSON(operation, field string, value any) ([]byte, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, writerInvalid(operation, field, "value is not canonically serializable: "+err.Error())
	}
	if int64(len(body)) > rkcstore.DefaultMemoryOptions().MaxRecordBytes {
		return nil, writerResource(operation, "", field, "value exceeds MaxRecordBytes")
	}
	return body, nil
}

// writerTransactionLocked runs one immediate transaction while the caller
// holds d.lifecycle for reading. Lease-aware operations use it to keep Close
// from releasing their OS liveness proof between ownership verification and
// transaction commit.
func (d *Database) writerTransactionLocked(
	ctx context.Context,
	operation string,
	apply func(*sql.Tx) error,
) (resultErr error) {
	if err := writerCheckContext(ctx, operation); err != nil {
		return err
	}
	if err := d.requireOpen(operation); err != nil {
		return writerOperationError(rkcstore.CodeConflict, operation, "", "", "database", err)
	}
	if d.binding != nil {
		if err := d.binding.verifyFilesystem(); err != nil {
			return writerOperationError(rkcstore.CodeConflict, operation, "", "", "database", err)
		}
	}
	transaction, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return writerDatabaseError(operation, "database", "", "", err)
	}
	defer func() {
		if resultErr != nil {
			_ = transaction.Rollback()
		}
	}()
	if err := apply(transaction); err != nil {
		return err
	}
	if err := writerCheckContext(ctx, operation); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return writerDatabaseError(operation, "transaction", "", "", err)
	}
	return nil
}

func writerCheckContext(ctx context.Context, operation string) error {
	if ctx == nil {
		return writerInvalid(operation, "context", "context is required")
	}
	if err := ctx.Err(); err != nil {
		return writerOperationError(rkcstore.CodeCanceled, operation, "", "", "", err)
	}
	return nil
}

func writerValidIdentifier(operation, field, value string) error {
	if !utf8.ValidString(value) || strings.TrimSpace(value) == "" {
		return writerInvalid(operation, field, "identifier is required and must be valid UTF-8")
	}
	if len(value) > rkcstore.MaxIdentifierSize {
		return writerInvalid(operation, field, "identifier exceeds MaxIdentifierSize")
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return writerInvalid(operation, field, "identifier contains a control character")
		}
	}
	return nil
}

func writerValidateBundleIdentifiers(operation string, snapshot rkcmodel.Snapshot) error {
	if err := writerValidIdentifier(operation, "snapshot_id", snapshot.ID); err != nil {
		return err
	}
	if err := writerValidIdentifier(operation, "repository_id", snapshot.RepositoryID); err != nil {
		return err
	}
	if snapshot.ParentSnapshotID != "" {
		return writerValidIdentifier(operation, "parent_snapshot_id", snapshot.ParentSnapshotID)
	}
	return nil
}

func writerValidateAllIdentifiers(
	operation string,
	buildID rkcstore.BuildID,
	snapshotID rkcstore.SnapshotID,
	bundle rkcmodel.Bundle,
) error {
	required := make([]struct{ field, value string }, 0, 32)
	optional := make([]struct{ field, value string }, 0, 32)
	for _, value := range bundle.Artifacts {
		required = append(required, struct{ field, value string }{"artifact_id", value.ID})
		optional = append(optional, struct{ field, value string }{"logical_id", value.LogicalID})
	}
	for _, value := range bundle.Nodes {
		required = append(required, struct{ field, value string }{"node_id", value.ID})
		optional = append(optional, struct{ field, value string }{"logical_id", value.LogicalID}, struct{ field, value string }{"artifact_id", value.ArtifactID})
		for _, id := range value.EvidenceIDs {
			required = append(required, struct{ field, value string }{"evidence_id", id})
		}
	}
	for _, value := range bundle.Edges {
		required = append(required, struct{ field, value string }{"edge_id", value.ID}, struct{ field, value string }{"from", value.From}, struct{ field, value string }{"to", value.To})
		for _, id := range value.EvidenceIDs {
			required = append(required, struct{ field, value string }{"evidence_id", id})
		}
	}
	for _, value := range bundle.Evidence {
		required = append(required, struct{ field, value string }{"evidence_id", value.ID})
	}
	for _, value := range bundle.Diagnostics {
		required = append(required, struct{ field, value string }{"diagnostic_id", value.ID})
	}
	for _, value := range bundle.Conflicts {
		required = append(required, struct{ field, value string }{"conflict_id", value.ID}, struct{ field, value string }{"subject_id", value.SubjectID})
		for _, id := range value.CandidateIDs {
			required = append(required, struct{ field, value string }{"candidate_id", id})
		}
		for _, id := range value.EvidenceIDs {
			required = append(required, struct{ field, value string }{"evidence_id", id})
		}
		optional = append(optional, struct{ field, value string }{"preferred_id", value.PreferredID})
	}
	for _, value := range bundle.Documents {
		required = append(required, struct{ field, value string }{"document_id", value.ID})
		optional = append(optional, struct{ field, value string }{"logical_id", value.LogicalID})
		for _, id := range value.SubjectIDs {
			required = append(required, struct{ field, value string }{"subject_id", id})
		}
		for _, section := range value.Sections {
			required = append(required, struct{ field, value string }{"section_id", section.ID})
			optional = append(optional, struct{ field, value string }{"parent_id", section.ParentID})
			for _, id := range section.ClaimIDs {
				required = append(required, struct{ field, value string }{"claim_id", id})
			}
			for _, id := range section.EvidenceIDs {
				required = append(required, struct{ field, value string }{"evidence_id", id})
			}
		}
	}
	for _, value := range bundle.Claims {
		required = append(required, struct{ field, value string }{"claim_id", value.ID}, struct{ field, value string }{"subject_id", value.SubjectID})
		for _, id := range value.EvidenceIDs {
			required = append(required, struct{ field, value string }{"evidence_id", id})
		}
	}
	for _, value := range bundle.Paths {
		required = append(required, struct{ field, value string }{"path_id", value.ID}, struct{ field, value string }{"entry_node_id", value.EntryNodeID})
		optional = append(optional, struct{ field, value string }{"exit_node_id", value.ExitNodeID})
		for _, id := range value.NodeIDs {
			required = append(required, struct{ field, value string }{"node_id", id})
		}
		for _, id := range value.EdgeIDs {
			required = append(required, struct{ field, value string }{"edge_id", id})
		}
		for _, id := range value.EvidenceIDs {
			required = append(required, struct{ field, value string }{"evidence_id", id})
		}
	}
	for _, candidate := range required {
		if err := writerValidIdentifier(operation, candidate.field, candidate.value); err != nil {
			return writerOperationError(rkcstore.CodeInvalidArgument, operation, buildID, snapshotID, candidate.field, err)
		}
	}
	for _, candidate := range optional {
		if candidate.value != "" {
			if err := writerValidIdentifier(operation, candidate.field, candidate.value); err != nil {
				return writerOperationError(rkcstore.CodeInvalidArgument, operation, buildID, snapshotID, candidate.field, err)
			}
		}
	}
	return nil
}

func writerStoredIDError(operation string, build rkcstore.BuildID, family, expected, observed string, decodeErr error) error {
	if decodeErr != nil {
		return decodeErr
	}
	return writerConflict(operation, build, "", fmt.Sprintf("stored %s record identifier %q disagrees with JSON identifier %q", family, expected, observed))
}

func writerDecodeJSON(body []byte, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func writerRandomID(prefix string, size int) (string, error) {
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(body), nil
}

func writerSHA256(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func writerTimestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

func writerNullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func writerNullString(value sql.NullString) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

func writerOperationError(
	code rkcstore.Code,
	operation string,
	build rkcstore.BuildID,
	snapshot rkcstore.SnapshotID,
	field string,
	cause error,
) error {
	return &rkcstore.OperationError{
		Code: code, Operation: operation, BuildID: build,
		SnapshotID: snapshot, Field: field, Err: cause,
	}
}

func writerInvalid(operation, field, message string) error {
	return writerOperationError(rkcstore.CodeInvalidArgument, operation, "", "", field, errors.New(message))
}

func writerResource(operation string, build rkcstore.BuildID, field, message string) error {
	return writerOperationError(rkcstore.CodeResourceExhausted, operation, build, "", field, errors.New(message))
}

func writerConflict(operation string, build rkcstore.BuildID, snapshot rkcstore.SnapshotID, message string) error {
	return writerOperationError(rkcstore.CodeConflict, operation, build, snapshot, "", errors.New(message))
}

func writerDatabaseError(
	operation, field string,
	build rkcstore.BuildID,
	snapshot rkcstore.SnapshotID,
	cause error,
) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) || errors.Is(classifyDatabaseError(cause), ErrCanceled) {
		return writerOperationError(rkcstore.CodeCanceled, operation, build, snapshot, field, cause)
	}
	return writerOperationError(rkcstore.CodeInternal, operation, build, snapshot, field, cause)
}

func writerIsConstraintError(err error) bool {
	var sqliteError *moderncsqlite.Error
	return errors.As(err, &sqliteError) && sqliteError.Code()&0xff == writerSQLiteConstraint
}

func writerSortedStrings(values []string) []string {
	result := append([]string(nil), values...)
	sort.Strings(result)
	return result
}
