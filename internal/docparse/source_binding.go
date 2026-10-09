package docparse

import (
	"context"
	"encoding/hex"
	"fmt"
	"path"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// SourceDocumentReferences validates the source-document producer's structural
// provenance before context or model layers quote its projection. It never
// reads source paths, treats log records as runtime proof, or establishes truth.
// Inconsistent bindings return no references; cancellation is checked by callers.
func SourceDocumentReferences(ctx context.Context, document rkcmodel.Document,
	artifacts map[string]rkcmodel.Artifact, nodes map[string]rkcmodel.Node,
	evidence map[string]rkcmodel.Evidence,
) (*rkcmodel.SourceRange, []string) {
	if ctx == nil || ctx.Err() != nil || document.Kind != "source_document" ||
		document.Generator != SourcePluginID || (document.GeneratorVersion != SourcePluginVersion && document.GeneratorVersion != "0.1.0") ||
		document.Status != "validated" || len(document.SubjectIDs) != 1 || len(document.Sections) == 0 ||
		len(document.Sections) > MaximumSourceSections {
		return nil, nil
	}
	complete, completeOK := document.Attributes["complete"].(bool)
	verified, verifiedOK := document.Attributes["producer_verified"].(bool)
	records, recordsOK := sourceNonnegativeInteger(document.Attributes["record_count"])
	invalidRecords, invalidOK := sourceNonnegativeInteger(document.Attributes["invalid_record_count"])
	if !completeOK || !verifiedOK || verified || !recordsOK || !invalidOK || invalidRecords > records {
		return nil, nil
	}
	artifactID, _ := document.Attributes["artifact_id"].(string)
	artifact, ok := artifacts[artifactID]
	if !ok || !artifact.Text || artifactID == "" || artifact.ID != artifactID || artifact.SizeBytes < 1 ||
		artifact.SizeBytes > MaximumSourceFileBytes || artifact.LineCount < 1 ||
		!IsSourceCandidate(pluginapi.FileRef{Language: artifact.Language}) ||
		artifact.Path == "" || artifact.Path == "." || path.IsAbs(artifact.Path) ||
		path.Clean(artifact.Path) != artifact.Path || artifact.Path == ".." ||
		strings.HasPrefix(artifact.Path, "../") || strings.ContainsAny(artifact.Path, "\\\x00\r\n") ||
		document.Path != artifact.Path || document.Title != path.Base(artifact.Path) ||
		document.Attributes["source_sha256"] != artifact.SHA256 ||
		document.Attributes["source_format"] != artifact.Language {
		return nil, nil
	}
	if digest, err := hex.DecodeString(artifact.SHA256); err != nil || len(digest) != 32 {
		return nil, nil
	}
	identity := artifact.ID
	if document.GeneratorVersion == "0.1.0" {
		// The legacy producer used original path bytes in its ID formulas.
		// It is admissible only when the artifact's opaque ID proves the display
		// path still equals that original path; no redacted-path bypass exists.
		if artifact.ID != rkcmodel.StableID("artifact", artifact.Path) {
			return nil, nil
		}
		identity = artifact.Path
	} else if digest, err := hex.DecodeString(strings.TrimPrefix(artifact.ID, "rkc:artifact:")); !strings.HasPrefix(artifact.ID, "rkc:artifact:") || err != nil || len(digest) != 12 || artifact.ID != strings.ToLower(artifact.ID) {
		// v0.2 uses the existing opaque inventoried StableID, never a display
		// path or a caller-provided string that could disclose original names.
		return nil, nil
	}
	if document.ID != rkcmodel.StableID("document", SourcePluginID, identity) ||
		document.LogicalID != rkcmodel.StableID("logical-document", SourcePluginID, identity) {
		return nil, nil
	}
	whole := rkcmodel.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path,
		StartLine: 1, EndLine: artifact.LineCount, EndByte: artifact.SizeBytes}
	nodeID := rkcmodel.StableID("node", SourcePluginID, identity)
	wholeID := rkcmodel.StableID("evidence", SourcePluginID, artifact.ID, "document")
	node, ok := nodes[document.SubjectIDs[0]]
	if !ok || document.SubjectIDs[0] != nodeID || node.ID != nodeID || node.LogicalID != rkcmodel.StableID("logical", SourcePluginID, identity) || node.Kind != "document" || node.Language != artifact.Language ||
		node.ArtifactID != artifact.ID || node.Name != document.Title || node.QualifiedName != artifact.Path ||
		!reflect.DeepEqual(node.Source, &whole) || len(node.EvidenceIDs) != 1 || node.EvidenceIDs[0] != wholeID ||
		!validSourceEvidence(evidence[wholeID], wholeID, "source.document", document.GeneratorVersion, artifact, &whole) {
		return nil, nil
	}
	ids := []string{wholeID}
	previousEnd, previousRecord := int64(0), int64(0)
	for ordinal, section := range document.Sections {
		if ctx.Err() != nil {
			return nil, nil
		}
		sectionNode, ok := nodes[section.ID]
		if !ok || sectionNode.Source == nil || sectionNode.ID != section.ID ||
			sectionNode.Kind != "document_section" || sectionNode.Language != artifact.Language ||
			sectionNode.ArtifactID != artifact.ID || section.Ordinal != ordinal || section.ParentID != nodeID ||
			section.Heading != sectionNode.Name || len(section.EvidenceIDs) != 1 ||
			len(sectionNode.EvidenceIDs) != 1 || section.EvidenceIDs[0] != sectionNode.EvidenceIDs[0] {
			return nil, nil
		}
		source := sectionNode.Source
		canonical := rkcmodel.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path,
			StartLine: source.StartLine, EndLine: source.EndLine, StartByte: source.StartByte, EndByte: source.EndByte}
		if !reflect.DeepEqual(source, &canonical) || source.StartLine < 1 || source.EndLine < source.StartLine ||
			source.EndLine > artifact.LineCount || source.StartByte < previousEnd || source.EndByte <= source.StartByte ||
			source.EndByte > artifact.SizeBytes || !sourceInteger(section.Attributes["start_byte"], source.StartByte) ||
			!sourceInteger(section.Attributes["end_byte"], source.EndByte) ||
			!sourceInteger(section.Attributes["start_line"], int64(source.StartLine)) ||
			!sourceInteger(section.Attributes["end_line"], int64(source.EndLine)) ||
			len(section.PlainText) > MaximumSourceSectionBytes {
			return nil, nil
		}
		start, end := strconv.FormatInt(source.StartByte, 10), strconv.FormatInt(source.EndByte, 10)
		id := rkcmodel.StableID("evidence", SourcePluginID, artifact.ID, start, end)
		if section.ID != rkcmodel.StableID("node", SourcePluginID, identity, start, end) ||
			sectionNode.LogicalID != rkcmodel.StableID("logical", SourcePluginID, identity, start, end) ||
			sectionNode.QualifiedName != artifact.Path+"#bytes-"+start+"-"+end || section.EvidenceIDs[0] != id {
			return nil, nil
		}
		item := evidence[id]
		method := item.Method
		if method != "source.text_chunk" && !(method == "source.jsonl_record" && artifact.Language == "jsonl") &&
			!(method == "source.csv_record" && (artifact.Language == "csv" || artifact.Language == "tsv")) {
			return nil, nil
		}
		if !validSourceEvidence(item, id, method, document.GeneratorVersion, artifact, source) {
			return nil, nil
		}
		projection, _ := section.Attributes["projection"].(string)
		truncated, truncatedOK := section.Attributes["projection_truncated"].(bool)
		valid, validOK := section.Attributes["record_valid"].(bool)
		if !truncatedOK || !validOK || (truncated && complete) {
			return nil, nil
		}
		if record, ok := sourceNonnegativeInteger(section.Attributes["record_index"]); !ok {
			return nil, nil
		} else {
			if method == "source.text_chunk" {
				csvSuffix := artifact.Language == "csv" || artifact.Language == "tsv"
				if record != 0 || artifact.Language == "jsonl" || valid == csvSuffix {
					return nil, nil
				}
			} else if record <= previousRecord || record > records {
				return nil, nil
			} else {
				previousRecord = record
			}
			if method == "source.csv_record" && record == 1 && !valid {
				return nil, nil
			}
			expectedProjection := "secret-redacted-source"
			if method == "source.csv_record" && record > 1 && valid {
				expectedProjection = "secret-redacted-column-values"
			}
			if projection != expectedProjection {
				return nil, nil
			}
			heading := fmt.Sprintf("Lines %d–%d", source.StartLine, source.EndLine)
			if record > 0 {
				heading = fmt.Sprintf("Record %d · %s", record, heading)
			}
			if section.Heading != heading {
				return nil, nil
			}
		}
		ids = append(ids, id)
		previousEnd = source.EndByte
	}
	sort.Strings(ids)
	return &whole, ids
}

func validSourceEvidence(item rkcmodel.Evidence, id, method, version string, artifact rkcmodel.Artifact, source *rkcmodel.SourceRange) bool {
	return item.ID == id && item.Kind == "documentation_asserted" && item.Method == method && item.Confidence == 1 &&
		item.Tool == SourcePluginID && item.ToolVersion == version &&
		item.InputDigest == artifact.SHA256 && reflect.DeepEqual(item.Source, source)
}

func sourceInteger(value any, expected int64) bool {
	number, ok := sourceNonnegativeInteger(value)
	return ok && number == expected
}

func sourceNonnegativeInteger(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), number >= 0
	case int64:
		return number, number >= 0
	case float64:
		if number >= 0 && number <= MaximumSourceTotalBytes && float64(int64(number)) == number {
			return int64(number), true
		}
	}
	return 0, false
}
