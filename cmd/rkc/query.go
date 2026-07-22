package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/modelassets"
	"github.com/neuroforge-io/RKC/internal/retrieval"
	"github.com/neuroforge-io/RKC/internal/search"
)

const (
	queryVectorReceiptVersion = "1.0"
	queryVectorTextBytes      = 16 * 1024
	maximumVectorIndexBytes   = int64(512 * 1024 * 1024)
	maximumVectorReceiptBytes = int64(256 * 1024)
)

func runQuery(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fs := flag.NewFlagSet("query", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	dir := fs.String("dir", ".rkc", "generated RKC output directory")
	database := fs.String("database", "", "durable SQLite store (mutually exclusive with --dir)")
	snapshotID := fs.String("snapshot", "", "SQLite snapshot ID")
	repositoryID := fs.String("repository", "", "SQLite repository ID; selects its current snapshot")
	kinds := fs.String("kinds", "", "comma-separated node kinds")
	languages := fs.String("languages", "", "comma-separated languages")
	objects := fs.String("objects", "", "comma-separated object types")
	pathPrefix := fs.String("path-prefix", "", "restrict results to path prefix")
	limit := fs.Int("limit", 20, "maximum results")
	graphHops := fs.Int("graph-hops", 0, "bounded graph expansion hops after retrieval")
	modeValue := fs.String("mode", "lexical", "retrieval mode: lexical, semantic, or hybrid")
	vectorIndexPath := fs.String("vector-index", "", "persisted semantic vector-index JSON (outside the verified atlas)")
	buildVectorIndex := fs.Bool("build-vector-index", false, "build and publish a new vector index before querying")
	embeddingModel := fs.String("embedding-model", "", "qualified GGUF embedding model")
	embeddingExecutable := fs.String("llama-embedding", "llama-embedding", "path to the receipt-bound llama-embedding executable")
	embeddingModelLock := fs.String("embedding-model-lock", defaultSynthesisModelLockPath(), "checksum-pinned model lock")
	embeddingAsset := fs.String("embedding-asset", "", "qualified embedding asset ID (defaults to the lock default)")
	runtimeReceipt := fs.String("embedding-runtime-receipt", "", "llama.cpp runtime build receipt")
	jsonOutput := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		return errors.New("query text is required")
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
	dataset, err := loadSelectedDataset(ctx, *dir, *database, *snapshotID, *repositoryID, flagWasSet(fs, "dir"))
	if err != nil {
		return err
	}
	query := search.Query{Text: strings.Join(fs.Args(), " "), Kinds: splitSet(*kinds), Languages: splitSet(*languages), ObjectTypes: splitSet(*objects), PathPrefix: *pathPrefix, Limit: *limit}
	engine := retrieval.Engine{Lexical: dataset.Search, Graph: dataset.Graph}
	var embedder *search.LlamaCPPEmbedder
	if mode != retrieval.ModeLexical {
		engine.Vector, embedder, err = prepareSemanticQuery(ctx, dataset.Root, dataset.Search, semanticQueryOptions{
			VectorIndexPath: *vectorIndexPath, BuildVectorIndex: *buildVectorIndex,
			ModelPath: *embeddingModel, ExecutablePath: *embeddingExecutable,
			ModelLockPath: *embeddingModelLock, AssetID: *embeddingAsset, RuntimeReceiptPath: *runtimeReceipt,
		})
		if err != nil {
			return err
		}
		engine.Embedder = embedder
	}
	result, err := engine.Search(ctx, query, retrieval.Options{Mode: mode, GraphHops: *graphHops, GraphNodeLimit: 500})
	if embedder != nil {
		err = errors.Join(err, embedder.Close())
	}
	if err != nil {
		return err
	}
	if *jsonOutput {
		return writeJSONStdout(result)
	}
	for index, hit := range result.Hits {
		fmt.Printf("%2d. %8.3f %-12s %-16s %s\n", index+1, hit.Score, hit.Document.ObjectType, hit.Document.Kind, firstNonBlank(hit.Document.QualifiedName, hit.Document.Title))
	}
	if result.Truncated {
		fmt.Println("... results truncated")
	}
	return nil
}

type semanticQueryOptions struct {
	VectorIndexPath    string
	BuildVectorIndex   bool
	ModelPath          string
	ExecutablePath     string
	ModelLockPath      string
	AssetID            string
	RuntimeReceiptPath string
}

type queryVectorReceipt struct {
	SchemaVersion    string                       `json:"schema_version"`
	IndexSHA256      string                       `json:"index_sha256"`
	CorpusSHA256     string                       `json:"corpus_sha256"`
	MaximumTextBytes int                          `json:"maximum_text_bytes"`
	DocumentCount    int                          `json:"document_count"`
	Dimensions       int                          `json:"dimensions"`
	Binding          modelassets.EmbeddingBinding `json:"binding"`
}

func parseQueryRetrievalMode(value string) (retrieval.Mode, error) {
	if value != strings.TrimSpace(value) {
		return "", errors.New("retrieval mode has surrounding whitespace")
	}
	switch retrieval.Mode(value) {
	case retrieval.ModeLexical, retrieval.ModeSemantic, retrieval.ModeHybrid:
		return retrieval.Mode(value), nil
	default:
		return "", fmt.Errorf("unsupported retrieval mode %q", value)
	}
}

func prepareSemanticQuery(ctx context.Context, datasetRoot string, lexical *search.Index, options semanticQueryOptions) (*search.VectorIndex, *search.LlamaCPPEmbedder, error) {
	if ctx == nil || lexical == nil {
		return nil, nil, errors.New("semantic query requires a context and lexical index")
	}
	if strings.TrimSpace(options.ModelPath) == "" {
		return nil, nil, errors.New("--embedding-model is required for semantic and hybrid retrieval")
	}
	binding, err := modelassets.ResolveEmbedding(modelassets.EmbeddingRequest{
		LockPath: options.ModelLockPath, RuntimeReceiptPath: options.RuntimeReceiptPath,
		ExecutablePath: options.ExecutablePath, ModelPath: options.ModelPath, AssetID: options.AssetID,
	})
	if err != nil {
		return nil, nil, err
	}
	indexPath := strings.TrimSpace(options.VectorIndexPath)
	if options.VectorIndexPath != "" && indexPath != options.VectorIndexPath {
		return nil, nil, errors.New("vector-index path has surrounding whitespace")
	}
	if indexPath == "" {
		if !options.BuildVectorIndex {
			return nil, nil, errors.New("--vector-index is required unless --build-vector-index is set")
		}
		indexPath, err = defaultQueryVectorIndexPath(datasetRoot, binding.AssetID)
		if err != nil {
			return nil, nil, err
		}
	}
	indexPath, err = resolveQueryVectorIndexPath(indexPath, datasetRoot)
	if err != nil {
		return nil, nil, err
	}
	var vectorIndex *search.VectorIndex
	dimensions := 0
	if options.BuildVectorIndex {
		if err := requireAbsentQueryVectorIndex(indexPath); err != nil {
			return nil, nil, err
		}
	} else {
		vectorIndex, err = loadQueryVectorIndex(indexPath, lexical, binding)
		if err != nil {
			return nil, nil, err
		}
		dimensions = vectorIndex.Descriptor.Dimensions
	}
	contextTokens := binding.NativeContextTokens
	if contextTokens > 8192 {
		contextTokens = 8192
	}
	embedder, err := search.NewLlamaCPPEmbedder(search.LlamaCPPEmbeddingConfig{
		Executable: binding.ExecutablePath, ModelPath: binding.ModelPath, ModelID: binding.AssetID,
		ExpectedExecutableSHA256: binding.RuntimeSHA256, ExpectedModelSHA256: binding.ModelSHA256,
		Dimensions: dimensions, ContextTokens: contextTokens, Threads: 1, Timeout: 5 * time.Minute,
		MaximumInputBytes: queryVectorTextBytes, MaximumRSSBytes: 2560 * 1024 * 1024,
	})
	if err != nil {
		return nil, nil, err
	}
	if !options.BuildVectorIndex {
		return vectorIndex, embedder, nil
	}
	vectorIndex, err = search.BuildVectorIndex(ctx, lexical, embedder, search.VectorBuildOptions{
		BatchSize: 16, MaximumTextBytes: queryVectorTextBytes,
	})
	if err != nil {
		return nil, nil, errors.Join(err, embedder.Close())
	}
	if err := validateQueryVectorIndex(vectorIndex, lexical, binding); err != nil {
		return nil, nil, errors.Join(err, embedder.Close())
	}
	if err := publishQueryVectorIndex(indexPath, vectorIndex, lexical, binding); err != nil {
		return nil, nil, errors.Join(err, embedder.Close())
	}
	return vectorIndex, embedder, nil
}

func defaultQueryVectorIndexPath(datasetRoot, assetID string) (string, error) {
	if strings.TrimSpace(datasetRoot) == "" || strings.TrimSpace(assetID) == "" {
		return "", errors.New("dataset root and embedding asset ID are required")
	}
	absolute, err := filepath.Abs(datasetRoot)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(absolute), filepath.Base(absolute)+".rkc-derived", "search", assetID, "vector-index.json"), nil
}

func resolveQueryVectorIndexPath(path, datasetRoot string) (string, error) {
	if strings.TrimSpace(path) == "" || filepath.Ext(path) != ".json" {
		return "", errors.New("vector index must be a non-empty .json path")
	}
	target, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	target = filepath.Clean(target)
	if info, statErr := os.Lstat(target); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("vector index cannot be a symlink")
	} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	parent, err := resolveExistingQueryParent(filepath.Dir(target))
	if err != nil {
		return "", err
	}
	target = filepath.Join(parent, filepath.Base(target))
	dataset, err := filepath.Abs(datasetRoot)
	if err != nil {
		return "", err
	}
	dataset, err = filepath.EvalSymlinks(filepath.Clean(dataset))
	if err != nil {
		return "", fmt.Errorf("resolve dataset root: %w", err)
	}
	relative, err := filepath.Rel(dataset, target)
	if err != nil {
		return "", err
	}
	if relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return "", errors.New("vector index must remain outside the verified atlas")
	}
	return target, nil
}

func resolveExistingQueryParent(path string) (string, error) {
	current := filepath.Clean(path)
	var missing []string
	for {
		info, err := os.Lstat(current)
		if err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
				return "", errors.New("vector-index parent is not a real directory")
			}
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for index := len(missing) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, missing[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func queryVectorReceiptPath(indexPath string) string { return indexPath + ".receipt.json" }

func requireAbsentQueryVectorIndex(indexPath string) error {
	for _, path := range []string{indexPath, queryVectorReceiptPath(indexPath)} {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("semantic index artifact already exists: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func validateQueryVectorIndex(index *search.VectorIndex, lexical *search.Index, binding modelassets.EmbeddingBinding) error {
	if index == nil || lexical == nil {
		return errors.New("semantic index and lexical index are required")
	}
	expectedDigest := "sha256:" + binding.ModelSHA256
	if index.Descriptor.Provider != "llama.cpp-embedding" || index.Descriptor.Model != binding.AssetID || index.Descriptor.Digest != expectedDigest || index.Descriptor.Dimensions <= 0 {
		return errors.New("semantic index descriptor does not match the qualified embedding binding")
	}
	if !reflect.DeepEqual(index.Documents, lexical.Documents) || len(index.Vectors) != len(lexical.Documents) {
		return errors.New("semantic index documents do not match the current lexical corpus")
	}
	for _, record := range index.Vectors {
		document, ok := lexical.Documents[record.DocumentID]
		if !ok {
			return fmt.Errorf("semantic vector references unknown document %q", record.DocumentID)
		}
		digest := sha256.Sum256([]byte(queryEmbeddingText(document, queryVectorTextBytes)))
		if record.ContentSHA256 != hex.EncodeToString(digest[:]) {
			return fmt.Errorf("semantic vector content digest does not match document %q", record.DocumentID)
		}
	}
	return nil
}

func queryEmbeddingText(document search.Document, maximumBytes int) string {
	text := strings.Join([]string{
		"type: " + document.ObjectType, "kind: " + document.Kind, "language: " + document.Language,
		"title: " + document.Title, "qualified_name: " + document.QualifiedName,
		"signature: " + document.Signature, "path: " + document.Path, "content: " + document.Body,
	}, "\n")
	if len(text) <= maximumBytes {
		return text
	}
	text = text[:maximumBytes]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text
}

func loadQueryVectorIndex(indexPath string, lexical *search.Index, binding modelassets.EmbeddingBinding) (*search.VectorIndex, error) {
	receiptData, _, err := readBoundQueryRegular(queryVectorReceiptPath(indexPath), maximumVectorReceiptBytes)
	if err != nil {
		return nil, fmt.Errorf("read semantic index receipt: %w", err)
	}
	var receipt queryVectorReceipt
	if err := json.Unmarshal(receiptData, &receipt); err != nil {
		return nil, fmt.Errorf("decode semantic index receipt: %w", err)
	}
	canonicalReceipt, err := marshalQueryVectorReceipt(receipt)
	if err != nil || !bytes.Equal(receiptData, canonicalReceipt) {
		return nil, errors.New("semantic index receipt is not canonical")
	}
	index, indexDigest, err := loadBoundQueryVectorIndex(indexPath)
	if err != nil {
		return nil, err
	}
	corpusDigest, err := queryCorpusDigest(lexical)
	if err != nil {
		return nil, err
	}
	publicBinding := queryPublicEmbeddingBinding(binding)
	if receipt.SchemaVersion != queryVectorReceiptVersion || receipt.IndexSHA256 != indexDigest ||
		receipt.CorpusSHA256 != corpusDigest || receipt.MaximumTextBytes != queryVectorTextBytes ||
		receipt.DocumentCount != len(lexical.Documents) || receipt.Dimensions != index.Descriptor.Dimensions ||
		!reflect.DeepEqual(receipt.Binding, publicBinding) {
		return nil, errors.New("semantic index receipt does not match the current corpus or embedding binding")
	}
	if err := validateQueryVectorIndex(index, lexical, binding); err != nil {
		return nil, err
	}
	return index, nil
}

func publishQueryVectorIndex(indexPath string, index *search.VectorIndex, lexical *search.Index, binding modelassets.EmbeddingBinding) error {
	if err := requireAbsentQueryVectorIndex(indexPath); err != nil {
		return err
	}
	parent := filepath.Dir(indexPath)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 || parentInfo.Mode().Perm()&0o022 != 0 {
		return errors.New("semantic index parent must be an owner-controlled real directory")
	}
	staging, err := os.MkdirTemp(parent, ".rkc-vector-index-")
	if err != nil {
		return err
	}
	stagedIndex := filepath.Join(staging, "vector-index.json")
	stagedReceipt := filepath.Join(staging, "vector-index.receipt.json")
	defer func() {
		_ = os.Remove(stagedReceipt)
		_ = os.Remove(stagedIndex)
		_ = os.Remove(staging)
	}()
	if err := index.Save(stagedIndex); err != nil {
		return err
	}
	_, indexDigest, err := readBoundQueryRegular(stagedIndex, maximumVectorIndexBytes)
	if err != nil {
		return err
	}
	corpusDigest, err := queryCorpusDigest(lexical)
	if err != nil {
		return err
	}
	receiptData, err := marshalQueryVectorReceipt(queryVectorReceipt{
		SchemaVersion: queryVectorReceiptVersion, IndexSHA256: indexDigest, CorpusSHA256: corpusDigest,
		MaximumTextBytes: queryVectorTextBytes, DocumentCount: len(lexical.Documents),
		Dimensions: index.Descriptor.Dimensions, Binding: queryPublicEmbeddingBinding(binding),
	})
	if err != nil {
		return err
	}
	if err := os.WriteFile(stagedReceipt, receiptData, 0o600); err != nil {
		return err
	}
	if err := os.Link(stagedIndex, indexPath); err != nil {
		return fmt.Errorf("publish semantic index without replacement: %w", err)
	}
	if err := os.Link(stagedReceipt, queryVectorReceiptPath(indexPath)); err != nil {
		removeQueryLinkIfSame(indexPath, stagedIndex)
		return fmt.Errorf("publish semantic index receipt without replacement: %w", err)
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func marshalQueryVectorReceipt(receipt queryVectorReceipt) ([]byte, error) {
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func queryPublicEmbeddingBinding(binding modelassets.EmbeddingBinding) modelassets.EmbeddingBinding {
	binding.ExecutablePath = ""
	binding.ModelPath = ""
	binding.RuntimeReceiptPath = ""
	binding.LockPath = ""
	return binding
}

func queryCorpusDigest(index *search.Index) (string, error) {
	if index == nil {
		return "", errors.New("lexical index is required")
	}
	data, err := json.Marshal(index.Documents)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func loadBoundQueryVectorIndex(path string) (*search.VectorIndex, string, error) {
	if runtime.GOOS != "linux" {
		return nil, "", errors.New("semantic index descriptor binding currently requires Linux procfs")
	}
	file, info, err := openBoundQueryRegular(path, maximumVectorIndexBytes)
	if err != nil {
		return nil, "", fmt.Errorf("open semantic index: %w", err)
	}
	defer file.Close()
	digest, err := digestOpenQueryRegular(file, maximumVectorIndexBytes)
	if err != nil {
		return nil, "", err
	}
	index, err := search.LoadVectorIndex(fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), file.Fd()))
	if err != nil {
		return nil, "", err
	}
	if err := verifyBoundQueryRegular(path, file, info); err != nil {
		return nil, "", err
	}
	return index, digest, nil
}

func readBoundQueryRegular(path string, maximum int64) ([]byte, string, error) {
	file, info, err := openBoundQueryRegular(path, maximum)
	if err != nil {
		return nil, "", err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, "", errors.New("semantic artifact exceeded its read limit")
	}
	if err := verifyBoundQueryRegular(path, file, info); err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(data)
	return data, hex.EncodeToString(digest[:]), nil
}

func openBoundQueryRegular(path string, maximum int64) (*os.File, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || info.Size() < 0 || info.Size() > maximum {
		return nil, nil, errors.New("semantic artifact must be a bounded, non-writable regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, nil, errors.New("semantic artifact identity changed while opening")
	}
	return file, opened, nil
}

func digestOpenQueryRegular(file *os.File, maximum int64) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil || written > maximum {
		return "", errors.New("semantic index exceeded its read limit")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func verifyBoundQueryRegular(path string, file *os.File, expected os.FileInfo) error {
	opened, openErr := file.Stat()
	pathname, pathErr := os.Lstat(path)
	if openErr != nil || pathErr != nil || !os.SameFile(expected, opened) || !os.SameFile(opened, pathname) || opened.Size() != expected.Size() {
		return errors.New("semantic artifact changed while reading")
	}
	return nil
}

func removeQueryLinkIfSame(path, expectedPath string) {
	actual, actualErr := os.Lstat(path)
	expected, expectedErr := os.Lstat(expectedPath)
	if actualErr == nil && expectedErr == nil && os.SameFile(actual, expected) {
		_ = os.Remove(path)
	}
}
