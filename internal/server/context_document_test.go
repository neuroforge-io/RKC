package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/docparse"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestContextSourceDocumentPreservesCanonicalReferences(t *testing.T) {
	t.Parallel()
	dataset := fictionalContextDocumentDataset(t, 14, true)
	document := dataset.Bundle.Documents[0]
	whole := dataset.NodeByID[document.SubjectIDs[0]]
	wantEvidence := append([]string(nil), whole.EvidenceIDs...)
	for _, section := range document.Sections {
		wantEvidence = append(wantEvidence, section.EvidenceIDs...)
	}
	sort.Strings(wantEvidence)
	canonicalBefore := fictionalContextCanonicalJSON(t, dataset)

	packet := fictionalContextDocumentPacket(t, dataset)
	item := packet.Items[0]
	if item.ObjectType != "document" || item.ObjectID != document.ID || item.Kind != "source_document" ||
		!reflect.DeepEqual(item.Source, whole.Source) || !reflect.DeepEqual(item.EvidenceIDs, wantEvidence) {
		t.Fatalf("canonical document references were lost: %+v", item)
	}
	if item.Source.StartLine != 1 || item.Source.EndLine != dataset.Bundle.Artifacts[0].LineCount {
		t.Fatalf("source does not locate the whole document: %+v", item.Source)
	}
	citation := sha256.Sum256([]byte(dataset.Manifest.ID + "\x00document\x00" + document.ID))
	if item.CitationID != hex.EncodeToString(citation[:]) || packet.SchemaVersion != "rkc-context/v1" {
		t.Fatalf("existing context citation contract changed: %+v", packet)
	}
	fictionalContextCheckAccounting(t, packet)
	if got := fictionalContextDocumentPacket(t, dataset); !reflect.DeepEqual(got, packet) {
		t.Fatal("enriched document packet is not deterministic")
	}

	packet.Items[0].Source.Path = "fictional-changed.md"
	packet.Items[0].EvidenceIDs[0] = "fictional-changed-evidence"
	if after := fictionalContextCanonicalJSON(t, dataset); !reflect.DeepEqual(after, canonicalBefore) {
		t.Fatal("context output aliases canonical source or evidence references")
	}
	fresh := fictionalContextDocumentPacket(t, dataset)
	if !reflect.DeepEqual(fresh.Items[0].Source, whole.Source) || !reflect.DeepEqual(fresh.Items[0].EvidenceIDs, wantEvidence) {
		t.Fatal("mutating a returned packet changed the immutable dataset")
	}
}

func TestContextSourceDocumentAcceptsProducerIntegerAttributes(t *testing.T) {
	t.Parallel()
	dataset := fictionalContextDocumentDataset(t, 14, false)
	item := fictionalContextDocumentPacket(t, dataset).Items[0]
	if item.Source == nil || len(item.EvidenceIDs) != 1+len(dataset.Bundle.Documents[0].Sections) {
		t.Fatalf("producer's integer section attributes lost provenance: %+v", item)
	}
}

func TestContextSourceDocumentSuppressesIncompleteOrMismatchedReferences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*Dataset, *rkcmodel.Document)
	}{
		{"missing document subject", func(_ *Dataset, document *rkcmodel.Document) { document.SubjectIDs = nil }},
		{"duplicate document subject", func(_ *Dataset, document *rkcmodel.Document) {
			document.SubjectIDs = append(document.SubjectIDs, document.SubjectIDs[0])
		}},
		{"generic generated document", func(_ *Dataset, document *rkcmodel.Document) { document.Kind = "overview" }},
		{"foreign document generator", func(_ *Dataset, document *rkcmodel.Document) { document.Generator = "fictional-generator" }},
		{"unvalidated document", func(_ *Dataset, document *rkcmodel.Document) { document.Status = "draft" }},
		{"document artifact mismatch", func(_ *Dataset, document *rkcmodel.Document) {
			document.Attributes["artifact_id"] = "fictional-other-artifact"
		}},
		{"document digest mismatch", func(_ *Dataset, document *rkcmodel.Document) {
			document.Attributes["source_sha256"] = strings.Repeat("f", 64)
		}},
		{"missing document node", func(dataset *Dataset, document *rkcmodel.Document) {
			delete(dataset.NodeByID, document.SubjectIDs[0])
		}},
		{"missing document evidence", func(dataset *Dataset, document *rkcmodel.Document) {
			delete(dataset.EvidenceByID, dataset.NodeByID[document.SubjectIDs[0]].EvidenceIDs[0])
		}},
		{"nil document evidence source", func(dataset *Dataset, document *rkcmodel.Document) {
			id := dataset.NodeByID[document.SubjectIDs[0]].EvidenceIDs[0]
			evidence := dataset.EvidenceByID[id]
			evidence.Source = nil
			dataset.EvidenceByID[id] = evidence
		}},
		{"document evidence ID mismatch", func(dataset *Dataset, document *rkcmodel.Document) {
			id := dataset.NodeByID[document.SubjectIDs[0]].EvidenceIDs[0]
			evidence := dataset.EvidenceByID[id]
			evidence.ID = "fictional-mismatched-id"
			dataset.EvidenceByID[id] = evidence
		}},
		{"missing section node", func(dataset *Dataset, document *rkcmodel.Document) {
			delete(dataset.NodeByID, document.Sections[0].ID)
		}},
		{"missing section evidence", func(dataset *Dataset, document *rkcmodel.Document) {
			delete(dataset.EvidenceByID, document.Sections[0].EvidenceIDs[0])
		}},
		{"nil section evidence source", func(dataset *Dataset, document *rkcmodel.Document) {
			id := document.Sections[0].EvidenceIDs[0]
			evidence := dataset.EvidenceByID[id]
			evidence.Source = nil
			dataset.EvidenceByID[id] = evidence
		}},
		{"foreign section artifact", func(dataset *Dataset, document *rkcmodel.Document) {
			fictionalContextMutateSectionSource(dataset, document, func(source *rkcmodel.SourceRange) { source.ArtifactID = "fictional-other-artifact" })
		}},
		{"foreign section path", func(dataset *Dataset, document *rkcmodel.Document) {
			fictionalContextMutateSectionSource(dataset, document, func(source *rkcmodel.SourceRange) { source.Path = "fictional-other.md" })
		}},
		{"section range beyond artifact", func(dataset *Dataset, document *rkcmodel.Document) {
			fictionalContextMutateSectionSource(dataset, document, func(source *rkcmodel.SourceRange) { source.EndLine = dataset.Bundle.Artifacts[0].LineCount + 1 })
		}},
		{"negative section byte offset", func(dataset *Dataset, document *rkcmodel.Document) {
			fictionalContextMutateSectionSource(dataset, document, func(source *rkcmodel.SourceRange) { source.StartByte = -1 })
		}},
		{"section byte offset beyond artifact", func(dataset *Dataset, document *rkcmodel.Document) {
			fictionalContextMutateSectionSource(dataset, document, func(source *rkcmodel.SourceRange) { source.EndByte = dataset.Bundle.Artifacts[0].SizeBytes + 1 })
		}},
		{"invented section start column", func(dataset *Dataset, document *rkcmodel.Document) {
			fictionalContextMutateSectionSource(dataset, document, func(source *rkcmodel.SourceRange) { source.StartColumn = 1 })
		}},
		{"invented section end column", func(dataset *Dataset, document *rkcmodel.Document) {
			fictionalContextMutateSectionSource(dataset, document, func(source *rkcmodel.SourceRange) { source.EndColumn = 1 })
		}},
		{"section digest mismatch", func(dataset *Dataset, document *rkcmodel.Document) {
			id := document.Sections[0].EvidenceIDs[0]
			evidence := dataset.EvidenceByID[id]
			evidence.InputDigest = strings.Repeat("f", 64)
			dataset.EvidenceByID[id] = evidence
		}},
		{"section heading mismatch", func(_ *Dataset, document *rkcmodel.Document) {
			document.Sections[0].Heading = "Fictional other heading"
		}},
		{"section anchor mismatch", func(_ *Dataset, document *rkcmodel.Document) {
			document.Sections[0].Attributes["anchor"] = "fictional-other-anchor"
		}},
		{"section line metadata mismatch", func(_ *Dataset, document *rkcmodel.Document) {
			document.Sections[0].Attributes["start_line"] = float64(2)
		}},
		{"string section line metadata", func(_ *Dataset, document *rkcmodel.Document) { document.Sections[0].Attributes["start_line"] = "1" }},
		{"duplicate section", func(_ *Dataset, document *rkcmodel.Document) {
			document.Sections = append(document.Sections, document.Sections[0])
		}},
		{"extra section evidence", func(dataset *Dataset, document *rkcmodel.Document) {
			document.Sections[0].EvidenceIDs = append(document.Sections[0].EvidenceIDs, dataset.NodeByID[document.SubjectIDs[0]].EvidenceIDs[0])
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dataset := fictionalContextDocumentDataset(t, 14, true)
			document := &dataset.Bundle.Documents[0]
			test.mutate(dataset, document)
			// Rebuild only from the fictional canonical projection so changes to
			// generator metadata cannot accidentally remove the selected row.
			dataset.Search = search.BuildFromBundle(dataset.Bundle)
			packet := fictionalContextDocumentPacket(t, dataset)
			item := packet.Items[0]
			if item.Source != nil || item.EvidenceIDs == nil || len(item.EvidenceIDs) != 0 {
				t.Fatalf("partial or invented references escaped validation: %+v", item)
			}
			if !strings.Contains(item.Text, "14 days") {
				t.Fatal("fail-closed references unexpectedly removed the indexed excerpt")
			}
			citation := sha256.Sum256([]byte(dataset.Manifest.ID + "\x00document\x00" + document.ID))
			if item.CitationID != hex.EncodeToString(citation[:]) {
				t.Fatal("invalid provenance changed the existing citation identity")
			}
			fictionalContextCheckAccounting(t, packet)
		})
	}
}

func TestContextSourceDocumentSnapshotIsolation(t *testing.T) {
	t.Parallel()
	previous := fictionalContextDocumentDataset(t, 14, true)
	updated := fictionalContextDocumentDataset(t, 21, true)
	before := fictionalContextDocumentPacket(t, previous)
	after := fictionalContextDocumentPacket(t, updated)
	if before.SnapshotID == after.SnapshotID || before.Items[0].ObjectID != after.Items[0].ObjectID ||
		before.Items[0].CitationID == after.Items[0].CitationID || before.Digest == after.Digest ||
		!strings.Contains(before.Items[0].Text, "14 days") || !strings.Contains(after.Items[0].Text, "21 days") ||
		before.Items[0].Source.ArtifactID == after.Items[0].Source.ArtifactID {
		t.Fatalf("source update lost immutable snapshot/citation identity: before=%+v after=%+v", before, after)
	}
	if retained := fictionalContextDocumentPacket(t, previous); !reflect.DeepEqual(retained, before) {
		t.Fatal("updated source rewrote the prior immutable context packet")
	}
}

func fictionalContextDocumentDataset(t *testing.T, lendingDays int, roundTrip bool) *Dataset {
	t.Helper()
	root := t.TempDir()
	text := fmt.Sprintf("# Lantern handbook\n\n## Lantern lending period\nLantern loans last %d days.\n\n## Lantern returns\nLantern items return to the blue shelf.", lendingDays)
	if err := os.WriteFile(filepath.Join(root, "handbook.md"), []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(text))
	digest := hex.EncodeToString(sum[:])
	artifact := rkcmodel.Artifact{
		ID: rkcmodel.StableID("artifact", "handbook.md", digest), Path: "handbook.md", Kind: "file",
		Language: "markdown", MediaType: "text/markdown", SHA256: digest,
		SizeBytes: int64(len(text)), LineCount: strings.Count(text, "\n") + 1, Text: true, Status: "parsed",
	}
	fragment, err := docparse.Extract(docparse.Options{
		Root: root, SnapshotID: fmt.Sprintf("fictional-lantern-%d", lendingDays),
		Files:     []pluginapi.FileRef{{ArtifactID: artifact.ID, Path: artifact.Path, SHA256: digest}},
		Artifacts: map[string]string{artifact.Path: artifact.ID},
	})
	if err != nil || len(fragment.Documents) != 1 || len(fragment.Diagnostics) != 0 {
		t.Fatalf("fictional Markdown producer failed: %+v %v", fragment, err)
	}
	bundle := rkcmodel.Bundle{
		Snapshot:  rkcmodel.Snapshot{SchemaVersion: rkcmodel.SchemaVersion, ID: fmt.Sprintf("fictional-lantern-%d", lendingDays)},
		Artifacts: []rkcmodel.Artifact{artifact}, Nodes: fragment.Nodes, Edges: fragment.Edges,
		Evidence: fragment.Evidence, Documents: fragment.Documents,
	}
	if roundTrip {
		encoded, err := json.Marshal(bundle)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &bundle); err != nil {
			t.Fatal(err)
		}
	}
	dataset := &Dataset{
		Manifest: bundle.Snapshot, Bundle: bundle, Integrity: IntegrityVerified,
		NodeByID: map[string]rkcmodel.Node{}, ArtifactByID: map[string]rkcmodel.Artifact{}, EvidenceByID: map[string]rkcmodel.Evidence{},
		Search: search.BuildFromBundle(bundle),
	}
	for _, node := range bundle.Nodes {
		dataset.NodeByID[node.ID] = node
	}
	for _, artifact := range bundle.Artifacts {
		dataset.ArtifactByID[artifact.ID] = artifact
	}
	for _, evidence := range bundle.Evidence {
		dataset.EvidenceByID[evidence.ID] = evidence
	}
	return dataset
}

func fictionalContextDocumentPacket(t *testing.T, dataset *Dataset) rkcapi.ContextPacket {
	t.Helper()
	packet, err := dataset.BuildContext(context.Background(), "type:document lantern", 12, 32768)
	if err != nil || len(packet.Items) != 1 {
		t.Fatalf("fictional document retrieval failed: %+v %v", packet, err)
	}
	return packet
}

func fictionalContextCheckAccounting(t *testing.T, packet rkcapi.ContextPacket) {
	t.Helper()
	encoded, err := json.Marshal(packet.Items)
	if err != nil {
		t.Fatal(err)
	}
	if packet.Bytes != len(encoded) || packet.Bytes > packet.MaxBytes {
		t.Fatalf("source/evidence refs escaped exact JSON accounting: %+v", packet)
	}
	digest := packet.Digest
	packet.Digest = ""
	encoded, err = json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(encoded)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatal("enriched packet digest is not reproducible")
	}
}

func fictionalContextCanonicalJSON(t *testing.T, dataset *Dataset) []byte {
	t.Helper()
	encoded, err := json.Marshal(struct {
		Bundle   rkcmodel.Bundle
		Nodes    map[string]rkcmodel.Node
		Evidence map[string]rkcmodel.Evidence
	}{dataset.Bundle, dataset.NodeByID, dataset.EvidenceByID})
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func fictionalContextMutateSectionSource(dataset *Dataset, document *rkcmodel.Document, mutate func(*rkcmodel.SourceRange)) {
	section := document.Sections[0]
	node := dataset.NodeByID[section.ID]
	evidence := dataset.EvidenceByID[section.EvidenceIDs[0]]
	nodeSource, evidenceSource := *node.Source, *evidence.Source
	mutate(&nodeSource)
	mutate(&evidenceSource)
	node.Source, evidence.Source = &nodeSource, &evidenceSource
	dataset.NodeByID[node.ID] = node
	dataset.EvidenceByID[evidence.ID] = evidence
}
