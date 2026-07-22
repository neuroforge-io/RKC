package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

type vectorTestProvider struct {
	descriptor EmbeddingDescriptor
	embed      func(context.Context, []string) ([][]float32, error)
	calls      [][]string
}

func (provider *vectorTestProvider) Descriptor() EmbeddingDescriptor {
	return provider.descriptor
}

func (provider *vectorTestProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	provider.calls = append(provider.calls, append([]string(nil), texts...))
	if provider.embed != nil {
		return provider.embed(ctx, texts)
	}
	vectors := make([][]float32, len(texts))
	for index := range vectors {
		vectors[index] = []float32{3, 4}
	}
	return vectors, nil
}

func TestBuildVectorIndexBatchesSortsNormalizesAndHashes(t *testing.T) {
	t.Parallel()

	documents := []Document{
		{ID: "z", ObjectType: "node", Kind: "function", Language: "go", Title: "Zulu", Body: strings.Repeat("界", 40)},
		{ID: "a", ObjectType: "document", Kind: "guide", Title: "Alpha", Path: "docs/alpha.md", Body: "first"},
		{ID: "m", ObjectType: "artifact", Kind: "source", Title: "Middle", Path: "middle.go", Body: "middle"},
	}
	provider := &vectorTestProvider{descriptor: EmbeddingDescriptor{Provider: "test", Model: "tiny", Digest: "sha256:model"}}
	index, err := BuildVectorIndex(context.Background(), Build(documents), provider, VectorBuildOptions{BatchSize: 2, MaximumTextBytes: 80})
	if err != nil {
		t.Fatal(err)
	}
	if index.Version != VectorIndexVersion || index.Descriptor.Dimensions != 2 || index.Descriptor.Provider != "test" ||
		index.Descriptor.Model != "tiny" || index.Descriptor.Digest != "sha256:model" || !reflect.DeepEqual(vectorRecordIDs(index.Vectors), []string{"a", "m", "z"}) {
		t.Fatalf("BuildVectorIndex metadata/order = %+v", index)
	}
	if got := []int{len(provider.calls[0]), len(provider.calls[1])}; !reflect.DeepEqual(got, []int{2, 1}) {
		t.Fatalf("embedding batch sizes = %v", got)
	}
	for callIndex, call := range provider.calls {
		for _, text := range call {
			if len(text) > 80 || !utf8.ValidString(text) {
				t.Fatalf("batch %d contains invalid bounded text %q", callIndex, text)
			}
		}
	}
	for _, record := range index.Vectors {
		if math.Abs(float64(record.Values[0])-0.6) > 1e-6 || math.Abs(float64(record.Values[1])-0.8) > 1e-6 {
			t.Errorf("vector %s was not normalized: %v", record.DocumentID, record.Values)
		}
		text := embeddingText(index.Documents[record.DocumentID], 80)
		digest := sha256.Sum256([]byte(text))
		if record.ContentSHA256 != hex.EncodeToString(digest[:]) {
			t.Errorf("vector %s digest = %q", record.DocumentID, record.ContentSHA256)
		}
	}
}

func TestBuildVectorIndexDefaultsAndBoundsBatchSize(t *testing.T) {
	t.Parallel()

	makeDocuments := func(count int) []Document {
		documents := make([]Document, count)
		for index := range documents {
			documents[index] = Document{ID: string(rune(0x1000 + index)), Title: "doc", Body: strings.Repeat("body", 5000)}
		}
		return documents
	}
	tests := []struct {
		name      string
		count     int
		options   VectorBuildOptions
		wantBatch []int
	}{
		{name: "defaults", count: 17, options: VectorBuildOptions{}, wantBatch: []int{16, 1}},
		{name: "batch cap", count: 257, options: VectorBuildOptions{BatchSize: 999}, wantBatch: []int{256, 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provider := &vectorTestProvider{descriptor: EmbeddingDescriptor{Dimensions: 1}, embed: func(_ context.Context, texts []string) ([][]float32, error) {
				vectors := make([][]float32, len(texts))
				for index := range texts {
					if len(texts[index]) > 16*1024 {
						t.Fatalf("default maximum text bytes not applied: %d", len(texts[index]))
					}
					vectors[index] = []float32{2}
				}
				return vectors, nil
			}}
			index, err := BuildVectorIndex(context.Background(), Build(makeDocuments(test.count)), provider, test.options)
			if err != nil {
				t.Fatal(err)
			}
			gotBatch := make([]int, len(provider.calls))
			for index := range provider.calls {
				gotBatch[index] = len(provider.calls[index])
			}
			if len(index.Vectors) != test.count || !reflect.DeepEqual(gotBatch, test.wantBatch) {
				t.Fatalf("vectors=%d batches=%v", len(index.Vectors), gotBatch)
			}
		})
	}

	emptyProvider := &vectorTestProvider{descriptor: EmbeddingDescriptor{Dimensions: 7}}
	empty, err := BuildVectorIndex(context.Background(), Build(nil), emptyProvider, VectorBuildOptions{})
	if err != nil || len(empty.Vectors) != 0 || len(empty.Documents) != 0 || len(emptyProvider.calls) != 0 || empty.Descriptor.Dimensions != 7 {
		t.Fatalf("empty vector index = %+v calls=%v err=%v", empty, emptyProvider.calls, err)
	}
}

func TestBuildVectorIndexRejectsProviderFailuresAndInvalidVectors(t *testing.T) {
	t.Parallel()

	lexical := Build([]Document{{ID: "a", Title: "alpha"}, {ID: "b", Title: "beta"}})
	if _, err := BuildVectorIndex(context.Background(), nil, &vectorTestProvider{}, VectorBuildOptions{}); err == nil {
		t.Fatal("BuildVectorIndex accepted a nil lexical index")
	}
	if _, err := BuildVectorIndex(context.Background(), lexical, nil, VectorBuildOptions{}); err == nil {
		t.Fatal("BuildVectorIndex accepted a nil provider")
	}

	sentinel := errors.New("provider unavailable")
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name       string
		descriptor EmbeddingDescriptor
		ctx        context.Context
		embed      func(context.Context, []string) ([][]float32, error)
		want       string
	}{
		{
			name: "provider error", ctx: context.Background(),
			embed: func(context.Context, []string) ([][]float32, error) { return nil, sentinel },
			want:  "embed documents 0-2: provider unavailable",
		},
		{
			name: "context propagation", ctx: cancelled,
			embed: func(ctx context.Context, _ []string) ([][]float32, error) { return nil, ctx.Err() },
			want:  "context canceled",
		},
		{
			name: "wrong count", ctx: context.Background(),
			embed: func(context.Context, []string) ([][]float32, error) { return [][]float32{{1}}, nil },
			want:  "returned 1 vectors for 2 documents",
		},
		{
			name: "empty vector", ctx: context.Background(),
			embed: func(context.Context, []string) ([][]float32, error) { return [][]float32{{}, {1}}, nil },
			want:  "embedding vector is empty",
		},
		{
			name: "zero vector", ctx: context.Background(),
			embed: func(context.Context, []string) ([][]float32, error) { return [][]float32{{0, 0}, {1, 0}}, nil },
			want:  "embedding vector has zero norm",
		},
		{
			name: "nan vector", ctx: context.Background(),
			embed: func(context.Context, []string) ([][]float32, error) {
				return [][]float32{{float32(math.NaN()), 1}, {1, 0}}, nil
			},
			want: "non-finite",
		},
		{
			name: "infinite vector", ctx: context.Background(),
			embed: func(context.Context, []string) ([][]float32, error) {
				return [][]float32{{float32(math.Inf(1)), 1}, {1, 0}}, nil
			},
			want: "non-finite",
		},
		{
			name: "descriptor dimension mismatch", descriptor: EmbeddingDescriptor{Dimensions: 3}, ctx: context.Background(),
			embed: func(context.Context, []string) ([][]float32, error) { return [][]float32{{1, 0}, {0, 1}}, nil },
			want:  "inconsistent dimensions",
		},
		{
			name: "inferred dimension mismatch", ctx: context.Background(),
			embed: func(context.Context, []string) ([][]float32, error) { return [][]float32{{1, 0}, {0, 1, 0}}, nil },
			want:  "inconsistent dimensions",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			provider := &vectorTestProvider{descriptor: test.descriptor, embed: test.embed}
			_, err := BuildVectorIndex(test.ctx, lexical, provider, VectorBuildOptions{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("BuildVectorIndex error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestVectorSearchRanksFiltersSkipsUnsafeRecordsAndTruncates(t *testing.T) {
	t.Parallel()

	index := &VectorIndex{
		Version:    VectorIndexVersion,
		Descriptor: EmbeddingDescriptor{Dimensions: 2},
		Documents: map[string]Document{
			"a":        {ID: "a", ObjectType: "node", Kind: "function", Language: "go", Path: "internal/a.go"},
			"b":        {ID: "b", ObjectType: "node", Kind: "function", Language: "go", Path: "internal/b.go"},
			"python":   {ID: "python", ObjectType: "node", Kind: "class", Language: "python", Path: "src/main.py"},
			"artifact": {ID: "artifact", ObjectType: "artifact", Kind: "source", Language: "go", Path: "internal/data.go"},
			"bad-dim":  {ID: "bad-dim"},
			"nan":      {ID: "nan"},
			"inf":      {ID: "inf"},
		},
		Vectors: []VectorRecord{
			{DocumentID: "b", Values: []float32{1, 0}},
			{DocumentID: "a", Values: []float32{1, 0}},
			{DocumentID: "python", Values: []float32{0, 1}},
			{DocumentID: "artifact", Values: []float32{0.8, 0.6}},
			{DocumentID: "missing", Values: []float32{1, 0}},
			{DocumentID: "bad-dim", Values: []float32{1}},
			{DocumentID: "nan", Values: []float32{float32(math.NaN()), 0}},
			{DocumentID: "inf", Values: []float32{float32(math.Inf(1)), 0}},
		},
	}
	queryVector := []float32{3, 4}
	response, err := index.Search(Query{Text: "meaning", Limit: 2}, queryVector)
	if err != nil {
		t.Fatal(err)
	}
	if response.Query != "meaning" || response.Mode != "dense-cosine" || response.IndexVersion != VectorIndexVersion || !response.Truncated ||
		!reflect.DeepEqual(hitIDs(response.Hits), []string{"artifact", "python"}) || !reflect.DeepEqual(queryVector, []float32{3, 4}) {
		t.Fatalf("semantic response = %+v queryVector=%v", response, queryVector)
	}
	for _, hit := range response.Hits {
		if !reflect.DeepEqual(hit.Reasons, []string{"dense_cosine"}) {
			t.Errorf("hit reasons = %v", hit.Reasons)
		}
	}

	filtered, err := index.Search(Query{
		Text: "filtered", Limit: 10,
		Kinds: map[string]struct{}{"function": {}}, Languages: map[string]struct{}{"go": {}},
		ObjectTypes: map[string]struct{}{"node": {}}, PathPrefix: "internal/",
	}, []float32{1, 0})
	if err != nil || filtered.Truncated || !reflect.DeepEqual(hitIDs(filtered.Hits), []string{"a", "b"}) {
		t.Fatalf("filtered semantic response = %+v err=%v", filtered, err)
	}
}

func TestVectorSearchValidatesIndexQueryAndLimitBounds(t *testing.T) {
	t.Parallel()

	if _, err := (*VectorIndex)(nil).Search(Query{}, []float32{1}); err == nil {
		t.Fatal("nil vector index was accepted")
	}
	if _, err := (&VectorIndex{Version: "future", Descriptor: EmbeddingDescriptor{Dimensions: 1}}).Search(Query{}, []float32{1}); err == nil {
		t.Fatal("unsupported vector index was accepted")
	}
	index := &VectorIndex{Version: VectorIndexVersion, Descriptor: EmbeddingDescriptor{Dimensions: 2}}
	for name, vector := range map[string][]float32{
		"wrong dimension": {1},
		"zero norm":       {0, 0},
		"not a number":    {float32(math.NaN()), 0},
		"infinite":        {float32(math.Inf(-1)), 0},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := index.Search(Query{}, vector); err == nil {
				t.Fatalf("Search accepted %s query vector", name)
			}
		})
	}

	many := &VectorIndex{Version: VectorIndexVersion, Descriptor: EmbeddingDescriptor{Dimensions: 1}, Documents: map[string]Document{}}
	for number := 0; number < 1002; number++ {
		id := leftPadInteger(number, 4)
		many.Documents[id] = Document{ID: id}
		many.Vectors = append(many.Vectors, VectorRecord{DocumentID: id, Values: []float32{1}})
	}
	defaulted, err := many.Search(Query{Limit: 0}, []float32{1})
	if err != nil || !defaulted.Truncated || len(defaulted.Hits) != 50 {
		t.Fatalf("default semantic limit count=%d truncated=%v err=%v", len(defaulted.Hits), defaulted.Truncated, err)
	}
	capped, err := many.Search(Query{Limit: 5000}, []float32{1})
	if err != nil || !capped.Truncated || len(capped.Hits) != 1000 || capped.Hits[0].Document.ID != "0000" {
		t.Fatalf("capped semantic limit count=%d truncated=%v err=%v", len(capped.Hits), capped.Truncated, err)
	}
}

func TestFuseUsesDeterministicReciprocalRanks(t *testing.T) {
	t.Parallel()

	document := func(id string) Document { return Document{ID: id, Title: id} }
	lexical := Response{Hits: []Hit{{Document: document("a"), Score: 100}, {Document: document("b"), Score: 90}, {Document: document("c"), Score: 80}}}
	semantic := Response{Hits: []Hit{{Document: document("b"), Score: 1}, {Document: document("a"), Score: 0.9}, {Document: document("d"), Score: 0.8}}}
	var first Response
	for run := 0; run < 25; run++ {
		got := Fuse("graph", lexical, semantic, 3)
		if got.Query != "graph" || got.Mode != "hybrid-rrf" || got.IndexVersion != "1+vector-1" || !got.Truncated ||
			!reflect.DeepEqual(hitIDs(got.Hits), []string{"a", "b", "c"}) {
			t.Fatalf("Fuse run %d = %+v", run, got)
		}
		if !reflect.DeepEqual(got.Hits[0].Reasons, []string{"lexical_rank:1", "semantic_rank:2"}) ||
			!reflect.DeepEqual(got.Hits[1].Reasons, []string{"lexical_rank:2", "semantic_rank:1"}) || got.Hits[0].Score != got.Hits[1].Score {
			t.Fatalf("fused ranks = %+v", got.Hits)
		}
		if run == 0 {
			first = got
		} else if !reflect.DeepEqual(got, first) {
			t.Fatalf("Fuse is non-deterministic:\nfirst=%+v\nrun=%+v", first, got)
		}
	}
	if got := Fuse("empty", Response{}, Response{}, 0); len(got.Hits) != 0 || got.Truncated {
		t.Fatalf("empty/default Fuse = %+v", got)
	}

	manyHits := make([]Hit, 1002)
	for index := range manyHits {
		manyHits[index] = Hit{Document: document(leftPadInteger(index, 4))}
	}
	capped := Fuse("many", Response{Hits: manyHits}, Response{}, 5000)
	if !capped.Truncated || len(capped.Hits) != 1000 {
		t.Fatalf("capped Fuse count=%d truncated=%v", len(capped.Hits), capped.Truncated)
	}
}

func TestVectorIndexSaveLoadRoundTripAndFailures(t *testing.T) {
	t.Parallel()

	index := &VectorIndex{
		Version:    VectorIndexVersion,
		Descriptor: EmbeddingDescriptor{Provider: "test", Model: "model", Dimensions: 2},
		Documents:  map[string]Document{"a": {ID: "a", Title: "alpha"}},
		Vectors:    []VectorRecord{{DocumentID: "a", ContentSHA256: strings.Repeat("a", 64), Values: []float32{0.6, 0.8}}},
	}
	path := filepath.Join(t.TempDir(), "nested", "vectors.json")
	if err := index.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadVectorIndex(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, index) {
		t.Fatalf("vector round trip mismatch:\nwant=%+v\ngot=%+v", index, loaded)
	}

	replacement := *index
	replacement.Descriptor.Model = "replacement"
	if err := replacement.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err = LoadVectorIndex(path)
	if err != nil || loaded.Descriptor.Model != "replacement" {
		t.Fatalf("atomic replacement = %+v, %v", loaded, err)
	}

	if err := (*VectorIndex)(nil).Save(filepath.Join(t.TempDir(), "nil.json")); err == nil {
		t.Fatal("Save accepted a nil vector index")
	}
	if err := (&VectorIndex{Version: "future"}).Save(filepath.Join(t.TempDir(), "future.json")); err == nil {
		t.Fatal("Save accepted an unsupported vector index")
	}
	fileParent := filepath.Join(t.TempDir(), "parent-file")
	if err := os.WriteFile(fileParent, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := index.Save(filepath.Join(fileParent, "vectors.json")); err == nil {
		t.Fatal("Save accepted a path beneath a regular file")
	}
	nonJSON := *index
	nonJSON.Vectors = []VectorRecord{{DocumentID: "a", Values: []float32{float32(math.NaN()), 0}}}
	if err := nonJSON.Save(filepath.Join(t.TempDir(), "nan.json")); err == nil {
		t.Fatal("Save accepted non-JSON floating-point state")
	}
	if err := index.Save(filepath.Join("/proc", "rkc-vector-index-test.json")); err == nil {
		t.Fatal("Save unexpectedly created a temporary file in /proc")
	}
	destinationDirectory := filepath.Join(t.TempDir(), "existing-directory")
	if err := os.Mkdir(destinationDirectory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(destinationDirectory, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := index.Save(destinationDirectory); err == nil {
		t.Fatal("Save unexpectedly replaced a non-empty directory")
	}
	leaks, err := filepath.Glob(filepath.Join(filepath.Dir(destinationDirectory), ".vector-index-*"))
	if err != nil || len(leaks) != 0 {
		t.Fatalf("failed Save leaked temporary files: %v, %v", leaks, err)
	}

	if _, err := LoadVectorIndex(filepath.Join(t.TempDir(), "missing.json")); err == nil {
		t.Fatal("LoadVectorIndex accepted a missing file")
	}
	writeVectorFixture := func(name, contents string) string {
		fixture := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(fixture, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		return fixture
	}
	for _, test := range []struct {
		name     string
		contents string
		want     string
	}{
		{name: "malformed", contents: "{", want: "decode vector index"},
		{name: "future", contents: `{"version":"future","descriptor":{"dimensions":2}}`, want: "unsupported or invalid"},
		{name: "zero dimensions", contents: `{"version":"1","descriptor":{"dimensions":0}}`, want: "unsupported or invalid"},
		{name: "provider required", contents: `{"version":"1","descriptor":{"dimensions":2}}`, want: "provider and model are required"},
		{name: "document id required", contents: `{"version":"1","descriptor":{"provider":"test","model":"model","dimensions":2},"documents":{"a":{"id":"a"}},"vectors":[{"document_id":" ","values":[0.6,0.8]}]}`, want: "document id is required"},
		{name: "duplicate vector", contents: `{"version":"1","descriptor":{"provider":"test","model":"model","dimensions":2},"documents":{"a":{"id":"a"}},"vectors":[{"document_id":"a","values":[0.6,0.8]},{"document_id":"a","values":[0.6,0.8]}]}`, want: "duplicate vector"},
		{name: "missing document", contents: `{"version":"1","descriptor":{"provider":"test","model":"model","dimensions":2},"documents":{},"vectors":[{"document_id":"a","values":[0.6,0.8]}]}`, want: "references a missing document"},
		{name: "mismatched dimensions", contents: `{"version":"1","descriptor":{"provider":"test","model":"model","dimensions":2},"documents":{"a":{"id":"a"}},"vectors":[{"document_id":"a","values":[1]}]}`, want: "invalid dimensions"},
		{name: "not normalized", contents: `{"version":"1","descriptor":{"provider":"test","model":"model","dimensions":2},"documents":{"a":{"id":"a"}},"vectors":[{"document_id":"a","values":[1,1]}]}`, want: "not normalized"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := LoadVectorIndex(writeVectorFixture(test.name+".json", test.contents))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("LoadVectorIndex error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestLoadVectorIndexRejectsNonFiniteAndZeroNormVectors(t *testing.T) {
	t.Parallel()

	for name, contents := range map[string]string{
		"zero":                 `{"version":"1","descriptor":{"provider":"test","model":"model","dimensions":2},"documents":{"a":{"id":"a"}},"vectors":[{"document_id":"a","values":[0,0]}]}`,
		"overflow to infinity": `{"version":"1","descriptor":{"provider":"test","model":"model","dimensions":2},"documents":{"a":{"id":"a"}},"vectors":[{"document_id":"a","values":[1e1000,1]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "vectors.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadVectorIndex(path); err == nil {
				t.Fatalf("LoadVectorIndex accepted %s stored vector", name)
			}
		})
	}
}

func TestEmbeddingTextAndVectorHelpers(t *testing.T) {
	t.Parallel()

	document := Document{
		ObjectType: "node", Kind: "function", Language: "go", Title: "Title", QualifiedName: "pkg.Title",
		Signature: "func Title()", Path: "title.go", Body: "content界界",
	}
	full := embeddingText(document, 4096)
	for _, fragment := range []string{"type: node", "kind: function", "language: go", "title: Title", "qualified_name: pkg.Title", "signature: func Title()", "path: title.go", "content: content界界"} {
		if !strings.Contains(full, fragment) {
			t.Errorf("embedding text missing %q: %q", fragment, full)
		}
	}
	cut := embeddingText(document, len(full)-1)
	if len(cut) > len(full)-1 || !utf8.ValidString(cut) {
		t.Fatalf("bounded embedding text is invalid: %q", cut)
	}

	vector := []float32{3, 4}
	if err := normalizeVector(vector); err != nil || !reflect.DeepEqual(vector, []float32{0.6, 0.8}) {
		t.Fatalf("normalizeVector = %v, %v", vector, err)
	}
	if got := dotProduct([]float32{1, 2, 3}, []float32{4, 5, 6}); got != 32 {
		t.Fatalf("dotProduct = %v", got)
	}
	if err := validateStoredVector([]float32{0.6, 0.8}); err != nil {
		t.Fatalf("validateStoredVector rejected a normalized vector: %v", err)
	}
	for name, vector := range map[string][]float32{
		"empty":          {},
		"non-finite":     {float32(math.Inf(1)), 0},
		"zero norm":      {0, 0},
		"not normalized": {1, 1},
	} {
		if err := validateStoredVector(vector); err == nil {
			t.Errorf("validateStoredVector accepted %s vector", name)
		}
	}
}

func vectorRecordIDs(records []VectorRecord) []string {
	ids := make([]string, len(records))
	for index, record := range records {
		ids[index] = record.DocumentID
	}
	return ids
}

func hitIDs(hits []Hit) []string {
	ids := make([]string, len(hits))
	for index, hit := range hits {
		ids[index] = hit.Document.ID
	}
	return ids
}

func leftPadInteger(value, width int) string {
	digits := make([]byte, width)
	for index := width - 1; index >= 0; index-- {
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits)
}
