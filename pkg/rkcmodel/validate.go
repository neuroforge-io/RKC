package rkcmodel

import (
	"fmt"
	"math"
	"sort"
)

type ValidationOptions struct {
	StrictVocabulary bool
	RequireEvidence  bool
}

type ValidationReport struct {
	Diagnostics []Diagnostic `json:"diagnostics"`
}

func (report ValidationReport) HasErrors() bool {
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.Severity == "error" || diagnostic.Severity == "fatal" {
			return true
		}
	}
	return false
}

func ValidateBundle(bundle Bundle, options ValidationOptions) ValidationReport {
	var diagnostics []Diagnostic
	artifactIDs := map[string]struct{}{}
	nodeIDs := map[string]struct{}{}
	evidenceIDs := map[string]struct{}{}
	edgeIDs := map[string]struct{}{}
	claimIDs := map[string]struct{}{}

	for _, artifact := range bundle.Artifacts {
		if artifact.ID == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-001", "artifact has empty id", artifact.Path))
			continue
		}
		if _, duplicate := artifactIDs[artifact.ID]; duplicate {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-002", "duplicate artifact id", artifact.ID))
		}
		artifactIDs[artifact.ID] = struct{}{}
		if options.StrictVocabulary && !IsKnownArtifactStatus(artifact.Status) {
			diagnostics = append(diagnostics, validationDiagnostic("warning", "RKC-MOD-003", "unknown artifact status", artifact.Status))
		}
	}

	for _, evidence := range bundle.Evidence {
		if evidence.ID == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-010", "evidence has empty id", evidence.Method))
			continue
		}
		if _, duplicate := evidenceIDs[evidence.ID]; duplicate {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-011", "duplicate evidence id", evidence.ID))
		}
		evidenceIDs[evidence.ID] = struct{}{}
		if math.IsNaN(evidence.Confidence) || evidence.Confidence < 0 || evidence.Confidence > 1 {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-012", "evidence confidence outside [0,1]", evidence.ID))
		}
		if evidence.Source != nil && evidence.Source.ArtifactID != "" {
			if _, ok := artifactIDs[evidence.Source.ArtifactID]; !ok {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-013", "evidence references missing artifact", evidence.Source.ArtifactID))
			}
		}
	}

	for _, node := range bundle.Nodes {
		if node.ID == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-020", "node has empty id", node.QualifiedName))
			continue
		}
		if _, duplicate := nodeIDs[node.ID]; duplicate {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-021", "duplicate node id", node.ID))
		}
		nodeIDs[node.ID] = struct{}{}
		if options.StrictVocabulary && !IsKnownNodeKind(node.Kind) {
			diagnostics = append(diagnostics, validationDiagnostic("warning", "RKC-MOD-022", "unknown node kind", node.Kind))
		}
		if node.ArtifactID != "" {
			if _, ok := artifactIDs[node.ArtifactID]; !ok {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-023", "node references missing artifact", node.ArtifactID))
			}
		}
		for _, evidenceID := range node.EvidenceIDs {
			if _, ok := evidenceIDs[evidenceID]; !ok {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-024", "node references missing evidence", evidenceID))
			}
		}
		if options.RequireEvidence && IsSymbolKind(node.Kind) && len(node.EvidenceIDs) == 0 {
			diagnostics = append(diagnostics, validationDiagnostic("warning", "RKC-MOD-025", "symbol has no evidence", node.ID))
		}
	}

	for _, edge := range bundle.Edges {
		if edge.ID == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-030", "edge has empty id", edge.Kind))
			continue
		}
		if _, duplicate := edgeIDs[edge.ID]; duplicate {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-031", "duplicate edge id", edge.ID))
		}
		edgeIDs[edge.ID] = struct{}{}
		if _, ok := nodeIDs[edge.From]; !ok {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-032", "edge source missing", edge.From))
		}
		if _, ok := nodeIDs[edge.To]; !ok {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-033", "edge target missing", edge.To))
		}
		if options.StrictVocabulary && !IsKnownEdgeKind(edge.Kind) {
			diagnostics = append(diagnostics, validationDiagnostic("warning", "RKC-MOD-034", "unknown edge kind", edge.Kind))
		}
		if !IsKnownResolution(edge.Resolution) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-035", "unknown edge resolution", edge.Resolution))
		}
		for _, evidenceID := range edge.EvidenceIDs {
			if _, ok := evidenceIDs[evidenceID]; !ok {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-036", "edge references missing evidence", evidenceID))
			}
		}
	}

	for _, claim := range bundle.Claims {
		if _, duplicate := claimIDs[claim.ID]; duplicate {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-040", "duplicate claim id", claim.ID))
		}
		claimIDs[claim.ID] = struct{}{}
		if _, ok := nodeIDs[claim.SubjectID]; !ok {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-041", "claim subject missing", claim.SubjectID))
		}
		for _, evidenceID := range claim.EvidenceIDs {
			if _, ok := evidenceIDs[evidenceID]; !ok {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-042", "claim references missing evidence", evidenceID))
			}
		}
	}

	sort.Slice(diagnostics, func(i, j int) bool { return diagnostics[i].ID < diagnostics[j].ID })
	return ValidationReport{Diagnostics: diagnostics}
}

func validationDiagnostic(severity, code, message, subject string) Diagnostic {
	return Diagnostic{
		ID:       StableID("diagnostic", code, subject),
		Severity: severity,
		Code:     code,
		Message:  fmt.Sprintf("%s: %s", message, subject),
		Stage:    "model_validate",
	}
}
