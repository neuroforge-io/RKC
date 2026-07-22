package rkcmodel

import "strings"

const (
	ResolutionDeclared              = "declared"
	ResolutionCompilerResolved      = "compiler_resolved"
	ResolutionSyntaxInferred        = "syntax_inferred"
	ResolutionRuntimeObserved       = "runtime_observed"
	ResolutionDocumentationAsserted = "documentation_asserted"
	ResolutionModelInferred         = "model_inferred"
	ResolutionUnresolved            = "unresolved"
)

var NodeKinds = set(
	"repository", "project", "package", "directory", "file", "symlink", "special", "archive", "archive_member", "notebook", "notebook_cell", "module", "namespace",
	"class", "interface", "trait", "type", "enum", "enum_member", "function", "method",
	"constructor", "field", "property", "variable", "constant", "parameter", "return_value",
	"api_service", "api_endpoint", "api_operation", "security_scheme", "graphql_type", "graphql_field", "rpc_service", "rpc_method",
	"cli_command", "cli_argument", "cli_flag", "config_key", "environment_variable", "secret",
	"database", "database_table", "database_column", "database_view", "database_index", "migration",
	"schema", "message", "event", "topic", "queue", "build_target", "deployment", "container_image",
	"test", "test_suite", "fixture", "document", "document_section", "external_dependency",
	"license", "owner", "execution_path", "unresolved_symbol",
)

var EdgeKinds = set(
	"contains", "declares", "imports", "exports", "references", "calls", "instantiates",
	"inherits", "implements", "overrides", "aliases", "reads", "writes", "mutates", "validates",
	"serializes", "deserializes", "exposes", "routes_to", "handles", "authenticates", "authorizes",
	"tests", "covers", "documents", "configures", "depends_on", "builds", "generates", "packages",
	"deploys", "emits", "subscribes", "publishes", "consumes", "migrates", "invoked_by", "supersedes",
	"owned_by", "licensed_under", "observed_with", "derived_from", "related_to",
)

var ArtifactKinds = set(
	"file", "directory", "symlink", "special", "archive", "archive_member", "notebook",
	"notebook_cell", "manifest", "source", "document", "binary", "generated", "vendored",
)

var ArtifactStatuses = set(
	"recorded", "included", "text", "parsed", "syntax_parsed", "semantic_parsed", "excluded",
	"inventory_only", "binary", "vendored", "generated", "redacted", "unreadable", "unsupported", "oversized",
)

var EvidenceKinds = set(
	"declared", "compiler_resolved", "syntax_inferred", "runtime_observed", "documentation_asserted",
	"model_inferred", "manifest", "build_metadata", "test_result", "coverage", "security_scan", "user_asserted",
)

var DiagnosticSeverities = set("note", "warning", "error", "fatal")
var DocumentStatuses = set("draft", "validated", "rejected", "published", "stale")
var ClaimValidationStates = set("pending", "accepted", "rejected", "inference", "stale")
var ClaimCertaintyStates = set("supported", "inferred", "uncertain", "contradicted")
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

func IsKnownNodeKind(value string) bool       { _, ok := NodeKinds[value]; return ok }
func IsKnownEdgeKind(value string) bool       { _, ok := EdgeKinds[value]; return ok }
func IsKnownArtifactKind(value string) bool   { _, ok := ArtifactKinds[value]; return ok }
func IsKnownArtifactStatus(value string) bool { _, ok := ArtifactStatuses[value]; return ok }
func IsKnownEvidenceKind(value string) bool   { _, ok := EvidenceKinds[value]; return ok }
func IsKnownSeverity(value string) bool       { _, ok := DiagnosticSeverities[value]; return ok }

func NormalizeResolution(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	if alias, ok := resolutionAliases[value]; ok {
		return alias
	}
	return value
}

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

func IsResolvedResolution(value string) bool {
	switch NormalizeResolution(value) {
	case ResolutionDeclared, ResolutionCompilerResolved, ResolutionRuntimeObserved:
		return true
	default:
		return false
	}
}

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
