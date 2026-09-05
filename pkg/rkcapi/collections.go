package rkcapi

import "github.com/neuroforge-io/RKC/pkg/rkcmodel"

// CollectionPage is one bounded page of canonical records from an immutable
// snapshot. A cursor continues the same endpoint and exact query/filter
// parameters (including their names and presence); callers
// must not combine pages from different snapshots.
type CollectionPage[T any] struct {
	// Items contains this page's records in the endpoint's deterministic order.
	Items []T `json:"items"`
	// Total counts every record matching the filters, including earlier pages.
	Total int `json:"total"`
	// Truncated reports whether further matching records follow this page.
	Truncated bool `json:"truncated"`
	// SnapshotID identifies the immutable dataset used to produce this page.
	SnapshotID string `json:"snapshot_id"`
	// NextCursor is an opaque continuation token; empty means there is no next
	// page. Send it unchanged with the same endpoint and query/filter parameters.
	// Limit may vary. Restarting the server or reloading the atlas invalidates it.
	NextCursor string `json:"next_cursor,omitempty"`
}

// NodePage contains canonical node records, optionally ordered by lexical rank
// when the nodes endpoint receives a query.
type NodePage = CollectionPage[rkcmodel.Node]

// ArtifactPage contains canonical inventory records.
type ArtifactPage = CollectionPage[rkcmodel.Artifact]

// EdgePage contains canonical graph relationships.
type EdgePage = CollectionPage[rkcmodel.Edge]

// DiagnosticPage contains canonical analyzer and validation findings.
type DiagnosticPage = CollectionPage[rkcmodel.Diagnostic]
