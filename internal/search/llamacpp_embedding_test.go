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
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestLlamaCPPEmbedderRunsOneSecureInputPerProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	executable, model := embeddingFixture(t, `
input=
previous=
for argument in "$@"; do
  if [ "$previous" = "--file" ]; then input=$argument; fi
  previous=$argument
done
[ -n "$input" ] || exit 91
[ "$(stat -Lc %a "$input")" = 600 ] || exit 92
for required in "--pooling last" "--embd-normalize 2" "--embd-output-format json" "--device none" "--n-gpu-layers 0"; do
  case " $* " in *" $required "*) ;; *) exit 93 ;; esac
done
[ -z "${RKC_SECRET_TEST:-}" ] || exit 94
[ "${CUDA_VISIBLE_DEVICES:-}" = -1 ] || exit 95
state=$(readlink -f "$0")
printf '%s\n' "$*" >> "$state.args"
cat "$input" >> "$state.inputs"
printf '\036' >> "$state.inputs"
printf '%s\n' '{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.6,0.8]}]}'
`)
	t.Setenv("RKC_SECRET_TEST", "must-not-leak")
	digest := sha256.Sum256([]byte("GGUFtiny deterministic gguf"))
	embedder, err := NewLlamaCPPEmbedder(LlamaCPPEmbeddingConfig{
		Executable: executable, ModelPath: model, ModelID: "qwen-test",
		ExpectedModelSHA256: hex.EncodeToString(digest[:]), Dimensions: 2,
		Timeout: 2 * time.Second, MaximumRSSBytes: 128 << 20, UnsafeDisableResourceGuard: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = embedder.Close() })
	wantDescriptor := EmbeddingDescriptor{
		Provider: "llama.cpp-embedding", Model: "qwen-test", Digest: "sha256:" + hex.EncodeToString(digest[:]), Dimensions: 2,
	}
	if descriptor := embedder.Descriptor(); !reflect.DeepEqual(descriptor, wantDescriptor) {
		t.Fatalf("descriptor = %+v", descriptor)
	}
	texts := []string{"alpha\nbeta --device cuda", "second text"}
	vectors, err := embedder.Embed(context.Background(), texts)
	if err != nil || !reflect.DeepEqual(vectors, [][]float32{{0.6, 0.8}, {0.6, 0.8}}) {
		t.Fatalf("Embed = %v, %v", vectors, err)
	}
	arguments, err := os.ReadFile(executable + ".args")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(arguments)), "\n")
	if len(lines) != 2 || strings.Contains(string(arguments), "alpha") || strings.Contains(string(arguments), "cuda --device") {
		t.Fatalf("inputs leaked into arguments or calls were batched: %q", arguments)
	}
	inputs, err := os.ReadFile(executable + ".inputs")
	if err != nil || string(inputs) != strings.Join(texts, "\x1e")+"\x1e" {
		t.Fatalf("private inputs = %q, %v", inputs, err)
	}
	for _, line := range lines {
		fields := strings.Fields(line)
		for index, field := range fields {
			if field == "--file" && index+1 < len(fields) {
				if _, err := os.Stat(fields[index+1]); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("private input was not removed: %s: %v", fields[index+1], err)
				}
			}
		}
	}
}

func TestLlamaCPPEmbedderLearnsAndEnforcesDimensions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	executable, model := embeddingFixture(t, `
state=$(readlink -f "$0")
count=0
[ -f "$state.count" ] && count=$(cat "$state.count")
count=$((count + 1))
printf '%s' "$count" > "$state.count"
if [ "$count" -eq 1 ]; then
  printf '%s\n' '{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,0]}]}'
else
  printf '%s\n' '{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,0,0]}]}'
fi
`)
	embedder, err := NewLlamaCPPEmbedder(LlamaCPPEmbeddingConfig{
		Executable: executable, ModelPath: model, MaximumRSSBytes: 128 << 20, UnsafeDisableResourceGuard: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = embedder.Close() })
	if _, err := embedder.Embed(context.Background(), []string{"first"}); err != nil || embedder.Descriptor().Dimensions != 2 {
		t.Fatalf("first embedding = descriptor %+v, %v", embedder.Descriptor(), err)
	}
	if _, err := embedder.Embed(context.Background(), []string{"second"}); err == nil || !strings.Contains(err.Error(), "does not match bound dimension") {
		t.Fatalf("dimension drift = %v", err)
	}
}

func TestLlamaCPPEmbedderInputAndConfigurationValidation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	executable, model := embeddingFixture(t, `printf '%s\n' '{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,0]}]}'`)
	base := LlamaCPPEmbeddingConfig{
		Executable: executable, ModelPath: model, MaximumRSSBytes: 128 << 20, UnsafeDisableResourceGuard: true,
	}
	for name, mutate := range map[string]func(*LlamaCPPEmbeddingConfig){
		"missing model":      func(config *LlamaCPPEmbeddingConfig) { config.ModelPath = "" },
		"missing executable": func(config *LlamaCPPEmbeddingConfig) { config.Executable = filepath.Join(t.TempDir(), "missing") },
		"digest mismatch":    func(config *LlamaCPPEmbeddingConfig) { config.ExpectedModelSHA256 = strings.Repeat("0", 64) },
		"dimensions":         func(config *LlamaCPPEmbeddingConfig) { config.Dimensions = 65537 },
		"context":            func(config *LlamaCPPEmbeddingConfig) { config.ContextTokens = 262145 },
		"threads":            func(config *LlamaCPPEmbeddingConfig) { config.Threads = 65 },
		"input count":        func(config *LlamaCPPEmbeddingConfig) { config.MaximumInputs = 257 },
		"input bytes":        func(config *LlamaCPPEmbeddingConfig) { config.MaximumInputBytes = 4*1024*1024 + 1 },
		"stdout":             func(config *LlamaCPPEmbeddingConfig) { config.MaximumStdoutBytes = 256*1024*1024 + 1 },
		"stderr":             func(config *LlamaCPPEmbeddingConfig) { config.MaximumStderrBytes = 16*1024*1024 + 1 },
		"RSS":                func(config *LlamaCPPEmbeddingConfig) { config.MaximumRSSBytes = 1 },
		"bad GGUF": func(config *LlamaCPPEmbeddingConfig) {
			path := filepath.Join(t.TempDir(), "bad.gguf")
			if err := os.WriteFile(path, []byte("not-a-model"), 0o600); err != nil {
				t.Fatal(err)
			}
			config.ModelPath = path
		},
		"writable model": func(config *LlamaCPPEmbeddingConfig) {
			path := filepath.Join(t.TempDir(), "writable.gguf")
			if err := os.WriteFile(path, []byte("GGUFmodel"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o622); err != nil {
				t.Fatal(err)
			}
			config.ModelPath = path
		},
		"model exceeds RSS": func(config *LlamaCPPEmbeddingConfig) {
			config.MaximumRSSBytes = 64 << 20
			config.ModelPath = largeSparseModel(t, 65<<20)
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := base
			mutate(&config)
			if _, err := NewLlamaCPPEmbedder(config); err == nil {
				t.Fatalf("accepted invalid %s config", name)
			}
		})
	}

	embedder, err := NewLlamaCPPEmbedder(LlamaCPPEmbeddingConfig{
		Executable: executable, ModelPath: model, MaximumInputs: 1, MaximumInputBytes: 4,
		MaximumRSSBytes: 128 << 20, UnsafeDisableResourceGuard: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = embedder.Close() })
	for name, texts := range map[string][]string{
		"empty": {""}, "too many": {"one", "two"}, "too large": {"12345"}, "NUL": {"a\x00b"}, "invalid UTF-8": {string([]byte{0xff})},
	} {
		if _, err := embedder.Embed(context.Background(), texts); err == nil {
			t.Errorf("accepted %s input", name)
		}
	}
	if vectors, err := embedder.Embed(context.Background(), nil); err != nil || vectors != nil {
		t.Fatalf("empty batch = %v, %v", vectors, err)
	}
	if _, err := (*LlamaCPPEmbedder)(nil).Embed(context.Background(), []string{"x"}); err == nil {
		t.Fatal("nil embedder was accepted")
	}
	if _, err := embedder.Embed(nil, []string{"x"}); err == nil {
		t.Fatal("nil context was accepted")
	}
	if err := (*LlamaCPPEmbedder)(nil).Close(); err != nil {
		t.Fatalf("nil close = %v", err)
	}
	if err := embedder.Close(); err != nil {
		t.Fatalf("close = %v", err)
	}
	if descriptor := embedder.Descriptor(); descriptor != (EmbeddingDescriptor{}) {
		t.Fatalf("closed descriptor = %+v", descriptor)
	}
	if _, err := embedder.Embed(context.Background(), []string{"x"}); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("closed embedder = %v", err)
	}
	if err := embedder.Close(); err != nil {
		t.Fatalf("second close = %v", err)
	}
}

func TestLlamaCPPEmbedderBoundsCancellationAndIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	t.Run("stdout bound", func(t *testing.T) {
		executable, model := embeddingFixture(t, `head -c 128 /dev/zero | tr '\000' x`)
		embedder, err := NewLlamaCPPEmbedder(LlamaCPPEmbeddingConfig{
			Executable: executable, ModelPath: model, MaximumStdoutBytes: 8, MaximumRSSBytes: 128 << 20, UnsafeDisableResourceGuard: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = embedder.Close() })
		if _, err := embedder.Embed(context.Background(), []string{"text"}); !errors.Is(err, errEmbeddingOutputTooLarge) {
			t.Fatalf("stdout limit = %v", err)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		executable, model := embeddingFixture(t, `sleep 10`)
		embedder, err := NewLlamaCPPEmbedder(LlamaCPPEmbeddingConfig{
			Executable: executable, ModelPath: model, Timeout: 30 * time.Millisecond, MaximumRSSBytes: 128 << 20, UnsafeDisableResourceGuard: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = embedder.Close() })
		if _, err := embedder.Embed(context.Background(), []string{"text"}); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("timeout = %v", err)
		}
	})
	t.Run("model mutation", func(t *testing.T) {
		executable, model := embeddingFixture(t, `printf '%s\n' '{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,0]}]}'`)
		embedder, err := NewLlamaCPPEmbedder(LlamaCPPEmbeddingConfig{
			Executable: executable, ModelPath: model, MaximumRSSBytes: 128 << 20, UnsafeDisableResourceGuard: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = embedder.Close() })
		if err := os.WriteFile(model, []byte("mutated model"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := embedder.Embed(context.Background(), []string{"text"}); err == nil || !strings.Contains(err.Error(), "digest changed") {
			t.Fatalf("model mutation = %v", err)
		}
	})
}

func TestParseLlamaEmbeddingStrictProtocol(t *testing.T) {
	t.Parallel()

	valid := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.6,0.8]}]}`
	if vector, err := parseLlamaEmbedding([]byte(valid)); err != nil || !reflect.DeepEqual(vector, []float32{0.6, 0.8}) {
		t.Fatalf("valid output = %v, %v", vector, err)
	}
	for name, output := range map[string]string{
		"malformed":        `{`,
		"unknown":          `{"object":"list","extra":true,"data":[]}`,
		"trailing":         valid + `{}`,
		"wrong object":     `{"object":"other","data":[{"object":"embedding","index":0,"embedding":[1]}]}`,
		"wrong index":      `{"object":"list","data":[{"object":"embedding","index":1,"embedding":[1]}]}`,
		"multiple":         `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1]},{"object":"embedding","index":1,"embedding":[1]}]}`,
		"empty":            `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[]}]}`,
		"zero":             `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0,0]}]}`,
		"not normalized":   `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1,1]}]}`,
		"numeric overflow": `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[1e1000]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseLlamaEmbedding([]byte(output)); err == nil {
				t.Fatalf("accepted %s output", name)
			}
		})
	}
	if err := validateLlamaEmbedding([]float32{float32(math.Inf(1))}); err == nil {
		t.Fatal("accepted non-finite vector")
	}
}

func TestEmbeddingBufferStringAndClosedBinding(t *testing.T) {
	t.Parallel()
	buffer := &embeddingBoundedBuffer{limit: 8}
	if written, err := buffer.Write([]byte("bound")); err != nil || written != 5 {
		t.Fatalf("buffer write = %d, %v", written, err)
	}
	if got := buffer.String(); got != "bound" {
		t.Fatalf("buffer string = %q", got)
	}
	if got := (&boundRegularFile{}).referencePath(); got != "" {
		t.Fatalf("closed bound-file reference = %q", got)
	}
}

func embeddingFixture(t *testing.T, body string) (string, string) {
	t.Helper()
	root := t.TempDir()
	executable := filepath.Join(root, "llama-embedding")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	model := filepath.Join(root, "model.gguf")
	if err := os.WriteFile(model, []byte("GGUFtiny deterministic gguf"), 0o600); err != nil {
		t.Fatal(err)
	}
	return executable, model
}

func largeSparseModel(t *testing.T, size int64) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "large.gguf")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(size); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("GGUF"), 0); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}
