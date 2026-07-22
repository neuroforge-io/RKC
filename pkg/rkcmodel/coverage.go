package rkcmodel

import (
	"strconv"
	"strings"
)

// BuildCoverage derives auditable numerators, denominators, and ratios from a
// canonical bundle. The calculation contains no wall-clock or host data.
func BuildCoverage(bundle Bundle) Coverage {
	coverage := Coverage{
		SnapshotID:            bundle.Snapshot.ID,
		ArtifactsEncountered:  len(bundle.Artifacts),
		ArtifactsInventoried:  len(bundle.Artifacts),
		DiagnosticsBySeverity: map[string]int{},
		NodeKinds:             map[string]int{}, EdgeKinds: map[string]int{},
		EvidenceKinds: map[string]int{}, ArtifactStatuses: map[string]int{},
	}
	eligibleSyntax := 0
	for _, artifact := range bundle.Artifacts {
		status := strings.TrimSpace(artifact.Status)
		coverage.ArtifactStatuses[status]++
		if artifact.Text {
			coverage.TextArtifacts++
			if status != "excluded" && status != "inventory_only" && status != "oversized" && status != "unsupported" && status != "redacted" && status != "unreadable" {
				eligibleSyntax++
			}
		}
		switch status {
		case "parsed", "syntax_parsed", "semantic_parsed":
			coverage.ArtifactsSyntacticallyParsed++
		}
		if status == "semantic_parsed" || boolString(artifact.Attributes["semantic_parsed"]) {
			coverage.ArtifactsSemanticallyParsed++
		}
		switch status {
		case "excluded", "inventory_only", "oversized", "unsupported", "redacted":
			coverage.ArtifactsExcluded++
		case "binary":
			coverage.ArtifactsBinary++
		case "unreadable":
			coverage.ArtifactsUnreadable++
		}
	}
	documentedSubjects := map[string]struct{}{}
	for _, document := range bundle.Documents {
		if document.Status == "rejected" || document.Status == "stale" {
			continue
		}
		for _, id := range document.SubjectIDs {
			documentedSubjects[id] = struct{}{}
		}
	}
	for _, claim := range bundle.Claims {
		coverage.ClaimsTotal++
		if len(claim.EvidenceIDs) > 0 {
			coverage.ClaimsWithEvidence++
		}
	}
	for _, node := range bundle.Nodes {
		coverage.NodesTotal++
		coverage.NodeKinds[node.Kind]++
		if node.Kind == "secret" {
			coverage.SecretFindings++
			if attributeFloat(node.Attributes, "confidence") >= 0.90 || attributeString(node.Attributes, "confidence_class") == "high" {
				coverage.HighConfidenceSecretFindings++
			}
		}
		if !IsSymbolKind(node.Kind) {
			continue
		}
		coverage.SymbolsTotal++
		if len(node.EvidenceIDs) > 0 {
			coverage.SymbolsWithEvidence++
		}
		if node.PublicSurface || node.Visibility == "public" || node.Visibility == "exported" {
			coverage.PublicSymbols++
			_, generated := documentedSubjects[node.ID]
			if generated || strings.TrimSpace(attributeString(node.Attributes, "docstring")) != "" || strings.TrimSpace(attributeString(node.Attributes, "documentation")) != "" {
				coverage.PublicSymbolsDocumented++
			}
		}
	}
	for _, edge := range bundle.Edges {
		coverage.EdgesTotal++
		coverage.EdgeKinds[edge.Kind]++
		if IsResolvedResolution(edge.Resolution) {
			coverage.ResolvedEdges++
		} else {
			coverage.UnresolvedEdges++
		}
	}
	for _, evidence := range bundle.Evidence {
		coverage.EvidenceKinds[evidence.Kind]++
	}
	for _, diagnostic := range bundle.Diagnostics {
		coverage.DiagnosticsBySeverity[diagnostic.Severity]++
	}
	coverage.ConflictsTotal = len(bundle.Conflicts)
	coverage.InventoryAccountingRatio = ratio(coverage.ArtifactsInventoried, coverage.ArtifactsEncountered)
	coverage.SyntacticParseRatio = ratio(coverage.ArtifactsSyntacticallyParsed, eligibleSyntax)
	coverage.SemanticParseRatio = ratio(coverage.ArtifactsSemanticallyParsed, eligibleSyntax)
	coverage.SymbolEvidenceRatio = ratio(coverage.SymbolsWithEvidence, coverage.SymbolsTotal)
	coverage.PublicDocumentationRatio = ratio(coverage.PublicSymbolsDocumented, coverage.PublicSymbols)
	coverage.EdgeResolutionRatio = ratio(coverage.ResolvedEdges, coverage.EdgesTotal)
	coverage.ClaimCitationRatio = ratio(coverage.ClaimsWithEvidence, coverage.ClaimsTotal)
	coverage.DeterministicOutputDigest = CanonicalDigest(bundle)
	return coverage
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 1
	}
	return float64(numerator) / float64(denominator)
}
func boolString(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		parsed, _ := strconv.ParseBool(typed)
		return parsed
	default:
		return false
	}
}
func attributeString(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value := values[key]
	switch typed := value.(type) {
	case string:
		return typed
	case []byte:
		return string(typed)
	default:
		return ""
	}
}
func attributeFloat(values map[string]any, key string) float64 {
	if values == nil {
		return 0
	}
	switch typed := values[key].(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case string:
		value, _ := strconv.ParseFloat(typed, 64)
		return value
	default:
		return 0
	}
}
