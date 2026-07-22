package retrieval

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type stubEmbedder struct {
	vectors [][]float32
	err     error
	calls   int
	texts   [][]string
}

func (stub *stubEmbedder) Descriptor() search.EmbeddingDescriptor {
	return search.EmbeddingDescriptor{Provider: "stub", Model: "test", Dimensions: 2}
}

func (stub *stubEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	stub.calls++
	stub.texts = append(stub.texts, append([]string(nil), texts...))
	if stub.err != nil {
		return nil, stub.err
	}
	result := make([][]float32, len(stub.vectors))
	for i := range stub.vectors {
		result[i] = append([]float32(nil), stub.vectors[i]...)
	}
	return result, nil
}

func TestSearchRequiresLexicalIndexAndValidModeDependencies(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	if _, err := (*Engine)(nil).Search(ctx, search.Query{Text: "alpha"}, Options{}); err == nil || !strings.Contains(err.Error(), "lexical index is required") {
		t.Fatalf("nil engine error = %v", err)
	}
	if _, err := (&Engine{}).Search(ctx, search.Query{Text: "alpha"}, Options{}); err == nil || !strings.Contains(err.Error(), "lexical index is required") {
		t.Fatalf("missing lexical index error = %v", err)
	}
	lexical, _, _ := retrievalFixture()
	for _, mode := range []Mode{ModeSemantic, ModeHybrid, ""} {
		if _, err := (&Engine{Lexical: lexical}).Search(ctx, search.Query{Text: "alpha"}, Options{Mode: mode}); err == nil || !strings.Contains(err.Error(), "semantic search requires") {
			t.Errorf("mode %q missing semantic dependency error = %v", mode, err)
		}
	}
	if _, err := (&Engine{Lexical: lexical}).Search(ctx, search.Query{Text: "alpha"}, Options{Mode: Mode("future")}); err == nil || !strings.Contains(err.Error(), `unknown retrieval mode "future"`) {
		t.Fatalf("unknown mode error = %v", err)
	}
}

func TestLexicalSemanticAndHybridModes(t *testing.T) {
	t.Parallel()

	lexical, vectors, graphIndex := retrievalFixture()
	embedder := &stubEmbedder{vectors: [][]float32{{1, 0}}}
	engine := &Engine{Lexical: lexical, Vector: vectors, Embedder: embedder, Graph: graphIndex}

	lexicalResponse, err := engine.Search(context.Background(), search.Query{Text: "alpha", Limit: 2}, Options{Mode: ModeLexical})
	if err != nil {
		t.Fatal(err)
	}
	if lexicalResponse.Mode != "embedded-bm25-lexical" || lexicalResponse.Query != "alpha" || len(lexicalResponse.Hits) != 1 || lexicalResponse.Hits[0].Document.ID != "a" || embedder.calls != 0 {
		t.Fatalf("lexical response = %+v, embed calls=%d", lexicalResponse, embedder.calls)
	}

	semanticResponse, err := engine.Search(context.Background(), search.Query{Text: "anything", Limit: 3}, Options{Mode: ModeSemantic})
	if err != nil {
		t.Fatal(err)
	}
	if semanticResponse.Mode != "dense-cosine" || semanticResponse.Query != "anything" || len(semanticResponse.Hits) != 3 || semanticResponse.Hits[0].Document.ID != "a" || embedder.calls != 1 || !reflect.DeepEqual(embedder.texts[0], []string{"anything"}) {
		t.Fatalf("semantic response = %+v, embedder=%+v", semanticResponse, embedder)
	}

	hybridResponse, err := engine.Search(context.Background(), search.Query{Text: "alpha", Limit: 3}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if hybridResponse.Mode != "hybrid-rrf" || hybridResponse.Query != "alpha" || len(hybridResponse.Hits) != 3 || hybridResponse.Hits[0].Document.ID != "a" || embedder.calls != 2 {
		t.Fatalf("default hybrid response = %+v, embed calls=%d", hybridResponse, embedder.calls)
	}
	if !containsReasonPrefix(hybridResponse.Hits[0].Reasons, "lexical_rank:") || !containsReasonPrefix(hybridResponse.Hits[0].Reasons, "semantic_rank:") {
		t.Fatalf("hybrid retrieval trace = %+v", hybridResponse.Hits[0])
	}
}

func TestSemanticProviderAndQueryVectorFailuresPropagate(t *testing.T) {
	t.Parallel()

	lexical, vectors, _ := retrievalFixture()
	ctx := context.Background()
	tests := []struct {
		name     string
		embedder *stubEmbedder
		want     string
	}{
		{"provider error", &stubEmbedder{err: errors.New("offline")}, "embed query: offline"},
		{"no vector", &stubEmbedder{}, "invalid query vector count"},
		{"many vectors", &stubEmbedder{vectors: [][]float32{{1, 0}, {0, 1}}}, "invalid query vector count"},
		{"wrong dimensions", &stubEmbedder{vectors: [][]float32{{1, 0, 0}}}, "does not match index dimension"},
		{"zero vector", &stubEmbedder{vectors: [][]float32{{0, 0}}}, "zero norm"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := &Engine{Lexical: lexical, Vector: vectors, Embedder: test.embedder}
			if _, err := engine.Search(ctx, search.Query{Text: "alpha"}, Options{Mode: ModeSemantic}); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Search error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestGraphExpansionIsBoundedDeterministicAndAuditable(t *testing.T) {
	t.Parallel()

	lexical, _, graphIndex := retrievalFixture()
	engine := &Engine{Lexical: lexical, Graph: graphIndex}
	response, err := engine.Search(context.Background(), search.Query{Text: "alpha", Limit: 10}, Options{Mode: ModeLexical, GraphHops: 100})
	if err != nil {
		t.Fatal(err)
	}
	if response.Mode != "embedded-bm25-lexical+graph" || !reflect.DeepEqual(hitIDs(response.Hits), []string{"a", "b", "c"}) {
		t.Fatalf("expanded graph response = %+v", response)
	}
	if !containsReason(response.Hits[1].Reasons, "graph_from:a:depth:1") || !containsReason(response.Hits[2].Reasons, "graph_from:a:depth:2") {
		t.Fatalf("graph evidence traces = %+v", response.Hits)
	}
	for _, hit := range response.Hits {
		if !sortStrings(hit.Reasons) {
			t.Errorf("reasons not sorted for %s: %v", hit.Document.ID, hit.Reasons)
		}
	}

	bounded, err := engine.Search(context.Background(), search.Query{Text: "alpha", Limit: 10}, Options{Mode: ModeLexical, GraphHops: 2, GraphNodeLimit: 1})
	if err != nil || !reflect.DeepEqual(hitIDs(bounded.Hits), []string{"a"}) {
		t.Fatalf("node-bounded expansion = %+v, %v", bounded, err)
	}
	withoutGraph, err := (&Engine{Lexical: lexical}).Search(context.Background(), search.Query{Text: "alpha"}, Options{Mode: ModeLexical, GraphHops: 2})
	if err != nil || strings.Contains(withoutGraph.Mode, "+graph") || len(withoutGraph.Hits) != 1 {
		t.Fatalf("missing graph behavior = %+v, %v", withoutGraph, err)
	}
	negativeHops, err := engine.Search(context.Background(), search.Query{Text: "alpha"}, Options{Mode: ModeLexical, GraphHops: -1})
	if err != nil || strings.Contains(negativeHops.Mode, "+graph") {
		t.Fatalf("negative graph hops = %+v, %v", negativeHops, err)
	}
}

func TestGraphExpansionPreservesQueryFilters(t *testing.T) {
	t.Parallel()

	lexical, _, graphIndex := retrievalFixture()
	engine := &Engine{Lexical: lexical, Graph: graphIndex}
	query := search.Query{
		Text: "alpha", Limit: 10,
		Kinds: map[string]struct{}{"function": {}}, Languages: map[string]struct{}{"go": {}},
		ObjectTypes: map[string]struct{}{"node": {}}, PathPrefix: "internal/",
	}
	response, err := engine.Search(context.Background(), query, Options{Mode: ModeLexical, GraphHops: 4})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hitIDs(response.Hits), []string{"a"}) {
		t.Fatalf("graph expansion bypassed retrieval filters: %+v", response.Hits)
	}
}

func TestGraphExpansionSkipsNonNodeMissingSeedsAndUnknownDocuments(t *testing.T) {
	t.Parallel()

	lexical, _, graphIndex := retrievalFixture()
	engine := &Engine{Lexical: lexical, Graph: graphIndex}
	artifact, err := engine.Search(context.Background(), search.Query{Text: "artifactunique", Limit: 10}, Options{Mode: ModeLexical, GraphHops: 2})
	if err != nil || !reflect.DeepEqual(hitIDs(artifact.Hits), []string{"artifact"}) || !strings.HasSuffix(artifact.Mode, "+graph") {
		t.Fatalf("non-node seed expansion = %+v, %v", artifact, err)
	}

	missingSeedIndex := search.Build([]search.Document{{ID: "missing", ObjectType: "node", Title: "missingunique"}})
	missingSeed, err := (&Engine{Lexical: missingSeedIndex, Graph: graphIndex}).Search(context.Background(), search.Query{Text: "missingunique"}, Options{Mode: ModeLexical, GraphHops: 1})
	if err != nil || !reflect.DeepEqual(hitIDs(missingSeed.Hits), []string{"missing"}) {
		t.Fatalf("missing graph seed = %+v, %v", missingSeed, err)
	}

	graphWithUnknown := graph.Build(
		[]rkcmodel.Node{{ID: "a"}, {ID: "unknown"}},
		[]rkcmodel.Edge{{ID: "a-unknown", Kind: "calls", From: "a", To: "unknown", Resolution: rkcmodel.ResolutionDeclared}},
	)
	unknown, err := (&Engine{Lexical: lexical, Graph: graphWithUnknown}).Search(context.Background(), search.Query{Text: "alpha"}, Options{Mode: ModeLexical, GraphHops: 1})
	if err != nil || !reflect.DeepEqual(hitIDs(unknown.Hits), []string{"a"}) {
		t.Fatalf("unknown lexical neighbor = %+v, %v", unknown, err)
	}
}

func TestFinalLimitAndTruncationAreAppliedAfterExpansion(t *testing.T) {
	t.Parallel()

	lexical, _, graphIndex := retrievalFixture()
	engine := &Engine{Lexical: lexical, Graph: graphIndex}
	response, err := engine.Search(context.Background(), search.Query{Text: "alpha", Limit: 1}, Options{Mode: ModeLexical, GraphHops: 2})
	if err != nil || len(response.Hits) != 1 || response.Hits[0].Document.ID != "a" || !response.Truncated {
		t.Fatalf("post-expansion limit = %+v, %v", response, err)
	}
	if min(1, 2) != 1 || min(3, 2) != 2 {
		t.Fatal("min helper mismatch")
	}
}

func TestGraphExpansionBreaksEqualScoreTiesByDocumentID(t *testing.T) {
	t.Parallel()

	lexical := search.Build([]search.Document{
		{ID: "a", ObjectType: "node", Title: "Alpha", Body: "alpha"},
		{ID: "b", ObjectType: "node", Title: "Beta", Body: "beta"},
		{ID: "c", ObjectType: "node", Title: "Gamma", Body: "gamma"},
	})
	graphIndex := graph.Build(
		[]rkcmodel.Node{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		[]rkcmodel.Edge{
			{ID: "a-c", From: "a", To: "c", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "a-b", From: "a", To: "b", Resolution: rkcmodel.ResolutionDeclared},
		},
	)
	response, err := (&Engine{Lexical: lexical, Graph: graphIndex}).Search(
		context.Background(), search.Query{Text: "alpha", Limit: 10}, Options{Mode: ModeLexical, GraphHops: 1},
	)
	if err != nil || !reflect.DeepEqual(hitIDs(response.Hits), []string{"a", "b", "c"}) || response.Hits[1].Score != response.Hits[2].Score {
		t.Fatalf("equal-score ordering = %+v, %v", response, err)
	}
}

func retrievalFixture() (*search.Index, *search.VectorIndex, *graph.Index) {
	documents := []search.Document{
		{ID: "a", ObjectType: "node", Kind: "function", Language: "go", Title: "Alpha", Path: "internal/a.go", Body: "alpha"},
		{ID: "b", ObjectType: "node", Kind: "method", Language: "go", Title: "Beta", Path: "pkg/b.go", Body: "beta"},
		{ID: "c", ObjectType: "node", Kind: "class", Language: "rust", Title: "Gamma", Path: "other/c.rs", Body: "gamma"},
		{ID: "artifact", ObjectType: "artifact", Kind: "source", Language: "go", Title: "Artifact", Path: "internal/artifact.go", Body: "artifactunique"},
	}
	lexical := search.Build(documents)
	vectors := &search.VectorIndex{
		Version:    search.VectorIndexVersion,
		Descriptor: search.EmbeddingDescriptor{Provider: "stub", Model: "test", Dimensions: 2},
		Documents:  lexical.Documents,
		Vectors: []search.VectorRecord{
			{DocumentID: "a", Values: []float32{1, 0}},
			{DocumentID: "b", Values: []float32{.8, .6}},
			{DocumentID: "c", Values: []float32{0, 1}},
			{DocumentID: "artifact", Values: []float32{-1, 0}},
		},
	}
	graphIndex := graph.Build(
		[]rkcmodel.Node{{ID: "a"}, {ID: "b"}, {ID: "c"}},
		[]rkcmodel.Edge{
			{ID: "a-b", Kind: "calls", From: "a", To: "b", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "b-c", Kind: "calls", From: "b", To: "c", Resolution: rkcmodel.ResolutionDeclared},
		},
	)
	return lexical, vectors, graphIndex
}

func hitIDs(hits []search.Hit) []string {
	ids := make([]string, len(hits))
	for i, hit := range hits {
		ids[i] = hit.Document.ID
	}
	return ids
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func containsReasonPrefix(reasons []string, prefix string) bool {
	for _, reason := range reasons {
		if strings.HasPrefix(reason, prefix) {
			return true
		}
	}
	return false
}

func sortStrings(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] > values[index] {
			return false
		}
	}
	return true
}
