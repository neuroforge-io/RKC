package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcstore"
)

const (
	ftsMaximumQueryBytes     = 16 * 1024
	ftsMaximumTerms          = 64
	ftsMaximumTermBytes      = 256
	ftsMaximumFilterValues   = 64
	ftsMaximumFilterBytes    = 512
	ftsMaximumBodyBytes      = 64 * 1024
	ftsMaximumResponseBytes  = 16 * 1024 * 1024
	ftsDefaultResultLimit    = 50
	ftsMaximumResultLimit    = 1000
	ftsCandidateMultiplier   = 4
	ftsMaximumCandidateLimit = ftsMaximumResultLimit * ftsCandidateMultiplier
)

// SearchFTS queries the immutable FTS5 projection for one committed snapshot.
// User text is tokenized into quoted literals instead of being interpreted as
// FTS syntax. This keeps the query surface deterministic and prevents callers
// from injecting expensive or surprising MATCH operators.
func (d *Database) SearchFTS(
	ctx context.Context,
	snapshotID rkcstore.SnapshotID,
	query search.Query,
) (search.Response, error) {
	const operation = "search FTS"
	if err := readerValidateID(ctx, operation, "snapshot_id", string(snapshotID)); err != nil {
		return search.Response{}, err
	}
	terms, match, err := ftsMatchExpression(query.Text)
	if err != nil {
		return search.Response{}, readerOperationError(
			rkcstore.CodeInvalidQuery, operation, snapshotID, "text", err,
		)
	}
	limit, err := ftsResultLimit(query.Limit)
	if err != nil {
		return search.Response{}, readerOperationError(
			rkcstore.CodeInvalidQuery, operation, snapshotID, "limit", err,
		)
	}
	filters, err := ftsQueryFilters(query)
	if err != nil {
		return search.Response{}, readerOperationError(
			rkcstore.CodeInvalidQuery, operation, snapshotID, "filters", err,
		)
	}
	if len(terms) == 0 {
		return search.Response{
			Query: query.Text, Mode: "sqlite-fts5-bm25", IndexVersion: "sqlite-fts5-1",
		}, nil
	}
	return readerWithConnection(d, ctx, operation, func(connection *sql.Conn) (search.Response, error) {
		exists, err := readerSnapshotExists(ctx, connection, snapshotID)
		if err != nil {
			return search.Response{}, readerStorageError(operation, snapshotID, "database", err)
		}
		if !exists {
			return search.Response{}, readerSnapshotNotFound(operation, snapshotID, "")
		}
		statement, arguments := ftsStatement(snapshotID, match, filters, limit)
		rows, err := connection.QueryContext(ctx, statement, arguments...)
		if err != nil {
			return search.Response{}, readerStorageError(operation, snapshotID, "search_fts", err)
		}
		defer rows.Close()

		response := search.Response{
			Query: query.Text, Mode: "sqlite-fts5-bm25", IndexVersion: "sqlite-fts5-1",
			Hits: make([]search.Hit, 0, limit),
		}
		responseBytes := 0
		for rows.Next() {
			var document search.Document
			var rank float64
			var bodyBytes int64
			if err := rows.Scan(
				&document.ID,
				&document.ObjectType,
				&document.Title,
				&document.QualifiedName,
				&document.Signature,
				&document.Body,
				&document.Kind,
				&document.Language,
				&document.Path,
				&rank,
				&bodyBytes,
			); err != nil {
				return search.Response{}, readerStorageError(operation, snapshotID, "search_fts", err)
			}
			if len(response.Hits) >= limit {
				response.Truncated = true
				break
			}
			document.Body = ftsBoundUTF8(document.Body, ftsMaximumBodyBytes)
			if err := ftsValidateStoredDocument(document, bodyBytes); err != nil {
				return search.Response{}, readerStoredDataError(
					operation, snapshotID, "search_fts", "invalid projected search document", err,
				)
			}
			reasons := []string{"fts5:bm25"}
			if bodyBytes > ftsMaximumBodyBytes {
				reasons = append(reasons, "body:truncated")
			}
			size := len(document.ID) + len(document.ObjectType) + len(document.Title) +
				len(document.QualifiedName) + len(document.Signature) + len(document.Body) +
				len(document.Kind) + len(document.Language) + len(document.Path)
			if responseBytes > ftsMaximumResponseBytes-size {
				response.Truncated = true
				break
			}
			responseBytes += size
			response.Hits = append(response.Hits, search.Hit{
				Document: document,
				Score:    math.Round((-rank)*1_000_000) / 1_000_000,
				Reasons:  reasons,
				Terms:    append([]string(nil), terms...),
			})
		}
		if err := rows.Err(); err != nil {
			return search.Response{}, readerStorageError(operation, snapshotID, "search_fts", err)
		}
		return response, nil
	})
}

type ftsFilters struct {
	kinds, languages, objectTypes []string
	pathPrefix                    string
}

func ftsQueryFilters(query search.Query) (ftsFilters, error) {
	kinds, err := ftsFilterValues(query.Kinds)
	if err != nil {
		return ftsFilters{}, fmt.Errorf("kinds: %w", err)
	}
	languages, err := ftsFilterValues(query.Languages)
	if err != nil {
		return ftsFilters{}, fmt.Errorf("languages: %w", err)
	}
	objectTypes, err := ftsFilterValues(query.ObjectTypes)
	if err != nil {
		return ftsFilters{}, fmt.Errorf("object types: %w", err)
	}
	if !utf8.ValidString(query.PathPrefix) || len(query.PathPrefix) > ftsMaximumQueryBytes ||
		strings.IndexByte(query.PathPrefix, 0) >= 0 {
		return ftsFilters{}, errors.New("path prefix is not bounded UTF-8 text")
	}
	return ftsFilters{
		kinds: kinds, languages: languages, objectTypes: objectTypes,
		pathPrefix: query.PathPrefix,
	}, nil
}

func ftsFilterValues(values map[string]struct{}) ([]string, error) {
	if len(values) > ftsMaximumFilterValues {
		return nil, fmt.Errorf("filter has more than %d values", ftsMaximumFilterValues)
	}
	result := make([]string, 0, len(values))
	for value := range values {
		if value == "" || len(value) > ftsMaximumFilterBytes || !utf8.ValidString(value) ||
			strings.IndexByte(value, 0) >= 0 {
			return nil, errors.New("filter values must be bounded non-empty UTF-8 text")
		}
		result = append(result, value)
	}
	sort.Strings(result)
	return result, nil
}

func ftsMatchExpression(text string) ([]string, string, error) {
	if len(text) > ftsMaximumQueryBytes || !utf8.ValidString(text) ||
		strings.IndexByte(text, 0) >= 0 {
		return nil, "", errors.New("query must be bounded UTF-8 text")
	}
	var terms []string
	seen := make(map[string]struct{})
	var current strings.Builder
	flush := func() error {
		if current.Len() == 0 {
			return nil
		}
		term := strings.ToLower(current.String())
		current.Reset()
		if len(term) > ftsMaximumTermBytes {
			return fmt.Errorf("query term exceeds %d bytes", ftsMaximumTermBytes)
		}
		if _, ok := seen[term]; ok {
			return nil
		}
		if len(terms) >= ftsMaximumTerms {
			return fmt.Errorf("query has more than %d terms", ftsMaximumTerms)
		}
		seen[term] = struct{}{}
		terms = append(terms, term)
		return nil
	}
	for _, character := range text {
		if unicode.IsLetter(character) || unicode.IsNumber(character) ||
			strings.ContainsRune("_:.#/-", character) {
			current.WriteRune(character)
			continue
		}
		if err := flush(); err != nil {
			return nil, "", err
		}
	}
	if err := flush(); err != nil {
		return nil, "", err
	}
	quoted := make([]string, len(terms))
	for index, term := range terms {
		quoted[index] = `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
	}
	return terms, strings.Join(quoted, " OR "), nil
}

func ftsResultLimit(value int) (int, error) {
	if value == 0 {
		return ftsDefaultResultLimit, nil
	}
	if value < 0 || value > ftsMaximumResultLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d", ftsMaximumResultLimit)
	}
	return value, nil
}

func ftsStatement(
	snapshotID rkcstore.SnapshotID,
	match string,
	filters ftsFilters,
	limit int,
) (string, []any) {
	statement := `
WITH ranked AS (
  SELECT search_fts.object_id,
         search_fts.object_type,
         search_fts.title,
         search_fts.qualified_name,
         search_fts.signature,
         substr(search_fts.body, 1, ?) AS body,
         COALESCE(node.kind, artifact.kind, document.kind,
                  CASE search_fts.object_type
                    WHEN 'document_section' THEN 'section'
                    WHEN 'claim' THEN claim.category
                    ELSE ''
                  END, '') AS kind,
         COALESCE(node.language, artifact.language, '') AS language,
         COALESCE(node_artifact.path, artifact.path, document.path,
                  section_document.path, '') AS path,
         bm25(search_fts, 0.0, 0.0, 0.0, 8.0, 7.0, 6.0, 1.0) AS rank,
         length(CAST(search_fts.body AS BLOB)) AS body_bytes
    FROM search_fts
    LEFT JOIN nodes AS node
      ON search_fts.object_type = 'node'
     AND node.snapshot_id = search_fts.snapshot_id
     AND node.node_id = search_fts.object_id
    LEFT JOIN artifacts AS node_artifact
      ON node_artifact.snapshot_id = node.snapshot_id
     AND node_artifact.artifact_id = node.artifact_id
    LEFT JOIN artifacts AS artifact
      ON search_fts.object_type = 'artifact'
     AND artifact.snapshot_id = search_fts.snapshot_id
     AND artifact.artifact_id = search_fts.object_id
    LEFT JOIN documents AS document
      ON search_fts.object_type = 'document'
     AND document.snapshot_id = search_fts.snapshot_id
     AND document.document_id = search_fts.object_id
    LEFT JOIN document_sections AS section
      ON search_fts.object_type = 'document_section'
     AND section.snapshot_id = search_fts.snapshot_id
     AND section.section_id = search_fts.object_id
    LEFT JOIN documents AS section_document
      ON section_document.snapshot_id = section.snapshot_id
     AND section_document.document_id = section.document_id
    LEFT JOIN claims AS claim
      ON search_fts.object_type = 'claim'
     AND claim.snapshot_id = search_fts.snapshot_id
     AND claim.claim_id = search_fts.object_id
   WHERE search_fts MATCH ?
     AND search_fts.snapshot_id = ?
)
SELECT object_id, object_type, title, qualified_name, signature, body,
       kind, language, path, rank, body_bytes
  FROM ranked
 WHERE 1 = 1`
	arguments := []any{ftsMaximumBodyBytes, match, snapshotID}
	statement, arguments = ftsAppendSetFilter(statement, arguments, "kind", filters.kinds)
	statement, arguments = ftsAppendSetFilter(statement, arguments, "language", filters.languages)
	statement, arguments = ftsAppendSetFilter(
		statement, arguments, "object_type", filters.objectTypes,
	)
	if filters.pathPrefix != "" {
		statement += " AND substr(path, 1, ?) = ?"
		arguments = append(arguments, utf8.RuneCountInString(filters.pathPrefix), filters.pathPrefix)
	}
	candidateLimit := limit * ftsCandidateMultiplier
	if candidateLimit > ftsMaximumCandidateLimit {
		candidateLimit = ftsMaximumCandidateLimit
	}
	statement += " ORDER BY rank ASC, object_type ASC, object_id ASC LIMIT ?"
	arguments = append(arguments, candidateLimit+1)
	return statement, arguments
}

func ftsAppendSetFilter(
	statement string,
	arguments []any,
	column string,
	values []string,
) (string, []any) {
	if len(values) == 0 {
		return statement, arguments
	}
	statement += " AND " + column + " IN (" + strings.TrimSuffix(strings.Repeat("?,", len(values)), ",") + ")"
	for _, value := range values {
		arguments = append(arguments, value)
	}
	return statement, arguments
}

func ftsValidateStoredDocument(document search.Document, bodyBytes int64) error {
	if document.ID == "" || document.ObjectType == "" {
		return errors.New("identity fields are empty")
	}
	if bodyBytes < 0 {
		return errors.New("body size is negative")
	}
	for _, value := range []string{
		document.ID, document.ObjectType, document.Title, document.QualifiedName,
		document.Signature, document.Body, document.Kind, document.Language, document.Path,
	} {
		if !utf8.ValidString(value) || strings.IndexByte(value, 0) >= 0 {
			return errors.New("document contains invalid UTF-8 or NUL")
		}
	}
	return nil
}

func ftsBoundUTF8(value string, maximum int) string {
	if len(value) <= maximum {
		return value
	}
	value = value[:maximum]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}
