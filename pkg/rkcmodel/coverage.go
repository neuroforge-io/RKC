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
	runtimeTraceIDs := map[string]struct{}{}
	producerAuthenticatedTraceIDs := map[string]struct{}{}
	assertionTraceIDs := map[string]struct{}{}
	captureIntegrityAssertionIDs := map[string]struct{}{}
	callEventTraceIDs := map[string]struct{}{}
	for _, document := range bundle.Documents {
		if document.Status == "rejected" || document.Status == "stale" {
			continue
		}
		for _, id := range document.SubjectIDs {
			documentedSubjects[id] = struct{}{}
		}
	}
	for _, node := range bundle.Nodes {
		if node.Kind != "trace" {
			continue
		}
		traceID := attributeString(node.Attributes, "trace_id")
		if traceID == "" {
			continue
		}
		runtimeTraceIDs[traceID] = struct{}{}
		producerAuthenticated, _ := node.Attributes["producer_authenticated"].(bool)
		if producerAuthenticated {
			producerAuthenticatedTraceIDs[traceID] = struct{}{}
			if available, _ := node.Attributes["call_observation_available"].(bool); available {
				callEventTraceIDs[traceID] = struct{}{}
				coverage.RuntimeCallObservationAvailable = true
			}
			continue
		}
		assertionTraceIDs[traceID] = struct{}{}
		if integrity, _ := node.Attributes["capture_integrity_authenticated"].(bool); integrity {
			captureIntegrityAssertionIDs[traceID] = struct{}{}
		}
	}
	for _, edge := range bundle.Edges {
		switch edge.Kind {
		case "calls":
			coverage.FlowCallEdges++
			resolved := NormalizeResolution(edge.Resolution) != ResolutionUnresolved
			if resolved {
				coverage.FlowCallEdgesResolved++
			}
			if resolved {
				observed, ok := edge.Attributes["observed"].(bool)
				if ok && observed &&
					coverage.RuntimeCallObservationAvailable &&
					stringValuesIntersect(edge.Attributes["observed_trace_ids"], callEventTraceIDs) {
					coverage.RuntimeProducerObservedCallEdges++
				}
			}
		case "flows_to", "binds_to", "returns_to", "sanitizes":
			coverage.FlowValueEdges++
		case "precedes":
			coverage.FlowCFGEdges++
		}
	}
	for _, evidence := range bundle.Evidence {
		if evidence.Kind == "test_result" && evidence.Confidence == 1 &&
			evidence.Attributes["producer_authenticated"] == true &&
			validRuntimeTestStatus(attributeString(evidence.Attributes, "status")) &&
			stringValueInSet(evidence.Attributes["trace_id"], producerAuthenticatedTraceIDs) {
			coverage.RuntimeProducerAuthenticatedTests++
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
		switch node.Kind {
		case "cfg_block":
			coverage.FlowCFGBlocks++
		case "value":
			switch attributeString(node.Attributes, "flow_role") {
			case "source":
				coverage.FlowSources++
			case "sink":
				coverage.FlowSinks++
			}
		case "function", "method", "constructor", "test":
			if stringValuesIntersect(node.Attributes["execution_asserted_trace_ids"], assertionTraceIDs) {
				coverage.RuntimeFunctionsExecutionAsserted++
			}
			if stringValuesIntersect(node.Attributes["execution_not_observed_trace_ids"], assertionTraceIDs) {
				coverage.RuntimeFunctionsNotObserved++
			}
		}
		if node.Kind == "secret" {
			coverage.SecretFindings++
			if IsHighConfidenceSecret(node) {
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
	coverage.RuntimeTraces = len(runtimeTraceIDs)
	coverage.RuntimeProducerAuthenticatedTraces = len(producerAuthenticatedTraceIDs)
	coverage.RuntimeAssertionTraces = len(assertionTraceIDs)
	coverage.RuntimeCaptureIntegrityAssertions = len(captureIntegrityAssertionIDs)
	if coverage.RuntimeCallObservationAvailable && coverage.FlowCallEdgesResolved > 0 {
		coverage.RuntimeProducerCallEdgeObservationRatio = ratio(coverage.RuntimeProducerObservedCallEdges, coverage.FlowCallEdgesResolved)
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

// IsHighConfidenceSecret applies the same finding classification used by
// coverage. Review consumers must not subtract lower-confidence findings from
// the high-confidence total.
func IsHighConfidenceSecret(node Node) bool {
	return node.Kind == "secret" && (attributeFloat(node.Attributes, "confidence") >= 0.90 || attributeString(node.Attributes, "confidence_class") == "high")
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

func stringValuesIntersect(value any, values map[string]struct{}) bool {
	switch typed := value.(type) {
	case []string:
		for _, item := range typed {
			if _, ok := values[item]; ok {
				return true
			}
		}
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				if _, exists := values[text]; exists {
					return true
				}
			}
		}
	}
	return false
}

func stringValueInSet(value any, values map[string]struct{}) bool {
	text, ok := value.(string)
	if !ok || text == "" {
		return false
	}
	_, exists := values[text]
	return exists
}

func validRuntimeTestStatus(status string) bool {
	return status == "pass" || status == "fail" || status == "skip"
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
