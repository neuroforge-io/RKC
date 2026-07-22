package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/groundedanswer"
	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

type cliAnswerProvider struct {
	descriptor modelruntime.ModelDescriptor
	closeErr   error
	closed     int
}

type cliAnswerEmbedder struct {
	descriptor search.EmbeddingDescriptor
	calls      int
}

func (embedder *cliAnswerEmbedder) Descriptor() search.EmbeddingDescriptor {
	return embedder.descriptor
}

func (embedder *cliAnswerEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	embedder.calls++
	result := make([][]float32, len(texts))
	for index := range result {
		result[index] = []float32{1, 0}
	}
	return result, nil
}

func (provider *cliAnswerProvider) Descriptor() modelruntime.ModelDescriptor {
	return provider.descriptor
}
func (provider *cliAnswerProvider) Supports(modelruntime.Task) bool { return true }
func (provider *cliAnswerProvider) Close() error {
	provider.closed++
	return provider.closeErr
}
func (provider *cliAnswerProvider) Generate(_ context.Context, request modelruntime.Request) (modelruntime.Response, error) {
	return modelruntime.Response{
		RequestID: request.RequestID, ModelID: provider.descriptor.ID,
		Claims: []modelruntime.ClaimDraft{{
			Text: "Alpha is the entry point.", Category: "purpose", Certainty: "supported",
			EvidenceIDs: []string{"e-alpha"},
		}},
	}, nil
}

func TestRunAnswerWritesOnlyStdoutAndClosesProvider(t *testing.T) {
	bundle := cliAnswerBundle()
	atlas := t.TempDir()
	marker := filepath.Join(atlas, "canonical.marker")
	if err := os.WriteFile(marker, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(atlas)
	if err != nil {
		t.Fatal(err)
	}
	provider := &cliAnswerProvider{descriptor: modelruntime.ModelDescriptor{ID: "qualified-test-model"}}
	var output bytes.Buffer
	requestedDir := ""
	dependencies := answerDependencies{
		loadDataset: func(dir string) (*server.Dataset, error) {
			requestedDir = dir
			return &server.Dataset{
				Root: atlas, Bundle: bundle, Search: search.BuildFromBundle(bundle),
				Graph: graph.Build(bundle.Nodes, bundle.Edges),
			}, nil
		},
		openProvider: func(request qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			return &qualifiedGenerationSession{
				Provider: provider, Descriptor: provider.descriptor,
				Inference: modelruntime.InferenceOptions{ContextTokens: request.ContextTokens, MaxOutputTokens: request.MaximumOutputTokens},
			}, nil
		},
		stdout: &output,
		now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	if err := runAnswerContext(context.Background(), []string{"--dir", atlas, "--json", "What", "is", "Alpha?"}, dependencies); err != nil {
		t.Fatal(err)
	}
	if requestedDir != atlas || provider.closed != 1 {
		t.Fatalf("dataset=%q provider closes=%d", requestedDir, provider.closed)
	}
	var result groundedanswer.Result
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatalf("decode stdout: %v\n%s", err, output.String())
	}
	if result.Status != groundedanswer.StatusAnswered || len(result.Claims) != 1 || len(result.Citations) != 1 {
		t.Fatalf("answer result = %+v", result)
	}
	after, err := os.ReadDir(atlas)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(directoryNames(before), directoryNames(after)) {
		t.Fatalf("answer mutated atlas: before=%v after=%v", directoryNames(before), directoryNames(after))
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "unchanged" {
		t.Fatalf("canonical marker changed: data=%q err=%v", data, err)
	}
}

func TestRunAnswerReturnsProviderCloseFailure(t *testing.T) {
	bundle := cliAnswerBundle()
	provider := &cliAnswerProvider{
		descriptor: modelruntime.ModelDescriptor{ID: "qualified-test-model"},
		closeErr:   errors.New("close failed"),
	}
	dependencies := answerDependencies{
		loadDataset: func(string) (*server.Dataset, error) {
			return &server.Dataset{
				Bundle: bundle, Search: search.BuildFromBundle(bundle),
				Graph: graph.Build(bundle.Nodes, bundle.Edges),
			}, nil
		},
		openProvider: func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			return &qualifiedGenerationSession{Provider: provider}, nil
		},
		stdout: &bytes.Buffer{},
		now:    time.Now,
	}
	err := runAnswerContext(context.Background(), []string{"What", "is", "Alpha?"}, dependencies)
	if err == nil || !strings.Contains(err.Error(), "close answer model provider") || provider.closed != 1 {
		t.Fatalf("close failure=%v closes=%d", err, provider.closed)
	}
}

func TestRunAnswerUsesQualifiedSemanticSessionAndClosesIt(t *testing.T) {
	bundle := cliAnswerBundle()
	lexical := search.BuildFromBundle(bundle)
	embedder := &cliAnswerEmbedder{descriptor: search.EmbeddingDescriptor{
		Provider: "test", Model: "qualified-embedding", Digest: "sha256:embedding", Dimensions: 2,
	}}
	closeCalls := 0
	provider := &cliAnswerProvider{descriptor: modelruntime.ModelDescriptor{ID: "qualified-test-model"}}
	var output bytes.Buffer
	var prepared semanticQueryOptions
	dependencies := answerDependencies{
		loadDataset: func(string) (*server.Dataset, error) {
			return &server.Dataset{
				Root: t.TempDir(), Bundle: bundle, Search: lexical,
				Graph: graph.Build(bundle.Nodes, bundle.Edges),
			}, nil
		},
		prepareSemantic: func(_ context.Context, _ string, actual *search.Index, options semanticQueryOptions) (*answerSemanticSession, error) {
			if actual != lexical {
				t.Fatal("semantic preparation did not receive the canonical lexical index")
			}
			prepared = options
			return &answerSemanticSession{
				Vector: cliAnswerVectorIndex(lexical, embedder.descriptor), Embedder: embedder,
				close: func() error { closeCalls++; return nil },
			}, nil
		},
		openProvider: func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			return &qualifiedGenerationSession{Provider: provider}, nil
		},
		stdout: &output,
		now:    time.Now,
	}
	err := runAnswerContext(context.Background(), []string{
		"--mode", "hybrid", "--vector-index", "/tmp/rkc-answer-vector.json",
		"--embedding-model", "/models/qualified.gguf", "--embedding-asset", "embedding-v1",
		"--embedding-runtime-receipt", "/runtime/receipt.json", "--objects", "node",
		"--graph-hops", "1", "--json", "What", "is", "Alpha?",
	}, dependencies)
	if err != nil {
		t.Fatal(err)
	}
	if closeCalls != 1 || provider.closed != 1 || embedder.calls != 1 {
		t.Fatalf("semantic closes=%d provider closes=%d embedding calls=%d", closeCalls, provider.closed, embedder.calls)
	}
	if prepared.VectorIndexPath != "/tmp/rkc-answer-vector.json" || prepared.ModelPath != "/models/qualified.gguf" ||
		prepared.AssetID != "embedding-v1" || prepared.RuntimeReceiptPath != "/runtime/receipt.json" {
		t.Fatalf("semantic options = %+v", prepared)
	}
	var result groundedanswer.Result
	if err := json.Unmarshal(output.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != groundedanswer.StatusAnswered || result.Provenance.Retrieval.Mode != "hybrid-rrf+graph" {
		t.Fatalf("semantic answer = %+v", result)
	}
}

func TestRunAnswerSemanticSetupCancellationAndFailuresCloseResources(t *testing.T) {
	bundle := cliAnswerBundle()
	lexical := search.BuildFromBundle(bundle)
	embedder := &cliAnswerEmbedder{descriptor: search.EmbeddingDescriptor{Dimensions: 2}}
	newSession := func(closeCalls *int, closeErr error) *answerSemanticSession {
		return &answerSemanticSession{
			Vector: cliAnswerVectorIndex(lexical, embedder.descriptor), Embedder: embedder,
			close: func() error { *closeCalls++; return closeErr },
		}
	}
	base := func() answerDependencies {
		return answerDependencies{
			loadDataset: func(string) (*server.Dataset, error) {
				return &server.Dataset{
					Root: t.TempDir(), Bundle: bundle, Search: lexical,
					Graph: graph.Build(bundle.Nodes, bundle.Edges),
				}, nil
			},
			stdout: &bytes.Buffer{}, now: time.Now,
		}
	}
	t.Run("cancelled after setup", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		closeCalls, providerCalls := 0, 0
		dependencies := base()
		dependencies.prepareSemantic = func(context.Context, string, *search.Index, semanticQueryOptions) (*answerSemanticSession, error) {
			cancel()
			return newSession(&closeCalls, nil), nil
		}
		dependencies.openProvider = func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			providerCalls++
			return nil, errors.New("must not open")
		}
		err := runAnswerContext(ctx, []string{"--mode", "semantic", "question"}, dependencies)
		if !errors.Is(err, context.Canceled) || closeCalls != 1 || providerCalls != 0 {
			t.Fatalf("error=%v semantic closes=%d provider calls=%d", err, closeCalls, providerCalls)
		}
	})
	t.Run("preparer returns session and error", func(t *testing.T) {
		closeCalls, providerCalls := 0, 0
		dependencies := base()
		dependencies.prepareSemantic = func(context.Context, string, *search.Index, semanticQueryOptions) (*answerSemanticSession, error) {
			return newSession(&closeCalls, errors.New("close failed")), errors.New("prepare failed")
		}
		dependencies.openProvider = func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			providerCalls++
			return nil, nil
		}
		err := runAnswerContext(context.Background(), []string{"--mode", "semantic", "question"}, dependencies)
		if err == nil || !strings.Contains(err.Error(), "prepare failed") || !strings.Contains(err.Error(), "close failed") || closeCalls != 1 || providerCalls != 0 {
			t.Fatalf("error=%v semantic closes=%d provider calls=%d", err, closeCalls, providerCalls)
		}
	})
	t.Run("generation setup fails", func(t *testing.T) {
		closeCalls := 0
		dependencies := base()
		dependencies.prepareSemantic = func(context.Context, string, *search.Index, semanticQueryOptions) (*answerSemanticSession, error) {
			return newSession(&closeCalls, errors.New("embedding close failed")), nil
		}
		dependencies.openProvider = func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			return nil, errors.New("generation setup failed")
		}
		err := runAnswerContext(context.Background(), []string{"--mode", "semantic", "question"}, dependencies)
		if err == nil || !strings.Contains(err.Error(), "generation setup failed") || !strings.Contains(err.Error(), "embedding close failed") || closeCalls != 1 {
			t.Fatalf("error=%v semantic closes=%d", err, closeCalls)
		}
	})
}

func TestRunAnswerSemanticOptionsFailClosedBeforeLoading(t *testing.T) {
	loadCalls := 0
	dependencies := answerDependencies{
		loadDataset:  func(string) (*server.Dataset, error) { loadCalls++; return nil, errors.New("must not load") },
		openProvider: func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) { return nil, nil },
		stdout:       &bytes.Buffer{}, now: time.Now,
	}
	for _, args := range [][]string{
		{"--embedding-model", "/model.gguf", "question"},
		{"--mode", "semantic", "question"},
		{"--mode", "remote", "question"},
	} {
		err := runAnswerContext(context.Background(), args, dependencies)
		if err == nil {
			t.Fatalf("args %v unexpectedly succeeded", args)
		}
	}
	if loadCalls != 0 {
		t.Fatalf("invalid semantic options loaded dataset %d times", loadCalls)
	}
}

func TestAnswerSemanticSessionCloseIsIdempotentAndCancellationIsEarly(t *testing.T) {
	closeCalls := 0
	session := &answerSemanticSession{close: func() error { closeCalls++; return errors.New("closed") }}
	if err := session.Close(); err == nil || err.Error() != "closed" {
		t.Fatalf("first close error = %v", err)
	}
	if err := session.Close(); err == nil || closeCalls != 1 {
		t.Fatalf("second close error=%v calls=%d", err, closeCalls)
	}
	if err := (*answerSemanticSession)(nil).Close(); err != nil {
		t.Fatalf("nil close error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := prepareQualifiedAnswerSemantic(ctx, "", nil, semanticQueryOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled semantic preparation = %v", err)
	}
	_, err = prepareQualifiedAnswerSemantic(nil, "", nil, semanticQueryOptions{})
	if err == nil || !strings.Contains(err.Error(), "context is required") {
		t.Fatalf("nil semantic context = %v", err)
	}
}

func TestQualifiedGenerationDefaultsFailClosed(t *testing.T) {
	_, err := openQualifiedGenerationProvider(qualifiedGenerationRequest{
		Provider: "disabled", ContextTokens: 4096, MaximumOutputTokens: 256,
		MaximumRSSMiB: 1024, BatchSize: 128, Timeout: time.Minute,
	})
	if err == nil || !strings.Contains(err.Error(), "provider is disabled") {
		t.Fatalf("disabled provider error = %v", err)
	}
}

func TestRunAnswerRejectsGroundingBoundsBeforeDatasetOrModel(t *testing.T) {
	loadCalls, providerCalls := 0, 0
	dependencies := answerDependencies{
		loadDataset: func(string) (*server.Dataset, error) {
			loadCalls++
			return nil, errors.New("dataset must not be loaded")
		},
		openProvider: func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			providerCalls++
			return nil, errors.New("provider must not be opened")
		},
		stdout: &bytes.Buffer{},
		now:    time.Now,
	}
	err := runAnswerContext(context.Background(), []string{"--max-nodes", "257", "question"}, dependencies)
	if err == nil || !strings.Contains(err.Error(), "max-nodes must be between") {
		t.Fatalf("invalid bound error = %v", err)
	}
	if loadCalls != 0 || providerCalls != 0 {
		t.Fatalf("invalid bound loaded dataset %d times and provider %d times", loadCalls, providerCalls)
	}
}

func TestWriteAnswerTextRendersAuditableAbstention(t *testing.T) {
	var output bytes.Buffer
	err := writeAnswerText(&output, groundedanswer.Result{
		Status: groundedanswer.StatusAbstained, Question: "Unknown\nquestion",
		Abstention: &groundedanswer.Abstention{
			Code: groundedanswer.AbstentionInsufficientEvidence, Reason: "No canonical evidence.",
			AvailableEvidence: 0, RequiredEvidence: 1,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := output.String()
	if !strings.Contains(text, "Status: abstained") || !strings.Contains(text, "insufficient_evidence") || strings.Contains(text, "Unknown\nquestion") {
		t.Fatalf("abstention output = %q", text)
	}
}

func directoryNames(entries []os.DirEntry) []string {
	names := make([]string, len(entries))
	for index, entry := range entries {
		names[index] = entry.Name()
	}
	return names
}

func cliAnswerBundle() rkcmodel.Bundle {
	return rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{SchemaVersion: rkcmodel.SchemaVersion, ID: "snapshot-cli-answer"},
		Artifacts: []rkcmodel.Artifact{{
			ID: "artifact-alpha", Path: "alpha.go", Kind: "source", Language: "go", Text: true, Status: "parsed",
		}},
		Nodes: []rkcmodel.Node{{
			ID: "node-alpha", ArtifactID: "artifact-alpha", Kind: "function", Name: "Alpha",
			QualifiedName: "pkg.Alpha", EvidenceIDs: []string{"e-alpha"},
		}},
		Evidence: []rkcmodel.Evidence{{
			ID: "e-alpha", Kind: "syntax", Method: "ast", Confidence: 1, Detail: "Alpha declaration.",
		}},
	}
}

func cliAnswerVectorIndex(lexical *search.Index, descriptor search.EmbeddingDescriptor) *search.VectorIndex {
	records := make([]search.VectorRecord, 0, len(lexical.Documents))
	for id := range lexical.Documents {
		records = append(records, search.VectorRecord{DocumentID: id, Values: []float32{1, 0}})
	}
	return &search.VectorIndex{
		Version: search.VectorIndexVersion, Descriptor: descriptor,
		Documents: lexical.Documents, Vectors: records,
	}
}
