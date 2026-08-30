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

// PageRequest selects one bounded page of a stable query. A zero Limit uses
// DefaultPageSize; positive limits must not exceed MaxPageSize. Cursor must be
// empty for the first page and returned unchanged for the same store and query.
type PageRequest struct {
	// Limit is the maximum number of returned items, or zero for the default.
	Limit int `json:"limit,omitempty"`
	// Cursor is the opaque continuation token returned by the preceding page.
	Cursor Cursor `json:"cursor,omitempty"`
}

// SnapshotQuery lists all snapshots or only those belonging to RepositoryID.
type SnapshotQuery struct {
	// RepositoryID optionally restricts results to one repository.
	RepositoryID RepositoryID `json:"repository_id,omitempty"`
	PageRequest
}

// NodeQuery selects nodes from one immutable snapshot using conjunctive,
// case-sensitive exact-match filters.
type NodeQuery struct {
	// SnapshotID identifies the required published snapshot.
	SnapshotID SnapshotID `json:"snapshot_id"`
	// Kind optionally matches rkcmodel.Node.Kind.
	Kind string `json:"kind,omitempty"`
	// Language optionally matches rkcmodel.Node.Language.
	Language string `json:"language,omitempty"`
	// ArtifactID optionally matches rkcmodel.Node.ArtifactID.
	ArtifactID string `json:"artifact_id,omitempty"`
	// Visibility optionally matches rkcmodel.Node.Visibility.
	Visibility string `json:"visibility,omitempty"`
	PageRequest
}

// EdgeQuery selects edges from one immutable snapshot using conjunctive,
// case-sensitive exact-match filters. Resolution accepts the canonical RKC
// vocabulary after rkcmodel normalization.
type EdgeQuery struct {
	// SnapshotID identifies the required published snapshot.
	SnapshotID SnapshotID `json:"snapshot_id"`
	// Kind optionally matches rkcmodel.Edge.Kind.
	Kind string `json:"kind,omitempty"`
	// From optionally matches rkcmodel.Edge.From.
	From string `json:"from,omitempty"`
	// To optionally matches rkcmodel.Edge.To.
	To string `json:"to,omitempty"`
	// Resolution optionally matches the normalized edge resolution.
	Resolution string `json:"resolution,omitempty"`
	PageRequest
}

// DiagnosticQuery selects diagnostics from one immutable snapshot using
// conjunctive, case-sensitive exact-match filters.
type DiagnosticQuery struct {
	// SnapshotID identifies the required published snapshot.
	SnapshotID SnapshotID `json:"snapshot_id"`
	// Severity optionally matches rkcmodel.Diagnostic.Severity.
	Severity string `json:"severity,omitempty"`
	// Code optionally matches rkcmodel.Diagnostic.Code.
	Code string `json:"code,omitempty"`
	// Stage optionally matches rkcmodel.Diagnostic.Stage.
	Stage string `json:"stage,omitempty"`
	PageRequest
}

// Page is an immutable query result view. Next is empty at end-of-results.
type Page[T any] struct {
	// Items contains the current page in the query's deterministic order.
	Items []T `json:"items"`
	// Next is empty when no later page exists.
	Next Cursor `json:"next_cursor,omitempty"`
}

// SnapshotPage is a page of snapshot metadata.
type SnapshotPage = Page[rkcmodel.Snapshot]

// NodePage is a page of canonical nodes.
type NodePage = Page[rkcmodel.Node]

// EdgePage is a page of canonical edges.
type EdgePage = Page[rkcmodel.Edge]

// DiagnosticPage is a page of canonical diagnostics.
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
