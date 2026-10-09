package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/docparse"
	"github.com/neuroforge-io/RKC/internal/export"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestRedactedSourceNamesPreserveDistinctIdentityExportAndPortableContext(t *testing.T) {
	root := t.TempDir()
	const identicalBody = "Fictional lantern report: preserve both independent source receipts.\n"
	const firstSecret = "source-secret-alpha"
	const secondSecret = "source-secret-beta"
	sourcePaths := []string{"scripts/synthetic_report.txt", "report-" + firstSecret + ".txt", "report-" + secondSecret + ".txt"}
	for _, path := range sourcePaths {
		mustWritePipelineFile(t, filepath.Join(root, filepath.FromSlash(path)), identicalBody)
	}
	// This is also an ordinary word occurring in an unrelated filename. It
	// exercises the same display-path transformation as credential filenames.
	credentials := fmt.Sprintf("%s=%s\n%s=%s\n%s=%s\n", "API_KEY", "synthetic", "PASSWORD", firstSecret, "ACCESS_TOKEN", secondSecret)
	mustWritePipelineFile(t, filepath.Join(root, ".env"), credentials)
	privateFiles := map[string]pluginapi.FileRef{}
	var identities map[string]os.FileInfo
	options := Options{Root: root, ToolVersion: "source-identity-test", SkipGitInspection: true, DisablePlugins: true,
		OnSourceInventory: func(files []pluginapi.FileRef, baseline map[string]os.FileInfo) {
			for _, file := range files {
				privateFiles[file.ArtifactID] = file
			}
			identities = baseline
		}}
	bundle, coverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if len(privateFiles) != 4 || len(identities) != 4 || len(bundle.Documents) != len(sourcePaths) {
		t.Fatalf("source inventory/documents were dropped: files=%d identities=%d documents=%d", len(privateFiles), len(identities), len(bundle.Documents))
	}
	artifacts := make(map[string]rkcmodel.Artifact, len(bundle.Artifacts))
	nodes := make(map[string]rkcmodel.Node, len(bundle.Nodes))
	evidence := make(map[string]rkcmodel.Evidence, len(bundle.Evidence))
	for _, artifact := range bundle.Artifacts {
		artifacts[artifact.ID] = artifact
	}
	for _, node := range bundle.Nodes {
		nodes[node.ID] = node
	}
	for _, item := range bundle.Evidence {
		evidence[item.ID] = item
	}
	documentByArtifact := make(map[string]rkcmodel.Document, len(bundle.Documents))
	logicalIDs := map[string]bool{}
	for _, document := range bundle.Documents {
		id := document.Attributes["artifact_id"].(string)
		documentByArtifact[id] = document
		if document.GeneratorVersion != docparse.SourcePluginVersion || document.ID != rkcmodel.StableID("document", docparse.SourcePluginID, id) || logicalIDs[document.LogicalID] {
			t.Fatalf("redacted display path changed or combined document identity: %+v", document)
		}
		logicalIDs[document.LogicalID] = true
		whole, ids := docparse.SourceDocumentReferences(context.Background(), document, artifacts, nodes, evidence)
		if whole == nil || whole.ArtifactID != id || whole.EndByte != int64(len(identicalBody)) || len(ids) != 2 {
			t.Fatalf("redacted path lost complete source binding: %+v, %v", whole, ids)
		}
	}
	firstID := rkcmodel.StableID("artifact", sourcePaths[1])
	secondID := rkcmodel.StableID("artifact", sourcePaths[2])
	if artifacts[firstID].Path != "report-[REDACTED].txt" || artifacts[secondID].Path != artifacts[firstID].Path ||
		artifacts[firstID].SHA256 != artifacts[secondID].SHA256 || artifacts[firstID].SizeBytes != artifacts[secondID].SizeBytes ||
		documentByArtifact[firstID].ID == documentByArtifact[secondID].ID || documentByArtifact[firstID].LogicalID == documentByArtifact[secondID].LogicalID {
		t.Fatalf("identical sources with colliding display names were not preserved: first=%+v second=%+v", artifacts[firstID], artifacts[secondID])
	}
	for _, rawPath := range sourcePaths {
		id := rkcmodel.StableID("artifact", rawPath)
		if privateFiles[id].Path != rawPath || artifacts[id].ID != id || !strings.Contains(artifacts[id].Path, "[REDACTED]") {
			t.Fatalf("private original authority and safe display were confused: private=%+v artifact=%+v", privateFiles[id], artifacts[id])
		}
	}
	encoded, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, literal := range []string{"synthetic", firstSecret, secondSecret} {
		if strings.Contains(string(encoded), literal) {
			t.Fatalf("canonical bundle exposed a private filename/value: %s", literal)
		}
	}
	output := filepath.Join(t.TempDir(), "atlas")
	if err := export.WriteAll(bundle, coverage, export.Options{Root: root, Output: output, IncludeSources: true, SourceFiles: privateFiles, SourceIdentities: identities}); err != nil {
		t.Fatalf("safe display paths broke verified original-source export: %v", err)
	}
	envelopes := map[string]string{}
	if err := filepath.WalkDir(output, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, literal := range []string{"synthetic", firstSecret, secondSecret} {
			if strings.Contains(string(body), literal) {
				t.Errorf("export %s disclosed a private filename/value: %s", path, literal)
			}
		}
		relative, err := filepath.Rel(output, path)
		if err != nil {
			return err
		}
		if strings.HasPrefix(filepath.ToSlash(relative), "normalized/") && strings.HasSuffix(path, ".md") {
			for _, rawPath := range sourcePaths {
				id := rkcmodel.StableID("artifact", rawPath)
				if strings.Contains(string(body), fmt.Sprintf("rkc_artifact_id: %q", id)) {
					if envelopes[id] != "" || !strings.Contains(string(body), identicalBody) {
						t.Errorf("normalized envelope duplicated or lost original source: %s", id)
					}
					envelopes[id] = relative
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(envelopes) != len(sourcePaths) || envelopes[firstID] == envelopes[secondID] {
		t.Fatalf("normalized sources overwrote a colliding display name: %+v", envelopes)
	}
	packs, err := filepath.Glob(filepath.Join(output, "notebooklm", "05_repository_sources*.md"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("missing repository NotebookLM packs: %v", err)
	}
	var packed strings.Builder
	for _, pack := range packs {
		body, err := os.ReadFile(pack)
		if err != nil {
			t.Fatal(err)
		}
		packed.Write(body)
	}
	for _, rawPath := range sourcePaths {
		id := rkcmodel.StableID("artifact", rawPath)
		if strings.Count(packed.String(), "- Artifact ID: "+id+"\n") != 1 {
			t.Fatalf("NotebookLM pack combined or omitted an independent source: %s", id)
		}
	}
	// Once published, citation construction must not need raw source authority
	// or be able to reopen the former checkout path.
	if err := os.Rename(root, root+"-moved"); err != nil {
		t.Fatal(err)
	}
	dataset, err := server.Load(output)
	if err != nil {
		t.Fatalf("portable source dataset load: %v", err)
	}
	packet, err := dataset.BuildContext(context.Background(), "type:document lantern", 12, 32768)
	if err != nil || len(packet.Items) != len(sourcePaths) || packet.Truncated {
		t.Fatalf("portable source context lost distinct files: %v %+v", err, packet)
	}
	citations := map[string]bool{}
	contextArtifacts := map[string]bool{}
	for _, item := range packet.Items {
		if item.Source == nil || len(item.EvidenceIDs) != 2 || item.Source.EndByte != int64(len(identicalBody)) || citations[item.CitationID] || contextArtifacts[item.Source.ArtifactID] {
			t.Fatalf("portable context combined or lost original source references: %+v", item)
		}
		if item.ObjectID != documentByArtifact[item.Source.ArtifactID].ID {
			t.Fatalf("portable source was bound to another document identity: %+v", item)
		}
		citations[item.CitationID], contextArtifacts[item.Source.ArtifactID] = true, true
	}
	if !contextArtifacts[firstID] || !contextArtifacts[secondID] {
		t.Fatal("colliding safe display names no longer have separate cited sources")
	}
}
