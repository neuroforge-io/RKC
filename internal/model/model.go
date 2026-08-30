// Package model is a compatibility facade retained for internal packages that
// predate the public canonical model. New code should import pkg/rkcmodel.
package model

import "github.com/neuroforge-io/RKC/pkg/rkcmodel"

// SchemaVersion mirrors the authoritative public schema version so legacy
// internal imports serialize the same canonical contract as pkg/rkcmodel.
const SchemaVersion = rkcmodel.SchemaVersion

// ResolutionUnresolved mirrors the public unresolved-edge vocabulary value;
// it is not a second internal resolution policy.
const ResolutionUnresolved = rkcmodel.ResolutionUnresolved

// SourceRange aliases the public source-coordinate contract for legacy
// internal imports.
type SourceRange = rkcmodel.SourceRange

// GitInfo aliases the public, privacy-filtered Git provenance record.
type GitInfo = rkcmodel.GitInfo

// ToolInfo aliases the public producer identity recorded on snapshots.
type ToolInfo = rkcmodel.ToolInfo

// Snapshot aliases the public immutable snapshot identity and provenance
// envelope.
type Snapshot = rkcmodel.Snapshot

// Artifact aliases the public admitted-file inventory record.
type Artifact = rkcmodel.Artifact

// Node aliases the public canonical knowledge-graph entity contract.
type Node = rkcmodel.Node

// Edge aliases the public typed relationship and resolution contract.
type Edge = rkcmodel.Edge

// Evidence aliases the public source-grounding record attached to facts.
type Evidence = rkcmodel.Evidence

// Diagnostic aliases the public analyzer and validation diagnostic record.
type Diagnostic = rkcmodel.Diagnostic

// Conflict aliases the public representation of competing evidence-backed
// facts.
type Conflict = rkcmodel.Conflict

// Claim aliases the public citation-bound generated-claim record.
type Claim = rkcmodel.Claim

// DocumentSection aliases the public ordered, evidence-linked section record.
type DocumentSection = rkcmodel.DocumentSection

// Document aliases the public deterministic documentation product contract.
type Document = rkcmodel.Document

// ExecutionPath aliases the public bounded execution-path evidence record.
type ExecutionPath = rkcmodel.ExecutionPath

// Fragment aliases the public analyzer output merged into a canonical bundle.
type Fragment = rkcmodel.Fragment

// Bundle aliases the public complete canonical snapshot data model.
type Bundle = rkcmodel.Bundle

// Coverage aliases the public accounting and evidence-ratio report.
type Coverage = rkcmodel.Coverage

// ValidationOptions aliases the public strictness controls for bundle
// validation.
type ValidationOptions = rkcmodel.ValidationOptions

// ValidationReport aliases the public deterministic validation outcome.
type ValidationReport = rkcmodel.ValidationReport

// StableID delegates legacy internal calls to the public deterministic
// identifier algorithm.
var StableID = rkcmodel.StableID

// SortBundle delegates to the public canonical ordering implementation.
var SortBundle = rkcmodel.SortBundle

// SortFragment delegates to the public canonical fragment ordering
// implementation.
var SortFragment = rkcmodel.SortFragment

// IsCanonicalDecodedBundle delegates the no-copy canonical-order check to the
// public model implementation.
var IsCanonicalDecodedBundle = rkcmodel.IsCanonicalDecodedBundle

// CanonicalBundle delegates canonical bundle normalization to pkg/rkcmodel.
var CanonicalBundle = rkcmodel.CanonicalBundle

// CanonicalJSON delegates deterministic JSON encoding to pkg/rkcmodel.
var CanonicalJSON = rkcmodel.CanonicalJSON

// CanonicalDigest delegates content-identity hashing to the public canonical
// implementation.
var CanonicalDigest = rkcmodel.CanonicalDigest

// ValidateBundle delegates structural and referential validation to the public
// model contract.
var ValidateBundle = rkcmodel.ValidateBundle

// BuildCoverage delegates evidence and inventory accounting to pkg/rkcmodel.
var BuildCoverage = rkcmodel.BuildCoverage

// IsSymbolKind delegates symbol-kind vocabulary classification to the public
// model implementation.
var IsSymbolKind = rkcmodel.IsSymbolKind

// IsKnownNodeKind delegates node-kind vocabulary validation to pkg/rkcmodel.
var IsKnownNodeKind = rkcmodel.IsKnownNodeKind

// IsKnownEdgeKind delegates edge-kind vocabulary validation to pkg/rkcmodel.
var IsKnownEdgeKind = rkcmodel.IsKnownEdgeKind

// IsKnownEvidenceKind delegates evidence-kind vocabulary validation to the
// public model implementation.
var IsKnownEvidenceKind = rkcmodel.IsKnownEvidenceKind

// IsResolvedResolution delegates the resolved-state predicate to the public
// resolution vocabulary.
var IsResolvedResolution = rkcmodel.IsResolvedResolution

// NormalizeResolution delegates compatibility normalization to pkg/rkcmodel so
// old internal imports cannot diverge from canonical resolution semantics.
var NormalizeResolution = rkcmodel.NormalizeResolution
