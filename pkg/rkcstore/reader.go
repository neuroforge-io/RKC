package rkcstore

import (
	"context"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type SnapshotReader interface {
	Snapshot(ctx context.Context, id SnapshotID) (rkcmodel.Snapshot, error)
	// Bundle returns the complete canonically ordered representation required
	// for lossless portable export. The returned bundle is a defensive clone.
	Bundle(ctx context.Context, id SnapshotID) (rkcmodel.Bundle, error)
	Current(ctx context.Context, repositoryID RepositoryID) (rkcmodel.Snapshot, error)
	ListSnapshots(ctx context.Context, query SnapshotQuery) (SnapshotPage, error)
	Artifact(ctx context.Context, snapshotID SnapshotID, artifactID string) (rkcmodel.Artifact, error)
	Node(ctx context.Context, snapshotID SnapshotID, nodeID string) (rkcmodel.Node, error)
	Evidence(ctx context.Context, snapshotID SnapshotID, evidenceID string) (rkcmodel.Evidence, error)
	QueryNodes(ctx context.Context, query NodeQuery) (NodePage, error)
	QueryEdges(ctx context.Context, query EdgeQuery) (EdgePage, error)
	QueryDiagnostics(ctx context.Context, query DiagnosticQuery) (DiagnosticPage, error)
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
