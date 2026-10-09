package groundedanswer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/docparse"
	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const fictionalHandbook = "# Lending\nFictional lantern kits may be borrowed for 7 days.\n\n# Returns\nFictional lantern kits return to the violet shelf.\n"

// syntheticMarkdownBundle uses the actual Markdown producer against a wholly
// fictional temporary file, so excerpt tests exercise its source-binding shape.
func syntheticMarkdownBundle(t *testing.T, text string) rkcmodel.Bundle {
	t.Helper()
	root := t.TempDir()
	const sourcePath = "handbook.md"
	if err := os.WriteFile(filepath.Join(root, sourcePath), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(text))
	artifact := rkcmodel.Artifact{ID: rkcmodel.StableID("artifact", sourcePath), Path: sourcePath,
		Kind: "document", Language: "markdown", MediaType: "text/markdown", Text: true, Status: "parsed",
		SizeBytes: int64(len(text)), SHA256: hex.EncodeToString(digest[:]), LineCount: strings.Count(text, "\n")}
	fragment, err := docparse.Extract(docparse.Options{Root: root, SnapshotID: "fictional-snapshot",
		Files:     []pluginapi.FileRef{{ArtifactID: artifact.ID, Path: sourcePath, Language: "markdown", SHA256: artifact.SHA256, SizeBytes: artifact.SizeBytes}},
		Artifacts: map[string]string{sourcePath: artifact.ID}})
	if err != nil || len(fragment.Diagnostics) != 0 {
		t.Fatalf("fictional parse failed: %v %+v", err, fragment.Diagnostics)
	}
	projection := rkcmodel.Node{ID: artifact.ID, ArtifactID: artifact.ID, Kind: artifact.Kind,
		Name: sourcePath, QualifiedName: sourcePath, Language: artifact.Language, Visibility: "repository",
		LogicalID: rkcmodel.StableID("logical", "artifact", artifact.Path)}
	return rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{ID: "fictional-snapshot", SchemaVersion: rkcmodel.SchemaVersion},
		Artifacts: []rkcmodel.Artifact{artifact}, Nodes: append([]rkcmodel.Node{projection}, fragment.Nodes...),
		Evidence: fragment.Evidence, Documents: fragment.Documents, Edges: fragment.Edges}
}

func fictionalGroundingRequest(bundle rkcmodel.Bundle, hitID string) Request {
	return Request{Question: "How long may fictional lantern kits be borrowed?", Bundle: bundle,
		Retrieval: search.Response{Hits: []search.Hit{{Document: search.Document{ID: hitID, Body: "FORGED RETRIEVAL BODY: 999 days"}}}}}
}

func fictionalExcerptProvider() *stubProvider {
	provider := testProvider()
	provider.respond = func(request modelruntime.Request) modelruntime.Response {
		response := modelruntime.Response{RequestID: request.RequestID, ModelID: provider.descriptor.ID}
		for _, excerpt := range request.Packet.SourceExcerpts {
			if strings.Contains(excerpt.Text, "7 days") {
				response.Claims = append(response.Claims, modelruntime.ClaimDraft{
					Text: "Fictional lantern kits may be borrowed for 7 days.", Category: "constraint", Certainty: "supported",
					EvidenceIDs: []string{excerpt.EvidenceID}})
				break
			}
		}
		return response
	}
	return provider
}

func TestArtifactProjectionAndCanonicalMarkdownEnableGroundedAnswer(t *testing.T) {
	for _, object := range []string{"artifact", "document", "section"} {
		t.Run(object, func(t *testing.T) {
			bundle := syntheticMarkdownBundle(t, fictionalHandbook)
			id := bundle.Artifacts[0].ID
			if object == "document" {
				id = bundle.Documents[0].ID
			}
			if object == "section" {
				id = bundle.Documents[0].Sections[0].ID
			}
			provider := fictionalExcerptProvider()
			service, _ := New(provider, Options{})
			result, err := service.Answer(context.Background(), fictionalGroundingRequest(bundle, id))
			if err != nil || result.Status != StatusAnswered || len(result.Claims) != 1 || len(result.Citations) != 1 {
				t.Fatalf("canonical fictional workflow failed: result=%+v error=%v", result, err)
			}
			packet := provider.requests[0].Packet
			encoded, _ := json.Marshal(packet)
			if strings.Contains(string(encoded), "FORGED RETRIEVAL BODY") {
				t.Fatal("retrieved body became evidence")
			}
			if len(packet.SourceExcerpts) == 0 {
				t.Fatal("canonical document text omitted")
			}
			for _, excerpt := range packet.SourceExcerpts {
				found := false
				for _, evidence := range bundle.Evidence {
					if evidence.ID == excerpt.EvidenceID {
						found = evidence.Source != nil && reflect.DeepEqual(excerpt.Source, *evidence.Source)
					}
				}
				if !found {
					t.Fatalf("excerpt lost canonical source binding: %+v", excerpt)
				}
			}
		})
	}
}

func TestArtifactProjectionRejectsUnrelatedAndDuplicateCollisions(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rkcmodel.Bundle)
	}{
		{"different kind", func(b *rkcmodel.Bundle) { b.Nodes[0].Kind = "function" }},
		{"different owner", func(b *rkcmodel.Bundle) { b.Nodes[0].ArtifactID = b.Nodes[1].ID }},
		{"different name", func(b *rkcmodel.Bundle) { b.Nodes[0].Name = "Other" }},
		{"different path", func(b *rkcmodel.Bundle) { b.Nodes[0].QualifiedName = "other.md" }},
		{"different logical ID", func(b *rkcmodel.Bundle) { b.Nodes[0].LogicalID = "other-logical" }},
		{"different language", func(b *rkcmodel.Bundle) { b.Nodes[0].Language = "go" }},
		{"different visibility", func(b *rkcmodel.Bundle) { b.Nodes[0].Visibility = "public" }},
		{"sourced symbol", func(b *rkcmodel.Bundle) {
			b.Nodes[0].Source = &rkcmodel.SourceRange{ArtifactID: b.Artifacts[0].ID, Path: b.Artifacts[0].Path}
		}},
		{"symbol signature", func(b *rkcmodel.Bundle) { b.Nodes[0].Signature = "func Other()" }},
		{"projection evidence", func(b *rkcmodel.Bundle) { b.Nodes[0].EvidenceIDs = []string{b.Evidence[0].ID} }},
		{"public symbol", func(b *rkcmodel.Bundle) { b.Nodes[0].PublicSurface = true }},
		{"duplicate projection", func(b *rkcmodel.Bundle) { b.Nodes = append(b.Nodes, b.Nodes[0]) }},
		{"document collision", func(b *rkcmodel.Bundle) { b.Documents[0].ID = b.Artifacts[0].ID }},
		{"duplicate source section", func(b *rkcmodel.Bundle) {
			b.Documents[0].Sections = append(b.Documents[0].Sections, b.Documents[0].Sections[0])
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			bundle := syntheticMarkdownBundle(t, fictionalHandbook)
			test.mutate(&bundle)
			provider := fictionalExcerptProvider()
			service, _ := New(provider, Options{})
			_, err := service.Answer(context.Background(), fictionalGroundingRequest(bundle, bundle.Artifacts[0].ID))
			if !errors.Is(err, ErrInvalidBundle) || provider.calls != 0 {
				t.Fatalf("ambiguous bundle admitted: err=%v calls=%d", err, provider.calls)
			}
		})
	}
}

func TestCanonicalMarkdownExcerptsRejectDerivedOrMismatchedSources(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*rkcmodel.Bundle)
	}{
		{"derived document", func(b *rkcmodel.Bundle) { b.Documents[0].Kind = "model_summary" }},
		{"wrong generator", func(b *rkcmodel.Bundle) { b.Documents[0].Generator = "fictional-model" }},
		{"unvalidated", func(b *rkcmodel.Bundle) { b.Documents[0].Status = "draft" }},
		{"wrong path", func(b *rkcmodel.Bundle) { b.Documents[0].Path = "other.md" }},
		{"wrong document hash", func(b *rkcmodel.Bundle) { b.Documents[0].Attributes["source_sha256"] = "other-hash" }},
		{"wrong document artifact", func(b *rkcmodel.Bundle) { b.Documents[0].Attributes["artifact_id"] = "other-artifact" }},
		{"wrong section range", func(b *rkcmodel.Bundle) {
			for i := range b.Documents[0].Sections {
				b.Documents[0].Sections[i].Attributes["start_line"] = 999
			}
		}},
		{"wrong section heading", func(b *rkcmodel.Bundle) {
			for i := range b.Documents[0].Sections {
				b.Documents[0].Sections[i].Heading = "Other"
			}
		}},
		{"wrong section anchor", func(b *rkcmodel.Bundle) {
			for i := range b.Documents[0].Sections {
				b.Documents[0].Sections[i].Attributes["anchor"] = "other"
			}
		}},
		{"wrong evidence digest", func(b *rkcmodel.Bundle) {
			for i := range b.Evidence {
				b.Evidence[i].InputDigest = "other-hash"
			}
		}},
		{"wrong evidence producer", func(b *rkcmodel.Bundle) {
			for i := range b.Evidence {
				b.Evidence[i].Tool = "fictional-model"
			}
		}},
		{"mismatched ranges", func(b *rkcmodel.Bundle) {
			for i := range b.Evidence {
				source := *b.Evidence[i].Source
				source.StartLine++
				b.Evidence[i].Source = &source
			}
		}},
		{"missing section evidence ownership", func(b *rkcmodel.Bundle) {
			for i := range b.Nodes {
				if b.Nodes[i].Kind == "document_section" {
					b.Nodes[i].EvidenceIDs = nil
				}
			}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			bundle := syntheticMarkdownBundle(t, fictionalHandbook)
			test.mutate(&bundle)
			provider := fictionalExcerptProvider()
			service, _ := New(provider, Options{})
			_, err := service.Answer(context.Background(), fictionalGroundingRequest(bundle, bundle.Artifacts[0].ID))
			if err != nil {
				t.Fatal(err)
			}
			if provider.calls != 1 || len(provider.requests[0].Packet.SourceExcerpts) != 0 {
				t.Fatalf("mismatched/derived text included: %+v", provider.requests)
			}
		})
	}
}

func TestCanonicalMarkdownExcerptsStaySelectedBoundedAndSnapshotSpecific(t *testing.T) {
	bundle := syntheticMarkdownBundle(t, fictionalHandbook)
	provider := fictionalExcerptProvider()
	service, _ := New(provider, Options{})
	section := bundle.Documents[0].Sections[0]
	first, err := service.Answer(context.Background(), fictionalGroundingRequest(bundle, section.ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests[0].Packet.SourceExcerpts) != 1 || strings.Contains(provider.requests[0].Packet.SourceExcerpts[0].Text, "violet shelf") {
		t.Fatal("unselected section entered model context")
	}
	updated := syntheticMarkdownBundle(t, strings.Replace(fictionalHandbook, "7 days", "12 days", 1))
	updated.Snapshot.ID = "fictional-updated-snapshot"
	_, err = service.Answer(context.Background(), fictionalGroundingRequest(updated, updated.Documents[0].Sections[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 2 || !strings.Contains(provider.requests[1].Packet.SourceExcerpts[0].Text, "12 days") ||
		provider.requests[1].Packet.PacketID == first.Provenance.PacketID {
		t.Fatal("source update reused old context identity")
	}
	for _, options := range []Options{{MaximumFieldBytes: 40}, {MaximumContextTextBytes: 600}} {
		provider := fictionalExcerptProvider()
		service, _ := New(provider, options)
		result, err := service.Answer(context.Background(), fictionalGroundingRequest(bundle, bundle.Artifacts[0].ID))
		if err != nil {
			t.Fatal(err)
		}
		if !result.Truncation.ContextText || result.Truncation.IncludedContextBytes > service.options.MaximumContextTextBytes {
			t.Fatal("context budget not recorded/enforced")
		}
		for _, request := range provider.requests {
			for _, excerpt := range request.Packet.SourceExcerpts {
				if len(excerpt.Text) > service.options.MaximumFieldBytes || !utf8.ValidString(excerpt.Text) {
					t.Fatalf("excerpt field escaped budget: %+v", excerpt)
				}
			}
		}
	}
}

func TestCanonicalMarkdownExcerptsRedactSyntheticSecretAndRetainConstraintCategory(t *testing.T) {
	const syntheticValue = "fictional_only_never_a_real_credential_1234"
	bundle := syntheticMarkdownBundle(t, "# Lending\nFictional lantern kits may be borrowed for 7 days.\napi_key="+syntheticValue+"\n")
	provider := fictionalExcerptProvider()
	service, _ := New(provider, Options{})
	result, err := service.Answer(context.Background(), fictionalGroundingRequest(bundle, bundle.Artifacts[0].ID))
	if err != nil || result.Status != StatusAnswered || result.Claims[0].Category != "constraint" {
		t.Fatalf("constraint rejected: %+v %v", result, err)
	}
	packet, _ := json.Marshal(provider.requests[0].Packet)
	if strings.Contains(string(packet), syntheticValue) || !strings.Contains(string(packet), "api_key="+strings.Repeat("*", len(syntheticValue))) {
		t.Fatal("synthetic credential was not redacted")
	}
	provider = testProvider()
	provider.respond = func(request modelruntime.Request) modelruntime.Response {
		return modelruntime.Response{RequestID: request.RequestID, ModelID: provider.descriptor.ID,
			Claims: []modelruntime.ClaimDraft{{Text: "`Alpha` takes no arguments.", Category: "signature", Certainty: "supported", EvidenceIDs: []string{"e-alpha"}}}}
	}
	service, _ = New(provider, Options{})
	result, err = service.Answer(context.Background(), Request{Question: "What is Alpha's signature?", Bundle: testBundle(), Retrieval: testRetrieval()})
	if err != nil || result.Status != StatusAnswered || result.Claims[0].Category != "signature" {
		t.Fatalf("signature rejected: %+v %v", result, err)
	}
}
