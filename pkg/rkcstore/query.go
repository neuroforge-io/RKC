package rkcstore

import "github.com/neuroforge-io/RKC/pkg/rkcmodel"

const (
	// DefaultPageSize is used when a query does not specify a limit.
	DefaultPageSize = 50
	// MaxPageSize is the hard upper bound for every stable reader API.
	MaxPageSize  = 200
	maxCursorLen = 4096
)

// Cursor is an opaque continuation token. Clients must only persist and return
// it to the same store and query; its representation is deliberately private.
type Cursor string

type PageRequest struct {
	Limit  int    `json:"limit,omitempty"`
	Cursor Cursor `json:"cursor,omitempty"`
}

type SnapshotQuery struct {
	RepositoryID RepositoryID `json:"repository_id,omitempty"`
	PageRequest
}

type NodeQuery struct {
	SnapshotID SnapshotID `json:"snapshot_id"`
	Kind       string     `json:"kind,omitempty"`
	Language   string     `json:"language,omitempty"`
	ArtifactID string     `json:"artifact_id,omitempty"`
	Visibility string     `json:"visibility,omitempty"`
	PageRequest
}

type EdgeQuery struct {
	SnapshotID SnapshotID `json:"snapshot_id"`
	Kind       string     `json:"kind,omitempty"`
	From       string     `json:"from,omitempty"`
	To         string     `json:"to,omitempty"`
	Resolution string     `json:"resolution,omitempty"`
	PageRequest
}

type DiagnosticQuery struct {
	SnapshotID SnapshotID `json:"snapshot_id"`
	Severity   string     `json:"severity,omitempty"`
	Code       string     `json:"code,omitempty"`
	Stage      string     `json:"stage,omitempty"`
	PageRequest
}

// Page is an immutable query result view. Next is empty at end-of-results.
type Page[T any] struct {
	Items []T    `json:"items"`
	Next  Cursor `json:"next_cursor,omitempty"`
}

type SnapshotPage = Page[rkcmodel.Snapshot]
type NodePage = Page[rkcmodel.Node]
type EdgePage = Page[rkcmodel.Edge]
type DiagnosticPage = Page[rkcmodel.Diagnostic]

func pageLimit(operation string, request PageRequest) (int, error) {
	if request.Limit < 0 {
		return 0, invalidQuery(operation, "limit", "limit must not be negative")
	}
	if request.Limit > MaxPageSize {
		return 0, invalidQuery(operation, "limit", "limit exceeds MaxPageSize")
	}
	if len(request.Cursor) > maxCursorLen {
		return 0, invalidCursor(operation, "cursor exceeds the safety limit")
	}
	if request.Limit == 0 {
		return DefaultPageSize, nil
	}
	return request.Limit, nil
}
