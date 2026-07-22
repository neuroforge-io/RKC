package search

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
)

var errEmbeddingOutputTooLarge = errors.New("embedding output exceeded configured limit")

// LlamaCPPEmbeddingConfig configures the native llama.cpp embedding executable.
// RKC invokes one input per process so repository text can never inject the
// delimiter used by llama-embedding's multi-prompt mode.
type LlamaCPPEmbeddingConfig struct {
	Executable               string
	ModelPath                string
	ModelID                  string
	ExpectedExecutableSHA256 string
	ExpectedModelSHA256      string
	Dimensions               int
	ContextTokens            int
	Threads                  int
	Timeout                  time.Duration
	MaximumInputs            int
	MaximumInputBytes        int
	MaximumStdoutBytes       int64
	MaximumStderrBytes       int64
	MaximumRSSBytes          int64
	Environment              []string
	// UnsafeDisableResourceGuard is limited to hermetic tests or explicitly
	// unsupported platforms. Production configuration must leave it false.
	UnsafeDisableResourceGuard bool
}

type boundRegularFile struct {
	path        string
	info        os.FileInfo
	digest      string
	executable  bool
	requireGGUF bool
	file        *os.File
}

// LlamaCPPEmbedder is a serialized, bounded native embedding provider.
type LlamaCPPEmbedder struct {
	config        LlamaCPPEmbeddingConfig
	executable    boundRegularFile
	model         boundRegularFile
	gate          chan struct{}
	lifecycleMu   sync.RWMutex
	closed        bool
	mu            sync.RWMutex
	dimensions    int
	priorityCheck func() error
}

func NewLlamaCPPEmbedder(config LlamaCPPEmbeddingConfig) (*LlamaCPPEmbedder, error) {
	var priorityCheck func() error
	if !config.UnsafeDisableResourceGuard {
		if err := resourceguard.CheckHigherPriority(); err != nil {
			return nil, err
		}
		if err := resourceguard.RequireCurrentProcessLowPriority(); err != nil {
			return nil, err
		}
		priorityCheck = resourceguard.CheckHigherPriority
	}
	expectedExecutable, err := expectedEmbeddingSHA256(
		config.ExpectedExecutableSHA256, "llama-embedding executable", !config.UnsafeDisableResourceGuard,
	)
	if err != nil {
		return nil, err
	}
	expectedModel, err := expectedEmbeddingSHA256(
		config.ExpectedModelSHA256, "embedding model", !config.UnsafeDisableResourceGuard,
	)
	if err != nil {
		return nil, err
	}
	config, err = normalizeLlamaCPPEmbeddingConfig(config)
	if err != nil {
		return nil, err
	}
	executable, err := bindRegularFile(config.Executable, true, false, priorityCheck)
	if err != nil {
		return nil, fmt.Errorf("bind llama-embedding executable: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = executable.close()
		}
	}()
	if expectedExecutable != "" && expectedExecutable != executable.digest {
		return nil, fmt.Errorf("llama-embedding executable SHA256 mismatch: got %s, want %s", executable.digest, expectedExecutable)
	}
	model, err := bindRegularFile(config.ModelPath, false, true, priorityCheck)
	if err != nil {
		return nil, fmt.Errorf("bind embedding model: %w", err)
	}
	defer func() {
		if !committed {
			_ = model.close()
		}
	}()
	if expectedModel != "" && expectedModel != model.digest {
		return nil, fmt.Errorf("embedding model SHA256 mismatch: got %s, want %s", model.digest, expectedModel)
	}
	config.Executable = executable.path
	config.ModelPath = model.path
	if strings.TrimSpace(config.ModelID) == "" {
		config.ModelID = strings.TrimSuffix(filepath.Base(model.path), filepath.Ext(model.path))
	}
	if strings.TrimSpace(config.ModelID) == "" {
		return nil, errors.New("embedding model id is required")
	}
	if model.info.Size() >= config.MaximumRSSBytes {
		return nil, errors.New("embedding model file alone exceeds the configured RSS limit")
	}
	embedder := &LlamaCPPEmbedder{
		config: config, executable: executable, model: model, gate: make(chan struct{}, 1), dimensions: config.Dimensions,
		priorityCheck: priorityCheck,
	}
	committed = true
	return embedder, nil
}

func normalizeLlamaCPPEmbeddingConfig(config LlamaCPPEmbeddingConfig) (LlamaCPPEmbeddingConfig, error) {
	if strings.TrimSpace(config.Executable) == "" {
		config.Executable = "llama-embedding"
	}
	if strings.TrimSpace(config.ModelPath) == "" {
		return config, errors.New("llama.cpp embedding model path is required")
	}
	if config.Dimensions < 0 || config.Dimensions > 65536 {
		return config, errors.New("embedding dimensions must be between 0 and 65536")
	}
	if config.ContextTokens <= 0 {
		config.ContextTokens = 8192
	}
	if config.ContextTokens > 262144 {
		return config, errors.New("embedding context must not exceed 262144 tokens")
	}
	if config.Threads <= 0 {
		config.Threads = 1
	}
	if config.Threads > 64 {
		return config, errors.New("embedding threads must not exceed 64")
	}
	if !config.UnsafeDisableResourceGuard && config.Threads != 1 {
		return config, errors.New("production embedding threads must equal 1")
	}
	if config.Timeout <= 0 {
		config.Timeout = 5 * time.Minute
	}
	if config.Timeout > 30*time.Minute {
		return config, errors.New("embedding timeout must not exceed 30 minutes")
	}
	if config.MaximumInputs <= 0 {
		config.MaximumInputs = 256
	}
	if config.MaximumInputs > 256 {
		return config, errors.New("embedding input count must not exceed 256")
	}
	if config.MaximumInputBytes <= 0 {
		config.MaximumInputBytes = 64 * 1024
	}
	if config.MaximumInputBytes > 4*1024*1024 {
		return config, errors.New("embedding input byte limit must not exceed 4 MiB")
	}
	if config.MaximumStdoutBytes <= 0 {
		config.MaximumStdoutBytes = 64 * 1024 * 1024
	}
	if config.MaximumStdoutBytes > 256*1024*1024 {
		return config, errors.New("embedding stdout limit must not exceed 256 MiB")
	}
	if config.MaximumStderrBytes <= 0 {
		config.MaximumStderrBytes = 2 * 1024 * 1024
	}
	if config.MaximumStderrBytes > 16*1024*1024 {
		return config, errors.New("embedding stderr limit must not exceed 16 MiB")
	}
	if config.MaximumRSSBytes == 0 {
		config.MaximumRSSBytes = 2560 * 1024 * 1024
	}
	if config.MaximumRSSBytes < 64*1024*1024 || config.MaximumRSSBytes > 64*1024*1024*1024 {
		return config, errors.New("embedding RSS limit must be between 64 MiB and 64 GiB")
	}
	if !config.UnsafeDisableResourceGuard && config.MaximumRSSBytes > 2560*1024*1024 {
		return config, errors.New("production embedding RSS limit must not exceed 2560 MiB")
	}
	return config, nil
}

func (embedder *LlamaCPPEmbedder) Descriptor() EmbeddingDescriptor {
	if embedder == nil {
		return EmbeddingDescriptor{}
	}
	embedder.lifecycleMu.RLock()
	defer embedder.lifecycleMu.RUnlock()
	if embedder.closed {
		return EmbeddingDescriptor{}
	}
	embedder.mu.RLock()
	defer embedder.mu.RUnlock()
	return EmbeddingDescriptor{
		Provider: "llama.cpp-embedding", Model: embedder.config.ModelID,
		Digest: "sha256:" + embedder.model.digest, Dimensions: embedder.dimensions,
	}
}

func (embedder *LlamaCPPEmbedder) Embed(parent context.Context, texts []string) ([][]float32, error) {
	if embedder == nil {
		return nil, errors.New("llama.cpp embedder is not configured")
	}
	embedder.lifecycleMu.RLock()
	defer embedder.lifecycleMu.RUnlock()
	if embedder.closed {
		return nil, errors.New("llama.cpp embedder is closed")
	}
	if parent == nil {
		return nil, errors.New("embedding context is required")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	if len(texts) > embedder.config.MaximumInputs {
		return nil, fmt.Errorf("embedding request contains %d inputs; limit is %d", len(texts), embedder.config.MaximumInputs)
	}
	for index, text := range texts {
		if text == "" {
			return nil, fmt.Errorf("embedding input %d is empty", index)
		}
		if len(text) > embedder.config.MaximumInputBytes {
			return nil, fmt.Errorf("embedding input %d exceeds the %d-byte limit", index, embedder.config.MaximumInputBytes)
		}
		if !utf8.ValidString(text) || strings.IndexByte(text, 0) >= 0 {
			return nil, fmt.Errorf("embedding input %d must be valid NUL-free UTF-8", index)
		}
	}
	select {
	case embedder.gate <- struct{}{}:
		defer func() { <-embedder.gate }()
	case <-parent.Done():
		return nil, parent.Err()
	}
	if embedder.priorityCheck != nil {
		if err := embedder.priorityCheck(); err != nil {
			return nil, err
		}
	}
	if err := verifyBoundRegularFile(&embedder.executable, embedder.priorityCheck); err != nil {
		return nil, fmt.Errorf("verify llama-embedding executable: %w", err)
	}
	if err := verifyBoundRegularFile(&embedder.model, embedder.priorityCheck); err != nil {
		return nil, fmt.Errorf("verify embedding model: %w", err)
	}
	ctx, cancel := context.WithTimeout(parent, embedder.config.Timeout)
	defer cancel()
	vectors := make([][]float32, 0, len(texts))
	for index, text := range texts {
		vector, err := embedder.embedOne(ctx, text)
		if err != nil {
			return nil, fmt.Errorf("embed input %d: %w", index, err)
		}
		if err := embedder.acceptDimensions(len(vector)); err != nil {
			return nil, fmt.Errorf("embed input %d: %w", index, err)
		}
		vectors = append(vectors, vector)
	}
	if err := verifyBoundRegularFile(&embedder.executable, embedder.priorityCheck); err != nil {
		return nil, fmt.Errorf("reverify llama-embedding executable: %w", err)
	}
	if err := verifyBoundRegularFile(&embedder.model, embedder.priorityCheck); err != nil {
		return nil, fmt.Errorf("reverify embedding model: %w", err)
	}
	return vectors, nil
}

// Close releases the exact executable and model inodes bound at construction.
// It is idempotent and waits for an active serialized embedding request.
func (embedder *LlamaCPPEmbedder) Close() error {
	if embedder == nil {
		return nil
	}
	embedder.lifecycleMu.Lock()
	defer embedder.lifecycleMu.Unlock()
	if embedder.closed {
		return nil
	}
	embedder.closed = true
	return errors.Join(embedder.executable.close(), embedder.model.close())
}

func (embedder *LlamaCPPEmbedder) embedOne(ctx context.Context, text string) (vector []float32, resultErr error) {
	input, err := writeSecureEmbeddingInput(text)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err := input.closeAndRemove(); err != nil && resultErr == nil {
			resultErr = fmt.Errorf("clean private embedding input: %w", err)
			vector = nil
		}
	}()
	arguments := []string{
		"--model", embedder.model.referencePath(),
		"--ctx-size", fmt.Sprint(embedder.config.ContextTokens),
		"--batch-size", fmt.Sprint(embedder.config.ContextTokens),
		"--ubatch-size", fmt.Sprint(minimum(512, embedder.config.ContextTokens)),
		"--threads", fmt.Sprint(embedder.config.Threads),
		"--threads-batch", fmt.Sprint(embedder.config.Threads),
		"--device", "none",
		"--n-gpu-layers", "0",
		"--pooling", "last",
		"--embd-normalize", "2",
		"--embd-output-format", "json",
		"--file", input.referencePath(),
	}
	command, err := resourceguard.NewCommand(ctx, resourceguard.Config{
		Executable:                 embedder.executable.referencePath(),
		Arguments:                  arguments,
		Environment:                resourceguard.SanitizedModelEnvironment(embedder.config.Environment),
		MaximumRSSBytes:            embedder.config.MaximumRSSBytes,
		UnitPrefix:                 "rkc-embedding",
		UnsafeDisableCgroup:        embedder.config.UnsafeDisableResourceGuard,
		UnsafeDisablePriorityCheck: embedder.config.UnsafeDisableResourceGuard,
	})
	if err != nil {
		return nil, err
	}
	stdout := &embeddingBoundedBuffer{limit: embedder.config.MaximumStdoutBytes}
	stderr := &embeddingBoundedBuffer{limit: embedder.config.MaximumStderrBytes}
	_, runErr := command.Run(ctx, stdout, stderr)
	if errors.Is(stdout.err, errEmbeddingOutputTooLarge) || errors.Is(stderr.err, errEmbeddingOutputTooLarge) {
		return nil, errEmbeddingOutputTooLarge
	}
	if runErr != nil {
		if errors.Is(runErr, resourceguard.ErrRSSLimitExceeded) {
			return nil, runErr
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("embedding deadline or cancellation: %w", ctx.Err())
		}
		message := strings.TrimSpace(stderr.String())
		if len(message) > 1200 {
			message = message[:1200] + "..."
		}
		if message != "" {
			return nil, fmt.Errorf("llama-embedding process failed: %w: %s", runErr, message)
		}
		return nil, fmt.Errorf("llama-embedding process failed: %w", runErr)
	}
	return parseLlamaEmbedding(stdout.Bytes())
}

func (embedder *LlamaCPPEmbedder) acceptDimensions(dimensions int) error {
	if dimensions <= 0 || dimensions > 65536 {
		return fmt.Errorf("embedding dimensions %d are outside the supported range", dimensions)
	}
	embedder.mu.Lock()
	defer embedder.mu.Unlock()
	if embedder.dimensions == 0 {
		embedder.dimensions = dimensions
		return nil
	}
	if embedder.dimensions != dimensions {
		return fmt.Errorf("embedding dimension %d does not match bound dimension %d", dimensions, embedder.dimensions)
	}
	return nil
}

func parseLlamaEmbedding(output []byte) ([]float32, error) {
	var envelope struct {
		Object string `json:"object"`
		Data   []struct {
			Object    string    `json:"object"`
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		} `json:"data"`
	}
	decoder := json.NewDecoder(bytes.NewReader(output))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode llama-embedding output: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("decode llama-embedding output: %w", err)
	}
	if envelope.Object != "list" || len(envelope.Data) != 1 || envelope.Data[0].Object != "embedding" || envelope.Data[0].Index != 0 {
		return nil, errors.New("llama-embedding output violated the single-input protocol")
	}
	vector := envelope.Data[0].Embedding
	if err := validateLlamaEmbedding(vector); err != nil {
		return nil, err
	}
	return vector, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func validateLlamaEmbedding(vector []float32) error {
	if len(vector) == 0 {
		return errors.New("llama-embedding returned an empty vector")
	}
	if len(vector) > 65536 {
		return errors.New("llama-embedding returned too many dimensions")
	}
	var squaredNorm float64
	for _, value := range vector {
		converted := float64(value)
		if math.IsNaN(converted) || math.IsInf(converted, 0) {
			return errors.New("llama-embedding returned a non-finite value")
		}
		squaredNorm += converted * converted
	}
	if squaredNorm == 0 {
		return errors.New("llama-embedding returned a zero vector")
	}
	if math.Abs(math.Sqrt(squaredNorm)-1) > 1e-3 {
		return errors.New("llama-embedding returned a vector that is not L2-normalized")
	}
	return nil
}

func bindRegularFile(path string, executable, requireGGUF bool, priorityCheck func() error) (boundRegularFile, error) {
	if strings.TrimSpace(path) == "" {
		return boundRegularFile{}, errors.New("file path is required")
	}
	resolved := path
	var err error
	if executable && !strings.ContainsRune(path, os.PathSeparator) {
		resolved, err = exec.LookPath(path)
		if err != nil {
			return boundRegularFile{}, err
		}
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return boundRegularFile{}, err
	}
	resolved, err = filepath.EvalSymlinks(resolved)
	if err != nil {
		return boundRegularFile{}, err
	}
	file, err := os.Open(resolved)
	if err != nil {
		return boundRegularFile{}, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return boundRegularFile{}, err
	}
	pathname, err := os.Lstat(resolved)
	if err != nil {
		return boundRegularFile{}, err
	}
	if !info.Mode().IsRegular() || !pathname.Mode().IsRegular() || !os.SameFile(info, pathname) {
		return boundRegularFile{}, errors.New("path must resolve to a regular file")
	}
	if executable && runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return boundRegularFile{}, errors.New("executable path has no execute permission")
	}
	if info.Mode().Perm()&0o022 != 0 {
		return boundRegularFile{}, errors.New("bound file must not be group/other writable")
	}
	bound := boundRegularFile{
		path: resolved, info: info, executable: executable, requireGGUF: requireGGUF, file: file,
	}
	digest, err := hashOpenBoundFile(&bound, priorityCheck)
	if err != nil {
		return boundRegularFile{}, err
	}
	bound.digest = digest
	if requireGGUF {
		magic := make([]byte, 4)
		if _, err := file.ReadAt(magic, 0); err != nil || string(magic) != "GGUF" {
			return boundRegularFile{}, errors.New("embedding model does not have GGUF magic")
		}
	}
	committed = true
	return bound, nil
}

func (bound *boundRegularFile) referencePath() string {
	if bound == nil || bound.file == nil {
		return ""
	}
	if runtime.GOOS == "linux" {
		return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), bound.file.Fd())
	}
	return bound.path
}

func (bound *boundRegularFile) close() error {
	if bound == nil || bound.file == nil {
		return nil
	}
	err := bound.file.Close()
	bound.file = nil
	return err
}

func verifyBoundRegularFile(bound *boundRegularFile, priorityCheck func() error) error {
	if bound == nil || bound.file == nil || bound.info == nil {
		return errors.New("bound file is closed or incomplete")
	}
	canonical, err := filepath.EvalSymlinks(bound.path)
	if err != nil {
		return err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil || filepath.Clean(canonical) != bound.path {
		return errors.New("bound file canonical path changed")
	}
	info, err := os.Lstat(bound.path)
	if err != nil {
		return err
	}
	opened, err := bound.file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !opened.Mode().IsRegular() || !os.SameFile(bound.info, info) || !os.SameFile(bound.info, opened) {
		return errors.New("bound file identity changed")
	}
	if info.Mode().Perm()&0o022 != 0 || opened.Mode().Perm()&0o022 != 0 {
		return errors.New("bound file became group/other writable")
	}
	if bound.executable && runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return errors.New("bound executable lost execute permission")
	}
	digest, err := hashOpenBoundFile(bound, priorityCheck)
	if err != nil {
		return err
	}
	if digest != bound.digest {
		return errors.New("bound file content digest changed")
	}
	if bound.requireGGUF {
		magic := make([]byte, 4)
		if _, err := bound.file.ReadAt(magic, 0); err != nil || string(magic) != "GGUF" {
			return errors.New("embedding model GGUF magic changed")
		}
	}
	return nil
}

func hashOpenBoundFile(bound *boundRegularFile, priorityCheck func() error) (string, error) {
	if bound == nil || bound.file == nil || bound.info == nil {
		return "", errors.New("bound file is closed or incomplete")
	}
	if priorityCheck != nil {
		if err := priorityCheck(); err != nil {
			return "", err
		}
	}
	opened, err := bound.file.Stat()
	if err != nil {
		return "", err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(bound.info, opened) || opened.Size() < 0 {
		return "", errors.New("bound file identity changed before hashing")
	}
	hash := sha256.New()
	buffer := make([]byte, 1024*1024)
	var sincePriorityCheck int64
	var offset int64
	for offset < opened.Size() {
		remaining := opened.Size() - offset
		size := len(buffer)
		if remaining < int64(size) {
			size = int(remaining)
		}
		read, readErr := bound.file.ReadAt(buffer[:size], offset)
		if read > 0 {
			_, _ = hash.Write(buffer[:read])
			offset += int64(read)
			sincePriorityCheck += int64(read)
		}
		if sincePriorityCheck >= 64*1024*1024 {
			if priorityCheck != nil {
				if err := priorityCheck(); err != nil {
					return "", err
				}
			}
			sincePriorityCheck = 0
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
		if read != size {
			return "", errors.New("bound file size changed while hashing")
		}
	}
	extra := []byte{0}
	if read, err := bound.file.ReadAt(extra, opened.Size()); read != 0 || (err != nil && !errors.Is(err, io.EOF)) {
		return "", errors.New("bound file grew while hashing")
	}
	if priorityCheck != nil {
		if err := priorityCheck(); err != nil {
			return "", err
		}
	}
	after, err := bound.file.Stat()
	if err != nil {
		return "", err
	}
	pathAfter, err := os.Lstat(bound.path)
	if err != nil {
		return "", err
	}
	if after.Size() != opened.Size() || after.ModTime() != opened.ModTime() || !os.SameFile(bound.info, after) || !os.SameFile(bound.info, pathAfter) {
		return "", errors.New("file changed while hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func expectedEmbeddingSHA256(value, label string, required bool) (string, error) {
	if value == "" {
		if required {
			return "", fmt.Errorf("%s SHA256 is required", label)
		}
		return "", nil
	}
	if value != strings.TrimSpace(value) || value != strings.ToLower(value) || strings.HasPrefix(value, "sha256:") {
		return "", fmt.Errorf("%s SHA256 must be canonical lowercase hex", label)
	}
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("%s SHA256 must contain 64 lowercase hexadecimal characters", label)
	}
	return value, nil
}

type secureEmbeddingInput struct {
	directory     string
	directoryInfo os.FileInfo
	path          string
	file          *os.File
	fileInfo      os.FileInfo
}

func (input *secureEmbeddingInput) referencePath() string {
	if input == nil || input.file == nil {
		return ""
	}
	if runtime.GOOS == "linux" {
		return fmt.Sprintf("/proc/%d/fd/%d", os.Getpid(), input.file.Fd())
	}
	return input.path
}

func (input *secureEmbeddingInput) closeAndRemove() error {
	if input == nil {
		return nil
	}
	var failures []error
	if input.file != nil {
		failures = append(failures, input.file.Close())
		input.file = nil
	}
	if input.path != "" && input.fileInfo != nil {
		current, err := os.Lstat(input.path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			failures = append(failures, err)
		case !current.Mode().IsRegular() || !os.SameFile(input.fileInfo, current):
			failures = append(failures, errors.New("private embedding input identity changed"))
		default:
			failures = append(failures, os.Remove(input.path))
		}
	}
	if input.directory != "" && input.directoryInfo != nil {
		current, err := os.Lstat(input.directory)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			failures = append(failures, err)
		case !current.IsDir() || !os.SameFile(input.directoryInfo, current):
			failures = append(failures, errors.New("private embedding directory identity changed"))
		default:
			failures = append(failures, os.Remove(input.directory))
		}
	}
	return errors.Join(failures...)
}

func writeSecureEmbeddingInput(text string) (*secureEmbeddingInput, error) {
	directory, err := os.MkdirTemp("", ".rkc-embedding-input-")
	if err != nil {
		return nil, fmt.Errorf("create private embedding directory: %w", err)
	}
	directoryInfo, err := os.Lstat(directory)
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o700 {
		_ = os.Remove(directory)
		return nil, errors.New("private embedding directory is not an owner-only real directory")
	}
	path := filepath.Join(directory, "input.txt")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		_ = os.Remove(directory)
		return nil, fmt.Errorf("create private embedding input: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = file.Close()
			_ = os.Remove(path)
			_ = os.Remove(directory)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("protect private embedding input: %w", err)
	}
	if _, err := io.WriteString(file, text); err != nil {
		return nil, fmt.Errorf("write private embedding input: %w", err)
	}
	if err := file.Sync(); err != nil {
		return nil, fmt.Errorf("sync private embedding input: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() != int64(len(text)) {
		return nil, errors.New("private embedding input metadata is invalid")
	}
	committed = true
	return &secureEmbeddingInput{
		directory: directory, directoryInfo: directoryInfo, path: path, file: file, fileInfo: info,
	}, nil
}

type embeddingBoundedBuffer struct {
	mu    sync.Mutex
	limit int64
	data  bytes.Buffer
	err   error
}

func (buffer *embeddingBoundedBuffer) Write(data []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	if buffer.err != nil {
		return 0, buffer.err
	}
	remaining := buffer.limit - int64(buffer.data.Len())
	if remaining <= 0 {
		buffer.err = errEmbeddingOutputTooLarge
		return 0, buffer.err
	}
	if int64(len(data)) > remaining {
		_, _ = buffer.data.Write(data[:remaining])
		buffer.err = errEmbeddingOutputTooLarge
		return int(remaining), buffer.err
	}
	return buffer.data.Write(data)
}

func (buffer *embeddingBoundedBuffer) Bytes() []byte {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return append([]byte(nil), buffer.data.Bytes()...)
}

func (buffer *embeddingBoundedBuffer) String() string { return string(buffer.Bytes()) }

func minimum(left, right int) int {
	if left < right {
		return left
	}
	return right
}
