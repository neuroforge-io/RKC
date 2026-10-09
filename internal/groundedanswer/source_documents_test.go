package groundedanswer

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/docparse"
	"github.com/neuroforge-io/RKC/internal/inventory"
	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func sourceExportBundle(t *testing.T, name, content string) rkcmodel.Bundle {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := inventory.Scan(inventory.Options{Root: root})
	if err != nil || len(result.Artifacts) != 1 {
		t.Fatalf("inventory: %v %+v", err, result)
	}
	artifact := result.Artifacts[0]
	fragment, err := docparse.ExtractSources(context.Background(), docparse.Options{Root: root,
		Files: []pluginapi.FileRef{{ArtifactID: artifact.ID, Path: artifact.Path,
			Language: artifact.Language, SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes}}})
	if err != nil || len(fragment.Documents) != 1 {
		t.Fatalf("source document extraction: %v %+v", err, fragment)
	}
	projection := rkcmodel.Node{ID: artifact.ID, ArtifactID: artifact.ID, Kind: artifact.Kind,
		Name: name, QualifiedName: name, Language: artifact.Language, Visibility: "repository",
		LogicalID: rkcmodel.StableID("logical", "artifact", name)}
	return rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{SchemaVersion: rkcmodel.SchemaVersion, ID: "fictional-export"},
		Artifacts: result.Artifacts, Nodes: append([]rkcmodel.Node{projection}, fragment.Nodes...),
		Documents: fragment.Documents, Evidence: fragment.Evidence, Edges: fragment.Edges}
}

func TestMessySourceDocumentsReachGroundedAnswers(t *testing.T) {
	for _, source := range []struct{ name, content string }{
		{"notes.txt", "Fictional lantern kits may be borrowed for 7 days.\n"},
		{"activity.log", "Fictional lantern kits may be borrowed for 7 days.\n"},
		{"records.jsonl", "{\"policy\":\"Fictional lantern kits may be borrowed for 7 days.\",\"api_key\":\"fictional-secret-only\"}\n"},
		{"records.csv", "policy,api_key\nFictional lantern kits may be borrowed for 7 days.,fictional-secret-only\n"},
	} {
		t.Run(source.name, func(t *testing.T) {
			bundle := sourceExportBundle(t, source.name, source.content)
			provider := fictionalExcerptProvider()
			service, err := New(provider, Options{})
			if err != nil {
				t.Fatal(err)
			}
			answer, err := service.Answer(context.Background(), fictionalGroundingRequest(bundle, bundle.Documents[0].ID))
			if err != nil || answer.Status != StatusAnswered || len(answer.Citations) != 1 {
				t.Fatalf("source document answer: %v %+v", err, answer)
			}
			packet := provider.requests[0].Packet
			data, _ := json.Marshal(packet)
			if len(packet.SourceExcerpts) == 0 || strings.Contains(string(data), "fictional-secret-only") || strings.Contains(string(data), "FORGED RETRIEVAL BODY") {
				t.Fatalf("source projection missing or untrusted retrieval/secret entered prompt: %s", data)
			}
			for _, excerpt := range packet.SourceExcerpts {
				if excerpt.Source.Path != source.name || excerpt.Source.EndByte > int64(len(source.content)) {
					t.Fatalf("source byte binding lost: %+v", excerpt)
				}
			}
		})
	}
}

func TestSourceDocumentTamperingCannotEnterModelContext(t *testing.T) {
	for _, mutate := range []struct {
		name  string
		apply func(*rkcmodel.Bundle)
	}{
		{"digest", func(bundle *rkcmodel.Bundle) {
			bundle.Documents[0].Attributes["source_sha256"] = strings.Repeat("0", 64)
		}},
		{"byte range", func(bundle *rkcmodel.Bundle) { bundle.Documents[0].Sections[0].Attributes["end_byte"] = 999999 }},
		{"producer", func(bundle *rkcmodel.Bundle) { bundle.Documents[0].Generator = "untrusted-generator" }},
		{"projection", func(bundle *rkcmodel.Bundle) {
			bundle.Documents[0].Sections[0].Attributes["projection"] = "model-authored"
		}},
	} {
		t.Run(mutate.name, func(t *testing.T) {
			bundle := sourceExportBundle(t, "notes.txt", "Fictional lantern kits may be borrowed for 7 days.\n")
			mutate.apply(&bundle)
			provider := fictionalExcerptProvider()
			service, _ := New(provider, Options{})
			result, err := service.Answer(context.Background(), fictionalGroundingRequest(bundle, bundle.Documents[0].ID))
			if err != nil || result.Status == StatusAnswered {
				t.Fatalf("tampered document accepted: %v %+v", err, result)
			}
			for _, request := range provider.requests {
				if len(request.Packet.SourceExcerpts) != 0 {
					t.Fatal("tampered source text reached model")
				}
			}
		})
	}
}

func TestProviderReportedRevisionIsAuditedAndBounded(t *testing.T) {
	for _, revision := range []string{"hosted-model-2026-10-09", "bad\nmodel", strings.Repeat("x", 257)} {
		bundle := syntheticMarkdownBundle(t, fictionalHandbook)
		provider := fictionalExcerptProvider()
		original := provider.respond
		provider.respond = func(request modelruntime.Request) modelruntime.Response {
			response := original(request)
			response.ReportedModelID = revision
			return response
		}
		service, _ := New(provider, Options{})
		result, err := service.Answer(context.Background(), fictionalGroundingRequest(bundle, bundle.Documents[0].ID))
		if revision == "hosted-model-2026-10-09" {
			if err != nil || result.Provenance.ReportedModelID != revision || result.Provenance.ModelID != provider.Descriptor().ID {
				t.Fatalf("requested and reported models lost: %v %+v", err, result)
			}
		} else if !errors.Is(err, ErrModelProtocol) {
			t.Fatalf("invalid reported revision accepted: %v", err)
		}
	}
}
