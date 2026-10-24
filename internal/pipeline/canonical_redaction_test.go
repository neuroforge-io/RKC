package pipeline

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/docparse"
	"github.com/neuroforge-io/RKC/internal/export"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestCredentialLiteralsCannotCorruptCanonicalHashesKindsOrSourceBindings(t *testing.T) {
	root := t.TempDir()
	bodies := map[string]string{
		"notes.txt": "Orchid source evidence.\n",
		"main.go":   "package orchid\n\nfunc Harvest() bool { return true }\n",
		"empty.txt": "",
	}
	digestOf := func(body string) string {
		digest := sha256.Sum256([]byte(body))
		return hex.EncodeToString(digest[:])
	}
	notesDigest := digestOf(bodies["notes.txt"])
	bodies["README.md"] = "# Orchid handbook\n\nDigest receipt " + notesDigest + "\n"
	// Materialize assignments only at runtime: tracked source is not itself a
	// credential fixture. The digest collisions are exact, independently computed
	// hashes, including the common empty-file digest.
	values := []string{"function", notesDigest, digestOf(bodies["README.md"]), digestOf("")}
	var credentials strings.Builder
	for _, value := range values {
		body, err := json.Marshal(map[string]string{"topic": "Orchid control", "api_key": value})
		if err != nil {
			t.Fatal(err)
		}
		credentials.Write(body)
		credentials.WriteByte('\n')
	}
	bodies["credentials.jsonl"] = credentials.String()
	for name, body := range bodies {
		mustWritePipelineFile(t, filepath.Join(root, name), body)
	}
	cache, err := OpenStageCache(filepath.Join(t.TempDir(), "cache"))
	if err != nil {
		t.Fatal(err)
	}
	options := Options{Root: root, ToolVersion: "structural-redaction-test", SkipGitInspection: true,
		DisablePythonAST: true, DisableTypeScript: true, Cache: cache}
	bundle, coverage, err := Scan(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	if coverage.HighConfidenceSecretFindings < len(values) {
		t.Fatalf("structural metadata exemption lost source secret findings: %+v", coverage)
	}
	if report := rkcmodel.ValidateBundle(bundle, rkcmodel.ValidationOptions{StrictVocabulary: true, RequireEvidence: true}); report.HasErrors() {
		t.Fatalf("strict canonical validation failed: %+v", report)
	}
	artifacts := map[string]rkcmodel.Artifact{}
	for _, artifact := range bundle.Artifacts {
		if artifact.SHA256 != digestOf(bodies[artifact.Path]) {
			t.Fatalf("original content digest changed for %s", artifact.Path)
		}
		artifacts[artifact.ID] = artifact
	}
	foundFunction := false
	for _, node := range bundle.Nodes {
		if node.Name == "Harvest" && node.Kind == "function" {
			foundFunction = true
		}
		if artifact, ok := artifacts[node.ID]; ok && (node.Attributes["sha256"] != artifact.SHA256 || node.Attributes["status"] != artifact.Status) {
			t.Fatal("inventory node receipt disagrees with canonical artifact")
		}
	}
	if !foundFunction {
		t.Fatal("source credential corrupted function analysis")
	}
	for _, document := range bundle.Documents {
		for _, section := range document.Sections {
			if strings.Contains(section.PlainText, notesDigest) || strings.Contains(section.Markdown, notesDigest) {
				t.Fatal("typed digest exemption leaked into document body redaction")
			}
		}
	}
	warm, warmCoverage, err := Scan(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	requireCanonicalScanEquality(t, bundle, coverage, warm, warmCoverage)
	options.Cache = nil
	sequential, sequentialCoverage, err := scanSequential(t.Context(), options)
	if err != nil {
		t.Fatal(err)
	}
	requireCanonicalScanEquality(t, bundle, coverage, sequential, sequentialCoverage)
	output := filepath.Join(t.TempDir(), "atlas")
	if err := export.WriteAll(bundle, coverage, export.Options{Root: root, Output: output, IncludeSources: true}); err != nil {
		t.Fatal(err)
	}
	dataset, err := server.Load(output)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := dataset.BuildContext(context.Background(), "type:document orchid", 12, 32768)
	if err != nil || len(packet.Items) != 3 {
		t.Fatalf("source context lost documents: %v %+v", err, packet)
	}
	for _, item := range packet.Items {
		if item.Source == nil || len(item.EvidenceIDs) == 0 {
			t.Fatal("metadata redaction lost verified document provenance")
		}
		for _, id := range item.EvidenceIDs {
			if evidence := dataset.EvidenceByID[id]; evidence.InputDigest != artifacts[item.Source.ArtifactID].SHA256 {
				t.Fatal("source evidence digest no longer matches its artifact")
			}
		}
		for _, value := range values {
			if strings.Contains(item.Text, value) {
				t.Fatal("source context exposed materialized credential value")
			}
		}
	}
}

func TestCanonicalReceiptCopiesRequireMatchingTypedArtifactMetadata(t *testing.T) {
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte("known content")))
	for _, kind := range []string{"valid source", "valid markdown", "wrong generator", "wrong artifact", "wrong path", "wrong digest", "malformed digest"} {
		t.Run(kind, func(t *testing.T) {
			artifact := rkcmodel.Artifact{ID: "artifact", Path: "notes.txt", SHA256: digest, Kind: "file", Status: "text"}
			document := rkcmodel.Document{Kind: "source_document", Generator: docparse.SourcePluginID, Path: artifact.Path,
				Attributes: map[string]any{"artifact_id": artifact.ID, "source_sha256": digest, "sha256": digest}}
			switch kind {
			case "valid markdown":
				document.Generator = docparse.PluginID
			case "wrong generator":
				document.Generator = "untrusted-plugin"
			case "wrong artifact":
				document.Attributes["artifact_id"] = "other"
			case "wrong path":
				document.Path = "other.txt"
			case "wrong digest":
				artifact.SHA256 = strings.Repeat("0", 64)
			case "malformed digest":
				artifact.SHA256 = "malformed-" + digest
				document.Attributes["source_sha256"] = artifact.SHA256
			}
			bundle := rkcmodel.Bundle{Artifacts: []rkcmodel.Artifact{artifact}, Documents: []rkcmodel.Document{document},
				Nodes: []rkcmodel.Node{{ID: "ordinary-node", Attributes: map[string]any{"sha256": digest, "kind": "function"}}}}
			sanitizeCanonicalBundle(&bundle, []string{digest, "function"})
			got := bundle.Documents[0].Attributes
			if (kind == "valid source" || kind == "valid markdown") != (got["source_sha256"] == digest) {
				t.Fatalf("source receipt scope incorrect for %s: %+v", kind, got)
			}
			if got["sha256"] == digest || bundle.Nodes[0].Attributes["sha256"] == digest || bundle.Nodes[0].Attributes["kind"] == "function" {
				t.Fatal("structural receipt exemption escaped into arbitrary attributes")
			}
		})
	}
}
