package server

import (
	"context"
	"encoding/hex"
	"path"
	"reflect"
	"sort"
	"strings"

	"github.com/neuroforge-io/RKC/internal/docparse"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// contextDocumentReferences preserves the Markdown producer's existing source
// bindings. A generated document's export path is not a repository source.
// All required bindings must agree before a full-document excerpt gains refs;
// incomplete or inconsistent provenance retains the legacy unreferenced row.
func (dataset *Dataset) contextDocumentReferences(ctx context.Context, document rkcmodel.Document) (*rkcmodel.SourceRange, []string) {
	empty := []string{}
	if document.Kind != "source_document" || document.Generator != docparse.PluginID ||
		document.Status != "validated" || len(document.SubjectIDs) != 1 || len(document.Sections) == 0 {
		return nil, empty
	}
	artifactID, _ := document.Attributes["artifact_id"].(string)
	artifact, ok := dataset.ArtifactByID[artifactID]
	if !ok || artifact.ID != artifactID || !artifact.Text || artifact.Language != "markdown" ||
		artifact.LineCount < 1 || artifact.Path == "" || artifact.Path == "." || path.IsAbs(artifact.Path) ||
		strings.ContainsAny(artifact.Path, "\\\x00") ||
		path.Clean(artifact.Path) != artifact.Path || artifact.Path == ".." || strings.HasPrefix(artifact.Path, "../") ||
		document.Path != artifact.Path || document.Attributes["source_sha256"] != artifact.SHA256 ||
		document.ID != rkcmodel.StableID("document", "markdown", artifact.Path) {
		return nil, empty
	}
	if digest, err := hex.DecodeString(artifact.SHA256); err != nil || len(digest) != 32 {
		return nil, empty
	}
	whole := rkcmodel.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path, StartLine: 1, EndLine: artifact.LineCount}
	nodeID := rkcmodel.StableID("node", "document", artifact.Path)
	node, ok := dataset.NodeByID[document.SubjectIDs[0]]
	if !ok || node.ID != nodeID || node.Kind != "document" || node.Language != "markdown" ||
		node.ArtifactID != artifact.ID || node.Name != document.Title || node.QualifiedName != artifact.Path ||
		!reflect.DeepEqual(node.Source, &whole) || len(node.EvidenceIDs) != 1 {
		return nil, empty
	}
	wholeID := rkcmodel.StableID("evidence", docparse.PluginID, artifact.ID, "document")
	if node.EvidenceIDs[0] != wholeID || !dataset.contextMarkdownEvidence(wholeID, "markdown.document", artifact, &whole) {
		return nil, empty
	}
	ids := map[string]struct{}{wholeID: {}}
	parents := map[string]struct{}{nodeID: {}}
	for ordinal, section := range document.Sections {
		if ctx.Err() != nil {
			return nil, empty
		}
		sectionNode, ok := dataset.NodeByID[section.ID]
		if !ok || sectionNode.ID != section.ID || sectionNode.Kind != "document_section" ||
			sectionNode.Language != "markdown" || sectionNode.ArtifactID != artifact.ID || sectionNode.Source == nil ||
			section.Ordinal != ordinal || section.Heading != sectionNode.Name ||
			len(section.EvidenceIDs) != 1 || len(sectionNode.EvidenceIDs) != 1 ||
			section.EvidenceIDs[0] != sectionNode.EvidenceIDs[0] {
			return nil, empty
		}
		source := sectionNode.Source
		canonicalSource := rkcmodel.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path,
			StartLine: source.StartLine, EndLine: source.EndLine, Anchor: source.Anchor}
		if source.ArtifactID != artifact.ID || source.Path != artifact.Path || source.Anchor == "" ||
			!reflect.DeepEqual(source, &canonicalSource) ||
			source.StartLine < 1 || source.EndLine < source.StartLine || source.EndLine > artifact.LineCount ||
			section.ID != rkcmodel.StableID("node", "document_section", artifact.Path, source.Anchor) ||
			sectionNode.QualifiedName != artifact.Path+"#"+source.Anchor ||
			section.Attributes["anchor"] != source.Anchor ||
			!contextLineAttribute(section.Attributes["start_line"], source.StartLine) ||
			!contextLineAttribute(section.Attributes["end_line"], source.EndLine) {
			return nil, empty
		}
		if _, known := parents[section.ParentID]; !known {
			return nil, empty
		}
		if _, duplicate := parents[section.ID]; duplicate {
			return nil, empty
		}
		evidenceID := rkcmodel.StableID("evidence", docparse.PluginID, artifact.ID, source.Anchor)
		if section.EvidenceIDs[0] != evidenceID ||
			!dataset.contextMarkdownEvidence(evidenceID, "markdown.heading", artifact, source) ||
			dataset.EvidenceByID[evidenceID].Detail != section.Heading {
			return nil, empty
		}
		ids[evidenceID] = struct{}{}
		parents[section.ID] = struct{}{}
	}
	evidenceIDs := make([]string, 0, len(ids))
	for id := range ids {
		evidenceIDs = append(evidenceIDs, id)
	}
	sort.Strings(evidenceIDs)
	return &whole, evidenceIDs
}

func (dataset *Dataset) contextMarkdownEvidence(id, method string, artifact rkcmodel.Artifact, source *rkcmodel.SourceRange) bool {
	evidence, ok := dataset.EvidenceByID[id]
	return ok && evidence.ID == id && evidence.Kind == "documentation_asserted" &&
		evidence.Method == method && evidence.Tool == docparse.PluginID &&
		evidence.InputDigest == artifact.SHA256 && reflect.DeepEqual(evidence.Source, source)
}

// Parser attributes use int before export and float64 after standard JSON
// decoding. Accept those exact integer values without string coercion.
func contextLineAttribute(value any, expected int) bool {
	switch number := value.(type) {
	case int:
		return number == expected
	case float64:
		return number == float64(expected)
	default:
		return false
	}
}
