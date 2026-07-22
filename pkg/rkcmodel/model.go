package rkcmodel

import "time"

// SchemaVersion identifies the canonical Repository Knowledge Representation
// emitted by this reference implementation. Compatibility is governed by the
// schemas and migration policy, not by the command-line version alone.
const SchemaVersion = "0.2.0"

// SourceRange identifies an occurrence in an immutable artifact. Byte offsets
// are preferred for exact slicing; line and column fields exist for humans and
// editor integrations. Columns are zero-based and lines are one-based.
type SourceRange struct {
	ArtifactID  string `json:"artifact_id,omitempty"`
	Path        string `json:"path"`
	StartByte   int64  `json:"start_byte,omitempty"`
	EndByte     int64  `json:"end_byte,omitempty"`
	StartLine   int    `json:"start_line,omitempty"`
	StartColumn int    `json:"start_column,omitempty"`
	EndLine     int    `json:"end_line,omitempty"`
	EndColumn   int    `json:"end_column,omitempty"`
	Anchor      string `json:"anchor,omitempty"`
}

// GitInfo captures repository state without claiming Git is universally
// available. WorkingTreeDigest is populated when the scan is not a clean commit.
type GitInfo struct {
	Commit            string `json:"commit,omitempty"`
	Branch            string `json:"branch,omitempty"`
	Origin            string `json:"origin,omitempty"`
	Dirty             bool   `json:"dirty,omitempty"`
	WorkingTreeDigest string `json:"working_tree_digest,omitempty"`
	Unavailable       bool   `json:"unavailable,omitempty"`
}

type ToolInfo struct {
	Name       string            `json:"name"`
	Version    string            `json:"version"`
	Build      string            `json:"build,omitempty"`
	Runtime    string            `json:"runtime,omitempty"`
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Snapshot is the immutable identity and provenance envelope for one analysis.
// CreatedAt and RootPath are operational metadata and are removed from the
// deterministic canonical digest.
type Snapshot struct {
	SchemaVersion    string            `json:"schema_version"`
	ID               string            `json:"id"`
	RepositoryID     string            `json:"repository_id,omitempty"`
	ParentSnapshotID string            `json:"parent_snapshot_id,omitempty"`
	CreatedAt        time.Time         `json:"created_at"`
	Status           string            `json:"status,omitempty"`
	RootName         string            `json:"root_name"`
	RootPath         string            `json:"root_path"`
	ContentDigest    string            `json:"content_digest"`
	ConfigDigest     string            `json:"config_digest,omitempty"`
	PolicyDigest     string            `json:"policy_digest,omitempty"`
	PluginLockDigest string            `json:"plugin_lock_digest,omitempty"`
	ToolchainDigest  string            `json:"toolchain_digest,omitempty"`
	Git              GitInfo           `json:"git"`
	Tool             ToolInfo          `json:"tool"`
	Policy           map[string]any    `json:"policy,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// Artifact is a physical repository object. ID is occurrence identity inside a
// snapshot; ContentID is an optional content-addressed identity shared by equal
// bytes across paths and snapshots.
type Artifact struct {
	ID                string            `json:"id"`
	LogicalID         string            `json:"logical_id,omitempty"`
	ContentID         string            `json:"content_id,omitempty"`
	Path              string            `json:"path"`
	Kind              string            `json:"kind"`
	Language          string            `json:"language,omitempty"`
	MediaType         string            `json:"media_type,omitempty"`
	Encoding          string            `json:"encoding,omitempty"`
	Newline           string            `json:"newline,omitempty"`
	SizeBytes         int64             `json:"size_bytes,omitempty"`
	SHA256            string            `json:"sha256,omitempty"`
	LineCount         int               `json:"line_count,omitempty"`
	Mode              uint32            `json:"mode,omitempty"`
	Text              bool              `json:"text"`
	Executable        bool              `json:"executable,omitempty"`
	Generated         bool              `json:"generated,omitempty"`
	Vendored          bool              `json:"vendored,omitempty"`
	Status            string            `json:"status"`
	DispositionReason string            `json:"disposition_reason,omitempty"`
	ExclusionReason   string            `json:"exclusion_reason,omitempty"` // Deprecated compatibility field.
	Target            string            `json:"target,omitempty"`
	LicenseExpression string            `json:"license_expression,omitempty"`
	Attributes        map[string]string `json:"attributes,omitempty"`
}

// Node is a logical repository entity. ArtifactID and Source point to one
// occurrence; LogicalID can remain stable across moves and refactorings.
type Node struct {
	ID            string         `json:"id"`
	LogicalID     string         `json:"logical_id,omitempty"`
	Kind          string         `json:"kind"`
	Name          string         `json:"name"`
	QualifiedName string         `json:"qualified_name,omitempty"`
	Signature     string         `json:"signature,omitempty"`
	Language      string         `json:"language,omitempty"`
	Visibility    string         `json:"visibility,omitempty"`
	Stability     string         `json:"stability,omitempty"`
	PublicSurface bool           `json:"public_surface,omitempty"`
	ArtifactID    string         `json:"artifact_id,omitempty"`
	Source        *SourceRange   `json:"source,omitempty"`
	SemanticHash  string         `json:"semantic_hash,omitempty"`
	EvidenceIDs   []string       `json:"evidence_ids,omitempty"`
	Attributes    map[string]any `json:"attributes,omitempty"`
}

// Edge joins two nodes. An unresolved target is still represented by an
// explicit unresolved_symbol node, preserving referential integrity.
type Edge struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	From        string         `json:"from"`
	To          string         `json:"to"`
	Resolution  string         `json:"resolution"`
	Confidence  float64        `json:"confidence,omitempty"`
	Producer    string         `json:"producer,omitempty"`
	Lifecycle   string         `json:"lifecycle,omitempty"`
	EvidenceIDs []string       `json:"evidence_ids,omitempty"`
	Attributes  map[string]any `json:"attributes,omitempty"`
}

// Evidence records why a fact exists. Confidence is meaningful only alongside
// Kind, Method, Producer, and source provenance.
type Evidence struct {
	ID          string         `json:"id"`
	Kind        string         `json:"kind"`
	Method      string         `json:"method"`
	Confidence  float64        `json:"confidence"`
	Source      *SourceRange   `json:"source,omitempty"`
	Tool        string         `json:"tool,omitempty"`
	ToolVersion string         `json:"tool_version,omitempty"`
	InputDigest string         `json:"input_digest,omitempty"`
	ObservedAt  *time.Time     `json:"observed_at,omitempty"`
	Detail      string         `json:"detail,omitempty"`
	Attributes  map[string]any `json:"attributes,omitempty"`
}

type Diagnostic struct {
	ID         string         `json:"id"`
	Severity   string         `json:"severity"`
	Code       string         `json:"code"`
	Message    string         `json:"message"`
	Source     *SourceRange   `json:"source,omitempty"`
	Stage      string         `json:"stage,omitempty"`
	Plugin     string         `json:"plugin,omitempty"`
	HelpURI    string         `json:"help_uri,omitempty"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Conflict preserves contradictory claims rather than letting merge precedence
// silently erase inconvenient evidence.
type Conflict struct {
	ID           string         `json:"id"`
	Kind         string         `json:"kind"`
	SubjectID    string         `json:"subject_id"`
	CandidateIDs []string       `json:"candidate_ids,omitempty"`
	PreferredID  string         `json:"preferred_id,omitempty"`
	Resolution   string         `json:"resolution,omitempty"`
	EvidenceIDs  []string       `json:"evidence_ids,omitempty"`
	Attributes   map[string]any `json:"attributes,omitempty"`
}

type Claim struct {
	ID               string         `json:"id"`
	SubjectID        string         `json:"subject_id"`
	Text             string         `json:"text"`
	Category         string         `json:"category,omitempty"`
	Certainty        string         `json:"certainty"`
	Generator        string         `json:"generator"`
	GeneratorVersion string         `json:"generator_version,omitempty"`
	EvidenceIDs      []string       `json:"evidence_ids"`
	Validation       string         `json:"validation"`
	Attributes       map[string]any `json:"attributes,omitempty"`
}

type DocumentSection struct {
	ID          string         `json:"id"`
	ParentID    string         `json:"parent_id,omitempty"`
	Ordinal     int            `json:"ordinal"`
	Heading     string         `json:"heading,omitempty"`
	Markdown    string         `json:"markdown"`
	PlainText   string         `json:"plain_text,omitempty"`
	ClaimIDs    []string       `json:"claim_ids,omitempty"`
	EvidenceIDs []string       `json:"evidence_ids,omitempty"`
	Attributes  map[string]any `json:"attributes,omitempty"`
}

type Document struct {
	ID               string            `json:"id"`
	LogicalID        string            `json:"logical_id,omitempty"`
	Kind             string            `json:"kind"`
	Title            string            `json:"title"`
	Path             string            `json:"path,omitempty"`
	SubjectIDs       []string          `json:"subject_ids,omitempty"`
	Generator        string            `json:"generator"`
	GeneratorVersion string            `json:"generator_version,omitempty"`
	ContentSHA256    string            `json:"content_sha256,omitempty"`
	Status           string            `json:"status"`
	Sections         []DocumentSection `json:"sections,omitempty"`
	Attributes       map[string]any    `json:"attributes,omitempty"`
}

// ExecutionPath is a named, evidence-backed path through the graph.
type ExecutionPath struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	EntryNodeID string         `json:"entry_node_id"`
	ExitNodeID  string         `json:"exit_node_id,omitempty"`
	NodeIDs     []string       `json:"node_ids"`
	EdgeIDs     []string       `json:"edge_ids"`
	EvidenceIDs []string       `json:"evidence_ids,omitempty"`
	Attributes  map[string]any `json:"attributes,omitempty"`
}

type Fragment struct {
	Artifacts   []Artifact      `json:"artifacts,omitempty"`
	Nodes       []Node          `json:"nodes,omitempty"`
	Edges       []Edge          `json:"edges,omitempty"`
	Evidence    []Evidence      `json:"evidence,omitempty"`
	Diagnostics []Diagnostic    `json:"diagnostics,omitempty"`
	Conflicts   []Conflict      `json:"conflicts,omitempty"`
	Documents   []Document      `json:"documents,omitempty"`
	Claims      []Claim         `json:"claims,omitempty"`
	Paths       []ExecutionPath `json:"execution_paths,omitempty"`
}

type Bundle struct {
	Snapshot    Snapshot        `json:"snapshot"`
	Artifacts   []Artifact      `json:"artifacts"`
	Nodes       []Node          `json:"nodes"`
	Edges       []Edge          `json:"edges"`
	Evidence    []Evidence      `json:"evidence"`
	Diagnostics []Diagnostic    `json:"diagnostics"`
	Conflicts   []Conflict      `json:"conflicts,omitempty"`
	Documents   []Document      `json:"documents,omitempty"`
	Claims      []Claim         `json:"claims,omitempty"`
	Paths       []ExecutionPath `json:"execution_paths,omitempty"`
}

type Coverage struct {
	SnapshotID                   string         `json:"snapshot_id"`
	ArtifactsEncountered         int            `json:"artifacts_encountered"`
	ArtifactsInventoried         int            `json:"artifacts_inventoried"`
	TextArtifacts                int            `json:"text_artifacts"`
	ArtifactsSyntacticallyParsed int            `json:"artifacts_syntactically_parsed"`
	ArtifactsSemanticallyParsed  int            `json:"artifacts_semantically_parsed"`
	ArtifactsExcluded            int            `json:"artifacts_excluded"`
	ArtifactsBinary              int            `json:"artifacts_binary"`
	ArtifactsUnreadable          int            `json:"artifacts_unreadable"`
	NodesTotal                   int            `json:"nodes_total"`
	SymbolsTotal                 int            `json:"symbols_total"`
	SymbolsWithEvidence          int            `json:"symbols_with_evidence"`
	PublicSymbols                int            `json:"public_symbols"`
	PublicSymbolsDocumented      int            `json:"public_symbols_documented"`
	EdgesTotal                   int            `json:"edges_total"`
	ResolvedEdges                int            `json:"resolved_edges"`
	UnresolvedEdges              int            `json:"unresolved_edges"`
	ClaimsTotal                  int            `json:"claims_total"`
	ClaimsWithEvidence           int            `json:"claims_with_evidence"`
	ConflictsTotal               int            `json:"conflicts_total"`
	SecretFindings               int            `json:"secret_findings"`
	HighConfidenceSecretFindings int            `json:"high_confidence_secret_findings"`
	DiagnosticsBySeverity        map[string]int `json:"diagnostics_by_severity"`
	NodeKinds                    map[string]int `json:"node_kinds"`
	EdgeKinds                    map[string]int `json:"edge_kinds"`
	EvidenceKinds                map[string]int `json:"evidence_kinds"`
	ArtifactStatuses             map[string]int `json:"artifact_statuses"`
	InventoryAccountingRatio     float64        `json:"inventory_accounting_ratio"`
	SyntacticParseRatio          float64        `json:"syntactic_parse_ratio"`
	SemanticParseRatio           float64        `json:"semantic_parse_ratio"`
	SymbolEvidenceRatio          float64        `json:"symbol_evidence_ratio"`
	PublicDocumentationRatio     float64        `json:"public_documentation_ratio"`
	EdgeResolutionRatio          float64        `json:"edge_resolution_ratio"`
	ClaimCitationRatio           float64        `json:"claim_citation_ratio"`
	DeterministicOutputDigest    string         `json:"deterministic_output_digest"`
}
