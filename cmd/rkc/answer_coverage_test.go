package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

type answerCoverageProvider struct {
	descriptor modelruntime.ModelDescriptor
	generate   func(context.Context, modelruntime.Request) (modelruntime.Response, error)
	closeErr   error
	closed     int
}

func (provider *answerCoverageProvider) Descriptor() modelruntime.ModelDescriptor {
	if provider == nil {
		return modelruntime.ModelDescriptor{}
	}
	return provider.descriptor
}

func (provider *answerCoverageProvider) Supports(modelruntime.Task) bool {
	return provider != nil
}

func (provider *answerCoverageProvider) Generate(ctx context.Context, request modelruntime.Request) (modelruntime.Response, error) {
	if provider == nil {
		return modelruntime.Response{}, errors.New("nil provider")
	}
	if provider.generate != nil {
		return provider.generate(ctx, request)
	}
	return modelruntime.Response{
		RequestID: request.RequestID,
		ModelID:   provider.descriptor.ID,
		Claims: []modelruntime.ClaimDraft{{
			Text: "Alpha is the entry point.", Category: "purpose", Certainty: "supported",
			EvidenceIDs: []string{"e-alpha"},
		}},
	}, nil
}

func (provider *answerCoverageProvider) Close() error {
	if provider == nil {
		return nil
	}
	provider.closed++
	return provider.closeErr
}

type answerFailWriter struct {
	failAt int
	writes int
}

func (writer *answerFailWriter) Write(data []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.failAt {
		return 0, errors.New("injected write failure")
	}
	return len(data), nil
}

func answerCoverageDependencies(provider modelruntime.Provider) answerDependencies {
	bundle := cliAnswerBundle()
	return answerDependencies{
		loadDataset: func(string) (*server.Dataset, error) {
			return &server.Dataset{
				Root: testrunAtlasRoot(), Bundle: bundle, Search: search.BuildFromBundle(bundle),
				Graph: graph.Build(bundle.Nodes, bundle.Edges),
			}, nil
		},
		openProvider: func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			return &qualifiedGenerationSession{Provider: provider}, nil
		},
		stdout: &bytes.Buffer{},
		now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
}

// testrunAtlasRoot is deliberately empty: lexical answer tests never persist
// derived state and semantic tests replace the preparation dependency.
func testrunAtlasRoot() string { return "" }

func requireAnswerCoverageError(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(contains)) {
		t.Fatalf("error = %v, want containing %q", err, contains)
	}
}

func TestAnswerCoverageRejectsInvalidRequestsBeforeSideEffects(t *testing.T) {
	provider := &answerCoverageProvider{descriptor: modelruntime.ModelDescriptor{ID: "coverage-model"}}
	valid := answerCoverageDependencies(provider)

	requireAnswerCoverageError(t, runAnswer(nil), "question text is required")
	requireAnswerCoverageError(t, runAnswerContext(nil, []string{"question"}, valid), "context is required")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runAnswerContext(cancelled, []string{"question"}, valid); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled preflight error = %v", err)
	}

	for name, dependencies := range map[string]answerDependencies{
		"all":              {},
		"loader":           {openProvider: valid.openProvider, stdout: valid.stdout, now: valid.now},
		"provider factory": {loadDataset: valid.loadDataset, stdout: valid.stdout, now: valid.now},
		"stdout":           {loadDataset: valid.loadDataset, openProvider: valid.openProvider, now: valid.now},
		"clock":            {loadDataset: valid.loadDataset, openProvider: valid.openProvider, stdout: valid.stdout},
	} {
		t.Run("dependencies-"+name, func(t *testing.T) {
			requireAnswerCoverageError(t, runAnswerContext(context.Background(), []string{"question"}, dependencies), "dependencies are incomplete")
		})
	}

	badConfig := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badConfig, []byte(`{"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	validation := []struct {
		name string
		args []string
		want string
	}{
		{"configuration", []string{"--config", badConfig, "question"}, "unknown"},
		{"flag", []string{"--not-a-real-answer-flag", "question"}, "flag provided but not defined"},
		{"missing question", nil, "question text is required"},
		{"blank question", []string{" \t "}, "question text is required"},
		{"mode whitespace", []string{"--mode", " lexical", "question"}, "surrounding whitespace"},
		{"mode unsupported", []string{"--mode", "remote", "question"}, "unsupported retrieval mode"},
		{"lexical embedding option", []string{"--embedding-asset", "asset", "question"}, "require --mode"},
		{"semantic dependency", []string{"--mode", "semantic", "question"}, "semantic dependency"},
		{"limit low", []string{"--limit", "0", "question"}, "limit must be"},
		{"limit high", []string{"--limit", "1001", "question"}, "limit must be"},
		{"hops low", []string{"--graph-hops", "-1", "question"}, "graph-hops must be"},
		{"hops high", []string{"--graph-hops", "5", "question"}, "graph-hops must be"},
		{"graph nodes low", []string{"--graph-node-limit", "0", "question"}, "graph-node-limit must be"},
		{"graph nodes high", []string{"--graph-node-limit", "5001", "question"}, "graph-node-limit must be"},
		{"nodes low", []string{"--max-nodes", "0", "question"}, "max-nodes must be"},
		{"edges high", []string{"--max-edges", "1025", "question"}, "max-edges must be"},
		{"evidence low", []string{"--max-evidence", "0", "question"}, "max-evidence must be"},
		{"minimum high", []string{"--min-evidence", "1025", "question"}, "min-evidence must be"},
		{"context high", []string{"--max-context-bytes", "4194305", "question"}, "max-context-bytes must be"},
		{"field high", []string{"--max-field-bytes", "262145", "question"}, "max-field-bytes must be"},
		{"prompt high", []string{"--max-prompt-bytes", "8388609", "question"}, "max-prompt-bytes must be"},
		{"claims high", []string{"--max-claims", "129", "question"}, "max-claims must be"},
		{"unresolved high", []string{"--max-unresolved", "129", "question"}, "max-unresolved must be"},
		{"minimum exceeds maximum", []string{"--min-evidence", "2", "--max-evidence", "1", "question"}, "cannot exceed"},
		{"task", []string{"--task", "free_form", "question"}, "invalid model task"},
	}
	for _, test := range validation {
		t.Run(test.name, func(t *testing.T) {
			dependencies := valid
			if strings.HasPrefix(test.name, "semantic dependency") {
				dependencies.prepareSemantic = nil
			}
			requireAnswerCoverageError(t, runAnswerContext(context.Background(), test.args, dependencies), test.want)
		})
	}

	oversized := strings.Repeat("q", 64*1024+1)
	requireAnswerCoverageError(t, runAnswerContext(context.Background(), []string{oversized}, valid), "no larger than")
}

func TestAnswerCoverageClosesPartialSessionsAndPropagatesFailures(t *testing.T) {
	provider := &answerCoverageProvider{descriptor: modelruntime.ModelDescriptor{ID: "coverage-model"}}

	t.Run("dataset load", func(t *testing.T) {
		dependencies := answerCoverageDependencies(provider)
		dependencies.loadDataset = func(string) (*server.Dataset, error) {
			return nil, errors.New("dataset failed")
		}
		requireAnswerCoverageError(t, runAnswerContext(context.Background(), []string{"question"}, dependencies), "dataset failed")
	})

	t.Run("incomplete dataset", func(t *testing.T) {
		dependencies := answerCoverageDependencies(provider)
		dependencies.loadDataset = func(string) (*server.Dataset, error) { return &server.Dataset{}, nil }
		requireAnswerCoverageError(t, runAnswerContext(context.Background(), []string{"question"}, dependencies), "dataset is incomplete")
	})

	t.Run("cancel after dataset", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		dependencies := answerCoverageDependencies(provider)
		original := dependencies.loadDataset
		dependencies.loadDataset = func(path string) (*server.Dataset, error) {
			dataset, err := original(path)
			cancel()
			return dataset, err
		}
		if err := runAnswerContext(ctx, []string{"question"}, dependencies); !errors.Is(err, context.Canceled) {
			t.Fatalf("post-load cancellation = %v", err)
		}
	})

	t.Run("nil semantic session", func(t *testing.T) {
		dependencies := answerCoverageDependencies(provider)
		dependencies.prepareSemantic = func(context.Context, string, *search.Index, semanticQueryOptions) (*answerSemanticSession, error) {
			return nil, nil
		}
		requireAnswerCoverageError(t, runAnswerContext(context.Background(), []string{"--mode", "semantic", "question"}, dependencies), "incomplete session")
	})

	t.Run("partial semantic session", func(t *testing.T) {
		closeCalls := 0
		dependencies := answerCoverageDependencies(provider)
		dependencies.prepareSemantic = func(context.Context, string, *search.Index, semanticQueryOptions) (*answerSemanticSession, error) {
			return &answerSemanticSession{close: func() error {
				closeCalls++
				return errors.New("partial semantic close")
			}}, nil
		}
		err := runAnswerContext(context.Background(), []string{"--mode", "hybrid", "question"}, dependencies)
		requireAnswerCoverageError(t, err, "partial semantic close")
		if closeCalls != 1 {
			t.Fatalf("partial semantic close calls = %d", closeCalls)
		}
	})

	t.Run("provider factory", func(t *testing.T) {
		dependencies := answerCoverageDependencies(provider)
		dependencies.openProvider = func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			return nil, errors.New("provider factory failed")
		}
		requireAnswerCoverageError(t, runAnswerContext(context.Background(), []string{"question"}, dependencies), "provider factory failed")
	})

	t.Run("nil provider session", func(t *testing.T) {
		dependencies := answerCoverageDependencies(provider)
		dependencies.openProvider = func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) { return nil, nil }
		requireAnswerCoverageError(t, runAnswerContext(context.Background(), []string{"question"}, dependencies), "returned no session")
	})

	t.Run("empty provider session", func(t *testing.T) {
		dependencies := answerCoverageDependencies(provider)
		dependencies.openProvider = func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			return &qualifiedGenerationSession{}, nil
		}
		requireAnswerCoverageError(t, runAnswerContext(context.Background(), []string{"question"}, dependencies), "provider is unavailable")
	})

	t.Run("typed nil provider", func(t *testing.T) {
		dependencies := answerCoverageDependencies(provider)
		var typedNil *answerCoverageProvider
		dependencies.openProvider = func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			return &qualifiedGenerationSession{Provider: typedNil}, nil
		}
		requireAnswerCoverageError(t, runAnswerContext(context.Background(), []string{"question"}, dependencies), "provider is required")
	})

	t.Run("generation and close", func(t *testing.T) {
		failing := &answerCoverageProvider{
			descriptor: modelruntime.ModelDescriptor{ID: "coverage-model"},
			generate: func(context.Context, modelruntime.Request) (modelruntime.Response, error) {
				return modelruntime.Response{}, errors.New("generation failed")
			},
			closeErr: errors.New("provider close failed"),
		}
		dependencies := answerCoverageDependencies(failing)
		err := runAnswerContext(context.Background(), []string{"Alpha"}, dependencies)
		requireAnswerCoverageError(t, err, "generation failed")
		requireAnswerCoverageError(t, err, "provider close failed")
		if failing.closed != 1 {
			t.Fatalf("provider close calls = %d", failing.closed)
		}
	})

	t.Run("json output", func(t *testing.T) {
		dependencies := answerCoverageDependencies(provider)
		dependencies.stdout = &answerFailWriter{failAt: 1}
		requireAnswerCoverageError(t, runAnswerContext(context.Background(), []string{"--json", "question"}, dependencies), "write failure")
	})
}

func TestAnswerCoverageRendersAnsweredResultsAndWriterFailures(t *testing.T) {
	result := groundedanswer.Result{
		Status:   groundedanswer.StatusAnswered,
		Question: "What\n is Alpha?",
		Claims: []groundedanswer.Claim{{
			Text: "Alpha\tis stable.", CitationIDs: []string{"citation-alpha"},
		}},
		Citations: []groundedanswer.Citation{
			{ID: "citation-alpha", EvidenceID: "e-alpha", NodeIDs: []string{"node-alpha"}, Source: &rkcmodel.SourceRange{Path: "alpha.go\n", StartLine: 7}},
			{ID: "citation-canonical", EvidenceID: "e-canonical", NodeIDs: []string{"node-beta"}},
		},
		Provenance: groundedanswer.Provenance{SnapshotID: "snapshot\n1", ModelID: "model\t1"},
	}
	var output bytes.Buffer
	if err := writeAnswerText(&output, result); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Status: answered", "Claims:", "Alpha is stable. [citation-alpha]", "Citations:",
		"source=alpha.go:7", "source=canonical evidence", "Snapshot: snapshot 1", "Model: model 1",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("answered output %q lacks %q", output.String(), expected)
		}
	}

	for failAt := 1; failAt <= 7; failAt++ {
		t.Run(fmt.Sprintf("answered-write-%d", failAt), func(t *testing.T) {
			writer := &answerFailWriter{failAt: failAt}
			if err := writeAnswerText(writer, result); err == nil {
				t.Fatalf("write %d unexpectedly succeeded after %d writes", failAt, writer.writes)
			}
		})
	}

	requireAnswerCoverageError(t, writeAnswerText(&bytes.Buffer{}, groundedanswer.Result{
		Status: groundedanswer.StatusAbstained, Question: "question",
	}), "missing its reason")
	abstained := groundedanswer.Result{
		Status: groundedanswer.StatusAbstained, Question: "question",
		Abstention: &groundedanswer.Abstention{
			Code: "insufficient_evidence", Reason: "none", AvailableEvidence: 0, RequiredEvidence: 1,
		},
	}
	for failAt := 1; failAt <= 2; failAt++ {
		writer := &answerFailWriter{failAt: failAt}
		if err := writeAnswerText(writer, abstained); err == nil {
			t.Fatalf("abstention write %d unexpectedly succeeded", failAt)
		}
	}

	if got := terminalLine("\n\t"); got != "" {
		t.Fatalf("control-only terminal line = %q", got)
	}
}

func TestAnswerCoverageValidatesQualifiedProviderRequests(t *testing.T) {
	base := qualifiedGenerationRequest{
		Provider: "llama.cpp", ModelPath: "missing.gguf", LlamaCLI: "missing-llama-cli",
		ModelLock: "missing-model-lock.json", ContextTokens: 512, MaximumOutputTokens: 64,
		MaximumRSSMiB: 512, Threads: 1, BatchSize: 32, Timeout: time.Minute,
	}
	tests := []struct {
		name   string
		mutate func(*qualifiedGenerationRequest)
		want   string
	}{
		{"disabled", func(request *qualifiedGenerationRequest) { request.Provider = "disabled"; request.ModelPath = "" }, "provider is disabled"},
		{"unknown provider", func(request *qualifiedGenerationRequest) { request.Provider = "remote" }, "not implemented"},
		{"missing model", func(request *qualifiedGenerationRequest) { request.ModelPath = "" }, "--model is required"},
		{"temperature", func(request *qualifiedGenerationRequest) { request.Temperature = 0.1 }, "temperature must be 0"},
		{"context low", func(request *qualifiedGenerationRequest) { request.ContextTokens = 511 }, "context must be"},
		{"context high", func(request *qualifiedGenerationRequest) { request.ContextTokens = 262145 }, "context must be"},
		{"output low", func(request *qualifiedGenerationRequest) { request.MaximumOutputTokens = 0 }, "max-output"},
		{"output high", func(request *qualifiedGenerationRequest) { request.MaximumOutputTokens = 513 }, "max-output"},
		{"rss low", func(request *qualifiedGenerationRequest) { request.MaximumRSSMiB = 255 }, "max-rss-mib"},
		{"rss high", func(request *qualifiedGenerationRequest) { request.MaximumRSSMiB = 2561 }, "max-rss-mib"},
		{"threads low", func(request *qualifiedGenerationRequest) { request.Threads = -1 }, "threads must be"},
		{"threads high", func(request *qualifiedGenerationRequest) { request.Threads = 65 }, "threads must be"},
		{"batch low", func(request *qualifiedGenerationRequest) { request.BatchSize = 0 }, "batch-size"},
		{"batch high", func(request *qualifiedGenerationRequest) { request.BatchSize = 4097 }, "batch-size"},
		{"timeout low", func(request *qualifiedGenerationRequest) { request.Timeout = 0 }, "timeout must be"},
		{"timeout high", func(request *qualifiedGenerationRequest) { request.Timeout = time.Hour + time.Second }, "timeout must be"},
		{"resolver", func(*qualifiedGenerationRequest) {}, "read model lock"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			test.mutate(&request)
			_, err := openQualifiedGenerationProvider(request)
			requireAnswerCoverageError(t, err, test.want)
		})
	}

	model := newCLIModelFixture(t)
	_, err := openQualifiedGenerationProvider(qualifiedGenerationRequest{
		Provider: "llama.cpp", ModelPath: model.modelPath, LlamaCLI: model.executablePath,
		ModelLock: model.lockPath, RuntimeReceipt: model.receiptPath,
		ContextTokens: 32769, MaximumOutputTokens: 64, MaximumRSSMiB: 512,
		Threads: 1, BatchSize: 32, Timeout: time.Minute,
	})
	requireAnswerCoverageError(t, err, "exceeds locked model context")

	if err := (*qualifiedGenerationSession)(nil).Close(); err != nil {
		t.Fatalf("nil generation close = %v", err)
	}
	if err := (&qualifiedGenerationSession{}).Close(); err != nil {
		t.Fatalf("empty generation close = %v", err)
	}
}

func TestAnswerCoverageSemanticPreparationFailsClosedBeforeModelExecution(t *testing.T) {
	lexical := search.BuildFromBundle(cliAnswerBundle())
	for _, test := range []struct {
		name    string
		ctx     context.Context
		lexical *search.Index
		options semanticQueryOptions
		want    string
	}{
		{"nil context", nil, lexical, semanticQueryOptions{}, "context and lexical index"},
		{"nil lexical", context.Background(), nil, semanticQueryOptions{}, "context and lexical index"},
		{"missing model", context.Background(), lexical, semanticQueryOptions{}, "embedding-model is required"},
		{"missing lock", context.Background(), lexical, semanticQueryOptions{ModelPath: "missing.gguf", ModelLockPath: "missing.lock"}, "read model lock"},
	} {
		t.Run(test.name, func(t *testing.T) {
			vector, embedder, err := prepareSemanticQuery(test.ctx, "", test.lexical, test.options)
			if vector != nil || embedder != nil {
				t.Fatalf("failed semantic preparation returned resources: vector=%v embedder=%v", vector, embedder)
			}
			requireAnswerCoverageError(t, err, test.want)
		})
	}

	if err := (&answerSemanticSession{}).Close(); err != nil {
		t.Fatalf("empty semantic close = %v", err)
	}
}
