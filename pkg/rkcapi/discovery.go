// Package rkcapi defines portable discovery and context exchange contracts.
// These records contain public RKC capabilities and evidence, never training policy.
package rkcapi

import "github.com/neuroforge-io/RKC/pkg/rkcmodel"

// Workflow is an executable CLI argument-vector example. It is never shell code.
type Workflow struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Mode        string   `json:"mode"`
	Argv        []string `json:"argv"`
	Guidance    string   `json:"guidance"`
}

// Output describes a supported output workflow, not a claim that a file exists.
type Output struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Format      string   `json:"format"`
	Command     []string `json:"command,omitempty"`
	Href        string   `json:"href,omitempty"`
}

// Capabilities is the versioned machine-readable local integration entry point.
type Capabilities struct {
	SchemaVersion   string            `json:"schema_version"`
	CanonicalSchema string            `json:"canonical_schema"`
	SnapshotID      string            `json:"snapshot_id,omitempty"`
	Integrity       string            `json:"integrity,omitempty"`
	Interfaces      map[string]string `json:"interfaces"`
	Workflows       []Workflow        `json:"workflows"`
	Outputs         []Output          `json:"outputs"`
	Limits          map[string]int    `json:"limits"`
	Boundaries      []string          `json:"boundaries"`
}

// ContextItem is a ranked, cited excerpt from an immutable search projection.
type ContextItem struct {
	CitationID  string                `json:"citation_id"`
	ObjectID    string                `json:"object_id"`
	ObjectType  string                `json:"object_type"`
	Title       string                `json:"title"`
	Path        string                `json:"path"`
	Kind        string                `json:"kind,omitempty"`
	Language    string                `json:"language,omitempty"`
	Text        string                `json:"text"`
	Source      *rkcmodel.SourceRange `json:"source,omitempty"`
	EvidenceIDs []string              `json:"evidence_ids"`
	Score       float64               `json:"score"`
}

// ContextPacket is deterministic retrieval, not a generated answer. Bytes counts
// the compact JSON items array; MaxBytes bounds that array, excluding the envelope.
// Digest hashes compact JSON of this complete record with Digest set to empty.
type ContextPacket struct {
	SchemaVersion string        `json:"schema_version"`
	SnapshotID    string        `json:"snapshot_id"`
	Query         string        `json:"query"`
	Integrity     string        `json:"integrity"`
	Items         []ContextItem `json:"items"`
	Truncated     bool          `json:"truncated"`
	Bytes         int           `json:"bytes"`
	MaxBytes      int           `json:"max_bytes"`
	Warnings      []string      `json:"warnings"`
	Digest        string        `json:"digest"`
}
