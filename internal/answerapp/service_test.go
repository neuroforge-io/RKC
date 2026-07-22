package answerapp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/groundedanswer"
	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/retrieval"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type answerProvider struct {
	descriptor modelruntime.ModelDescriptor
	requests   []modelruntime.Request
}

func (provider *answerProvider) Descriptor() modelruntime.ModelDescriptor { return provider.descriptor }
func (provider *answerProvider) Supports(modelruntime.Task) bool          { return true }
func (provider *answerProvider) Close() error                             { return nil }
func (provider *answerProvider) Generate(_ context.Context, request modelruntime.Request) (modelruntime.Response, error) {
	provider.requests = append(provider.requests, request)
	return modelruntime.Response{
		RequestID: request.RequestID,
		ModelID:   provider.descriptor.ID,
		Claims: []modelruntime.ClaimDraft{{
			Text: "Alpha calls Beta.", Category: "relationship", Certainty: "supported",
			EvidenceIDs: []string{"e-alpha", "e-edge"},
		}},
	}, nil
}

func TestAnswerUsesLexicalGraphRetrievalAndCanonicalizesTamperedHits(t *testing.T) {
	bundle := answerBundle()
	lexical := search.BuildFromBundle(bundle)
	forged := lexical.Documents["node-alpha"]
	forged.Body = "FORGED RETRIEVAL BODY MUST NOT REACH THE MODEL"
	lexical.Documents[forged.ID] = forged
	provider := &answerProvider{descriptor: modelruntime.ModelDescriptor{
		ID: "test-model", Digest: "sha256:model", RuntimeDigest: "sha256:runtime",
	}}
	service, err := New(
		bundle,
		&retrieval.Engine{Lexical: lexical, Graph: graph.Build(bundle.Nodes, bundle.Edges)},
		provider,
		groundedanswer.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := service.Answer(context.Background(), Request{
		Question: "How does Alpha call Beta?", Limit: 10, GraphHops: 2,
		GraphNodeLimit: 100, Task: modelruntime.TaskModuleSummary,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != groundedanswer.StatusAnswered || len(result.Claims) != 1 || len(result.Citations) != 2 {
		t.Fatalf("answer result = %+v", result)
	}
	if result.Provenance.Retrieval.Mode != "embedded-bm25-lexical+graph" || result.Provenance.Retrieval.Query != "How does Alpha call Beta?" {
		t.Fatalf("retrieval provenance = %+v", result.Provenance.Retrieval)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("provider calls = %d", len(provider.requests))
	}
	packet, err := json.Marshal(provider.requests[0].Packet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(packet), "FORGED RETRIEVAL BODY") {
		t.Fatalf("untrusted search document entered canonical packet: %s", packet)
	}
	if !reflect.DeepEqual(result.Claims[0].EvidenceIDs, []string{"e-alpha", "e-edge"}) {
		t.Fatalf("claim evidence = %v", result.Claims[0].EvidenceIDs)
	}
}

func TestAnswerCancellationStopsBeforeRetrievalOrGeneration(t *testing.T) {
	bundle := answerBundle()
	provider := &answerProvider{descriptor: modelruntime.ModelDescriptor{ID: "test-model"}}
	service, err := New(
		bundle,
		&retrieval.Engine{Lexical: search.BuildFromBundle(bundle), Graph: graph.Build(bundle.Nodes, bundle.Edges)},
		provider,
		groundedanswer.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.Answer(ctx, Request{Question: "What is Alpha?", Limit: 10, GraphNodeLimit: 100})
	if !errors.Is(err, context.Canceled) || len(provider.requests) != 0 {
		t.Fatalf("cancelled answer error=%v provider calls=%d", err, len(provider.requests))
	}
}

func answerBundle() rkcmodel.Bundle {
	return rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{SchemaVersion: rkcmodel.SchemaVersion, ID: "snapshot-answer"},
		Artifacts: []rkcmodel.Artifact{
			{ID: "artifact-alpha", Path: "alpha.go", Kind: "source", Language: "go", Text: true, Status: "parsed"},
			{ID: "artifact-beta", Path: "beta.go", Kind: "source", Language: "go", Text: true, Status: "parsed"},
		},
		Nodes: []rkcmodel.Node{
			{ID: "node-alpha", ArtifactID: "artifact-alpha", Kind: "function", Name: "Alpha", QualifiedName: "pkg.Alpha", EvidenceIDs: []string{"e-alpha"}},
			{ID: "node-beta", ArtifactID: "artifact-beta", Kind: "function", Name: "Beta", QualifiedName: "pkg.Beta", EvidenceIDs: []string{"e-beta"}},
		},
		Edges: []rkcmodel.Edge{{
			ID: "edge-alpha-beta", Kind: "calls", From: "node-alpha", To: "node-beta",
			Resolution: "resolved", Confidence: 1, EvidenceIDs: []string{"e-edge"},
		}},
		Evidence: []rkcmodel.Evidence{
			{ID: "e-alpha", Kind: "syntax", Method: "ast", Confidence: 1, Detail: "Alpha declaration."},
			{ID: "e-beta", Kind: "syntax", Method: "ast", Confidence: 1, Detail: "Beta declaration."},
			{ID: "e-edge", Kind: "relationship", Method: "ast", Confidence: 1, Detail: "Alpha calls Beta."},
		},
	}
}
