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

const httpTestSchema = `{"type":"object","additionalProperties":false,"required":["claims","unresolved_questions"],"properties":{"claims":{"type":"array"},"unresolved_questions":{"type":"array"}}}`
const httpTestClaims = `{"claims":[{"text":"The fictional beacon uses amber light.","category":"purpose","certainty":"supported","evidence_ids":["fictional-evidence"]}],"unresolved_questions":[]}`

func httpTestRequest() Request {
	return Request{RequestID: "fictional-request", Task: TaskModuleSummary,
		Packet: EvidencePacket{PacketID: "fictional-packet", SnapshotID: "fictional-snapshot",
			Subject:                rkcmodel.Node{ID: "fictional-node", Name: "Beacon"},
			Evidence:               []rkcmodel.Evidence{{ID: "fictional-evidence"}},
			SourceExcerpts:         []SourceExcerpt{{EvidenceID: "fictional-evidence", Text: "The fictional beacon uses amber light."}},
			AllowedClaimCategories: []string{"purpose"}, Policy: PacketPolicy{RequireCitations: true}},
		Options: InferenceOptions{ContextTokens: 4096, MaxOutputTokens: 64}}
}

func httpTestCompletion(content string) string {
	body, _ := json.Marshal(map[string]any{
		"id": "fictional-completion", "object": "chat.completion", "created": 1, "model": "fictional-model",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "stop",
			"message": map[string]any{"role": "assistant", "content": content}}},
		"usage": map[string]int{"prompt_tokens": 100, "completion_tokens": 32, "total_tokens": 132},
	})
	return string(body)
}

func httpTestProvider(t *testing.T, handler http.HandlerFunc) *OpenAICompatibleProvider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	provider, err := NewOpenAICompatibleProvider(OpenAICompatibleConfig{
		Endpoint: server.URL + "/v1/chat/completions", Model: "fictional-model",
		ContextLimit: 4096, Timeout: time.Second, ResponseSchema: httpTestSchema,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider
}

func TestOpenAICompatibleEndpointAndConfigurationFailClosed(t *testing.T) {
	badEndpoints := []string{
		"", "https://api.example.test/v1/chat/completions", "http://localhost/v1/chat/completions",
		"http://192.0.2.1/v1/chat/completions", "http://2130706433/v1/chat/completions",
		"http://127.0.0.1.example.test/v1/chat/completions", "http://[::ffff:192.0.2.1]/v1/chat/completions",
		fmt.Sprintf("http://%s:%s@127.0.0.1/v1/chat/completions", "user", "synthetic"), "http://127.0.0.1/v1/chat/completions?token=synthetic",
		"http://127.0.0.1/v1/chat/completions?", "http://127.0.0.1/v1/chat/completions#synthetic",
		"http://127.0.0.1/v1/chat/completions#",
		"http://127.0.0.1/v1/%63hat/completions", "http://127.0.0.1/v1//chat/completions",
		"http://127.0.0.1/v1/../chat/completions", "http://127.0.0.1:0/v1/chat/completions",
		"http://127.0.0.1:65536/v1/chat/completions", "http://127.0.0.1:/v1/chat/completions",
		"file:///v1/chat/completions", "http://127.0.0.1/v1/models",
	}
	for _, endpoint := range badEndpoints {
		if err := validateOpenAICompatibleEndpoint(endpoint); err == nil {
			t.Errorf("accepted unsafe endpoint %q", endpoint)
		}
	}
	for _, endpoint := range []string{"http://127.0.0.1:8789/v1/chat/completions", "http://[::1]:8789/v1/chat/completions", "https://127.0.0.1/v1/chat/completions"} {
		if err := validateOpenAICompatibleEndpoint(endpoint); err != nil {
			t.Errorf("rejected loopback endpoint %q: %v", endpoint, err)
		}
	}
	base := OpenAICompatibleConfig{Endpoint: "http://127.0.0.1/v1/chat/completions", Model: "fictional-model", ContextLimit: 4096, Timeout: time.Second, ResponseSchema: httpTestSchema}
	cases := []struct {
		name   string
		change func(*OpenAICompatibleConfig)
	}{
		{"empty model", func(c *OpenAICompatibleConfig) { c.Model = "" }},
		{"model control", func(c *OpenAICompatibleConfig) { c.Model = "fictional\nmodel" }},
		{"model tab", func(c *OpenAICompatibleConfig) { c.Model = "fictional\tmodel" }},
		{"model whitespace", func(c *OpenAICompatibleConfig) { c.Model = " fictional" }},
		{"small context", func(c *OpenAICompatibleConfig) { c.ContextLimit = 511 }},
		{"large context", func(c *OpenAICompatibleConfig) { c.ContextLimit = 262145 }},
		{"no timeout", func(c *OpenAICompatibleConfig) { c.Timeout = 0 }},
		{"large timeout", func(c *OpenAICompatibleConfig) { c.Timeout = time.Hour + 1 }},
		{"missing schema", func(c *OpenAICompatibleConfig) { c.ResponseSchema = "" }},
		{"malformed schema", func(c *OpenAICompatibleConfig) { c.ResponseSchema = "{" }},
		{"array schema", func(c *OpenAICompatibleConfig) { c.ResponseSchema = "[]" }},
		{"wrong schema type", func(c *OpenAICompatibleConfig) { c.ResponseSchema = `{"type":"string"}` }},
		{"oversized schema", func(c *OpenAICompatibleConfig) { c.ResponseSchema = strings.Repeat("x", 65537) }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.change(&config)
			if _, err := NewOpenAICompatibleProvider(config); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
}

func TestOpenAICompatiblePayloadProvenanceAndValidation(t *testing.T) {
	provider := httpTestProvider(t, func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" ||
			request.Header.Get("Authorization") != "" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected credential-free request: method=%s path=%s", request.Method, request.URL.Path)
		}
		var body struct {
			Model          string              `json:"model"`
			Messages       []map[string]string `json:"messages"`
			Temperature    int                 `json:"temperature"`
			MaxTokens      int                 `json:"max_tokens"`
			Stream         bool                `json:"stream"`
			ResponseFormat struct {
				Type       string `json:"type"`
				JSONSchema struct {
					Name   string          `json:"name"`
					Strict bool            `json:"strict"`
					Schema json.RawMessage `json:"schema"`
				} `json:"json_schema"`
			} `json:"response_format"`
		}
		decoder := json.NewDecoder(request.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil {
			t.Error(err)
		}
		prompt, _ := BuildPrompt(httpTestRequest())
		if body.Model != "fictional-model" || body.MaxTokens != 64 || body.Temperature != 0 || body.Stream ||
			len(body.Messages) != 1 || body.Messages[0]["role"] != "user" || body.Messages[0]["content"] != prompt ||
			body.ResponseFormat.Type != "json_schema" || !body.ResponseFormat.JSONSchema.Strict || body.ResponseFormat.JSONSchema.Name != "rkc_claims" ||
			string(body.ResponseFormat.JSONSchema.Schema) != httpTestSchema {
			t.Errorf("unexpected bounded request: %+v", body)
		}
		writer.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = fmt.Fprint(writer, httpTestCompletion(httpTestClaims))
	})
	descriptor := provider.Descriptor()
	if descriptor.ID != "fictional-model" || descriptor.Runtime != "openai-compatible-http" ||
		descriptor.ContextLimit != 4096 || descriptor.WeightBytes != 0 || descriptor.Digest != "" {
		t.Fatalf("external runtime provenance is dishonest: %+v", descriptor)
	}
	for _, task := range []Task{TaskModuleSummary, TaskExecutionExplanation, TaskGapAnalysis, TaskSymbolSummary} {
		if !provider.Supports(task) {
			t.Errorf("missing task %s", task)
		}
	}
	if provider.Supports("unknown") {
		t.Fatal("unknown task accepted")
	}
	request := httpTestRequest()
	response, err := provider.Generate(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if response.RequestID != request.RequestID || response.ModelID != "fictional-model" || response.Usage.PromptTokens != 100 ||
		response.Usage.OutputTokens != 32 || response.Usage.PeakRSSBytes != 0 {
		t.Fatalf("unexpected response provenance/usage: %+v", response)
	}
	validated := ValidateResponse(request.Packet, response, "synthetic-http-v1")
	if len(validated.Accepted) != 1 || len(validated.Rejected) != 0 {
		t.Fatalf("valid fictional claim rejected: %+v", validated)
	}
}

func TestOpenAICompatibleRefusesRedirectsAndNeverUsesProxy(t *testing.T) {
	var proxyCalls, targetCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { proxyCalls.Add(1) }))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	provider := httpTestProvider(t, func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL+"/v1/chat/completions", http.StatusTemporaryRedirect)
	})
	if provider.transport.Proxy != nil {
		t.Fatal("proxy callback is enabled")
	}
	if _, err := provider.Generate(context.Background(), httpTestRequest()); err == nil {
		t.Fatal("redirect followed")
	}
	if proxyCalls.Load() != 0 || targetCalls.Load() != 0 {
		t.Fatal("request escaped selected endpoint")
	}
}

func TestOpenAICompatibleRejectsMalformedBoundedAndErrorResponses(t *testing.T) {
	tests := []struct {
		name, body, contentType string
		status                  int
		tooLarge                bool
	}{
		{"prose", "fictional-private-sentinel", "application/json", 200, false},
		{"wrong media", httpTestCompletion(httpTestClaims), "text/plain", 200, false},
		{"non2xx", "fictional-private-sentinel", "application/json", 500, false},
		{"oversized", strings.Repeat("x", openAICompatibleResponseBytes+1), "application/json", 200, true},
		{"trailing envelope", httpTestCompletion(httpTestClaims) + " {}", "application/json", 200, false},
		{"malformed content", httpTestCompletion(`{"claims":oops}`), "application/json", 200, false},
		{"content prose", httpTestCompletion("prefix " + httpTestClaims), "application/json", 200, false},
		{"trailing content", httpTestCompletion(httpTestClaims + " {}"), "application/json", 200, false},
		{"duplicate content key", httpTestCompletion(strings.TrimSuffix(httpTestClaims, "}") + `,"claims":[]}`), "application/json", 200, false},
		{"duplicate envelope key", strings.TrimSuffix(httpTestCompletion(httpTestClaims), "}") + `,"model":"fictional-model"}`, "application/json", 200, false},
		{"content array", httpTestCompletion("[" + httpTestClaims + "]"), "application/json", 200, false},
		{"unknown content field", httpTestCompletion(strings.TrimSuffix(httpTestClaims, "}") + `,"future":"fictional-private-sentinel"}`), "application/json", 200, false},
		{"unknown claim field", httpTestCompletion(strings.Replace(httpTestClaims, `"certainty":"supported"`, `"certainty":"supported","future":true`, 1)), "application/json", 200, false},
		{"empty", httpTestCompletion(`{"claims":[],"unresolved_questions":[]}`), "application/json", 200, false},
		{"null claims", httpTestCompletion(`{"claims":null,"unresolved_questions":["Unknown."]}`), "application/json", 200, false},
		{"wrong model", strings.Replace(httpTestCompletion(httpTestClaims), "fictional-model", "other-model", 1), "application/json", 200, false},
		{"wrong object", strings.Replace(httpTestCompletion(httpTestClaims), "chat.completion", "chat.completion.chunk", 1), "application/json", 200, false},
		{"missing choice index", strings.Replace(httpTestCompletion(httpTestClaims), `"index":0,`, "", 1), "application/json", 200, false},
		{"missing usage member", strings.Replace(httpTestCompletion(httpTestClaims), `"prompt_tokens":100,`, "", 1), "application/json", 200, false},
		{"truncated", strings.Replace(httpTestCompletion(httpTestClaims), `"finish_reason":"stop"`, `"finish_reason":"length"`, 1), "application/json", 200, false},
		{"refusal", strings.Replace(httpTestCompletion(httpTestClaims), `"role":"assistant"`, `"role":"assistant","refusal":"fictional-private-sentinel"`, 1), "application/json", 200, false},
		{"tool call", strings.Replace(httpTestCompletion(httpTestClaims), `"role":"assistant"`, `"role":"assistant","tool_calls":[]`, 1), "application/json", 200, false},
		{"usage mismatch", strings.Replace(httpTestCompletion(httpTestClaims), `"total_tokens":132`, `"total_tokens":133`, 1), "application/json", 200, false},
		{"usage over cap", strings.Replace(httpTestCompletion(httpTestClaims), `"completion_tokens":32`, `"completion_tokens":100`, 1), "application/json", 200, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := httpTestProvider(t, func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", test.contentType)
				writer.WriteHeader(test.status)
				_, _ = fmt.Fprint(writer, test.body)
			})
			_, err := provider.Generate(context.Background(), httpTestRequest())
			if err == nil || strings.Contains(err.Error(), "fictional-private-sentinel") {
				t.Fatalf("invalid output accepted or body exposed: %v", err)
			}
			if test.tooLarge && !errors.Is(err, ErrModelOutputTooLarge) {
				t.Fatalf("missing output bound: %v", err)
			}
		})
	}
}

func TestOpenAICompatibleRequestBoundsAndLifecycle(t *testing.T) {
	var calls atomic.Int32
	provider := httpTestProvider(t, func(writer http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(writer, httpTestCompletion(httpTestClaims))
	})
	cases := []struct {
		name   string
		change func(*Request)
	}{
		{"unsupported", func(r *Request) { r.Task = "unknown" }},
		{"context too large", func(r *Request) { r.Options.ContextTokens = 4097 }},
		{"negative context", func(r *Request) { r.Options.ContextTokens = -1 }},
		{"negative output", func(r *Request) { r.Options.MaxOutputTokens = -1 }},
		{"output too large", func(r *Request) { r.Options.MaxOutputTokens = 4097 }},
		{"prompt too large", func(r *Request) { r.Packet.SourceExcerpts[0].Text = strings.Repeat("x", openAICompatiblePromptBytes) }},
		{"invalid pass", func(r *Request) { r.ValidationPass = 3 }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			request := httpTestRequest()
			test.change(&request)
			if _, err := provider.Generate(context.Background(), request); err == nil {
				t.Fatal("invalid request admitted")
			}
		})
	}
	if _, err := provider.Generate(nil, httpTestRequest()); err == nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Generate(ctx, httpTestRequest()); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("invalid requests contacted server")
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Generate(context.Background(), httpTestRequest()); err == nil {
		t.Fatal("closed provider generated")
	}
	var absent *OpenAICompatibleProvider
	if err := absent.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := absent.Generate(context.Background(), httpTestRequest()); err == nil {
		t.Fatal("nil provider generated")
	}
}

func TestOpenAICompatibleTimeoutDeadlineCancellationAndClose(t *testing.T) {
	for _, mode := range []string{"timeout", "request deadline", "parent cancellation", "close"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{})
			release := make(chan struct{})
			provider := httpTestProvider(t, func(writer http.ResponseWriter, request *http.Request) {
				close(entered)
				select {
				case <-request.Context().Done():
				case <-release:
				}
			})
			defer close(release)
			provider.config.Timeout = 40 * time.Millisecond
			provider.client.Timeout = time.Second
			request := httpTestRequest()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "request deadline" {
				deadline := time.Now().Add(20 * time.Millisecond)
				request.Deadline = &deadline
			}
			result := make(chan error, 1)
			go func() { _, err := provider.Generate(ctx, request); result <- err }()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("request did not enter local mock")
			}
			if mode == "parent cancellation" {
				cancel()
			}
			if mode == "close" {
				_ = provider.Close()
			}
			select {
			case err := <-result:
				want := context.DeadlineExceeded
				if mode == "parent cancellation" || mode == "close" {
					want = context.Canceled
				}
				if !errors.Is(err, want) {
					t.Fatalf("expected %v, got %v", want, err)
				}
			case <-time.After(time.Second):
				t.Fatal("request cancellation was not bounded")
			}
		})
	}
}
