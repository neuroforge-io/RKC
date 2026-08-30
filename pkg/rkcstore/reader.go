package rkcstore

import (
	"context"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// SnapshotReader provides context-aware, read-only access to immutable RKC
// snapshots. Returned values must not alias mutable store state. Query cursors
// are opaque and valid only for the same reader and unchanged query.
type SnapshotReader interface {
	// Snapshot returns metadata for one published snapshot.
	Snapshot(ctx context.Context, id SnapshotID) (rkcmodel.Snapshot, error)
	// Bundle returns the complete canonically ordered representation required
	// for lossless portable export. The returned bundle is a defensive clone.
	Bundle(ctx context.Context, id SnapshotID) (rkcmodel.Bundle, error)
	// Current returns the current snapshot metadata for one repository.
	Current(ctx context.Context, repositoryID RepositoryID) (rkcmodel.Snapshot, error)
	// ListSnapshots returns a stable page of snapshots matching query.
	ListSnapshots(ctx context.Context, query SnapshotQuery) (SnapshotPage, error)
	// Artifact returns one artifact from a published snapshot.
	Artifact(ctx context.Context, snapshotID SnapshotID, artifactID string) (rkcmodel.Artifact, error)
	// Node returns one node from a published snapshot.
	Node(ctx context.Context, snapshotID SnapshotID, nodeID string) (rkcmodel.Node, error)
	// Evidence returns one evidence record from a published snapshot.
	Evidence(ctx context.Context, snapshotID SnapshotID, evidenceID string) (rkcmodel.Evidence, error)
	// QueryNodes returns a stable page of nodes matching query.
	QueryNodes(ctx context.Context, query NodeQuery) (NodePage, error)
	// QueryEdges returns a stable page of edges matching query.
	QueryEdges(ctx context.Context, query EdgeQuery) (EdgePage, error)
	// QueryDiagnostics returns a stable page of diagnostics matching query.
	QueryDiagnostics(ctx context.Context, query DiagnosticQuery) (DiagnosticPage, error)
	// Coverage returns the coverage record bound to a published snapshot.
	Coverage(ctx context.Context, snapshotID SnapshotID) (rkcmodel.Coverage, error)
}

// Store is the complete local storage contract. Durable implementations may
// expose additional operational controls, but canonical consumers depend only
// on this boundary.
type Store interface {
	SnapshotReader
	SnapshotWriter
	BuildRecoverer
}
