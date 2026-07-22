package rkcstore

import (
	"context"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type BuildID string
type SnapshotID string
type RepositoryID string

type BuildOptions struct {
	RepositoryID     RepositoryID      `json:"repository_id"`
	ParentSnapshotID SnapshotID        `json:"parent_snapshot_id,omitempty"`
	ExpectedSchema   string            `json:"expected_schema,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// ValidationResult reports semantic validity without confusing invalid input
// with an infrastructure failure. ExpectedCoverage uses provisional snapshot
// identity during Validate; Commit performs exact snapshot-bound comparison.
type ValidationResult struct {
	Report             rkcmodel.ValidationReport `json:"report"`
	ExpectedCoverage   rkcmodel.Coverage         `json:"expected_coverage"`
	CoveragePresent    bool                      `json:"coverage_present"`
	CoverageConsistent bool                      `json:"coverage_consistent"`
}

func (result ValidationResult) Valid() bool {
	return !result.Report.HasErrors() && result.CoveragePresent && result.CoverageConsistent
}

type SnapshotWriter interface {
	BeginBuild(ctx context.Context, opts BuildOptions) (BuildID, error)
	PutArtifacts(ctx context.Context, build BuildID, values []rkcmodel.Artifact) error
	PutNodes(ctx context.Context, build BuildID, values []rkcmodel.Node) error
	PutEdges(ctx context.Context, build BuildID, values []rkcmodel.Edge) error
	PutEvidence(ctx context.Context, build BuildID, values []rkcmodel.Evidence) error
	PutDiagnostics(ctx context.Context, build BuildID, values []rkcmodel.Diagnostic) error
	PutConflicts(ctx context.Context, build BuildID, values []rkcmodel.Conflict) error
	PutDocuments(ctx context.Context, build BuildID, values []rkcmodel.Document) error
	PutClaims(ctx context.Context, build BuildID, values []rkcmodel.Claim) error
	PutPaths(ctx context.Context, build BuildID, values []rkcmodel.ExecutionPath) error
	PutCoverage(ctx context.Context, build BuildID, coverage rkcmodel.Coverage) error
	Validate(ctx context.Context, build BuildID) (ValidationResult, error)
	Commit(ctx context.Context, build BuildID, snapshot rkcmodel.Snapshot) error
	Abort(ctx context.Context, build BuildID, reason error) error
}

type RecoveryResult struct {
	AbortedBuilds []BuildID `json:"aborted_builds"`
}

type BuildRecoverer interface {
	Recover(ctx context.Context) (RecoveryResult, error)
}
