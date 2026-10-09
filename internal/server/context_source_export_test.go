package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/docparse"
	"github.com/neuroforge-io/RKC/internal/inventory"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestSourceExportContextPreservesPortableByteAndEvidenceReferences(t *testing.T) {
	for _, name := range []string{"notes.txt", "events.log", "rules.jsonl", "rules.csv"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			body := "Fictional lantern lending period is 14 days.\n"
			if strings.HasSuffix(name, ".jsonl") {
				body = "{\"policy\":\"Fictional lantern lending period is 14 days.\"}\n"
			}
			if strings.HasSuffix(name, ".csv") {
				body = "policy\nFictional lantern lending period is 14 days.\n"
			}
			if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
				t.Fatal(err)
			}
			inv, err := inventory.Scan(inventory.Options{Root: root})
			if err != nil || len(inv.Artifacts) != 1 {
				t.Fatalf("inventory: %v %+v", err, inv)
			}
			artifact := inv.Artifacts[0]
			fragment, err := docparse.ExtractSources(context.Background(), docparse.Options{Root: root,
				Files: []pluginapi.FileRef{{ArtifactID: artifact.ID, Path: artifact.Path, Language: artifact.Language,
					SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes}}})
			if err != nil || len(fragment.Documents) != 1 {
				t.Fatalf("document extraction: %v %+v", err, fragment)
			}
			bundle := rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{ID: "fictional-export"}, Artifacts: inv.Artifacts,
				Documents: fragment.Documents, Nodes: fragment.Nodes, Evidence: fragment.Evidence}
			// Exercise the imported/relocated JSON shape as well as producer ints.
			encoded, _ := json.Marshal(bundle)
			if err := json.Unmarshal(encoded, &bundle); err != nil {
				t.Fatal(err)
			}
			dataset := &Dataset{Manifest: bundle.Snapshot, Bundle: bundle,
				ArtifactByID: map[string]rkcmodel.Artifact{artifact.ID: artifact},
				NodeByID:     map[string]rkcmodel.Node{}, EvidenceByID: map[string]rkcmodel.Evidence{},
				Search: search.BuildFromBundle(bundle)}
			for _, node := range bundle.Nodes {
				dataset.NodeByID[node.ID] = node
			}
			for _, evidence := range bundle.Evidence {
				dataset.EvidenceByID[evidence.ID] = evidence
			}
			packet, err := dataset.BuildContext(context.Background(), "type:document lantern", 12, 32768)
			if err != nil || len(packet.Items) != 1 {
				t.Fatalf("source export context: %v %+v", err, packet)
			}
			item := packet.Items[0]
			if item.Source == nil || item.Source.Path != name || item.Source.StartByte != 0 ||
				item.Source.EndByte != int64(len(body)) || len(item.EvidenceIDs) != 1+len(bundle.Documents[0].Sections) {
				t.Fatalf("source references missing or inaccurate: %+v", item)
			}
			fictionalContextCheckAccounting(t, packet)
			id := bundle.Documents[0].Sections[0].EvidenceIDs[0]
			delete(dataset.EvidenceByID, id)
			packet, err = dataset.BuildContext(context.Background(), "type:document lantern", 12, 32768)
			if err != nil || len(packet.Items) != 1 || packet.Items[0].Source != nil || len(packet.Items[0].EvidenceIDs) != 0 {
				t.Fatalf("incomplete provenance gained references: %v %+v", err, packet)
			}
		})
	}
}

func TestTruncatedSourceProjectionWarnsWhenEntireContextItemFits(t *testing.T) {
	root := t.TempDir()
	name := "research.jsonl"
	body := "{\"policy\":\"Fictional lantern lending policy: " + strings.Repeat("x", docparse.MaximumSourceSectionBytes) + "\"}\n"
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	inv, err := inventory.Scan(inventory.Options{Root: root})
	if err != nil || len(inv.Artifacts) != 1 {
		t.Fatalf("inventory: %v %+v", err, inv)
	}
	artifact := inv.Artifacts[0]
	fragment, err := docparse.ExtractSources(context.Background(), docparse.Options{Root: root,
		Files: []pluginapi.FileRef{{ArtifactID: artifact.ID, Path: artifact.Path,
			Language: artifact.Language, SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes}}})
	if err != nil || len(fragment.Documents) != 1 || len(fragment.Documents[0].Sections) != 1 {
		t.Fatalf("record extraction: %v %+v", err, fragment)
	}
	document := fragment.Documents[0]
	if document.Attributes["complete"] != false || document.Sections[0].Attributes["projection_truncated"] != true {
		t.Fatalf("fixture did not reach the actual record projection limit: %+v", document)
	}
	bundle := rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{ID: "fictional-truncated-export"},
		Artifacts: inv.Artifacts, Documents: fragment.Documents, Nodes: fragment.Nodes, Evidence: fragment.Evidence}
	dataset := &Dataset{Manifest: bundle.Snapshot, Bundle: bundle,
		ArtifactByID: map[string]rkcmodel.Artifact{artifact.ID: artifact},
		NodeByID:     map[string]rkcmodel.Node{}, EvidenceByID: map[string]rkcmodel.Evidence{},
		Search: search.BuildFromBundle(bundle)}
	for _, node := range bundle.Nodes {
		dataset.NodeByID[node.ID] = node
	}
	for _, evidence := range bundle.Evidence {
		dataset.EvidenceByID[evidence.ID] = evidence
	}
	const budget = 32768
	packet, err := dataset.BuildContext(context.Background(), "type:document lantern", 12, budget)
	if err != nil || len(packet.Items) != 1 || packet.Bytes >= budget || packet.Items[0].Source == nil {
		t.Fatalf("bounded partial record did not fit as one cited item: %v %+v", err, packet)
	}
	if !packet.Truncated {
		t.Fatal("source projection omission was hidden because the item fit the context budget")
	}
	warnings := strings.Join(packet.Warnings, "\n")
	if !strings.Contains(strings.ToLower(warnings), "projection") || !strings.Contains(strings.ToLower(warnings), "incomplete") {
		t.Fatalf("context does not explain the incomplete source projection: %v", packet.Warnings)
	}
	if strings.Contains(warnings, "item or byte budget") {
		t.Fatalf("source truncation was mislabeled as a context budget omission: %v", packet.Warnings)
	}
	fictionalContextCheckAccounting(t, packet)
}
