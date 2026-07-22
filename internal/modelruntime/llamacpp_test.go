package modelruntime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

func TestPromptMarksRepositoryDataUntrusted(t *testing.T) {
	prompt, err := BuildPrompt(Request{Task: TaskSymbolSummary, Packet: EvidencePacket{PacketID: "p", Subject: rkcmodel.Node{ID: "n", Name: "N"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range []string{"BEGIN_UNTRUSTED_REPOSITORY_DATA", "Return exactly one JSON object", "\"packet_id\":\"p\""} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("prompt missing %q: %s", required, prompt)
		}
	}
}

func TestParseResponseExtractsJSONFromNoisyCLIOutput(t *testing.T) {
	response, err := ParseResponse([]byte("loading model...\n{\"summary\":\"A {safe} summary\",\"claims\":[{\"text\":\"`N` exists.\",\"certainty\":\"supported\",\"evidence_ids\":[\"e\"]}],\"unresolved_questions\":[]}\nperf: done"))
	if err != nil {
		t.Fatal(err)
	}
	if response.Summary != "A {safe} summary" || len(response.Claims) != 1 {
		t.Fatalf("unexpected response: %+v", response)
	}
}

func TestLlamaCPPProviderRunsBoundedFakeExecutable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	root := t.TempDir()
	modelPath := filepath.Join(root, "tiny.gguf")
	if err := os.WriteFile(modelPath, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "fake-llama")
	script := `#!/bin/sh
printf '%s\n' 'diagnostic noise' '{"summary":"Login summary","claims":[{"text":"` + "`Login`" + ` exists.","category":"purpose","certainty":"supported","evidence_ids":["e1"]}],"unresolved_questions":[]}'
`
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	provider, err := NewLlamaCPPProvider(LlamaCPPConfig{Executable: executable, ModelPath: modelPath, Timeout: 5 * time.Second, Budget: Budget{MaximumRSSBytes: 512 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	packet := EvidencePacket{PacketID: "p", Task: TaskSymbolSummary, Subject: rkcmodel.Node{ID: "n", Name: "Login", QualifiedName: "auth.Login"}, Evidence: []rkcmodel.Evidence{{ID: "e1"}}, Policy: PacketPolicy{RequireCitations: true, MaximumClaims: 4}}
	response, err := provider.Generate(context.Background(), Request{RequestID: "r", Task: TaskSymbolSummary, Packet: packet, Options: InferenceOptions{ContextTokens: 128, MaxOutputTokens: 64, KVBytesPerToken: 1024, RuntimeOverheadBytes: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	if response.RequestID != "r" || response.ModelID != "tiny" || len(response.Claims) != 1 {
		t.Fatalf("unexpected response: %+v", response)
	}
	validation := ValidateResponse(packet, response, "test")
	if len(validation.Accepted) != 1 || len(validation.Rejected) != 0 {
		t.Fatalf("unexpected validation: %+v", validation)
	}
}

func TestLlamaCPPProviderRejectsMemoryEstimateBeforeExecution(t *testing.T) {
	root := t.TempDir()
	modelPath := filepath.Join(root, "large.gguf")
	if err := os.WriteFile(modelPath, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	provider, err := NewLlamaCPPProvider(LlamaCPPConfig{Executable: "does-not-matter", ModelPath: modelPath, Budget: Budget{MaximumRSSBytes: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = provider.Generate(context.Background(), Request{Task: TaskSymbolSummary, Packet: EvidencePacket{Subject: rkcmodel.Node{ID: "n"}}, Options: InferenceOptions{ContextTokens: 4096}})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected budget error, got %v", err)
	}
}
