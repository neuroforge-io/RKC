package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

type readerJSONFilter struct {
	path  string
	value string
}

type readerRecordPageSpec[T any] struct {
	operation  string
	kind       string
	family     string
	snapshotID rkcstore.SnapshotID
	scope      string
	request    rkcstore.PageRequest
	filters    []readerJSONFilter
	identifier func(T) string
}

func (d *Database) ListSnapshots(
	ctx context.Context,
	query rkcstore.SnapshotQuery,
) (rkcstore.SnapshotPage, error) {
	const operation = "list snapshots"
	if err := readerValidateContext(ctx, operation); err != nil {
		return rkcstore.SnapshotPage{}, err
	}
	limit, err := readerPageLimit(operation, query.PageRequest)
	if err != nil {
		return rkcstore.SnapshotPage{}, err
	}
	if query.RepositoryID != "" {
		if err := readerValidateID(ctx, operation, "repository_id", string(query.RepositoryID)); err != nil {
			return rkcstore.SnapshotPage{}, err
		}
	}
	scope := readerScopeFingerprint(string(query.RepositoryID))
	return readerWithConnection(d, ctx, operation, func(connection *sql.Conn) (rkcstore.SnapshotPage, error) {
		key, err := readerCursorKey(ctx, connection, operation, !d.options.ReadOnly)
		if err != nil {
			return rkcstore.SnapshotPage{}, err
		}
		cursor, err := readerOpenCursor(operation, key, query.Cursor, "snapshots", scope)
		if err != nil {
			return rkcstore.SnapshotPage{}, err
		}
		if err := readerValidateSnapshotCursor(
			ctx,
			connection,
			operation,
			query.RepositoryID,
			cursor,
		); err != nil {
			return rkcstore.SnapshotPage{}, err
		}

		statement := `SELECT snapshot_id, repository_id, schema_version,
		                     published_at,
		                     length(CAST(canonical_snapshot_json AS BLOB)),
		                     CASE
		                       WHEN length(CAST(canonical_snapshot_json AS BLOB)) <= ?
		                       THEN canonical_snapshot_json
		                     END
		              FROM canonical_snapshots
		              WHERE 1 = 1`
		arguments := []any{readerMaxObjectJSONBytes}
		if query.RepositoryID != "" {
			statement += " AND repository_id = ?"
			arguments = append(arguments, query.RepositoryID)
		}
		if cursor.ID != "" {
			statement += ` AND (
			  published_at < ?
			  OR (published_at = ? AND snapshot_id > ?)
			)`
			arguments = append(arguments, cursor.Primary, cursor.Primary, cursor.ID)
		}
		statement += " ORDER BY published_at DESC, snapshot_id ASC LIMIT ?"
		arguments = append(arguments, limit+1)
		rows, err := connection.QueryContext(ctx, statement, arguments...)
		if err != nil {
			return rkcstore.SnapshotPage{}, readerStorageError(operation, "", "database", err)
		}
		defer rows.Close()

		items := make([]rkcmodel.Snapshot, 0, limit)
		var pageBytes int
		var lastPublished string
		hasMore := false
		for rows.Next() {
			var rowID, repositoryID, schemaVersion, publishedAt string
			var size int64
			var payload sql.NullString
			if err := rows.Scan(
				&rowID,
				&repositoryID,
				&schemaVersion,
				&publishedAt,
				&size,
				&payload,
			); err != nil {
				return rkcstore.SnapshotPage{}, readerStorageError(operation, "", "database", err)
			}
			if len(items) >= limit {
				hasMore = true
				break
			}
			if size > readerMaxObjectJSONBytes || !payload.Valid {
				return rkcstore.SnapshotPage{}, readerResourceError(
					operation,
					rkcstore.SnapshotID(rowID),
					"snapshot",
					"stored snapshot exceeds the durable reader limit",
				)
			}
			if pageBytes > readerMaxPageJSONBytes-len(payload.String) {
				hasMore = true
				break
			}
			if err := readerValidatePublishedAt(publishedAt); err != nil {
				return rkcstore.SnapshotPage{}, readerStoredDataError(
					operation,
					rkcstore.SnapshotID(rowID),
					"published_at",
					"snapshot publication time is not canonical UTC",
					err,
				)
			}
			snapshot, err := readerDecodeSnapshot(
				operation,
				rkcstore.SnapshotID(rowID),
				rowID,
				repositoryID,
				schemaVersion,
				payload.String,
			)
			if err != nil {
				return rkcstore.SnapshotPage{}, err
			}
			items = append(items, snapshot)
			pageBytes += len(payload.String)
			lastPublished = publishedAt
		}
		if err := rows.Err(); err != nil {
			return rkcstore.SnapshotPage{}, readerStorageError(operation, "", "database", err)
		}
		page := rkcstore.SnapshotPage{Items: items}
		if hasMore && len(items) != 0 {
			last := items[len(items)-1]
			page.Next, err = readerSealCursor(
				operation,
				key,
				"snapshots",
				scope,
				lastPublished,
				last.ID,
			)
		}
		return page, err
	})
}

func (d *Database) QueryNodes(
	ctx context.Context,
	query rkcstore.NodeQuery,
) (rkcstore.NodePage, error) {
	const operation = "query nodes"
	if err := readerValidateQuery(
		ctx,
		operation,
		query.SnapshotID,
		query.PageRequest,
		map[string]string{
			"kind": query.Kind, "language": query.Language,
			"artifact_id": query.ArtifactID, "visibility": query.Visibility,
		},
	); err != nil {
		return rkcstore.NodePage{}, err
	}
	scope := readerScopeFingerprint(
		string(query.SnapshotID),
		query.Kind,
		query.Language,
		query.ArtifactID,
		query.Visibility,
	)
	items, next, err := readerQueryRecordPage(
		d,
		ctx,
		readerRecordPageSpec[rkcmodel.Node]{
			operation: operation, kind: "nodes", family: "node",
			snapshotID: query.SnapshotID, scope: scope, request: query.PageRequest,
			filters: []readerJSONFilter{
				{"$.kind", query.Kind},
				{"$.language", query.Language},
				{"$.artifact_id", query.ArtifactID},
				{"$.visibility", query.Visibility},
			},
			identifier: func(value rkcmodel.Node) string { return value.ID },
		},
	)
	return rkcstore.NodePage{Items: items, Next: next}, err
}

func (d *Database) QueryEdges(
	ctx context.Context,
	query rkcstore.EdgeQuery,
) (rkcstore.EdgePage, error) {
	const operation = "query edges"
	if err := readerValidateContext(ctx, operation); err != nil {
		return rkcstore.EdgePage{}, err
	}
	resolution := query.Resolution
	if resolution != "" {
		resolution = rkcmodel.NormalizeResolution(resolution)
		if !rkcmodel.IsKnownResolution(resolution) {
			return rkcstore.EdgePage{}, readerOperationError(
				rkcstore.CodeInvalidQuery,
				operation,
				query.SnapshotID,
				"resolution",
				errors.New("unknown resolution"),
			)
		}
	}
	if err := readerValidateQuery(
		ctx,
		operation,
		query.SnapshotID,
		query.PageRequest,
		map[string]string{
			"kind": query.Kind, "from": query.From,
			"to": query.To, "resolution": resolution,
		},
	); err != nil {
		return rkcstore.EdgePage{}, err
	}
	scope := readerScopeFingerprint(
		string(query.SnapshotID),
		query.Kind,
		query.From,
		query.To,
		resolution,
	)
	items, next, err := readerQueryRecordPage(
		d,
		ctx,
		readerRecordPageSpec[rkcmodel.Edge]{
			operation: operation, kind: "edges", family: "edge",
			snapshotID: query.SnapshotID, scope: scope, request: query.PageRequest,
			filters: []readerJSONFilter{
				{"$.kind", query.Kind},
				{"$.from", query.From},
				{"$.to", query.To},
				{"$.resolution", resolution},
			},
			identifier: func(value rkcmodel.Edge) string { return value.ID },
		},
	)
	return rkcstore.EdgePage{Items: items, Next: next}, err
}

func (d *Database) QueryDiagnostics(
	ctx context.Context,
	query rkcstore.DiagnosticQuery,
) (rkcstore.DiagnosticPage, error) {
	const operation = "query diagnostics"
	if err := readerValidateQuery(
		ctx,
		operation,
		query.SnapshotID,
		query.PageRequest,
		map[string]string{
			"severity": query.Severity, "code": query.Code, "stage": query.Stage,
		},
	); err != nil {
		return rkcstore.DiagnosticPage{}, err
	}
	scope := readerScopeFingerprint(
		string(query.SnapshotID),
		query.Severity,
		query.Code,
		query.Stage,
	)
	items, next, err := readerQueryRecordPage(
		d,
		ctx,
		readerRecordPageSpec[rkcmodel.Diagnostic]{
			operation: operation, kind: "diagnostics", family: "diagnostic",
			snapshotID: query.SnapshotID, scope: scope, request: query.PageRequest,
			filters: []readerJSONFilter{
				{"$.severity", query.Severity},
				{"$.code", query.Code},
				{"$.stage", query.Stage},
			},
			identifier: func(value rkcmodel.Diagnostic) string { return value.ID },
		},
	)
	return rkcstore.DiagnosticPage{Items: items, Next: next}, err
}

func readerQueryRecordPage[T any](
	database *Database,
	ctx context.Context,
	spec readerRecordPageSpec[T],
) ([]T, rkcstore.Cursor, error) {
	limit, err := readerPageLimit(spec.operation, spec.request)
	if err != nil {
		return nil, "", err
	}
	return readerQueryRecordPageImpl(database, ctx, spec, limit)
}

// readerWithConnectionPage is the page-shaped lifecycle wrapper. Go functions
// cannot return a tuple through readerWithConnection, so the tuple is carried in
// this private value without exposing mutable storage state.
type readerPageResult[T any] struct {
	items []T
	next  rkcstore.Cursor
}

func readerQueryRecordPageImpl[T any](
	database *Database,
	ctx context.Context,
	spec readerRecordPageSpec[T],
	limit int,
) ([]T, rkcstore.Cursor, error) {
	result, err := readerWithConnection(database, ctx, spec.operation, func(connection *sql.Conn) (readerPageResult[T], error) {
		exists, err := readerSnapshotExists(ctx, connection, spec.snapshotID)
		if err != nil {
			return readerPageResult[T]{}, readerStorageError(spec.operation, spec.snapshotID, "database", err)
		}
		if !exists {
			return readerPageResult[T]{}, readerSnapshotNotFound(spec.operation, spec.snapshotID, "")
		}
		key, err := readerCursorKey(ctx, connection, spec.operation, !database.options.ReadOnly)
		if err != nil {
			return readerPageResult[T]{}, err
		}
		cursor, err := readerOpenCursor(spec.operation, key, spec.request.Cursor, spec.kind, spec.scope)
		if err != nil {
			return readerPageResult[T]{}, err
		}
		if cursor.Primary != "" {
			return readerPageResult[T]{}, readerInvalidCursor(spec.operation, "cursor sort key is invalid")
		}
		if cursor.ID != "" {
			positionExists, err := readerRecordCursorExists(ctx, connection, spec, cursor.ID)
			if err != nil {
				return readerPageResult[T]{}, readerStorageError(spec.operation, spec.snapshotID, "cursor", err)
			}
			if !positionExists {
				return readerPageResult[T]{}, readerInvalidCursor(spec.operation, "cursor position no longer exists")
			}
		}

		statement := `SELECT record_id,
		                     length(CAST(canonical_record_json AS BLOB)),
		                     CASE
		                       WHEN length(CAST(canonical_record_json AS BLOB)) <= ?
		                       THEN canonical_record_json
		                     END,
		                     canonical_record_sha256
		              FROM canonical_snapshot_records
		              WHERE snapshot_id = ?
		                AND record_family = ?
		                AND record_id > ?`
		arguments := []any{
			readerMaxObjectJSONBytes,
			spec.snapshotID,
			spec.family,
			cursor.ID,
		}
		statement, arguments = readerAppendFilters(statement, arguments, spec.filters)
		statement += " ORDER BY record_id ASC LIMIT ?"
		arguments = append(arguments, limit+1)

		rows, err := connection.QueryContext(ctx, statement, arguments...)
		if err != nil {
			return readerPageResult[T]{}, readerStorageError(spec.operation, spec.snapshotID, "database", err)
		}
		defer rows.Close()
		items := make([]T, 0, limit)
		pageBytes := 0
		hasMore := false
		for rows.Next() {
			var recordID string
			var size int64
			var payload sql.NullString
			var digest string
			if err := rows.Scan(&recordID, &size, &payload, &digest); err != nil {
				return readerPageResult[T]{}, readerStorageError(spec.operation, spec.snapshotID, "database", err)
			}
			if len(items) >= limit {
				hasMore = true
				break
			}
			if size > readerMaxObjectJSONBytes || !payload.Valid {
				return readerPageResult[T]{}, readerResourceError(
					spec.operation,
					spec.snapshotID,
					spec.family,
					"stored record exceeds the durable reader limit",
				)
			}
			if pageBytes > readerMaxPageJSONBytes-len(payload.String) {
				hasMore = true
				break
			}
			if err := readerVerifyDigest(
				spec.operation,
				spec.snapshotID,
				spec.family,
				payload.String,
				digest,
			); err != nil {
				return readerPageResult[T]{}, err
			}
			value, err := readerDecodeObject[T](
				spec.operation,
				spec.snapshotID,
				spec.family,
				payload.String,
				readerMaxObjectJSONBytes,
			)
			if err != nil {
				return readerPageResult[T]{}, err
			}
			if spec.identifier(value) != recordID {
				return readerPageResult[T]{}, readerStoredDataError(
					spec.operation,
					spec.snapshotID,
					spec.family,
					"record identity does not match its row",
					nil,
				)
			}
			items = append(items, value)
			pageBytes += len(payload.String)
		}
		if err := rows.Err(); err != nil {
			return readerPageResult[T]{}, readerStorageError(spec.operation, spec.snapshotID, "database", err)
		}
		result := readerPageResult[T]{items: items}
		if hasMore && len(items) != 0 {
			result.next, err = readerSealCursor(
				spec.operation,
				key,
				spec.kind,
				spec.scope,
				"",
				spec.identifier(items[len(items)-1]),
			)
		}
		return result, err
	})
	if err != nil {
		return nil, "", err
	}
	return result.items, result.next, nil
}

func readerSnapshotExists(
	ctx context.Context,
	connection *sql.Conn,
	snapshotID rkcstore.SnapshotID,
) (bool, error) {
	var exists int
	err := connection.QueryRowContext(
		ctx,
		"SELECT EXISTS(SELECT 1 FROM canonical_snapshots WHERE snapshot_id = ?)",
		snapshotID,
	).Scan(&exists)
	return exists != 0, err
}

func readerRecordCursorExists[T any](
	ctx context.Context,
	connection *sql.Conn,
	spec readerRecordPageSpec[T],
	recordID string,
) (bool, error) {
	statement := `SELECT EXISTS(
	  SELECT 1
	  FROM canonical_snapshot_records
	  WHERE snapshot_id = ?
	    AND record_family = ?
	    AND record_id = ?`
	arguments := []any{spec.snapshotID, spec.family, recordID}
	statement, arguments = readerAppendFilters(statement, arguments, spec.filters)
	statement += ")"
	var exists int
	err := connection.QueryRowContext(ctx, statement, arguments...).Scan(&exists)
	return exists != 0, err
}

func readerAppendFilters(
	statement string,
	arguments []any,
	filters []readerJSONFilter,
) (string, []any) {
	for _, filter := range filters {
		if filter.value == "" {
			continue
		}
		statement += " AND json_extract(canonical_record_json, ?) = ?"
		arguments = append(arguments, filter.path, filter.value)
	}
	return statement, arguments
}

func readerValidateSnapshotCursor(
	ctx context.Context,
	connection *sql.Conn,
	operation string,
	repositoryID rkcstore.RepositoryID,
	cursor readerCursorPayload,
) error {
	if cursor.ID == "" {
		return nil
	}
	if cursor.Primary == "" {
		return readerInvalidCursor(operation, "snapshot cursor has no publication time")
	}
	if err := readerValidatePublishedAt(cursor.Primary); err != nil {
		return readerInvalidCursor(operation, "snapshot cursor publication time is invalid")
	}
	statement := `SELECT EXISTS(
	  SELECT 1 FROM canonical_snapshots
	  WHERE snapshot_id = ? AND published_at = ?`
	arguments := []any{cursor.ID, cursor.Primary}
	if repositoryID != "" {
		statement += " AND repository_id = ?"
		arguments = append(arguments, repositoryID)
	}
	statement += ")"
	var exists int
	if err := connection.QueryRowContext(ctx, statement, arguments...).Scan(&exists); err != nil {
		return readerStorageError(operation, "", "cursor", err)
	}
	if exists == 0 {
		return readerInvalidCursor(operation, "cursor position no longer exists")
	}
	return nil
}

func readerValidatePublishedAt(value string) error {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return err
	}
	if parsed.UTC().Format(time.RFC3339Nano) != value {
		return fmt.Errorf("time is not canonical UTC")
	}
	return nil
}

func readerValidateQuery(
	ctx context.Context,
	operation string,
	snapshotID rkcstore.SnapshotID,
	request rkcstore.PageRequest,
	filters map[string]string,
) error {
	if err := readerValidateID(ctx, operation, "snapshot_id", string(snapshotID)); err != nil {
		return err
	}
	if _, err := readerPageLimit(operation, request); err != nil {
		return err
	}
	for field, value := range filters {
		if len(value) > rkcstore.MaxIdentifierSize {
			return readerOperationError(
				rkcstore.CodeInvalidQuery,
				operation,
				snapshotID,
				field,
				errors.New("filter exceeds MaxIdentifierSize"),
			)
		}
	}
	return nil
}

func readerPageLimit(
	operation string,
	request rkcstore.PageRequest,
) (int, error) {
	if request.Limit < 0 {
		return 0, readerOperationError(
			rkcstore.CodeInvalidQuery,
			operation,
			"",
			"limit",
			errors.New("limit must not be negative"),
		)
	}
	if request.Limit > rkcstore.MaxPageSize {
		return 0, readerOperationError(
			rkcstore.CodeInvalidQuery,
			operation,
			"",
			"limit",
			errors.New("limit exceeds MaxPageSize"),
		)
	}
	if len(request.Cursor) > readerMaxCursorBytes {
		return 0, readerInvalidCursor(operation, "cursor exceeds the safety limit")
	}
	if request.Limit == 0 {
		return rkcstore.DefaultPageSize, nil
	}
	return request.Limit, nil
}
