package answerapp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

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

type answerEmbedder struct {
	descriptor search.EmbeddingDescriptor
	calls      int
}

func (embedder *answerEmbedder) Descriptor() search.EmbeddingDescriptor {
	return embedder.descriptor
}

func (embedder *answerEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	embedder.calls++
	vectors := make([][]float32, len(texts))
	for index := range vectors {
		vectors[index] = []float32{1, 0}
	}
	return vectors, nil
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

func TestAnswerSelectsSemanticAndHybridGraphRetrieval(t *testing.T) {
	bundle := answerBundle()
	lexical := search.BuildFromBundle(bundle)
	embedder := &answerEmbedder{descriptor: search.EmbeddingDescriptor{
		Provider: "test-embedding", Model: "embedding-model", Digest: "sha256:embedding", Dimensions: 2,
	}}
	vector := answerVectorIndex(lexical, embedder.descriptor)
	for _, test := range []struct {
		mode       retrieval.Mode
		provenance string
	}{
		{retrieval.ModeSemantic, "dense-cosine+graph"},
		{retrieval.ModeHybrid, "hybrid-rrf+graph"},
	} {
		t.Run(string(test.mode), func(t *testing.T) {
			provider := &answerProvider{descriptor: modelruntime.ModelDescriptor{ID: "test-model"}}
			service, err := New(
				bundle,
				&retrieval.Engine{
					Lexical: lexical, Vector: vector, Embedder: embedder,
					Graph: graph.Build(bundle.Nodes, bundle.Edges),
				},
				provider,
				groundedanswer.Options{},
			)
			if err != nil {
				t.Fatal(err)
			}
			result, err := service.Answer(context.Background(), Request{
				Question: "How does Alpha call Beta?", RetrievalMode: test.mode,
				ObjectTypes: map[string]struct{}{"node": {}}, Limit: 10,
				GraphHops: 1, GraphNodeLimit: 100, Task: modelruntime.TaskModuleSummary,
			})
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != groundedanswer.StatusAnswered || result.Provenance.Retrieval.Mode != test.provenance {
				t.Fatalf("result status=%s retrieval=%+v", result.Status, result.Provenance.Retrieval)
			}
		})
	}
	if embedder.calls != 2 {
		t.Fatalf("embedding calls = %d", embedder.calls)
	}
}

func TestAnswerSemanticRetrievalFailsClosedWithoutEmbeddingResources(t *testing.T) {
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
	_, err = service.Answer(context.Background(), Request{
		Question: "What is Alpha?", RetrievalMode: retrieval.ModeSemantic,
		Limit: 10, GraphNodeLimit: 100,
	})
	if err == nil || !strings.Contains(err.Error(), "semantic search requires") || len(provider.requests) != 0 {
		t.Fatalf("semantic failure=%v provider calls=%d", err, len(provider.requests))
	}
	_, err = service.Answer(context.Background(), Request{
		Question: "What is Alpha?", RetrievalMode: retrieval.Mode("remote"),
		Limit: 10, GraphNodeLimit: 100,
	})
	if !errors.Is(err, ErrInvalidRequest) || len(provider.requests) != 0 {
		t.Fatalf("invalid mode failure=%v provider calls=%d", err, len(provider.requests))
	}
}

func TestAnswerValidatesConstructionAndRequestBounds(t *testing.T) {
	bundle := answerBundle()
	provider := &answerProvider{descriptor: modelruntime.ModelDescriptor{ID: "test-model"}}
	if _, err := New(bundle, nil, provider, groundedanswer.Options{}); err == nil || !strings.Contains(err.Error(), "lexical index") {
		t.Fatalf("nil engine error = %v", err)
	}
	if _, err := New(bundle, &retrieval.Engine{}, provider, groundedanswer.Options{}); err == nil || !strings.Contains(err.Error(), "lexical index") {
		t.Fatalf("empty engine error = %v", err)
	}
	if _, err := New(bundle, &retrieval.Engine{Lexical: search.BuildFromBundle(bundle)}, nil, groundedanswer.Options{}); err == nil {
		t.Fatal("nil provider unexpectedly accepted")
	}
	var nilService *Service
	if _, err := nilService.Answer(context.Background(), Request{}); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("nil service error = %v", err)
	}
	service, err := New(
		bundle,
		&retrieval.Engine{Lexical: search.BuildFromBundle(bundle), Graph: graph.Build(bundle.Nodes, bundle.Edges)},
		provider,
		groundedanswer.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	valid := Request{Question: "What is Alpha?", Limit: 10, GraphNodeLimit: 100}
	for _, test := range []struct {
		name    string
		ctx     context.Context
		request Request
	}{
		{"nil context", nil, valid},
		{"empty question", context.Background(), Request{Limit: 10, GraphNodeLimit: 100}},
		{"question whitespace", context.Background(), Request{Question: " question ", Limit: 10, GraphNodeLimit: 100}},
		{"limit low", context.Background(), Request{Question: "question", Limit: 0, GraphNodeLimit: 100}},
		{"limit high", context.Background(), Request{Question: "question", Limit: 1001, GraphNodeLimit: 100}},
		{"hops low", context.Background(), Request{Question: "question", Limit: 10, GraphHops: -1, GraphNodeLimit: 100}},
		{"hops high", context.Background(), Request{Question: "question", Limit: 10, GraphHops: 5, GraphNodeLimit: 100}},
		{"nodes low", context.Background(), Request{Question: "question", Limit: 10, GraphNodeLimit: 0}},
		{"nodes high", context.Background(), Request{Question: "question", Limit: 10, GraphNodeLimit: 5001}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := service.Answer(test.ctx, test.request)
			if !errors.Is(err, ErrInvalidRequest) && test.ctx != nil {
				t.Fatalf("validation error = %v", err)
			}
			if test.ctx == nil && (err == nil || !strings.Contains(err.Error(), "context is required")) {
				t.Fatalf("nil context error = %v", err)
			}
		})
	}
}

func TestAnswerCopiesMutableRequestInputs(t *testing.T) {
	values := map[string]struct{}{"go": {}}
	copied := copySet(values)
	delete(copied, "go")
	if _, ok := values["go"]; !ok || copySet(nil) != nil {
		t.Fatalf("set copy aliased input: original=%v copied=%v", values, copied)
	}
	deadline := time.Unix(1_800_000_000, 123)
	deadlineCopy := copyDeadline(&deadline)
	if deadlineCopy == &deadline || !deadlineCopy.Equal(deadline) || copyDeadline(nil) != nil {
		t.Fatalf("deadline copy original=%p copy=%p", &deadline, deadlineCopy)
	}

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
	_, err = service.Answer(context.Background(), Request{
		Question: "What is Alpha?", Languages: values, Limit: 10,
		GraphNodeLimit: 100, Deadline: &deadline,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || provider.requests[0].Deadline == nil ||
		provider.requests[0].Deadline == &deadline || !provider.requests[0].Deadline.Equal(deadline) {
		t.Fatalf("provider deadline = %+v", provider.requests)
	}
}

func answerVectorIndex(lexical *search.Index, descriptor search.EmbeddingDescriptor) *search.VectorIndex {
	records := make([]search.VectorRecord, 0, len(lexical.Documents))
	for id := range lexical.Documents {
		records = append(records, search.VectorRecord{DocumentID: id, Values: []float32{1, 0}})
	}
	return &search.VectorIndex{
		Version: search.VectorIndexVersion, Descriptor: descriptor,
		Documents: lexical.Documents, Vectors: records,
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
