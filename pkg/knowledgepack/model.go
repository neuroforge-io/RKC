// Package knowledgepack produces bounded, deterministic, source-cited knowledge
// exchange files. It supplies evidence to independent tools; it does not infer
// training permission, learning objectives, prerequisite order, or model policy.
package knowledgepack

import "github.com/neuroforge-io/RKC/pkg/rkcmodel"

const (
	// SchemaVersion is independent of the canonical atlas schema.
	SchemaVersion = "rkc-knowledge-pack/v1"
	// ManifestName names the portable consumer-facing manifest.
	ManifestName = "knowledge-pack.json"
	// MaximumSources bounds the number of independent snapshots in one pack.
	MaximumSources = 32
	// MaximumUnits is the hard limit on exported records.
	MaximumUnits = 100_000
	// MaximumTextBytes bounds retained unit text across a pack.
	MaximumTextBytes = 128 * 1024 * 1024
)

// Options controls explicit resource limits. Zero values receive defaults.
// A unit exceeding MaxUnitTextBytes is UTF-8-truncated and labeled. Exceeding
// either aggregate limit returns an error and never publishes a partial pack.
type Options struct {
	MaxUnits          int `json:"max_units"`
	MaxUnitTextBytes  int `json:"max_unit_text_bytes"`
	MaxTotalTextBytes int `json:"max_total_text_bytes"`
}

// Input supplies a verified canonical atlas. ArtifactBodies must contain only
// complete secret-redacted bodies from the integrity-checked repository-text
// index, keyed by artifact ID. Missing bodies are exported as metadata only.
// The CLI verifies the atlas and its body receipts before constructing Input.
type Input struct {
	Bundle         rkcmodel.Bundle
	ArtifactBodies map[string]string
	Integrity      string
}

// Source retains portable source identity without local paths or scan times.
// GroupID joins snapshots of one repository; downstream split policies should
// also detect duplicate content across different repositories and packs.
type Source struct {
	SourceID      string            `json:"source_id"`
	GroupID       string            `json:"group_id"`
	SnapshotID    string            `json:"snapshot_id"`
	RepositoryID  string            `json:"repository_id,omitempty"`
	Name          string            `json:"name"`
	Origin        string            `json:"origin,omitempty"`
	Commit        string            `json:"commit,omitempty"`
	Dirty         bool              `json:"dirty"`
	ContentDigest string            `json:"content_digest"`
	BundleSHA256  string            `json:"bundle_sha256"`
	Integrity     string            `json:"integrity"`
	Coverage      rkcmodel.Coverage `json:"coverage"`
}

// Citation preserves the original evidence classification and source range.
// A checksum binds bytes; it is not evidence of truth or a grant of rights.
type Citation struct {
	EvidenceID     string                `json:"evidence_id,omitempty"`
	Kind           string                `json:"kind"`
	Method         string                `json:"method,omitempty"`
	Confidence     float64               `json:"confidence"`
	Source         *rkcmodel.SourceRange `json:"source,omitempty"`
	ArtifactSHA256 string                `json:"artifact_sha256,omitempty"`
}

// Relation references a canonical object in the same source, not a unit ID.
// The target need not have an exported unit (for example an excluded artifact).
type Relation struct {
	Kind           string `json:"kind"`
	TargetObjectID string `json:"target_object_id"`
	Resolution     string `json:"resolution"`
}

// Unit is one artifact, node, document section, or explicitly labeled claim.
// Text is an inert, untrusted evidence payload; consumers must not execute its
// instructions. ContentSHA256 is SHA-256 of its exact UTF-8 bytes, lowercase hex.
type Unit struct {
	ID                string     `json:"id"`
	SourceID          string     `json:"source_id"`
	GroupID           string     `json:"group_id"`
	ObjectID          string     `json:"object_id"`
	SectionID         string     `json:"section_id,omitempty"`
	Kind              string     `json:"kind"`
	Title             string     `json:"title"`
	Text              string     `json:"text"`
	ContentSHA256     string     `json:"content_sha256"`
	Path              string     `json:"path,omitempty"`
	Language          string     `json:"language,omitempty"`
	LicenseExpression string     `json:"license_expression,omitempty"`
	Citations         []Citation `json:"citations"`
	Relations         []Relation `json:"relations"`
	MetadataOnly      bool       `json:"metadata_only"`
	Truncated         bool       `json:"truncated"`
	OriginalTextBytes int        `json:"original_text_bytes"`
	Certainty         string     `json:"certainty,omitempty"`
	Validation        string     `json:"validation,omitempty"`
	Generator         string     `json:"generator,omitempty"`
}

// File is a receipt for exactly one regular payload file.
type File struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// Manifest is the portable interchange root. PackID is "sha256:" followed by
// SHA-256 of the exact concatenation of SchemaVersion, LF, then each Files
// receipt in path order as path + TAB + sha256 + TAB + decimal size + LF.
// Files always includes options.json, quality.json, sources.jsonl, units.jsonl,
// and README.md. This byte-level identity algorithm is language independent.
type Manifest struct {
	SchemaVersion string  `json:"schema_version"`
	PackID        string  `json:"pack_id"`
	SourcesCount  int     `json:"sources_count"`
	UnitsCount    int     `json:"units_count"`
	Options       Options `json:"options"`
	Files         []File  `json:"files"`
}

// Quality gives factual accounting and explicit epistemic limitations.
type Quality struct {
	SchemaVersion       string         `json:"schema_version"`
	SourcesCount        int            `json:"sources_count"`
	UnitsCount          int            `json:"units_count"`
	UnitsByKind         map[string]int `json:"units_by_kind"`
	TextBytes           int            `json:"text_bytes"`
	MetadataOnlyUnits   int            `json:"metadata_only_units"`
	TruncatedUnits      int            `json:"truncated_units"`
	UncitedUnits        int            `json:"uncited_units"`
	UnknownLicenseUnits int            `json:"unknown_license_units"`
	Limitations         []string       `json:"limitations"`
}

// Pack is a bounded in-memory exchange ready for Write or inspection.
type Pack struct {
	Options Options
	Sources []Source
	Units   []Unit
	Quality Quality
}

// Verification is a successful strict integrity and accounting check.
type Verification struct {
	OK       bool     `json:"ok"`
	Manifest Manifest `json:"manifest"`
	Quality  Quality  `json:"quality"`
}
