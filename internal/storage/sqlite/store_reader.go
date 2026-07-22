package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

const (
	// Durable records use the same per-record ceiling as the bounded reference
	// store. Pages have a separate aggregate ceiling so MaxPageSize cannot turn a
	// page of individually valid records into an unbounded allocation.
	readerMaxObjectJSONBytes = 4 << 20
	readerMaxPageJSONBytes   = 16 << 20
	readerMaxBundleJSONBytes = 128 << 20
	readerMaxCursorBytes     = 4096
)

var _ rkcstore.SnapshotReader = (*Database)(nil)

// Snapshot returns the full operational snapshot envelope stored alongside the
// deterministic canonical bundle.
func (d *Database) Snapshot(
	ctx context.Context,
	id rkcstore.SnapshotID,
) (rkcmodel.Snapshot, error) {
	const operation = "read snapshot"
	if err := readerValidateID(ctx, operation, "snapshot_id", string(id)); err != nil {
		return rkcmodel.Snapshot{}, err
	}
	return readerWithConnection(d, ctx, operation, func(connection *sql.Conn) (rkcmodel.Snapshot, error) {
		return readerSnapshotByID(ctx, connection, operation, id)
	})
}

// Bundle returns the complete lossless bundle. canonical_bundle_json contains
// deterministic canonical data, while canonical_snapshot_json retains the
// operational snapshot fields deliberately removed from canonical digests.
func (d *Database) Bundle(
	ctx context.Context,
	id rkcstore.SnapshotID,
) (rkcmodel.Bundle, error) {
	const operation = "read bundle"
	if err := readerValidateID(ctx, operation, "snapshot_id", string(id)); err != nil {
		return rkcmodel.Bundle{}, err
	}
	return readerWithConnection(d, ctx, operation, func(connection *sql.Conn) (rkcmodel.Bundle, error) {
		row := connection.QueryRowContext(
			ctx,
			`SELECT snapshot_id, repository_id, schema_version,
			        length(CAST(canonical_snapshot_json AS BLOB)),
			        CASE
			          WHEN length(CAST(canonical_snapshot_json AS BLOB)) <= ?
			          THEN canonical_snapshot_json
			        END,
			        length(CAST(canonical_bundle_json AS BLOB)),
			        CASE
			          WHEN length(CAST(canonical_bundle_json AS BLOB)) <= ?
			          THEN canonical_bundle_json
			        END,
			        canonical_digest
			 FROM canonical_snapshots
			 WHERE snapshot_id = ?`,
			readerMaxObjectJSONBytes,
			readerMaxBundleJSONBytes,
			id,
		)
		var (
			rowID, repositoryID, schemaVersion string
			snapshotSize, bundleSize           int64
			snapshotJSON, bundleJSON           sql.NullString
			canonicalDigest                    string
		)
		if err := row.Scan(
			&rowID,
			&repositoryID,
			&schemaVersion,
			&snapshotSize,
			&snapshotJSON,
			&bundleSize,
			&bundleJSON,
			&canonicalDigest,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return rkcmodel.Bundle{}, readerSnapshotNotFound(operation, id, "")
			}
			return rkcmodel.Bundle{}, readerStorageError(operation, id, "database", err)
		}
		if snapshotSize > readerMaxObjectJSONBytes || !snapshotJSON.Valid {
			return rkcmodel.Bundle{}, readerResourceError(
				operation,
				id,
				"snapshot",
				"stored snapshot exceeds the durable reader limit",
			)
		}
		if bundleSize > readerMaxBundleJSONBytes || !bundleJSON.Valid {
			return rkcmodel.Bundle{}, readerResourceError(
				operation,
				id,
				"bundle",
				"stored bundle exceeds the durable reader limit",
			)
		}
		if err := readerVerifyDigest(
			operation,
			id,
			"canonical_bundle_json",
			bundleJSON.String,
			canonicalDigest,
		); err != nil {
			return rkcmodel.Bundle{}, err
		}

		fullSnapshot, err := readerDecodeSnapshot(
			operation,
			id,
			rowID,
			repositoryID,
			schemaVersion,
			snapshotJSON.String,
		)
		if err != nil {
			return rkcmodel.Bundle{}, err
		}
		bundle, err := readerDecodeObject[rkcmodel.Bundle](
			operation,
			id,
			"canonical_bundle_json",
			bundleJSON.String,
			readerMaxBundleJSONBytes,
		)
		if err != nil {
			return rkcmodel.Bundle{}, err
		}
		if bundle.Snapshot.ID != rowID || bundle.Snapshot.RepositoryID != repositoryID ||
			bundle.Snapshot.SchemaVersion != schemaVersion || bundle.Snapshot.Status != "committed" {
			return rkcmodel.Bundle{}, readerStoredDataError(
				operation,
				id,
				"canonical_bundle_json",
				"canonical bundle snapshot identity does not match its row",
				nil,
			)
		}
		bundle.Snapshot = fullSnapshot
		canonical, err := rkcmodel.CanonicalJSON(bundle)
		if err != nil || !bytes.Equal(canonical, []byte(bundleJSON.String)) ||
			rkcmodel.CanonicalDigest(bundle) != canonicalDigest {
			return rkcmodel.Bundle{}, readerStoredDataError(
				operation,
				id,
				"canonical_bundle_json",
				"stored bundle does not match its canonical form and digest",
				err,
			)
		}
		report := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: true})
		if report.HasErrors() {
			return rkcmodel.Bundle{}, readerStoredDataError(
				operation,
				id,
				"canonical_bundle_json",
				"stored bundle fails canonical model validation",
				errors.New("validation report contains error diagnostics"),
			)
		}
		return bundle, nil
	})
}

// Current resolves a repository's atomically published current snapshot.
func (d *Database) Current(
	ctx context.Context,
	repositoryID rkcstore.RepositoryID,
) (rkcmodel.Snapshot, error) {
	const operation = "read current snapshot"
	if err := readerValidateID(ctx, operation, "repository_id", string(repositoryID)); err != nil {
		return rkcmodel.Snapshot{}, err
	}
	return readerWithConnection(d, ctx, operation, func(connection *sql.Conn) (rkcmodel.Snapshot, error) {
		row := connection.QueryRowContext(
			ctx,
			`SELECT snapshot.snapshot_id, snapshot.repository_id,
			        snapshot.schema_version,
			        length(CAST(snapshot.canonical_snapshot_json AS BLOB)),
			        CASE
			          WHEN length(CAST(snapshot.canonical_snapshot_json AS BLOB)) <= ?
			          THEN snapshot.canonical_snapshot_json
			        END
			 FROM repositories AS repository
			 JOIN canonical_snapshots AS snapshot
			   ON snapshot.snapshot_id = repository.current_snapshot_id
			  AND snapshot.repository_id = repository.repository_id
			 WHERE repository.repository_id = ?`,
			readerMaxObjectJSONBytes,
			repositoryID,
		)
		snapshot, err := readerSnapshotFromRow(operation, "", row)
		if errors.Is(err, sql.ErrNoRows) {
			return rkcmodel.Snapshot{}, readerSnapshotNotFound(operation, "", "repository_id")
		}
		if err != nil {
			return rkcmodel.Snapshot{}, readerStorageOrTypedError(operation, "", "database", err)
		}
		if snapshot.RepositoryID != string(repositoryID) {
			return rkcmodel.Snapshot{}, readerStoredDataError(
				operation,
				"",
				"canonical_snapshot_json",
				"current snapshot belongs to another repository",
				nil,
			)
		}
		return snapshot, nil
	})
}

func (d *Database) Artifact(
	ctx context.Context,
	snapshotID rkcstore.SnapshotID,
	artifactID string,
) (rkcmodel.Artifact, error) {
	const operation = "read artifact"
	if err := readerValidateLookup(ctx, operation, snapshotID, "artifact_id", artifactID); err != nil {
		return rkcmodel.Artifact{}, err
	}
	return readerReadPointRecord(
		d,
		ctx,
		operation,
		snapshotID,
		"artifact",
		"artifact_id",
		artifactID,
		func(value rkcmodel.Artifact) string { return value.ID },
	)
}

func (d *Database) Node(
	ctx context.Context,
	snapshotID rkcstore.SnapshotID,
	nodeID string,
) (rkcmodel.Node, error) {
	const operation = "read node"
	if err := readerValidateLookup(ctx, operation, snapshotID, "node_id", nodeID); err != nil {
		return rkcmodel.Node{}, err
	}
	return readerReadPointRecord(
		d,
		ctx,
		operation,
		snapshotID,
		"node",
		"node_id",
		nodeID,
		func(value rkcmodel.Node) string { return value.ID },
	)
}

func (d *Database) Evidence(
	ctx context.Context,
	snapshotID rkcstore.SnapshotID,
	evidenceID string,
) (rkcmodel.Evidence, error) {
	const operation = "read evidence"
	if err := readerValidateLookup(ctx, operation, snapshotID, "evidence_id", evidenceID); err != nil {
		return rkcmodel.Evidence{}, err
	}
	return readerReadPointRecord(
		d,
		ctx,
		operation,
		snapshotID,
		"evidence",
		"evidence_id",
		evidenceID,
		func(value rkcmodel.Evidence) string { return value.ID },
	)
}

// Coverage returns the single canonical coverage record bound to a snapshot.
func (d *Database) Coverage(
	ctx context.Context,
	snapshotID rkcstore.SnapshotID,
) (rkcmodel.Coverage, error) {
	const operation = "read coverage"
	if err := readerValidateID(ctx, operation, "snapshot_id", string(snapshotID)); err != nil {
		return rkcmodel.Coverage{}, err
	}
	return readerWithConnection(d, ctx, operation, func(connection *sql.Conn) (rkcmodel.Coverage, error) {
		rows, err := connection.QueryContext(
			ctx,
			`SELECT record.record_id,
			        length(CAST(record.canonical_record_json AS BLOB)),
			        CASE
			          WHEN length(CAST(record.canonical_record_json AS BLOB)) <= ?
			          THEN record.canonical_record_json
			        END,
			        record.canonical_record_sha256
			 FROM canonical_snapshots AS snapshot
			 LEFT JOIN canonical_snapshot_records AS record
			   ON record.snapshot_id = snapshot.snapshot_id
			  AND record.record_family = 'coverage'
			 WHERE snapshot.snapshot_id = ?
			 ORDER BY record.ordinal
			 LIMIT 2`,
			readerMaxObjectJSONBytes,
			snapshotID,
		)
		if err != nil {
			return rkcmodel.Coverage{}, readerStorageError(operation, snapshotID, "database", err)
		}
		defer rows.Close()
		if !rows.Next() {
			if err := rows.Err(); err != nil {
				return rkcmodel.Coverage{}, readerStorageError(operation, snapshotID, "database", err)
			}
			return rkcmodel.Coverage{}, readerSnapshotNotFound(operation, snapshotID, "")
		}
		var recordID, payload sql.NullString
		var size sql.NullInt64
		var digest sql.NullString
		if err := rows.Scan(&recordID, &size, &payload, &digest); err != nil {
			return rkcmodel.Coverage{}, readerStorageError(operation, snapshotID, "database", err)
		}
		if !recordID.Valid {
			return rkcmodel.Coverage{}, readerRecordNotFound(operation, snapshotID, "coverage", "coverage")
		}
		if recordID.String != "coverage" {
			return rkcmodel.Coverage{}, readerStoredDataError(
				operation,
				snapshotID,
				"coverage",
				"coverage record has a non-canonical identity",
				nil,
			)
		}
		if !size.Valid || size.Int64 > readerMaxObjectJSONBytes || !payload.Valid {
			return rkcmodel.Coverage{}, readerResourceError(
				operation,
				snapshotID,
				"coverage",
				"stored coverage exceeds the durable reader limit",
			)
		}
		if !digest.Valid {
			return rkcmodel.Coverage{}, readerStoredDataError(
				operation,
				snapshotID,
				"coverage",
				"stored coverage digest is absent",
				nil,
			)
		}
		if err := readerVerifyDigest(
			operation,
			snapshotID,
			"coverage",
			payload.String,
			digest.String,
		); err != nil {
			return rkcmodel.Coverage{}, err
		}
		coverage, err := readerDecodeObject[rkcmodel.Coverage](
			operation,
			snapshotID,
			"coverage",
			payload.String,
			readerMaxObjectJSONBytes,
		)
		if err != nil {
			return rkcmodel.Coverage{}, err
		}
		if coverage.SnapshotID != string(snapshotID) {
			return rkcmodel.Coverage{}, readerStoredDataError(
				operation,
				snapshotID,
				"coverage",
				"coverage snapshot identity does not match its row",
				nil,
			)
		}
		if rows.Next() {
			return rkcmodel.Coverage{}, readerStoredDataError(
				operation,
				snapshotID,
				"coverage",
				"snapshot contains multiple coverage records",
				nil,
			)
		}
		if err := rows.Err(); err != nil {
			return rkcmodel.Coverage{}, readerStorageError(operation, snapshotID, "database", err)
		}
		return coverage, nil
	})
}

func readerSnapshotByID(
	ctx context.Context,
	connection *sql.Conn,
	operation string,
	id rkcstore.SnapshotID,
) (rkcmodel.Snapshot, error) {
	row := connection.QueryRowContext(
		ctx,
		`SELECT snapshot_id, repository_id, schema_version,
		        length(CAST(canonical_snapshot_json AS BLOB)),
		        CASE
		          WHEN length(CAST(canonical_snapshot_json AS BLOB)) <= ?
		          THEN canonical_snapshot_json
		        END
		 FROM canonical_snapshots
		 WHERE snapshot_id = ?`,
		readerMaxObjectJSONBytes,
		id,
	)
	snapshot, err := readerSnapshotFromRow(operation, id, row)
	if errors.Is(err, sql.ErrNoRows) {
		return rkcmodel.Snapshot{}, readerSnapshotNotFound(operation, id, "")
	}
	if err != nil {
		return rkcmodel.Snapshot{}, readerStorageOrTypedError(operation, id, "database", err)
	}
	return snapshot, nil
}

func readerSnapshotFromRow(
	operation string,
	requestedID rkcstore.SnapshotID,
	row *sql.Row,
) (rkcmodel.Snapshot, error) {
	var rowID, repositoryID, schemaVersion string
	var size int64
	var payload sql.NullString
	if err := row.Scan(&rowID, &repositoryID, &schemaVersion, &size, &payload); err != nil {
		return rkcmodel.Snapshot{}, err
	}
	if size > readerMaxObjectJSONBytes || !payload.Valid {
		return rkcmodel.Snapshot{}, readerResourceError(
			operation,
			requestedID,
			"snapshot",
			"stored snapshot exceeds the durable reader limit",
		)
	}
	return readerDecodeSnapshot(
		operation,
		requestedID,
		rowID,
		repositoryID,
		schemaVersion,
		payload.String,
	)
}

func readerDecodeSnapshot(
	operation string,
	requestedID rkcstore.SnapshotID,
	rowID string,
	repositoryID string,
	schemaVersion string,
	payload string,
) (rkcmodel.Snapshot, error) {
	snapshot, err := readerDecodeObject[rkcmodel.Snapshot](
		operation,
		requestedID,
		"canonical_snapshot_json",
		payload,
		readerMaxObjectJSONBytes,
	)
	if err != nil {
		return rkcmodel.Snapshot{}, err
	}
	if snapshot.ID != rowID || snapshot.RepositoryID != repositoryID ||
		snapshot.SchemaVersion != schemaVersion || snapshot.Status != "committed" {
		return rkcmodel.Snapshot{}, readerStoredDataError(
			operation,
			requestedID,
			"canonical_snapshot_json",
			"snapshot identity does not match its row",
			nil,
		)
	}
	if requestedID != "" && snapshot.ID != string(requestedID) {
		return rkcmodel.Snapshot{}, readerStoredDataError(
			operation,
			requestedID,
			"snapshot_id",
			"snapshot lookup returned another identity",
			nil,
		)
	}
	return snapshot, nil
}

func readerReadPointRecord[T any](
	database *Database,
	ctx context.Context,
	operation string,
	snapshotID rkcstore.SnapshotID,
	family string,
	field string,
	recordID string,
	identifier func(T) string,
) (T, error) {
	return readerWithConnection(database, ctx, operation, func(connection *sql.Conn) (T, error) {
		var zero T
		row := connection.QueryRowContext(
			ctx,
			`SELECT record.record_id,
			        length(CAST(record.canonical_record_json AS BLOB)),
			        CASE
			          WHEN length(CAST(record.canonical_record_json AS BLOB)) <= ?
			          THEN record.canonical_record_json
			        END,
			        record.canonical_record_sha256
			 FROM canonical_snapshots AS snapshot
			 LEFT JOIN canonical_snapshot_records AS record
			   ON record.snapshot_id = snapshot.snapshot_id
			  AND record.record_family = ?
			  AND record.record_id = ?
			 WHERE snapshot.snapshot_id = ?`,
			readerMaxObjectJSONBytes,
			family,
			recordID,
			snapshotID,
		)
		var storedID, payload sql.NullString
		var size sql.NullInt64
		var digest sql.NullString
		if err := row.Scan(&storedID, &size, &payload, &digest); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return zero, readerSnapshotNotFound(operation, snapshotID, "")
			}
			return zero, readerStorageError(operation, snapshotID, "database", err)
		}
		if !storedID.Valid {
			return zero, readerRecordNotFound(operation, snapshotID, field, recordID)
		}
		if !size.Valid || size.Int64 > readerMaxObjectJSONBytes || !payload.Valid {
			return zero, readerResourceError(
				operation,
				snapshotID,
				field,
				"stored record exceeds the durable reader limit",
			)
		}
		if !digest.Valid {
			return zero, readerStoredDataError(
				operation,
				snapshotID,
				field,
				"stored record digest is absent",
				nil,
			)
		}
		if err := readerVerifyDigest(
			operation,
			snapshotID,
			field,
			payload.String,
			digest.String,
		); err != nil {
			return zero, err
		}
		value, err := readerDecodeObject[T](
			operation,
			snapshotID,
			field,
			payload.String,
			readerMaxObjectJSONBytes,
		)
		if err != nil {
			return zero, err
		}
		if storedID.String != recordID || identifier(value) != recordID {
			return zero, readerStoredDataError(
				operation,
				snapshotID,
				field,
				"record identity does not match its row",
				nil,
			)
		}
		return value, nil
	})
}

func readerWithConnection[T any](
	database *Database,
	ctx context.Context,
	operation string,
	callback func(*sql.Conn) (T, error),
) (T, error) {
	var zero T
	if err := readerValidateContext(ctx, operation); err != nil {
		return zero, err
	}
	if database == nil {
		return zero, readerStorageError(operation, "", "database", ErrClosed)
	}
	database.lifecycle.RLock()
	defer database.lifecycle.RUnlock()
	if err := database.requireOpen(operation); err != nil {
		return zero, readerStorageError(operation, "", "database", err)
	}
	if database.binding != nil {
		if err := database.binding.verifyFilesystem(); err != nil {
			return zero, readerStorageError(operation, "", "database", err)
		}
	}
	connection, err := database.db.Conn(ctx)
	if err != nil {
		return zero, readerStorageError(operation, "", "database", err)
	}
	result, callbackErr := callback(connection)
	closeErr := connection.Close()
	if callbackErr != nil {
		return zero, callbackErr
	}
	if closeErr != nil {
		return zero, readerStorageError(operation, "", "database", closeErr)
	}
	if err := ctx.Err(); err != nil {
		return zero, readerCanceledError(operation, err)
	}
	if database.binding != nil {
		if err := database.binding.verifyFilesystem(); err != nil {
			return zero, readerStorageError(operation, "", "database", err)
		}
	}
	return result, nil
}

func readerDecodeObject[T any](
	operation string,
	snapshotID rkcstore.SnapshotID,
	field string,
	payload string,
	maximum int,
) (T, error) {
	var value T
	if len(payload) > maximum {
		return value, readerResourceError(
			operation,
			snapshotID,
			field,
			"stored JSON exceeds the durable reader limit",
		)
	}
	if !utf8.ValidString(payload) {
		return value, readerStoredDataError(
			operation,
			snapshotID,
			field,
			"stored JSON is not UTF-8",
			nil,
		)
	}
	trimmed := bytes.TrimSpace([]byte(payload))
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return value, readerStoredDataError(
			operation,
			snapshotID,
			field,
			"stored JSON must be an object",
			nil,
		)
	}
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, readerStoredDataError(
			operation,
			snapshotID,
			field,
			"stored JSON does not match its canonical type",
			err,
		)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = fmt.Errorf("trailing JSON value")
		}
		return value, readerStoredDataError(
			operation,
			snapshotID,
			field,
			"stored JSON has trailing data",
			err,
		)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return value, readerStoredDataError(
			operation,
			snapshotID,
			field,
			"decoded stored JSON is not serializable",
			err,
		)
	}
	if !bytes.Equal(canonical, []byte(payload)) {
		return value, readerStoredDataError(
			operation,
			snapshotID,
			field,
			"stored JSON is not canonical",
			nil,
		)
	}
	return value, nil
}

func readerVerifyDigest(
	operation string,
	snapshotID rkcstore.SnapshotID,
	field string,
	payload string,
	digest string,
) error {
	if len(digest) != sha256.Size*2 || digest != strings.ToLower(digest) {
		return readerStoredDataError(
			operation,
			snapshotID,
			field,
			"stored SHA-256 digest is not canonical",
			nil,
		)
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return readerStoredDataError(
			operation,
			snapshotID,
			field,
			"stored SHA-256 digest is invalid",
			err,
		)
	}
	observed := sha256.Sum256([]byte(payload))
	if hex.EncodeToString(observed[:]) != digest {
		return readerStoredDataError(
			operation,
			snapshotID,
			field,
			"stored bytes do not match their SHA-256 digest",
			nil,
		)
	}
	return nil
}

func readerValidateContext(ctx context.Context, operation string) error {
	if ctx == nil {
		return readerOperationError(
			rkcstore.CodeInvalidArgument,
			operation,
			"",
			"context",
			errors.New("context is required"),
		)
	}
	if err := ctx.Err(); err != nil {
		return readerCanceledError(operation, err)
	}
	return nil
}

func readerValidateID(
	ctx context.Context,
	operation string,
	field string,
	value string,
) error {
	if err := readerValidateContext(ctx, operation); err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return readerOperationError(
			rkcstore.CodeInvalidArgument,
			operation,
			"",
			field,
			errors.New("identifier is required"),
		)
	}
	if len(value) > rkcstore.MaxIdentifierSize {
		return readerOperationError(
			rkcstore.CodeInvalidArgument,
			operation,
			"",
			field,
			errors.New("identifier exceeds MaxIdentifierSize"),
		)
	}
	return nil
}

func readerValidateLookup(
	ctx context.Context,
	operation string,
	snapshotID rkcstore.SnapshotID,
	field string,
	recordID string,
) error {
	if err := readerValidateID(ctx, operation, "snapshot_id", string(snapshotID)); err != nil {
		return err
	}
	return readerValidateID(ctx, operation, field, recordID)
}

func readerOperationError(
	code rkcstore.Code,
	operation string,
	snapshotID rkcstore.SnapshotID,
	field string,
	cause error,
) error {
	return &rkcstore.OperationError{
		Code:       code,
		Operation:  operation,
		SnapshotID: snapshotID,
		Field:      field,
		Err:        cause,
	}
}

func readerCanceledError(operation string, cause error) error {
	return readerOperationError(rkcstore.CodeCanceled, operation, "", "", cause)
}

func readerSnapshotNotFound(
	operation string,
	snapshotID rkcstore.SnapshotID,
	field string,
) error {
	return readerOperationError(
		rkcstore.CodeSnapshotNotFound,
		operation,
		snapshotID,
		field,
		nil,
	)
}

func readerRecordNotFound(
	operation string,
	snapshotID rkcstore.SnapshotID,
	field string,
	recordID string,
) error {
	return readerOperationError(
		rkcstore.CodeRecordNotFound,
		operation,
		snapshotID,
		field,
		errors.New(recordID),
	)
}

func readerResourceError(
	operation string,
	snapshotID rkcstore.SnapshotID,
	field string,
	message string,
) error {
	return readerOperationError(
		rkcstore.CodeResourceExhausted,
		operation,
		snapshotID,
		field,
		errors.New(message),
	)
}

func readerStoredDataError(
	operation string,
	snapshotID rkcstore.SnapshotID,
	field string,
	message string,
	cause error,
) error {
	if cause == nil {
		cause = errors.New(message)
	} else {
		cause = fmt.Errorf("%s: %w", message, cause)
	}
	return readerOperationError(
		rkcstore.CodeValidation,
		operation,
		snapshotID,
		field,
		cause,
	)
}

func readerStorageOrTypedError(
	operation string,
	snapshotID rkcstore.SnapshotID,
	field string,
	err error,
) error {
	var typed *rkcstore.OperationError
	if errors.As(err, &typed) {
		return err
	}
	return readerStorageError(operation, snapshotID, field, err)
}

func readerStorageError(
	operation string,
	snapshotID rkcstore.SnapshotID,
	field string,
	err error,
) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, ErrCanceled) {
		return readerCanceledError(operation, err)
	}
	kind := classifyDatabaseError(err)
	code := rkcstore.CodeInternal
	if kind == ErrBusy || errors.Is(err, ErrClosed) {
		code = rkcstore.CodeConflict
	}
	return readerOperationError(code, operation, snapshotID, field, err)
}
