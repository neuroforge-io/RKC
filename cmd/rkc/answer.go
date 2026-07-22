package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/neuroforge-io/RKC/internal/answerapp"
	"github.com/neuroforge-io/RKC/internal/groundedanswer"
	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/retrieval"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/server"
)

type answerDependencies struct {
	loadDataset     func(string) (*server.Dataset, error)
	openProvider    func(qualifiedGenerationRequest) (*qualifiedGenerationSession, error)
	prepareSemantic func(context.Context, string, *search.Index, semanticQueryOptions) (*answerSemanticSession, error)
	stdout          io.Writer
	now             func() time.Time
}

// answerSemanticSession owns the embedding process used for one answer. Close
// is idempotent so every error and cancellation path can release it safely.
type answerSemanticSession struct {
	Vector    *search.VectorIndex
	Embedder  search.EmbeddingProvider
	close     func() error
	closeOnce sync.Once
	closeErr  error
}

func (session *answerSemanticSession) Close() error {
	if session == nil {
		return nil
	}
	session.closeOnce.Do(func() {
		if session.close != nil {
			session.closeErr = session.close()
		}
	})
	return session.closeErr
}

func prepareQualifiedAnswerSemantic(
	ctx context.Context,
	datasetRoot string,
	lexical *search.Index,
	options semanticQueryOptions,
) (*answerSemanticSession, error) {
	if ctx == nil {
		return nil, errors.New("semantic answer context is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	vector, embedder, err := prepareSemanticQuery(ctx, datasetRoot, lexical, options)
	if err != nil {
		return nil, err
	}
	if vector == nil || embedder == nil {
		if embedder != nil {
			return nil, errors.Join(
				errors.New("semantic query preparation returned incomplete resources"),
				embedder.Close(),
			)
		}
		return nil, errors.New("semantic query preparation returned incomplete resources")
	}
	session := &answerSemanticSession{Vector: vector, Embedder: embedder, close: embedder.Close}
	if err := ctx.Err(); err != nil {
		return nil, errors.Join(err, session.Close())
	}
	return session, nil
}

func runAnswer(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runAnswerContext(ctx, args, answerDependencies{
		loadDataset: loadDataset, openProvider: openQualifiedGenerationProvider,
		prepareSemantic: prepareQualifiedAnswerSemantic,
		stdout:          os.Stdout, now: time.Now,
	})
}

func runAnswerContext(ctx context.Context, args []string, dependencies answerDependencies) (returnErr error) {
	if ctx == nil {
		return errors.New("answer context is required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if dependencies.loadDataset == nil || dependencies.openProvider == nil || dependencies.stdout == nil || dependencies.now == nil {
		return errors.New("answer dependencies are incomplete")
	}
	configPath := discoverFlagValue(args, "config")
	cfg, err := loadConfiguration(configPath)
	if err != nil {
		return err
	}
	fs := flag.NewFlagSet("answer", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	_ = fs.String("config", configPath, "JSON configuration file")
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	kinds := fs.String("kinds", "", "comma-separated node or artifact kinds")
	languages := fs.String("languages", "", "comma-separated languages")
	objects := fs.String("objects", "", "comma-separated object types")
	pathPrefix := fs.String("path-prefix", "", "restrict retrieval to a repository-relative path prefix")
	limit := fs.Int("limit", 20, "maximum lexical retrieval results")
	graphHops := fs.Int("graph-hops", cfg.Search.GraphExpansionHops, "bounded graph expansion hops after lexical retrieval")
	graphNodeLimit := fs.Int("graph-node-limit", 500, "maximum graph nodes inspected during expansion")
	modeValue := fs.String("mode", "lexical", "retrieval mode: lexical, semantic, or hybrid")
	vectorIndexPath := fs.String("vector-index", "", "persisted semantic vector-index JSON (outside the verified atlas)")
	buildVectorIndex := fs.Bool("build-vector-index", false, "build and publish a new vector index before answering")
	embeddingModel := fs.String("embedding-model", "", "qualified GGUF embedding model")
	embeddingExecutable := fs.String("llama-embedding", "llama-embedding", "path to the receipt-bound llama-embedding executable")
	embeddingModelLock := fs.String("embedding-model-lock", defaultSynthesisModelLockPath(), "checksum-pinned embedding model lock")
	embeddingAsset := fs.String("embedding-asset", "", "qualified embedding asset ID; defaults to the lock default")
	embeddingRuntimeReceipt := fs.String("embedding-runtime-receipt", "", "llama.cpp embedding runtime build receipt")
	taskValue := fs.String("task", string(modelruntime.TaskModuleSummary), "module_summary, execution_explanation, symbol_summary, or documentation_gap_analysis")
	providerType := fs.String("provider", cfg.Model.Provider, "model provider: llama.cpp")
	modelPath := fs.String("model", cfg.Model.ModelPath, "qualified GGUF generation model file")
	llamaCLI := fs.String("llama-cli", "llama-cli", "pinned llama.cpp CLI executable")
	modelLock := fs.String("model-lock", defaultSynthesisModelLockPath(), "trusted RKC model supply-chain lock")
	modelAsset := fs.String("model-asset", "", "qualified generation asset ID; defaults to the lock default")
	runtimeReceipt := fs.String("runtime-receipt", "", "llama.cpp build receipt; derived from a standard runtime layout when empty")
	contextTokens := fs.Int("context", cfg.Model.ContextTokens, "model context tokens")
	maxOutputTokens := fs.Int("max-output", cfg.Model.MaxOutputTokens, "maximum generated tokens")
	maxRSSMiB := fs.Int64("max-rss-mib", cfg.Model.MaxRSSMiB, "estimated and observed process RSS limit")
	threads := fs.Int("threads", 0, "model inference threads; 0 chooses a conservative default")
	batchSize := fs.Int("batch-size", 128, "llama.cpp logical batch size")
	timeout := fs.Duration("timeout", 10*time.Minute, "model inference timeout, at most 1h")
	maximumNodes := fs.Int("max-nodes", 24, "maximum canonical nodes supplied to the model")
	maximumEdges := fs.Int("max-edges", 64, "maximum canonical edges supplied to the model")
	maximumEvidence := fs.Int("max-evidence", 64, "maximum canonical evidence records supplied to the model")
	minimumEvidence := fs.Int("min-evidence", 1, "minimum canonical evidence records required to invoke the model")
	maximumContextBytes := fs.Int("max-context-bytes", 64*1024, "maximum canonical context text bytes")
	maximumFieldBytes := fs.Int("max-field-bytes", 8*1024, "maximum bytes retained from one canonical text field")
	maximumPromptBytes := fs.Int("max-prompt-bytes", 256*1024, "maximum serialized prompt bytes")
	maximumClaims := fs.Int("max-claims", 8, "maximum publishable grounded claims")
	maximumUnresolved := fs.Int("max-unresolved", 8, "maximum untrusted unresolved questions retained for audit")
	jsonOutput := fs.Bool("json", false, "print the complete machine-readable answer envelope")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("question text is required")
	}
	mode, err := parseQueryRetrievalMode(*modeValue)
	if err != nil {
		return err
	}
	semanticOptionSet := false
	fs.Visit(func(option *flag.Flag) {
		switch option.Name {
		case "vector-index", "build-vector-index", "embedding-model", "llama-embedding", "embedding-model-lock", "embedding-asset", "embedding-runtime-receipt":
			semanticOptionSet = true
		}
	})
	if mode == retrieval.ModeLexical && semanticOptionSet {
		return errors.New("embedding and vector-index options require --mode semantic or --mode hybrid")
	}
	if mode != retrieval.ModeLexical && dependencies.prepareSemantic == nil {
		return errors.New("answer semantic dependency is unavailable")
	}
	question := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if question == "" {
		return errors.New("question text is required")
	}
	if *limit < 1 || *limit > 1_000 {
		return errors.New("limit must be between 1 and 1000")
	}
	if *graphHops < 0 || *graphHops > 4 {
		return errors.New("graph-hops must be between 0 and 4")
	}
	if *graphNodeLimit < 1 || *graphNodeLimit > 5_000 {
		return errors.New("graph-node-limit must be between 1 and 5000")
	}
	groundingOptions := groundedanswer.Options{
		MaximumRetrievalHits: *limit,
		MaximumNodes:         *maximumNodes, MaximumEdges: *maximumEdges,
		MaximumEvidence: *maximumEvidence, MinimumEvidence: *minimumEvidence,
		MaximumContextTextBytes: *maximumContextBytes, MaximumFieldBytes: *maximumFieldBytes,
		MaximumPromptBytes: *maximumPromptBytes, MaximumClaims: *maximumClaims,
		MaximumUnresolved: *maximumUnresolved,
	}
	if err := validateAnswerGroundingOptions(question, groundingOptions); err != nil {
		return err
	}
	task := modelruntime.Task(strings.TrimSpace(*taskValue))
	if !validModelTask(task) {
		return fmt.Errorf("invalid model task %q", *taskValue)
	}
	dataset, err := dependencies.loadDataset(*dir)
	if err != nil {
		return err
	}
	if dataset == nil || dataset.Search == nil || dataset.Graph == nil {
		return errors.New("loaded dataset is incomplete")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	engine := &retrieval.Engine{Lexical: dataset.Search, Graph: dataset.Graph}
	var semantic *answerSemanticSession
	if mode != retrieval.ModeLexical {
		semantic, err = dependencies.prepareSemantic(ctx, dataset.Root, dataset.Search, semanticQueryOptions{
			VectorIndexPath: *vectorIndexPath, BuildVectorIndex: *buildVectorIndex,
			ModelPath: *embeddingModel, ExecutablePath: *embeddingExecutable,
			ModelLockPath: *embeddingModelLock, AssetID: *embeddingAsset,
			RuntimeReceiptPath: *embeddingRuntimeReceipt,
		})
		if err != nil {
			if semantic != nil {
				err = errors.Join(err, semantic.Close())
			}
			return err
		}
		if semantic == nil || semantic.Vector == nil || semantic.Embedder == nil {
			incompleteErr := errors.New("semantic answer preparation returned an incomplete session")
			if semantic != nil {
				incompleteErr = errors.Join(incompleteErr, semantic.Close())
			}
			return incompleteErr
		}
		defer func() {
			if closeErr := semantic.Close(); closeErr != nil {
				returnErr = errors.Join(returnErr, fmt.Errorf("close answer embedding provider: %w", closeErr))
			}
		}()
		engine.Vector = semantic.Vector
		engine.Embedder = semantic.Embedder
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	generation, err := dependencies.openProvider(qualifiedGenerationRequest{
		Provider: *providerType, ModelPath: *modelPath, LlamaCLI: *llamaCLI,
		ModelLock: *modelLock, ModelAsset: *modelAsset, RuntimeReceipt: *runtimeReceipt,
		ContextTokens: *contextTokens, MaximumOutputTokens: *maxOutputTokens,
		MaximumRSSMiB: *maxRSSMiB, Threads: *threads, BatchSize: *batchSize,
		Timeout: *timeout, Temperature: cfg.Model.Temperature,
	})
	if err != nil {
		return err
	}
	if generation == nil {
		return errors.New("answer model provider factory returned no session")
	}
	defer func() {
		if closeErr := generation.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close answer model provider: %w", closeErr))
		}
	}()
	if generation.Provider == nil {
		return errors.New("answer model provider is unavailable")
	}
	service, err := answerapp.New(
		dataset.Bundle,
		engine,
		generation.Provider,
		groundingOptions,
	)
	if err != nil {
		return err
	}
	deadline := dependencies.now().Add(*timeout)
	result, err := service.Answer(ctx, answerapp.Request{
		Question: question, Kinds: splitSet(*kinds), Languages: splitSet(*languages),
		ObjectTypes: splitSet(*objects), PathPrefix: strings.TrimSpace(*pathPrefix),
		Limit: *limit, GraphHops: *graphHops, GraphNodeLimit: *graphNodeLimit,
		RetrievalMode: mode,
		Task:          task, Inference: generation.Inference, Deadline: &deadline,
	})
	if err != nil {
		return err
	}
	if *jsonOutput {
		encoder := json.NewEncoder(dependencies.stdout)
		encoder.SetIndent("", "  ")
		encoder.SetEscapeHTML(false)
		return encoder.Encode(result)
	}
	return writeAnswerText(dependencies.stdout, result)
}

func validateAnswerGroundingOptions(question string, options groundedanswer.Options) error {
	if len(question) > 64*1024 {
		return errors.New("question must be no larger than 65536 bytes")
	}
	values := []struct {
		name    string
		value   int
		maximum int
	}{
		{"max-nodes", options.MaximumNodes, 256},
		{"max-edges", options.MaximumEdges, 1_024},
		{"max-evidence", options.MaximumEvidence, 1_024},
		{"min-evidence", options.MinimumEvidence, 1_024},
		{"max-context-bytes", options.MaximumContextTextBytes, 4 * 1024 * 1024},
		{"max-field-bytes", options.MaximumFieldBytes, 256 * 1024},
		{"max-prompt-bytes", options.MaximumPromptBytes, 8 * 1024 * 1024},
		{"max-claims", options.MaximumClaims, 128},
		{"max-unresolved", options.MaximumUnresolved, 128},
	}
	for _, item := range values {
		if item.value < 1 || item.value > item.maximum {
			return fmt.Errorf("%s must be between 1 and %d", item.name, item.maximum)
		}
	}
	if options.MinimumEvidence > options.MaximumEvidence {
		return errors.New("min-evidence cannot exceed max-evidence")
	}
	return nil
}

func writeAnswerText(writer io.Writer, result groundedanswer.Result) error {
	if _, err := fmt.Fprintf(writer, "Status: %s\nQuestion: %s\n", result.Status, terminalLine(result.Question)); err != nil {
		return err
	}
	if result.Status == groundedanswer.StatusAbstained {
		if result.Abstention == nil {
			return errors.New("abstained answer is missing its reason")
		}
		_, err := fmt.Fprintf(
			writer, "Abstention: %s\nReason: %s\nEvidence: %d available; %d required\n",
			terminalLine(result.Abstention.Code), terminalLine(result.Abstention.Reason),
			result.Abstention.AvailableEvidence, result.Abstention.RequiredEvidence,
		)
		return err
	}
	if _, err := fmt.Fprintln(writer, "Claims:"); err != nil {
		return err
	}
	for _, claim := range result.Claims {
		if _, err := fmt.Fprintf(writer, "- %s [%s]\n", terminalLine(claim.Text), strings.Join(claim.CitationIDs, ", ")); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintln(writer, "Citations:"); err != nil {
		return err
	}
	for _, citation := range result.Citations {
		location := "canonical evidence"
		if citation.Source != nil {
			if path := terminalLine(citation.Source.Path); path != "" {
				location = path
			}
			if citation.Source.StartLine > 0 {
				location += fmt.Sprintf(":%d", citation.Source.StartLine)
			}
		}
		if _, err := fmt.Fprintf(
			writer, "- [%s] evidence=%s source=%s nodes=%s\n",
			terminalLine(citation.ID), terminalLine(citation.EvidenceID), location,
			strings.Join(citation.NodeIDs, ","),
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(
		writer, "Snapshot: %s\nModel: %s\n",
		terminalLine(result.Provenance.SnapshotID), terminalLine(result.Provenance.ModelID),
	)
	return err
}

func terminalLine(value string) string {
	return strings.Join(strings.FieldsFunc(value, func(character rune) bool {
		return unicode.IsControl(character)
	}), " ")
}
