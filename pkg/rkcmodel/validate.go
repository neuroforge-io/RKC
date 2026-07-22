package rkcmodel

import (
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
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

// ValidateBundle validates the entire canonical representation, not only the
// graph core. Publication and plugin boundaries rely on this function to fail
// closed on malformed provenance, citations, documents, and execution paths.
func ValidateBundle(bundle Bundle, options ValidationOptions) ValidationReport {
	var diagnostics []Diagnostic
	artifactIDs := map[string]struct{}{}
	artifactsByID := map[string]Artifact{}
	nodeIDs := map[string]struct{}{}
	evidenceIDs := map[string]struct{}{}
	edgeIDs := map[string]struct{}{}
	claimIDs := map[string]struct{}{}
	documentIDs := map[string]struct{}{}
	diagnosticIDs := map[string]struct{}{}
	conflictIDs := map[string]struct{}{}
	pathIDs := map[string]struct{}{}

	validateSnapshot(&diagnostics, bundle.Snapshot)

	for _, artifact := range bundle.Artifacts {
		if artifact.ID == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-001", "artifact has empty id", artifact.Path))
			continue
		}
		if _, duplicate := artifactIDs[artifact.ID]; duplicate {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-002", "duplicate artifact id", artifact.ID))
		}
		artifactIDs[artifact.ID] = struct{}{}
		artifactsByID[artifact.ID] = artifact
		if options.StrictVocabulary && !IsKnownArtifactStatus(artifact.Status) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-003", "unknown artifact status", artifact.Status))
		}
		if strings.TrimSpace(artifact.Path) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-004", "artifact path is required", artifact.ID))
		}
		if strings.TrimSpace(artifact.Kind) == "" || options.StrictVocabulary && !IsKnownArtifactKind(artifact.Kind) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-005", "unknown or empty artifact kind", artifact.Kind))
		}
		if artifact.SizeBytes < 0 || artifact.LineCount < 0 {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-006", "artifact size and line count must be non-negative", artifact.ID))
		}
		if artifact.SHA256 != "" && !validHexDigest(artifact.SHA256) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-007", "artifact sha256 is invalid", artifact.ID))
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
		if !finiteProbability(evidence.Confidence) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-012", "evidence confidence outside [0,1]", evidence.ID))
		}
		if evidence.Source != nil && evidence.Source.ArtifactID != "" {
			if _, ok := artifactIDs[evidence.Source.ArtifactID]; !ok {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-013", "evidence references missing artifact", evidence.Source.ArtifactID))
			}
		}
		if strings.TrimSpace(evidence.Kind) == "" || options.StrictVocabulary && !IsKnownEvidenceKind(evidence.Kind) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-014", "unknown or empty evidence kind", evidence.Kind))
		}
		if strings.TrimSpace(evidence.Method) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-015", "evidence method is required", evidence.ID))
		}
		validateSourceRange(&diagnostics, evidence.Source, artifactsByID, "RKC-MOD-016", evidence.ID)
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
		if strings.TrimSpace(node.Kind) == "" || options.StrictVocabulary && !IsKnownNodeKind(node.Kind) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-022", "unknown or empty node kind", node.Kind))
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
		if strings.TrimSpace(node.Name) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-026", "node name is required", node.ID))
		}
		if node.Source != nil && node.ArtifactID != "" && node.Source.ArtifactID != "" && node.Source.ArtifactID != node.ArtifactID {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-027", "node source artifact disagrees with node artifact", node.ID))
		}
		validateSourceRange(&diagnostics, node.Source, artifactsByID, "RKC-MOD-028", node.ID)
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
		if strings.TrimSpace(edge.Kind) == "" || options.StrictVocabulary && !IsKnownEdgeKind(edge.Kind) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-034", "unknown or empty edge kind", edge.Kind))
		}
		if !IsKnownResolution(edge.Resolution) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-035", "unknown edge resolution", edge.Resolution))
		}
		for _, evidenceID := range edge.EvidenceIDs {
			if _, ok := evidenceIDs[evidenceID]; !ok {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-036", "edge references missing evidence", evidenceID))
			}
		}
		if !finiteProbability(edge.Confidence) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-037", "edge confidence outside [0,1]", edge.ID))
		}
	}

	for _, claim := range bundle.Claims {
		if strings.TrimSpace(claim.ID) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-040", "claim id is required", claim.SubjectID))
			continue
		}
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
		if strings.TrimSpace(claim.Text) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-043", "claim text is required", claim.ID))
		}
		if strings.TrimSpace(claim.Generator) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-044", "claim generator is required", claim.ID))
		}
		if !IsKnownClaimCertainty(claim.Certainty) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-045", "unknown claim certainty", claim.Certainty))
		}
		if !IsKnownClaimValidation(claim.Validation) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-046", "unknown claim validation", claim.Validation))
		}
		if options.RequireEvidence && len(claim.EvidenceIDs) == 0 {
			diagnostics = append(diagnostics, validationDiagnostic("warning", "RKC-MOD-047", "claim has no evidence", claim.ID))
		}
	}

	for _, diagnostic := range bundle.Diagnostics {
		if strings.TrimSpace(diagnostic.ID) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-060", "diagnostic id is required", diagnostic.Code))
			continue
		}
		if _, duplicate := diagnosticIDs[diagnostic.ID]; duplicate {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-061", "duplicate diagnostic id", diagnostic.ID))
		}
		diagnosticIDs[diagnostic.ID] = struct{}{}
		if !IsKnownSeverity(diagnostic.Severity) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-062", "unknown diagnostic severity", diagnostic.Severity))
		}
		if strings.TrimSpace(diagnostic.Code) == "" || strings.TrimSpace(diagnostic.Message) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-063", "diagnostic code and message are required", diagnostic.ID))
		}
		validateSourceRange(&diagnostics, diagnostic.Source, artifactsByID, "RKC-MOD-064", diagnostic.ID)
	}

	for _, conflict := range bundle.Conflicts {
		if strings.TrimSpace(conflict.ID) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-070", "conflict id is required", conflict.SubjectID))
			continue
		}
		if _, duplicate := conflictIDs[conflict.ID]; duplicate {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-071", "duplicate conflict id", conflict.ID))
		}
		conflictIDs[conflict.ID] = struct{}{}
		if strings.TrimSpace(conflict.Kind) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-072", "conflict kind is required", conflict.ID))
		}
		if !knownSubject(conflict.SubjectID, nodeIDs, artifactIDs, claimIDs) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-073", "conflict subject is missing", conflict.SubjectID))
		}
		if len(conflict.CandidateIDs) < 2 {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-074", "conflict requires at least two candidates", conflict.ID))
		}
		for _, candidateID := range conflict.CandidateIDs {
			if !knownSubject(candidateID, nodeIDs, artifactIDs, claimIDs) {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-075", "conflict candidate is missing", candidateID))
			}
		}
		if conflict.PreferredID != "" && !containsString(conflict.CandidateIDs, conflict.PreferredID) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-076", "preferred conflict candidate is not in candidates", conflict.ID))
		}
		validateEvidenceReferences(&diagnostics, conflict.EvidenceIDs, evidenceIDs, "RKC-MOD-077", "conflict")
	}

	for _, document := range bundle.Documents {
		if strings.TrimSpace(document.ID) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-080", "document id is required", document.Path))
			continue
		}
		if _, duplicate := documentIDs[document.ID]; duplicate {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-081", "duplicate document id", document.ID))
		}
		documentIDs[document.ID] = struct{}{}
		if strings.TrimSpace(document.Kind) == "" || strings.TrimSpace(document.Title) == "" || strings.TrimSpace(document.Generator) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-082", "document kind, title, and generator are required", document.ID))
		}
		if !IsKnownDocumentStatus(document.Status) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-083", "unknown document status", document.Status))
		}
		if document.ContentSHA256 != "" && !validHexDigest(document.ContentSHA256) {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-084", "document content sha256 is invalid", document.ID))
		}
		for _, subjectID := range document.SubjectIDs {
			if !knownSubject(subjectID, nodeIDs, artifactIDs, claimIDs) {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-085", "document subject is missing", subjectID))
			}
		}
		sectionIDs := map[string]struct{}{}
		for _, section := range document.Sections {
			if strings.TrimSpace(section.ID) == "" {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-086", "document section id is required", document.ID))
				continue
			}
			if _, duplicate := sectionIDs[section.ID]; duplicate {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-087", "duplicate document section id", section.ID))
			}
			sectionIDs[section.ID] = struct{}{}
			if section.Ordinal < 0 || strings.TrimSpace(section.Markdown) == "" && strings.TrimSpace(section.PlainText) == "" {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-088", "document section content or ordinal is invalid", section.ID))
			}
			validateEvidenceReferences(&diagnostics, section.EvidenceIDs, evidenceIDs, "RKC-MOD-089", "document section")
			for _, claimID := range section.ClaimIDs {
				if _, ok := claimIDs[claimID]; !ok {
					diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-090", "document section claim is missing", claimID))
				}
			}
		}
		for _, section := range document.Sections {
			if section.ParentID == "" {
				continue
			}
			if _, sectionParent := sectionIDs[section.ParentID]; sectionParent {
				continue
			}
			if _, nodeParent := nodeIDs[section.ParentID]; !nodeParent {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-091", "document section parent is missing", section.ParentID))
			}
		}
	}

	for _, path := range bundle.Paths {
		if strings.TrimSpace(path.ID) == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-100", "execution path id is required", path.Name))
			continue
		}
		if _, duplicate := pathIDs[path.ID]; duplicate {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-101", "duplicate execution path id", path.ID))
		}
		pathIDs[path.ID] = struct{}{}
		if strings.TrimSpace(path.Name) == "" || len(path.NodeIDs) == 0 || path.EntryNodeID == "" {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-102", "execution path name, entry, and nodes are required", path.ID))
		}
		for _, nodeID := range path.NodeIDs {
			if _, ok := nodeIDs[nodeID]; !ok {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-103", "execution path node is missing", nodeID))
			}
		}
		if len(path.NodeIDs) > 0 && path.EntryNodeID != path.NodeIDs[0] {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-104", "execution path entry must be the first node", path.ID))
		}
		if path.ExitNodeID != "" && len(path.NodeIDs) > 0 && path.ExitNodeID != path.NodeIDs[len(path.NodeIDs)-1] {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-105", "execution path exit must be the last node", path.ID))
		}
		if len(path.NodeIDs) > 0 && len(path.EdgeIDs) != len(path.NodeIDs)-1 {
			diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-106", "execution path edge count must connect consecutive nodes", path.ID))
		}
		for index, edgeID := range path.EdgeIDs {
			edge, ok := findEdge(bundle.Edges, edgeID)
			if !ok {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-107", "execution path edge is missing", edgeID))
				continue
			}
			if index+1 < len(path.NodeIDs) && (edge.From != path.NodeIDs[index] || edge.To != path.NodeIDs[index+1]) {
				diagnostics = append(diagnostics, validationDiagnostic("error", "RKC-MOD-108", "execution path edge does not connect consecutive nodes", edgeID))
			}
		}
		validateEvidenceReferences(&diagnostics, path.EvidenceIDs, evidenceIDs, "RKC-MOD-109", "execution path")
	}

	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].ID == diagnostics[j].ID {
			return diagnostics[i].Message < diagnostics[j].Message
		}
		return diagnostics[i].ID < diagnostics[j].ID
	})
	return ValidationReport{Diagnostics: diagnostics}
}

func validateSnapshot(diagnostics *[]Diagnostic, snapshot Snapshot) {
	if snapshot.SchemaVersion != SchemaVersion {
		*diagnostics = append(*diagnostics, validationDiagnostic("error", "RKC-MOD-050", "snapshot schema version is unsupported", snapshot.SchemaVersion))
	}
	if strings.TrimSpace(snapshot.ID) == "" {
		*diagnostics = append(*diagnostics, validationDiagnostic("error", "RKC-MOD-051", "snapshot id is required", snapshot.RootName))
	}
	if strings.TrimSpace(snapshot.RootName) == "" {
		*diagnostics = append(*diagnostics, validationDiagnostic("error", "RKC-MOD-052", "snapshot root name is required", snapshot.ID))
	}
	if !validHexDigest(snapshot.ContentDigest) {
		*diagnostics = append(*diagnostics, validationDiagnostic("error", "RKC-MOD-053", "snapshot content digest is invalid", snapshot.ID))
	}
	if !IsKnownSnapshotStatus(snapshot.Status) {
		*diagnostics = append(*diagnostics, validationDiagnostic("error", "RKC-MOD-054", "unknown snapshot status", snapshot.Status))
	}
	if strings.TrimSpace(snapshot.Tool.Name) == "" || strings.TrimSpace(snapshot.Tool.Version) == "" {
		*diagnostics = append(*diagnostics, validationDiagnostic("error", "RKC-MOD-055", "snapshot tool name and version are required", snapshot.ID))
	}
}

func validateSourceRange(diagnostics *[]Diagnostic, source *SourceRange, artifacts map[string]Artifact, code, subject string) {
	if source == nil {
		return
	}
	issue := func(message, detail string) {
		*diagnostics = append(*diagnostics, validationDiagnostic("error", code, message, subject+":"+detail))
	}
	if strings.TrimSpace(source.Path) == "" {
		issue("source path is required", "path")
	}
	if source.StartByte < 0 || source.EndByte < 0 || source.EndByte != 0 && source.EndByte < source.StartByte {
		issue("source byte range is invalid", "bytes")
	}
	if source.StartLine < 0 || source.EndLine < 0 || source.StartColumn < 0 || source.EndColumn < 0 || source.EndLine != 0 && source.StartLine != 0 && source.EndLine < source.StartLine {
		issue("source line range is invalid", "lines")
	}
	if source.StartLine != 0 && source.EndLine == source.StartLine && source.EndColumn < source.StartColumn {
		issue("source column range is invalid", "columns")
	}
	if source.ArtifactID != "" {
		artifact, ok := artifacts[source.ArtifactID]
		if !ok {
			issue("source references missing artifact", "artifact")
		} else if source.Path != "" && artifact.Path != "" && source.Path != artifact.Path {
			issue("source path disagrees with artifact path", "artifact-path")
		}
	}
}

func validateEvidenceReferences(diagnostics *[]Diagnostic, ids []string, evidenceIDs map[string]struct{}, code, family string) {
	for _, id := range ids {
		if _, ok := evidenceIDs[id]; !ok {
			*diagnostics = append(*diagnostics, validationDiagnostic("error", code, family+" references missing evidence", id))
		}
	}
}

func finiteProbability(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= 1
}

func validHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func knownSubject(id string, sets ...map[string]struct{}) bool {
	if id == "" {
		return false
	}
	for _, values := range sets {
		if _, ok := values[id]; ok {
			return true
		}
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func findEdge(edges []Edge, id string) (Edge, bool) {
	for _, edge := range edges {
		if edge.ID == id {
			return edge, true
		}
	}
	return Edge{}, false
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
