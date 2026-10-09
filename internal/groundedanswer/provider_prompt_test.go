package groundedanswer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/modelruntime"
)

type compactTestProvider struct {
	*stubProvider
	prompt             string
	mutatePromptPacket bool
	capabilities       modelruntime.ProviderCapabilities
}

func (provider *compactTestProvider) BuildPrompt(input modelruntime.PromptRequest) (string, error) {
	if provider.mutatePromptPacket {
		input.Packet.RelatedNodes[0].Name = "FORGED PROMPT BUILDER NODE"
		input.Packet.Evidence[0].ID = "invented-prompt-evidence"
	}
	return provider.prompt, nil
}

func (provider *compactTestProvider) Capabilities() modelruntime.ProviderCapabilities {
	return provider.capabilities
}

func TestProviderPromptControlsProvenanceBudgetAndRequestIdentity(t *testing.T) {
	provider := &compactTestProvider{stubProvider: testProvider(), prompt: "compact fictional prompt", mutatePromptPacket: true,
		capabilities: modelruntime.ProviderCapabilities{Profile: "fictional-extractive", MaximumPromptBytes: 2048}}
	service, err := New(provider, Options{MaximumPromptBytes: 256})
	if err != nil {
		t.Fatal(err)
	}
	request := Request{Question: "What is Alpha?", Bundle: testBundle(), Retrieval: testRetrieval()}
	result, err := service.Answer(context.Background(), request)
	if err != nil || result.Status != StatusAnswered || provider.calls != 1 {
		t.Fatalf("compact provider failed: result=%+v err=%v calls=%d", result, err, provider.calls)
	}
	if result.Provenance.PromptBytes != len(provider.prompt) || result.Provenance.PromptDigest != contentDigest([]byte(provider.prompt)) || result.Provenance.ProviderCapabilities == nil || result.Provenance.ProviderCapabilities.Profile != "fictional-extractive" {
		t.Fatalf("incorrect transmitted prompt audit: %+v", result.Provenance)
	}
	firstID := result.RequestID
	if provider.requests[0].Packet.RelatedNodes[0].Name != "Alpha" || provider.requests[0].Packet.Evidence[0].ID == "invented-prompt-evidence" {
		t.Fatal("prompt builder mutated canonical validation/generation packet")
	}
	provider.prompt = "different compact fictional prompt"
	second, err := service.Answer(context.Background(), request)
	if err != nil || second.RequestID == firstID {
		t.Fatalf("different transmitted prompt reused request identity: %v", err)
	}
	provider.prompt = strings.Repeat("x", 257)
	before := provider.calls
	tooLarge, err := service.Answer(context.Background(), request)
	if err != nil || tooLarge.Status != StatusAbstained || tooLarge.Abstention.Code != AbstentionContextBudget || provider.calls != before {
		t.Fatalf("compact prompt budget bypass: result=%+v err=%v", tooLarge, err)
	}
	provider.capabilities.Profile = "silently-changed"
	_, err = service.Answer(context.Background(), request)
	if !errors.Is(err, ErrInvalidProvider) || provider.calls != before {
		t.Fatalf("mutable capabilities admitted: err=%v", err)
	}
}

func TestNativeExtractiveHTTPRetainsCanonicalGroundingAndExactPromptAudit(t *testing.T) {
	for _, content := range []string{
		"Fictional lantern kits may be borrowed for 7 days.",
		"Fictional lantern kits may be borrowed for 999 days.",
	} {
		t.Run(content, func(t *testing.T) {
			var transmitted string
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var payload struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || len(payload.Messages) != 1 {
					t.Error("invalid synthetic request")
					return
				}
				transmitted = payload.Messages[0].Content
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"object": "chat.completion", "model": "fictional-native", "choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]string{"role": "assistant", "content": content}}}})
			}))
			defer endpoint.Close()
			provider, err := modelruntime.NewOpenAICompatibleProvider(modelruntime.OpenAICompatibleConfig{
				Endpoint: endpoint.URL + "/v1/chat/completions", Model: "fictional-native", ContextLimit: 512,
				Profile: modelruntime.ProfileNeuroForgeNativeExtractive, Timeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			defer provider.Close()
			service, err := New(provider, Options{})
			if err != nil {
				t.Fatal(err)
			}
			bundle := syntheticMarkdownBundle(t, fictionalHandbook)
			request := fictionalGroundingRequest(bundle, bundle.Artifacts[0].ID)
			request.Inference = modelruntime.InferenceOptions{ContextTokens: 512, MaxOutputTokens: 128}
			result, err := service.Answer(context.Background(), request)
			if strings.Contains(content, "999") {
				if err == nil || len(result.Claims) != 0 {
					t.Fatalf("ungrounded native response published: %+v %v", result, err)
				}
				return
			}
			if err != nil || result.Status != StatusAnswered || len(result.Claims) != 1 || len(result.Citations) != 1 {
				t.Fatalf("extractive canonical answer failed: %+v %v", result, err)
			}
			if result.Provenance.PromptDigest != contentDigest([]byte(transmitted)) || result.Provenance.PromptBytes != len(transmitted) || len(transmitted) > 2048 {
				t.Fatalf("native prompt audit mismatch: %+v", result.Provenance)
			}
			if result.Provenance.ProviderCapabilities == nil || result.Provenance.ProviderCapabilities.JSONSchemaOutput || result.Provenance.ProviderCapabilities.Profile != string(modelruntime.ProfileNeuroForgeNativeExtractive) {
				t.Fatalf("native capability provenance missing: %+v", result.Provenance)
			}
			if result.Citations[0].Source == nil || result.Citations[0].Source.Path != "handbook.md" || result.Claims[0].Text != content {
				t.Fatalf("canonical source binding lost: %+v", result)
			}
		})
	}
}
