package modelruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestBoundArtifactsRequireCanonicalDigestsAndSafeFiles(t *testing.T) {
	root := t.TempDir()
	model := filepath.Join(root, "model.gguf")
	if err := os.WriteFile(model, []byte("GGUFpayload"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest := testFileSHA256(t, model)

	for _, value := range []string{"", strings.ToUpper(digest), "abc", " " + digest} {
		if _, err := bindModel(model, value); !errors.Is(err, ErrModelArtifactIntegrity) {
			t.Fatalf("noncanonical digest %q was accepted: %v", value, err)
		}
	}
	if _, err := bindModel(model, strings.Repeat("0", 64)); !errors.Is(err, ErrModelArtifactIntegrity) || !strings.Contains(err.Error(), "mismatch") {
		t.Fatalf("incorrect model digest was accepted: %v", err)
	}

	badMagic := filepath.Join(root, "bad.gguf")
	if err := os.WriteFile(badMagic, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := bindModel(badMagic, testFileSHA256(t, badMagic)); !errors.Is(err, ErrModelArtifactIntegrity) || !strings.Contains(err.Error(), "GGUF magic") {
		t.Fatalf("non-GGUF model was accepted: %v", err)
	}

	writable := filepath.Join(root, "writable.gguf")
	if err := os.WriteFile(writable, []byte("GGUFpayload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(writable, 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := bindModel(writable, testFileSHA256(t, writable)); !errors.Is(err, ErrModelArtifactIntegrity) || !strings.Contains(err.Error(), "group/other writable") {
		t.Fatalf("group-writable model was accepted: %v", err)
	}

	symlink := filepath.Join(root, "model-link.gguf")
	if err := os.Symlink(model, symlink); err != nil {
		t.Fatal(err)
	}
	bound, err := bindModel(symlink, digest)
	if err != nil {
		t.Fatalf("canonicalizable model symlink was rejected: %v", err)
	}
	defer bound.file.Close()
	if bound.path != model {
		t.Fatalf("model path was not canonicalized: got %q want %q", bound.path, model)
	}
}

func TestBoundExecutableRequiresExecutableRegularFile(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "llama-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	nonExecutable, err := bindExecutable(path, testFileSHA256(t, path))
	if runtime.GOOS == "windows" {
		if err == nil {
			_ = nonExecutable.file.Close()
		}
	} else if !errors.Is(err, ErrModelArtifactIntegrity) || !strings.Contains(err.Error(), "execute bit") {
		t.Fatalf("non-executable runtime was accepted: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	bound, err := bindExecutable(path, testFileSHA256(t, path))
	if err != nil {
		t.Fatal(err)
	}
	if err := bound.file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bound.verify(); !errors.Is(err, ErrModelArtifactIntegrity) {
		t.Fatalf("closed executable binding verified successfully: %v", err)
	}
}

func TestArtifactHashingRechecksHigherPriorityWorkPeriodically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "artifact")
	if err := os.WriteFile(path, []byte("abcdefgh"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	checks := 0
	digest, size, err := digestOpenFileWithInterval(file, 8, 2, func() error {
		checks++
		return nil
	})
	if err != nil || size != 8 || digest != testFileSHA256(t, path) || checks < 5 {
		t.Fatalf("periodic hashing = digest %q size %d checks %d err %v", digest, size, checks, err)
	}
	blocked := errors.New("higher priority arrived")
	checks = 0
	_, size, err = digestOpenFileWithInterval(file, 8, 2, func() error {
		checks++
		if checks == 3 {
			return blocked
		}
		return nil
	})
	if !errors.Is(err, blocked) || size >= 8 || checks != 3 {
		t.Fatalf("mid-hash priority arrival was not surfaced: size %d checks %d err %v", size, checks, err)
	}
	if _, _, err := digestOpenFileWithInterval(file, 8, 0, nil); err == nil {
		t.Fatal("zero priority interval was accepted")
	}
}

func TestLlamaCPPProviderRejectsPathReplacementBeforeExecution(t *testing.T) {
	provider := testLlamaProvider(t, `printf '%s\n' '{"claims":[{"text":"ok","certainty":"supported","evidence_ids":[]}]}'`, LlamaCPPConfig{})
	defer provider.Close()
	replacement := provider.config.ModelPath + ".replacement"
	contents, err := os.ReadFile(provider.config.ModelPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, provider.config.ModelPath); err != nil {
		t.Fatal(err)
	}
	_, err = provider.Generate(context.Background(), minimalModelRequest())
	if !errors.Is(err, ErrModelArtifactIntegrity) || !strings.Contains(err.Error(), "before execution") || !strings.Contains(err.Error(), "inode changed") {
		t.Fatalf("replaced model path was not rejected before spawn: %v", err)
	}
}

func TestLlamaCPPProviderRejectsInPlaceMutationAfterExecution(t *testing.T) {
	body := `
model=
previous=
for argument in "$@"; do
  if [ "$previous" = "-m" ]; then model=$argument; fi
  previous=$argument
done
[ -n "$model" ] || exit 90
printf 'mutation' >> "$model"
printf '%s\n' '{"claims":[{"text":"ok","certainty":"supported","evidence_ids":[]}]}'
`
	provider := testLlamaProvider(t, body, LlamaCPPConfig{})
	defer provider.Close()
	_, err := provider.Generate(context.Background(), minimalModelRequest())
	if !errors.Is(err, ErrModelArtifactIntegrity) || !strings.Contains(err.Error(), "after execution") {
		t.Fatalf("model mutation was not rejected after execution: %v", err)
	}
}

func minimalModelRequest() Request {
	return Request{
		Task:   TaskSymbolSummary,
		Packet: EvidencePacket{Subject: rkcmodel.Node{ID: "n"}},
		Options: InferenceOptions{
			ContextTokens: 8, MaxOutputTokens: 1, Threads: 1, BatchSize: 1,
			KVBytesPerToken: 1, RuntimeOverheadBytes: 1,
		},
	}
}
