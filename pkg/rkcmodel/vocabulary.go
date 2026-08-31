package rkcmodel

import "strings"

const (
	// ResolutionDeclared marks a relationship stated directly by source data.
	ResolutionDeclared = "declared"
	// ResolutionCompilerResolved marks a target proven by semantic tooling.
	ResolutionCompilerResolved = "compiler_resolved"
	// ResolutionSyntaxInferred marks a target inferred from syntax alone.
	ResolutionSyntaxInferred = "syntax_inferred"
	// ResolutionRuntimeObserved marks a relationship observed during execution.
	ResolutionRuntimeObserved = "runtime_observed"
	// ResolutionDocumentationAsserted marks a relationship asserted by docs.
	ResolutionDocumentationAsserted = "documentation_asserted"
	// ResolutionModelInferred marks a relationship proposed by a model.
	ResolutionModelInferred = "model_inferred"
	// ResolutionUnresolved marks a relationship whose target is not proven.
	ResolutionUnresolved = "unresolved"
)

// NodeKinds contains the canonical node vocabulary. Callers must treat the map
// as read-only.
var NodeKinds = set(
	"repository", "project", "package", "directory", "file", "symlink", "special", "archive", "archive_member", "notebook", "notebook_cell", "module", "namespace",
	"class", "interface", "trait", "type", "enum", "enum_member", "function", "method",
	"constructor", "field", "property", "variable", "constant", "parameter", "return_value",
	"value", "cfg_block", "trace",
	"api_service", "api_endpoint", "api_operation", "security_scheme", "graphql_type", "graphql_field", "rpc_service", "rpc_method",
	"cli_command", "cli_argument", "cli_flag", "config_key", "environment_variable", "secret",
	"database", "database_table", "database_column", "database_view", "database_index", "migration",
	"schema", "message", "event", "topic", "queue", "build_target", "deployment", "container_image",
	"test", "test_suite", "fixture", "document", "document_section", "external_dependency",
	"license", "owner", "execution_path", "unresolved_symbol",
)

// EdgeKinds contains the canonical relationship vocabulary. Callers must treat
// the map as read-only.
var EdgeKinds = set(
	"contains", "declares", "imports", "exports", "references", "calls", "instantiates",
	"inherits", "implements", "overrides", "aliases", "reads", "writes", "mutates", "validates",
	"serializes", "deserializes", "exposes", "routes_to", "handles", "authenticates", "authorizes",
	"tests", "covers", "documents", "configures", "depends_on", "builds", "generates", "packages",
	"deploys", "emits", "subscribes", "publishes", "consumes", "migrates", "invoked_by", "supersedes",
	"owned_by", "licensed_under", "observed_with", "derived_from", "related_to",
	"precedes", "flows_to", "binds_to", "returns_to", "sanitizes",
)

// ArtifactKinds contains the canonical physical-object vocabulary. Callers
// must treat the map as read-only.
var ArtifactKinds = set(
	"file", "directory", "symlink", "special", "archive", "archive_member", "notebook",
	"notebook_cell", "manifest", "source", "document", "binary", "generated", "vendored",
)

// ArtifactStatuses contains the canonical inventory and processing outcomes.
// Callers must treat the map as read-only.
var ArtifactStatuses = set(
	"recorded", "included", "text", "parsed", "syntax_parsed", "semantic_parsed", "excluded",
	"inventory_only", "binary", "vendored", "generated", "redacted", "unreadable", "unsupported", "oversized",
)

// EvidenceKinds contains the canonical provenance-method vocabulary. Callers
// must treat the map as read-only.
var EvidenceKinds = set(
	"declared", "compiler_resolved", "syntax_inferred", "runtime_observed", "documentation_asserted",
	"model_inferred", "manifest", "build_metadata", "test_result", "coverage", "security_scan", "user_asserted",
)

// DiagnosticSeverities contains accepted diagnostic impact levels. Callers
// must treat the map as read-only.
var DiagnosticSeverities = set("note", "warning", "error", "fatal")

// DocumentStatuses contains accepted generated-document lifecycle states.
// Callers must treat the map as read-only.
var DocumentStatuses = set("draft", "validated", "rejected", "published", "stale")

// ClaimValidationStates contains accepted claim review outcomes. Callers must
// treat the map as read-only.
var ClaimValidationStates = set("pending", "accepted", "rejected", "inference", "stale")

// ClaimCertaintyStates contains accepted epistemic-strength labels. Callers
// must treat the map as read-only.
var ClaimCertaintyStates = set("supported", "inferred", "uncertain", "contradicted")

// SnapshotStatuses contains accepted snapshot lifecycle states. Callers must
// treat the map as read-only.
var SnapshotStatuses = set("building", "validating", "committed", "failed", "superseded")

var resolutionAliases = map[string]string{
	"resolved":         ResolutionCompilerResolved,
	"observed":         ResolutionRuntimeObserved,
	"syntax-inferred":  ResolutionSyntaxInferred,
	"runtime-observed": ResolutionRuntimeObserved,
}

func set(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

// IsKnownNodeKind reports whether value exactly matches a canonical node kind.
func IsKnownNodeKind(value string) bool { _, ok := NodeKinds[value]; return ok }

// IsKnownEdgeKind reports whether value exactly matches a canonical edge kind.
func IsKnownEdgeKind(value string) bool { _, ok := EdgeKinds[value]; return ok }

// IsKnownArtifactKind reports whether value exactly matches a canonical
// artifact kind.
func IsKnownArtifactKind(value string) bool { _, ok := ArtifactKinds[value]; return ok }

// IsKnownArtifactStatus reports whether value exactly matches a canonical
// artifact processing status.
func IsKnownArtifactStatus(value string) bool { _, ok := ArtifactStatuses[value]; return ok }

// IsKnownEvidenceKind reports whether value exactly matches a canonical
// evidence kind.
func IsKnownEvidenceKind(value string) bool { _, ok := EvidenceKinds[value]; return ok }

// IsKnownSeverity reports whether value exactly matches a diagnostic severity.
func IsKnownSeverity(value string) bool { _, ok := DiagnosticSeverities[value]; return ok }

// IsKnownDocumentStatus reports whether value exactly matches a document
// lifecycle status.
func IsKnownDocumentStatus(value string) bool { _, ok := DocumentStatuses[value]; return ok }

// IsKnownClaimValidation reports whether value exactly matches a claim review
// state.
func IsKnownClaimValidation(value string) bool {
	_, ok := ClaimValidationStates[value]
	return ok
}

// IsKnownClaimCertainty reports whether value exactly matches a claim certainty
// state.
func IsKnownClaimCertainty(value string) bool {
	_, ok := ClaimCertaintyStates[value]
	return ok
}

// IsKnownSnapshotStatus reports whether value exactly matches a snapshot
// lifecycle status.
func IsKnownSnapshotStatus(value string) bool { _, ok := SnapshotStatuses[value]; return ok }

// NormalizeResolution trims and lowercases value, then maps supported legacy
// aliases to canonical resolution vocabulary. Unknown values remain normalized
// but otherwise unchanged.
func NormalizeResolution(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if alias, ok := resolutionAliases[value]; ok {
		return alias
	}
	return value
}

// IsKnownResolution reports whether value normalizes to a canonical resolution
// term.
func IsKnownResolution(value string) bool {
	switch NormalizeResolution(value) {
	case ResolutionDeclared, ResolutionCompilerResolved, ResolutionSyntaxInferred,
		ResolutionRuntimeObserved, ResolutionDocumentationAsserted,
		ResolutionModelInferred, ResolutionUnresolved:
		return true
	default:
		return false
	}
}

// IsResolvedResolution reports whether value represents a declared,
// compiler-resolved, or runtime-observed relationship. Inferences remain
// unresolved for coverage accounting.
func IsResolvedResolution(value string) bool {
	switch NormalizeResolution(value) {
	case ResolutionDeclared, ResolutionCompilerResolved, ResolutionRuntimeObserved:
		return true
	default:
		return false
	}
}

// IsSymbolKind reports whether kind participates in symbol evidence and
// documentation coverage accounting.
func IsSymbolKind(kind string) bool {
	switch kind {
	case "module", "namespace", "package", "class", "interface", "trait", "type", "enum", "enum_member",
		"function", "method", "constructor", "field", "property", "variable", "constant", "parameter",
		"test", "api_service", "api_endpoint", "api_operation", "security_scheme", "rpc_service", "rpc_method", "cli_command", "cli_argument",
		"cli_flag", "config_key", "environment_variable", "database_table", "database_column", "database_view",
		"schema", "message", "event", "topic", "queue", "build_target", "deployment":
		return true
	default:
		return false
	}
}
