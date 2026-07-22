// Package model is a compatibility facade retained for internal packages that
// predate the public canonical model. New code should import pkg/rkcmodel.
package model

import "github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"

const SchemaVersion = rkcmodel.SchemaVersion
const ResolutionUnresolved = rkcmodel.ResolutionUnresolved

type SourceRange = rkcmodel.SourceRange
type GitInfo = rkcmodel.GitInfo
type ToolInfo = rkcmodel.ToolInfo
type Snapshot = rkcmodel.Snapshot
type Artifact = rkcmodel.Artifact
type Node = rkcmodel.Node
type Edge = rkcmodel.Edge
type Evidence = rkcmodel.Evidence
type Diagnostic = rkcmodel.Diagnostic
type Conflict = rkcmodel.Conflict
type Claim = rkcmodel.Claim
type DocumentSection = rkcmodel.DocumentSection
type Document = rkcmodel.Document
type ExecutionPath = rkcmodel.ExecutionPath
type Fragment = rkcmodel.Fragment
type Bundle = rkcmodel.Bundle
type Coverage = rkcmodel.Coverage
type ValidationOptions = rkcmodel.ValidationOptions
type ValidationReport = rkcmodel.ValidationReport

var StableID = rkcmodel.StableID
var SortBundle = rkcmodel.SortBundle
var SortFragment = rkcmodel.SortFragment
var CanonicalBundle = rkcmodel.CanonicalBundle
var CanonicalJSON = rkcmodel.CanonicalJSON
var CanonicalDigest = rkcmodel.CanonicalDigest
var ValidateBundle = rkcmodel.ValidateBundle
var BuildCoverage = rkcmodel.BuildCoverage
var IsSymbolKind = rkcmodel.IsSymbolKind
var IsKnownNodeKind = rkcmodel.IsKnownNodeKind
var IsKnownEdgeKind = rkcmodel.IsKnownEdgeKind
var IsKnownEvidenceKind = rkcmodel.IsKnownEvidenceKind
var IsResolvedResolution = rkcmodel.IsResolvedResolution
var NormalizeResolution = rkcmodel.NormalizeResolution
