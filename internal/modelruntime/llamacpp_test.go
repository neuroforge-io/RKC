package modelruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/resourceguard"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
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

func TestLlamaCPPProviderRunsThroughLiveUserServiceGuard(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux user-systemd resource guard")
	}
	for _, name := range []string{"systemd-run", "systemctl", "choom", "ionice", "nice", "env"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Skipf("resource guard prerequisite unavailable: %s", name)
		}
	}
	if err := exec.Command("systemctl", "--user", "is-active", "--quiet", "default.target").Run(); err != nil {
		t.Skipf("user-systemd manager unavailable: %v", err)
	}
	root := t.TempDir()
	modelPath := filepath.Join(root, "tiny.gguf")
	modelContents := make([]byte, 1024)
	copy(modelContents, ggufMagic)
	if err := os.WriteFile(modelPath, modelContents, 0o600); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "fake-llama")
	script := `#!/bin/sh
set -eu
sleep 0.1
printf '%s\n' '{"claims":[{"text":"` + "`Login`" + ` exists.","category":"purpose","certainty":"supported","evidence_ids":["e1"]}],"unresolved_questions":[]}'
`
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	provider, err := NewLlamaCPPProvider(LlamaCPPConfig{
		Executable: executable, ModelPath: modelPath, Timeout: 10 * time.Second,
		ExpectedExecutableSHA256: testFileSHA256(t, executable),
		ExpectedModelSHA256:      testFileSHA256(t, modelPath),
		Budget:                   Budget{MaximumRSSBytes: 512 << 20},
	})
	if err != nil {
		if errors.Is(err, resourceguard.ErrHigherPriorityActive) {
			t.Skipf("ERAIS became active before live guard admission: %v", err)
		}
		t.Fatal(err)
	}
	packet := EvidencePacket{PacketID: "p", Task: TaskSymbolSummary, Subject: rkcmodel.Node{ID: "n", Name: "Login"}, Evidence: []rkcmodel.Evidence{{ID: "e1"}}, Policy: PacketPolicy{RequireCitations: true, MaximumClaims: 4}}
	response, err := provider.Generate(context.Background(), Request{RequestID: "guarded", Task: TaskSymbolSummary, Packet: packet, Options: InferenceOptions{ContextTokens: 128, MaxOutputTokens: 64, KVBytesPerToken: 1024, RuntimeOverheadBytes: 1024}})
	if err != nil {
		if errors.Is(err, resourceguard.ErrHigherPriorityActive) {
			t.Skipf("ERAIS became active during live guarded execution: %v", err)
		}
		t.Fatalf("guarded model execution: %v", err)
	}
	if response.RequestID != "guarded" || len(response.Claims) != 1 || response.Usage.PeakRSSBytes <= 0 {
		t.Fatalf("guarded response did not include measured execution: %+v", response)
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
	modelContents := make([]byte, 1024)
	copy(modelContents, ggufMagic)
	if err := os.WriteFile(modelPath, modelContents, 0o644); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "fake-llama")
	script := `#!/bin/sh
prompt_file=
previous=
for argument in "$@"; do
  if [ "$previous" = "-f" ]; then prompt_file=$argument; fi
  previous=$argument
done
case "$*" in *BEGIN_UNTRUSTED_REPOSITORY_DATA*) exit 91;; esac
[ -n "$prompt_file" ] || exit 92
grep -q RKC_MODEL_PROTOCOL "$prompt_file" || exit 93
self=$0
resolved=$(readlink "$0" 2>/dev/null || true)
[ -z "$resolved" ] || self=$resolved
printf '%s' "$prompt_file" > "$self.prompt-path"
printf '%s\n' 'diagnostic noise' '{"summary":"Login summary","claims":[{"text":"` + "`Login`" + ` exists.","category":"purpose","certainty":"supported","evidence_ids":["e1"]}],"unresolved_questions":[]}'
`
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	provider, err := NewLlamaCPPProvider(LlamaCPPConfig{
		Executable: executable, ModelPath: modelPath,
		ExpectedExecutableSHA256: testFileSHA256(t, executable),
		ExpectedModelSHA256:      testFileSHA256(t, modelPath),
		Timeout:                  5 * time.Second, Budget: Budget{MaximumRSSBytes: 512 << 20}, UnsafeDisableResourceGuard: true,
	})
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
	promptPath, err := os.ReadFile(executable + ".prompt-path")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(promptPath)); !os.IsNotExist(err) {
		t.Fatalf("private prompt file was not removed after inference: %v", err)
	}
	validation := ValidateResponse(packet, response, "test")
	if len(validation.Accepted) != 1 || len(validation.Rejected) != 0 || validation.AcceptedSummary != "" || !containsString(validation.SummaryRejectedReasons, "free-form summary lacks claim-level evidence binding") {
		t.Fatalf("unexpected validation: %+v", validation)
	}
}

func TestValidateResponseNeverPromotesUnboundSummary(t *testing.T) {
	packet := EvidencePacket{
		PacketID: "packet", Subject: rkcmodel.Node{ID: "subject", Name: "Login"},
		Evidence: []rkcmodel.Evidence{{ID: "evidence"}},
		Policy:   PacketPolicy{RequireCitations: true, MaximumClaims: 4, MaximumSummaryCharacters: 1000},
	}
	validation := ValidateResponse(packet, Response{
		ModelID: "model", Summary: "Unrelated behavior that no cited claim supports.",
		Claims: []ClaimDraft{{Text: "`Login` exists.", Certainty: "supported", EvidenceIDs: []string{"evidence"}}},
	}, "test")
	if len(validation.Accepted) != 1 {
		t.Fatalf("cited claim should be accepted: %+v", validation)
	}
	if validation.AcceptedSummary != "" {
		t.Fatalf("unbound summary was promoted: %+v", validation)
	}
	if !containsString(validation.SummaryRejectedReasons, "free-form summary lacks claim-level evidence binding") {
		t.Fatalf("missing evidence-binding rejection: %+v", validation)
	}
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestLlamaCPPProviderRejectsMemoryEstimateBeforeExecution(t *testing.T) {
	provider := testLlamaProvider(t, "exit 99", LlamaCPPConfig{Budget: Budget{MaximumRSSBytes: 1024}})
	defer provider.Close()
	_, err := provider.Generate(context.Background(), Request{Task: TaskSymbolSummary, Packet: EvidencePacket{Subject: rkcmodel.Node{ID: "n"}}, Options: InferenceOptions{ContextTokens: 4096}})
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected budget error, got %v", err)
	}
}

func testFileSHA256(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
