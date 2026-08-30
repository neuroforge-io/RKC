package rkcstore

import (
	"context"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// BuildID is an opaque identifier for one open, committed, or aborted staging
// transaction. Callers obtain it from SnapshotWriter.BeginBuild.
type BuildID string

// SnapshotID identifies an immutable published snapshot. Its value is supplied
// in rkcmodel.Snapshot at commit time and must be unique within a store.
type SnapshotID string

// RepositoryID identifies one logical repository and its current snapshot.
// A store preserves repository identity affinity across commits.
type RepositoryID string

// BuildOptions binds a staging transaction to one repository head and schema.
type BuildOptions struct {
	// RepositoryID is the required logical repository identity.
	RepositoryID RepositoryID `json:"repository_id"`
	// ParentSnapshotID must equal the repository's current snapshot, or be
	// empty before its first commit.
	ParentSnapshotID SnapshotID `json:"parent_snapshot_id,omitempty"`
	// ExpectedSchema is the schema the committed snapshot must declare. An
	// empty value requests rkcmodel.SchemaVersion.
	ExpectedSchema string `json:"expected_schema,omitempty"`
	// Metadata is bounded caller-defined transaction metadata. Stores must
	// defensively clone it.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// ValidationResult reports semantic validity without confusing invalid input
// with an infrastructure failure. ExpectedCoverage uses provisional snapshot
// identity during Validate; Commit performs exact snapshot-bound comparison.
type ValidationResult struct {
	// Report contains deterministic semantic diagnostics for the provisional
	// bundle.
	Report rkcmodel.ValidationReport `json:"report"`
	// ExpectedCoverage is the derived coverage the caller must stage. Validate
	// uses provisional identity; Commit binds the final snapshot exactly.
	ExpectedCoverage rkcmodel.Coverage `json:"expected_coverage"`
	// CoveragePresent reports whether the build has staged coverage.
	CoveragePresent bool `json:"coverage_present"`
	// CoverageConsistent reports whether staged coverage matches all
	// identity-independent expected values.
	CoverageConsistent bool `json:"coverage_consistent"`
}

// Valid reports whether the bundle has no error diagnostics and contains
// consistent staged coverage. It does not commit or otherwise mutate a build.
func (result ValidationResult) Valid() bool {
	return !result.Report.HasErrors() && result.CoveragePresent && result.CoverageConsistent
}

// SnapshotWriter defines bounded, transactional construction of immutable RKC
// snapshots. Each Put call is atomic, but a sequence of successful Put calls
// remains private until Commit. Callers should Abort a build they will not
// retry or commit.
type SnapshotWriter interface {
	// BeginBuild opens staging against the repository's expected current head.
	BeginBuild(ctx context.Context, opts BuildOptions) (BuildID, error)
	// PutArtifacts stages one bounded batch of uniquely identified artifacts.
	PutArtifacts(ctx context.Context, build BuildID, values []rkcmodel.Artifact) error
	// PutNodes stages one bounded batch of uniquely identified nodes.
	PutNodes(ctx context.Context, build BuildID, values []rkcmodel.Node) error
	// PutEdges stages one bounded batch of uniquely identified edges.
	PutEdges(ctx context.Context, build BuildID, values []rkcmodel.Edge) error
	// PutEvidence stages one bounded batch of uniquely identified evidence.
	PutEvidence(ctx context.Context, build BuildID, values []rkcmodel.Evidence) error
	// PutDiagnostics stages one bounded batch of uniquely identified diagnostics.
	PutDiagnostics(ctx context.Context, build BuildID, values []rkcmodel.Diagnostic) error
	// PutConflicts stages one bounded batch of uniquely identified conflicts.
	PutConflicts(ctx context.Context, build BuildID, values []rkcmodel.Conflict) error
	// PutDocuments stages one bounded batch of uniquely identified documents.
	PutDocuments(ctx context.Context, build BuildID, values []rkcmodel.Document) error
	// PutClaims stages one bounded batch of uniquely identified claims.
	PutClaims(ctx context.Context, build BuildID, values []rkcmodel.Claim) error
	// PutPaths stages one bounded batch of uniquely identified execution paths.
	PutPaths(ctx context.Context, build BuildID, values []rkcmodel.ExecutionPath) error
	// PutCoverage stages or replaces derived coverage for the build.
	PutCoverage(ctx context.Context, build BuildID, coverage rkcmodel.Coverage) error
	// Validate checks staged content without publishing or closing the build.
	Validate(ctx context.Context, build BuildID) (ValidationResult, error)
	// Commit validates and atomically publishes the build as snapshot.
	Commit(ctx context.Context, build BuildID, snapshot rkcmodel.Snapshot) error
	// Abort closes an uncommitted build and releases its staged payload.
	Abort(ctx context.Context, build BuildID, reason error) error
}

// RecoveryResult records the staging transactions closed during recovery.
type RecoveryResult struct {
	// AbortedBuilds contains recovered build identifiers in deterministic order.
	AbortedBuilds []BuildID `json:"aborted_builds"`
}

// BuildRecoverer closes incomplete staging transactions left by an interrupted
// process or abandoned workflow.
type BuildRecoverer interface {
	// Recover aborts every incomplete build and reports their identifiers.
	Recover(ctx context.Context) (RecoveryResult, error)
}
