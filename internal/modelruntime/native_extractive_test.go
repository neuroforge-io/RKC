package modelruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const nativeFictionalRule = "Fictional lantern kits may be borrowed for 7 days."

func nativeTestRequest() Request {
	source := rkcmodel.SourceRange{ArtifactID: "fictional-artifact", Path: "handbook.md", StartLine: 1, EndLine: 2, Anchor: "lending"}
	return Request{RequestID: "fictional-native-request", Task: TaskModuleSummary,
		Packet: EvidencePacket{PacketID: "fictional-native-packet", SnapshotID: "fictional-native-snapshot",
			Subject: rkcmodel.Node{ID: "fictional-question", Attributes: map[string]any{"question": "How long may fictional lantern kits be borrowed?"}},
			RelatedNodes: []rkcmodel.Node{{ID: "fictional-section", Kind: "document_section", Name: "Lending", ArtifactID: source.ArtifactID,
				Source: &source, EvidenceIDs: []string{"fictional-evidence"}}},
			Evidence: []rkcmodel.Evidence{{ID: "fictional-evidence", Kind: "documentation_asserted", Method: "markdown.heading",
				Tool: "rkc.markdown", Source: &source, InputDigest: strings.Repeat("a", 64)}},
			SourceExcerpts:         []SourceExcerpt{{EvidenceID: "fictional-evidence", Source: source, Text: "# Lending\n" + nativeFictionalRule}},
			AllowedClaimCategories: []string{"constraint"}, Policy: PacketPolicy{RequireCitations: true}},
		Options: InferenceOptions{ContextTokens: 512, MaxOutputTokens: 128}}
}

func nativeTestProvider(t *testing.T, handler http.HandlerFunc) *OpenAICompatibleProvider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := NewOpenAICompatibleProvider(OpenAICompatibleConfig{Endpoint: server.URL + "/v1/chat/completions",
		Model: "fictional-model", ContextLimit: 512, Timeout: time.Second, Profile: ProfileNeuroForgeNativeExtractive})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider
}

func TestNativeExtractiveCapabilitiesAndExactPlainTextWorkflow(t *testing.T) {
	request := nativeTestRequest()
	var sentPrompt string
	provider := nativeTestProvider(t, func(writer http.ResponseWriter, incoming *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(incoming.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		for _, name := range []string{"temperature", "response_format", "system"} {
			if _, present := body[name]; present {
				t.Errorf("unsupported native field %q sent", name)
			}
		}
		if len(body) != 5 || string(body["model"]) != `"fictional-model"` || string(body["max_tokens"]) != "128" ||
			string(body["stream"]) != "false" || string(body["n"]) != "1" || incoming.Header.Get("Authorization") != "" {
			t.Errorf("native request shape differs: %+v", body)
		}
		var messages []map[string]string
		if err := json.Unmarshal(body["messages"], &messages); err != nil || len(messages) != 1 || messages[0]["role"] != "user" {
			t.Errorf("native messages invalid: %+v %v", messages, err)
			return
		}
		sentPrompt = messages[0]["content"]
		if len(sentPrompt) > 2048 || !strings.Contains(sentPrompt, nativeFictionalRule) || strings.Contains(sentPrompt, "# Lending") {
			t.Errorf("native prompt not compact/extractive: %q", sentPrompt)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, httpTestCompletion(nativeFictionalRule))
	})
	caps := provider.Capabilities()
	if caps.Profile != string(ProfileNeuroForgeNativeExtractive) || caps.JSONSchemaOutput || caps.TemperatureControl || caps.SystemPrompt ||
		caps.MaximumPromptBytes != 2048 || caps.MaximumOutputTokens != 128 || caps.MaximumContextTokens != 512 {
		t.Fatalf("native capabilities are dishonest: %+v", caps)
	}
	if provider.Descriptor().RuntimeRevision != nativeExtractiveProtocol || provider.Descriptor().WeightBytes != 0 {
		t.Fatal("native protocol provenance missing")
	}
	prompt, err := BuildProviderPrompt(provider, request)
	if err != nil {
		t.Fatal(err)
	}
	response, err := provider.Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if sentPrompt != prompt || len(response.Claims) != 1 || response.Claims[0].Text != nativeFictionalRule ||
		response.Claims[0].Category != "constraint" || strings.Join(response.Claims[0].EvidenceIDs, ",") != "fictional-evidence" ||
		response.RequestID != request.RequestID || response.ModelID != "fictional-model" || response.Usage.PeakRSSBytes != 0 {
		t.Fatalf("native extraction/prompt provenance failed: %+v", response)
	}
	if validated := ValidateResponse(request.Packet, response, nativeExtractiveProtocol); len(validated.Accepted) != 1 || len(validated.Rejected) != 0 {
		t.Fatalf("canonical quote rejected: %+v", validated)
	}
}

func TestNativeExtractiveRejectsInventedMalformedAndProtocolFallbackOutput(t *testing.T) {
	for _, text := range []string{
		"Fictional lantern kits may be borrowed for 99 days.", "Answer: " + nativeFictionalRule,
		nativeFictionalRule + " Additional unsupported text.", nativeFictionalRule + "\n",
		" " + nativeFictionalRule, "```\n" + nativeFictionalRule + "\n```", `{"claims":[]}`, httpTestClaims,
		"fictional-evidence: " + nativeFictionalRule, strings.TrimSuffix(nativeFictionalRule, "."),
	} {
		t.Run(text, func(t *testing.T) {
			provider := nativeTestProvider(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(writer, httpTestCompletion(text))
			})
			if _, err := provider.Generate(context.Background(), nativeTestRequest()); !errors.Is(err, ErrModelOutputInvalid) {
				t.Fatalf("unmatched text admitted: %v", err)
			}
		})
	}
	provider := httpTestProvider(t, func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, httpTestCompletion(nativeFictionalRule))
	})
	if caps := provider.Capabilities(); caps.Profile != string(ProfileStructuredClaims) ||
		!caps.JSONSchemaOutput || !caps.TemperatureControl || caps.SystemPrompt ||
		caps.ResponseProtocol != structuredClaimsProtocol {
		t.Fatalf("default structured controls are inaccurate: %+v", caps)
	}
	if _, err := provider.Generate(context.Background(), nativeTestRequest()); !errors.Is(err, ErrModelOutputInvalid) {
		t.Fatalf("structured mode silently fell back to plain text: %v", err)
	}
}

func TestNativeExtractiveRejectsReportedUsageBeyondTotalContext(t *testing.T) {
	provider := nativeTestProvider(t, func(writer http.ResponseWriter, _ *http.Request) {
		var envelope map[string]any
		if err := json.Unmarshal([]byte(httpTestCompletion(nativeFictionalRule)), &envelope); err != nil {
			t.Error(err)
			return
		}
		// Both individual counts fit their limits, but the complete sequence
		// exceeds the deliberately configured 512-token context window.
		envelope["usage"] = map[string]int{"prompt_tokens": 512, "completion_tokens": 128, "total_tokens": 640}
		writer.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(writer).Encode(envelope); err != nil {
			t.Error(err)
		}
	})
	if _, err := provider.Generate(context.Background(), nativeTestRequest()); !errors.Is(err, ErrModelOutputInvalid) {
		t.Fatalf("reported usage exceeds complete context window: %v", err)
	}
}

func TestNativeExtractiveRejectsAmbiguousMismatchedAndUnselectedEvidenceBeforeHTTP(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Request)
	}{
		{"duplicate evidence ID", func(r *Request) { r.Packet.Evidence = append(r.Packet.Evidence, r.Packet.Evidence[0]) }},
		{"duplicate excerpt", func(r *Request) {
			r.Packet.SourceExcerpts = append(r.Packet.SourceExcerpts, r.Packet.SourceExcerpts[0])
		}},
		{"duplicate owner ID", func(r *Request) { r.Packet.RelatedNodes = append(r.Packet.RelatedNodes, r.Packet.RelatedNodes[0]) }},
		{"ambiguous owners", func(r *Request) {
			n := r.Packet.RelatedNodes[0]
			n.ID = "another-owner"
			r.Packet.RelatedNodes = append(r.Packet.RelatedNodes, n)
		}},
		{"duplicate ownership citation", func(r *Request) {
			r.Packet.RelatedNodes[0].EvidenceIDs = []string{"fictional-evidence", "fictional-evidence"}
		}},
		{"missing selected owner", func(r *Request) { r.Packet.RelatedNodes = nil }},
		{"wrong source path", func(r *Request) { r.Packet.SourceExcerpts[0].Source.Path = "other.md" }},
		{"unknown evidence", func(r *Request) { r.Packet.SourceExcerpts[0].EvidenceID = "unknown" }},
		{"quoted evidence ID", func(r *Request) { r.Packet.Evidence[0].ID = `fictional"evidence` }},
		{"wrong producer", func(r *Request) { r.Packet.Evidence[0].Tool = "fictional-model" }},
		{"wrong evidence kind", func(r *Request) { r.Packet.Evidence[0].Kind = "model_inferred" }},
		{"invalid input digest", func(r *Request) { r.Packet.Evidence[0].InputDigest = "not-a-digest" }},
		{"category unavailable", func(r *Request) { r.Packet.AllowedClaimCategories = []string{"purpose"} }},
		{"duplicate candidate text", func(r *Request) { r.Packet.SourceExcerpts[0].Text += "\n" + nativeFictionalRule }},
		{"compound source", func(r *Request) {
			r.Packet.SourceExcerpts[0].Text = "A fictional rule exists. Another fictional rule exists."
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			provider := nativeTestProvider(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
			request := nativeTestRequest()
			test.mutate(&request)
			if _, err := provider.Generate(context.Background(), request); err == nil || calls.Load() != 0 {
				t.Fatalf("invalid evidence contacted HTTP or was admitted: %v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestNativeExtractiveMissingTruncatedAndBoundedEvidence(t *testing.T) {
	for _, missing := range []string{"missing excerpts", "truncated excerpt", "heading only"} {
		t.Run(missing, func(t *testing.T) {
			var calls atomic.Int32
			provider := nativeTestProvider(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
			request := nativeTestRequest()
			switch missing {
			case "missing excerpts":
				request.Packet.SourceExcerpts = nil
			case "truncated excerpt":
				request.Packet.SourceExcerpts[0].Truncated = true
			case "heading only":
				request.Packet.SourceExcerpts[0].Text = "# Lending"
			}
			prompt, err := BuildProviderPrompt(provider, request)
			if err != nil || len(prompt) > 2048 || !strings.Contains(prompt, `"candidates":[]`) {
				t.Fatalf("bounded abstention prompt failed: %q %v", prompt, err)
			}
			if _, err := provider.Generate(context.Background(), request); !errors.Is(err, ErrNoExtractiveEvidence) || calls.Load() != 0 {
				t.Fatalf("missing evidence made HTTP request: %v", err)
			}
		})
	}
	for _, bound := range []string{"prompt bytes", "output tokens", "context tokens", "question missing", "validation pass"} {
		t.Run(bound, func(t *testing.T) {
			var calls atomic.Int32
			provider := nativeTestProvider(t, func(http.ResponseWriter, *http.Request) { calls.Add(1) })
			request := nativeTestRequest()
			switch bound {
			case "prompt bytes":
				request.Packet.Subject.Attributes["question"] = strings.Repeat("界", 700)
			case "output tokens":
				request.Options.MaxOutputTokens = 129
			case "context tokens":
				request.Options.ContextTokens = 513
			case "question missing":
				delete(request.Packet.Subject.Attributes, "question")
			case "validation pass":
				request.ValidationPass = 3
			}
			if _, err := provider.Generate(context.Background(), request); err == nil || calls.Load() != 0 {
				t.Fatalf("native bound not enforced: %v", err)
			}
		})
	}
	config := OpenAICompatibleConfig{Endpoint: "http://127.0.0.1/v1/chat/completions", Model: "fictional-model", ContextLimit: 4096, Timeout: time.Second, Profile: ProfileNeuroForgeNativeExtractive}
	if _, err := NewOpenAICompatibleProvider(config); err == nil {
		t.Fatal("native context silently clamped")
	}
	config.ContextLimit = 512
	config.Profile = "unknown"
	if _, err := NewOpenAICompatibleProvider(config); err == nil {
		t.Fatal("unknown profile accepted")
	}
	provider := testGenericPromptProvider{}
	want, _ := BuildPrompt(nativeTestRequest())
	got, err := BuildProviderPrompt(provider, nativeTestRequest())
	if err != nil || want != got {
		t.Fatal("generic provider prompt compatibility changed")
	}
}

type testGenericPromptProvider struct{}

func (testGenericPromptProvider) Descriptor() ModelDescriptor {
	return ModelDescriptor{ID: "fictional-generic"}
}
func (testGenericPromptProvider) Supports(Task) bool { return true }
func (testGenericPromptProvider) Generate(context.Context, Request) (Response, error) {
	return Response{}, nil
}
func (testGenericPromptProvider) Close() error { return nil }
