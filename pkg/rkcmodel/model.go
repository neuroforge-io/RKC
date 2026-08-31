// Package rkcmodel defines the canonical, portable Repository Knowledge
// Representation shared by analyzers, stores, exports, and clients.
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
// available. Origin, when present, is a canonical credential-free repository
// origin. WorkingTreeDigest is populated when the scan is not a clean commit.
type GitInfo struct {
	Commit            string `json:"commit,omitempty"`
	Branch            string `json:"branch,omitempty"`
	Origin            string `json:"origin,omitempty"`
	Dirty             bool   `json:"dirty,omitempty"`
	WorkingTreeDigest string `json:"working_tree_digest,omitempty"`
	Unavailable       bool   `json:"unavailable,omitempty"`
}

// ToolInfo identifies the RKC implementation that produced a Snapshot.
type ToolInfo struct {
	// Name is the producer's stable product or executable name.
	Name string `json:"name"`
	// Version is the producer release version.
	Version string `json:"version"`
	// Build identifies a more specific binary or source build when available.
	Build string `json:"build,omitempty"`
	// Runtime describes the execution runtime relevant to reproducibility.
	Runtime string `json:"runtime,omitempty"`
	// Attributes carries producer-specific, non-core provenance.
	Attributes map[string]string `json:"attributes,omitempty"`
}

// Snapshot is the immutable identity and provenance envelope for one analysis.
// For origin-backed snapshots, RepositoryID is derived only from the canonical
// credential-free origin. CreatedAt and RootPath are operational metadata and
// are removed from the deterministic canonical digest.
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

// Diagnostic is a stable, machine-readable finding emitted while constructing
// or validating repository knowledge.
type Diagnostic struct {
	// ID uniquely identifies this diagnostic occurrence within a snapshot.
	ID string `json:"id"`
	// Severity is one of DiagnosticSeverities.
	Severity string `json:"severity"`
	// Code is the stable identifier integrations should use for classification.
	Code string `json:"code"`
	// Message is the human-readable explanation of the finding.
	Message string `json:"message"`
	// Source locates the finding in an immutable artifact when applicable.
	Source *SourceRange `json:"source,omitempty"`
	// Stage identifies the pipeline stage that emitted the finding.
	Stage string `json:"stage,omitempty"`
	// Plugin identifies the analyzer plugin that emitted the finding, if any.
	Plugin string `json:"plugin,omitempty"`
	// HelpURI points to operator guidance for the diagnostic code.
	HelpURI string `json:"help_uri,omitempty"`
	// Attributes carries producer-specific structured detail.
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

// Claim is a generated or curated statement about a canonical subject. Its
// certainty describes epistemic strength; Validation records review state.
type Claim struct {
	// ID uniquely identifies the claim within a snapshot.
	ID string `json:"id"`
	// SubjectID references the node described by Text.
	SubjectID string `json:"subject_id"`
	// Text contains the statement presented to people or downstream agents.
	Text string `json:"text"`
	// Category is a producer-defined claim classification.
	Category string `json:"category,omitempty"`
	// Certainty is one of ClaimCertaintyStates.
	Certainty string `json:"certainty"`
	// Generator identifies the process or model responsible for the claim.
	Generator string `json:"generator"`
	// GeneratorVersion identifies the specific generator release when known.
	GeneratorVersion string `json:"generator_version,omitempty"`
	// EvidenceIDs cite canonical evidence supporting or challenging the claim.
	EvidenceIDs []string `json:"evidence_ids"`
	// Validation is one of ClaimValidationStates.
	Validation string `json:"validation"`
	// Attributes carries generator-specific structured detail.
	Attributes map[string]any `json:"attributes,omitempty"`
}

// DocumentSection is an ordered, evidence-bearing unit of a generated
// Document. Markdown is canonical display content; PlainText is an optional
// accessibility and machine-consumption projection.
type DocumentSection struct {
	// ID uniquely identifies the section within its document.
	ID string `json:"id"`
	// ParentID references either a parent section or a canonical node.
	ParentID string `json:"parent_id,omitempty"`
	// Ordinal determines section order; IDs break ties deterministically.
	Ordinal int `json:"ordinal"`
	// Heading is the optional display heading.
	Heading string `json:"heading,omitempty"`
	// Markdown is the section's canonical rich-text content.
	Markdown string `json:"markdown"`
	// PlainText is an optional formatting-free projection of Markdown.
	PlainText string `json:"plain_text,omitempty"`
	// ClaimIDs reference claims presented or discussed by this section.
	ClaimIDs []string `json:"claim_ids,omitempty"`
	// EvidenceIDs cite evidence supporting the section.
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
	// Attributes carries generator-specific structured detail.
	Attributes map[string]any `json:"attributes,omitempty"`
}

// Document is a versioned knowledge artifact linked to canonical subjects.
// Sections carry its display content, claims, and evidence citations.
type Document struct {
	// ID identifies this document occurrence within a snapshot.
	ID string `json:"id"`
	// LogicalID can remain stable when the generated occurrence changes.
	LogicalID string `json:"logical_id,omitempty"`
	// Kind classifies the document for renderers and downstream consumers.
	Kind string `json:"kind"`
	// Title is the human-readable document name.
	Title string `json:"title"`
	// Path is the optional relative export location.
	Path string `json:"path,omitempty"`
	// SubjectIDs reference the nodes, artifacts, or claims covered as a whole.
	SubjectIDs []string `json:"subject_ids,omitempty"`
	// Generator identifies the process or model that produced the document.
	Generator string `json:"generator"`
	// GeneratorVersion identifies the specific generator release when known.
	GeneratorVersion string `json:"generator_version,omitempty"`
	// ContentSHA256 binds the document to exported content when populated.
	ContentSHA256 string `json:"content_sha256,omitempty"`
	// Status is one of DocumentStatuses.
	Status string `json:"status"`
	// Sections contains the ordered, evidence-bearing document body.
	Sections []DocumentSection `json:"sections,omitempty"`
	// Attributes carries generator-specific structured detail.
	Attributes map[string]any `json:"attributes,omitempty"`
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

// Fragment is a snapshot-less collection of canonical records produced by one
// analyzer. References may target records supplied by the host Bundle.
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

// Bundle is the complete portable knowledge representation for one Snapshot.
// ValidateBundle checks its vocabulary, provenance, and referential integrity;
// CanonicalJSON produces its deterministic interchange representation.
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

// Coverage contains auditable counts and ratios derived from one Bundle by
// BuildCoverage. Ratios use 1 when their denominator is zero, representing no
// outstanding eligible item rather than missing evidence.
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
	// InventoryAccountingRatio compares inventoried with encountered artifacts.
	InventoryAccountingRatio float64 `json:"inventory_accounting_ratio"`
	// SyntacticParseRatio covers eligible text artifacts parsed syntactically.
	SyntacticParseRatio float64 `json:"syntactic_parse_ratio"`
	// SemanticParseRatio covers eligible text artifacts parsed semantically.
	SemanticParseRatio float64 `json:"semantic_parse_ratio"`
	// SymbolEvidenceRatio compares evidence-backed with all symbol nodes.
	SymbolEvidenceRatio float64 `json:"symbol_evidence_ratio"`
	// PublicDocumentationRatio compares documented with public symbol nodes.
	PublicDocumentationRatio float64 `json:"public_documentation_ratio"`
	// EdgeResolutionRatio compares resolved with all graph edges.
	EdgeResolutionRatio float64 `json:"edge_resolution_ratio"`
	// ClaimCitationRatio compares evidence-citing with all claims.
	ClaimCitationRatio float64 `json:"claim_citation_ratio"`
	// FlowCFGBlocks counts bounded control-flow-graph blocks produced by the
	// value-flow stage (zero when the stage is disabled).
	FlowCFGBlocks int `json:"flow_cfg_blocks,omitempty"`
	// FlowCFGEdges counts precedes edges between CFG blocks.
	FlowCFGEdges int `json:"flow_cfg_edges,omitempty"`
	// FlowCallEdges counts call-graph edges (resolved and unresolved).
	FlowCallEdges int `json:"flow_call_edges,omitempty"`
	// FlowCallEdgesResolved counts call edges with a resolved target.
	FlowCallEdgesResolved int `json:"flow_call_edges_resolved,omitempty"`
	// FlowValueEdges counts value-flow edges (flows_to, binds_to, returns_to,
	// sanitizes) produced by the value-flow stage.
	FlowValueEdges int `json:"flow_value_edges,omitempty"`
	// FlowSources counts deterministic source-role value entities.
	FlowSources int `json:"flow_sources,omitempty"`
	// FlowSinks counts deterministic sink-role value entities.
	FlowSinks int `json:"flow_sinks,omitempty"`
	// RuntimeTraces counts imported trace records, including assertions.
	RuntimeTraces int `json:"runtime_traces,omitempty"`
	// RuntimeProducerAuthenticatedTraces counts traces whose evidence producer,
	// not merely their capture record, is authenticated.
	RuntimeProducerAuthenticatedTraces int `json:"runtime_producer_authenticated_traces,omitempty"`
	// RuntimeAssertionTraces counts producer-unverified trace inputs.
	RuntimeAssertionTraces int `json:"runtime_assertion_traces,omitempty"`
	// RuntimeCaptureIntegrityAssertions counts assertion traces whose exact record
	// was produced by the importing RKC process. This is not producer authority.
	RuntimeCaptureIntegrityAssertions int `json:"runtime_capture_integrity_assertions,omitempty"`
	// RuntimeProducerAuthenticatedTests counts results from a
	// producer-authenticated test-event source.
	RuntimeProducerAuthenticatedTests int `json:"runtime_producer_authenticated_tests,omitempty"`
	// RuntimeProducerObservedCallEdges counts call edges supported by actual
	// events from a producer-authenticated call-event source.
	RuntimeProducerObservedCallEdges int `json:"runtime_producer_observed_call_edges,omitempty"`
	// RuntimeFunctionsExecutionAsserted counts functions for which a trace makes
	// a positive, producer-unverified statement-coverage assertion.
	RuntimeFunctionsExecutionAsserted int `json:"runtime_functions_execution_asserted,omitempty"`
	// RuntimeFunctionsNotObserved counts functions carrying one or more
	// trace-scoped negative assertions. It is not a dead-code, non-execution, or
	// impossibility claim.
	RuntimeFunctionsNotObserved int `json:"runtime_functions_not_observed,omitempty"`
	// RuntimeCallObservationAvailable states whether an admitted producer
	// supplied actual call events.
	RuntimeCallObservationAvailable bool `json:"runtime_call_observation_available,omitempty"`
	// RuntimeProducerCallEdgeObservationRatio compares producer-observed with
	// resolved static call edges only when call-event observation is available.
	RuntimeProducerCallEdgeObservationRatio float64 `json:"runtime_producer_call_edge_observation_ratio,omitempty"`
	// DeterministicOutputDigest is CanonicalDigest of the measured Bundle.
	DeterministicOutputDigest string `json:"deterministic_output_digest"`
}
