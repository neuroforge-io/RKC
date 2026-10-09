package modelruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func apiTestProvider(t *testing.T, protocol string, handler http.HandlerFunc) *APIProvider {
	t.Helper()
	t.Setenv("RKC_TEST_API_TOKEN", "fictional-private-token")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	profile, err := NewProviderProfile(protocol, "fictional-model", true)
	if err != nil {
		t.Fatal(err)
	}
	suffix := "/v1/chat/completions"
	if protocol == "anthropic" {
		suffix = "/v1/messages"
	}
	if protocol == "gemini" {
		suffix = "/v1beta/models/fictional-model:generateContent"
	}
	profile.Endpoint, profile.APIKeyEnv = server.URL+suffix, "RKC_TEST_API_TOKEN"
	profile.MaxOutputTokens = 64
	profile.TimeoutSeconds = 1
	provider, err := NewAPIProvider(profile, httpTestSchema)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = provider.Close() })
	return provider
}

func apiTestCompletion(protocol, content string) string {
	if protocol == "openai-compatible" {
		return httpTestCompletion(content)
	}
	if protocol == "openai" {
		return strings.Replace(httpTestCompletion(content), `"model":"fictional-model"`, `"model":"fictional-model-revision"`, 1)
	}
	var envelope map[string]any
	if protocol == "anthropic" {
		envelope = map[string]any{"type": "message", "role": "assistant", "model": "fictional-model-revision", "stop_reason": "end_turn",
			"content": []any{map[string]any{"type": "text", "text": content}}, "usage": map[string]int{"input_tokens": 100, "output_tokens": 32}}
	} else {
		envelope = map[string]any{"modelVersion": "fictional-model-revision", "candidates": []any{map[string]any{"finishReason": "STOP", "content": map[string]any{"role": "model", "parts": []any{map[string]string{"text": content}}}}},
			"usageMetadata": map[string]int{"promptTokenCount": 100, "candidatesTokenCount": 32, "totalTokenCount": 132}}
	}
	data, _ := json.Marshal(envelope)
	return string(data)
}

func TestAPIProvidersNativePayloadsCredentialsAndLogicalIdentity(t *testing.T) {
	for _, protocol := range []string{"openai-compatible", "openai", "anthropic", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			calls := 0
			provider := apiTestProvider(t, protocol, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.RawQuery != "" {
					t.Errorf("unexpected method/query: %s %s", r.Method, r.URL.RawQuery)
				}
				expectedHeader := "Authorization"
				expectedValue := "Bearer fictional-private-token"
				if protocol == "anthropic" {
					expectedHeader, expectedValue = "x-api-key", "fictional-private-token"
					if r.Header.Get("anthropic-version") != "2023-06-01" {
						t.Error("missing API version")
					}
				}
				if protocol == "gemini" {
					expectedHeader, expectedValue = "x-goog-api-key", "fictional-private-token"
				}
				if r.Header.Get(expectedHeader) != expectedValue {
					t.Error("selected credential header missing")
				}
				for _, other := range []string{"Authorization", "x-api-key", "x-goog-api-key"} {
					if other != expectedHeader && r.Header.Get(other) != "" {
						t.Errorf("credential sent in extra header %s", other)
					}
				}
				var payload map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&payload) != nil {
					t.Error("invalid payload")
				}
				if protocol == "openai" && (string(payload["max_completion_tokens"]) != "64" || payload["max_tokens"] != nil || payload["temperature"] != nil) {
					t.Error("OpenAI request used incompatible legacy/sampling fields")
				}
				if protocol == "anthropic" && (payload["output_config"] == nil || payload["messages"] == nil || string(payload["max_tokens"]) != "64") {
					t.Error("native Messages payload missing")
				}
				if protocol == "gemini" && (payload["contents"] == nil || payload["generationConfig"] == nil || payload["messages"] != nil) {
					t.Error("native GenerateContent payload missing")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, apiTestCompletion(protocol, httpTestClaims))
			})
			response, err := provider.Generate(context.Background(), httpTestRequest())
			if err != nil {
				t.Fatal(err)
			}
			if response.ModelID != "fictional-model" || response.RequestID != "fictional-request" || len(response.Claims) != 1 || calls != 1 {
				t.Fatalf("response=%+v calls=%d", response, calls)
			}
			if protocol != "openai-compatible" && response.ReportedModelID != "fictional-model-revision" {
				t.Fatalf("alias resolution was not audited: %+v", response)
			}
			if provider.Descriptor().Digest != "" || response.Usage.PeakRSSBytes != 0 || provider.ResponseSchemaSHA256() == "" {
				t.Fatal("misleading attestation or missing schema digest")
			}
			if provider.transport.Proxy != nil || provider.client.CheckRedirect == nil {
				t.Fatal("proxy/redirect policy missing")
			}
		})
	}
}

func TestAPIProvidersRejectMalformedTruncatedAndOverBudgetOutputs(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic", "gemini"} {
		for _, test := range []struct {
			name   string
			mutate func(string) string
		}{
			{"truncated", func(output string) string {
				output = strings.Replace(output, `"finish_reason":"stop"`, `"finish_reason":"length"`, 1)
				output = strings.Replace(output, `"stop_reason":"end_turn"`, `"stop_reason":"max_tokens"`, 1)
				return strings.Replace(output, `"finishReason":"STOP"`, `"finishReason":"MAX_TOKENS"`, 1)
			}},
			{"duplicate identity", func(output string) string {
				return strings.TrimSuffix(output, "}") + `,"model":"fictional-a","model":"fictional-b"}`
			}},
			{"over budget", func(output string) string {
				output = strings.ReplaceAll(output, `"completion_tokens":32`, `"completion_tokens":65`)
				output = strings.ReplaceAll(output, `"output_tokens":32`, `"output_tokens":65`)
				return strings.ReplaceAll(output, `"candidatesTokenCount":32`, `"candidatesTokenCount":65`)
			}},
		} {
			t.Run(protocol+"/"+test.name, func(t *testing.T) {
				provider := apiTestProvider(t, protocol, func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, test.mutate(apiTestCompletion(protocol, httpTestClaims)))
				})
				if _, err := provider.Generate(context.Background(), httpTestRequest()); !errors.Is(err, ErrModelOutputInvalid) {
					t.Fatalf("output accepted: %v", err)
				}
			})
		}
	}
	provider := apiTestProvider(t, "gemini", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, strings.Replace(apiTestCompletion("gemini", httpTestClaims), `"modelVersion":"fictional-model-revision",`, "", 1))
	})
	response, err := provider.Generate(context.Background(), httpTestRequest())
	if err != nil || response.ReportedModelID != "" {
		t.Fatalf("absent reported model was invented: %+v %v", response, err)
	}
}

func TestAPIProviderNetworkErrorsDoNotEchoSecretsOrRetry(t *testing.T) {
	for _, code := range []int{401, 429, 500, 307} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			calls := 0
			provider := apiTestProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/redirect-with-fictional-private-token")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "fictional-private-token")
			})
			_, err := provider.Generate(context.Background(), httpTestRequest())
			if err == nil || strings.Contains(err.Error(), "fictional-private-token") || calls != 1 {
				t.Fatalf("error=%v calls=%d", err, calls)
			}
		})
	}
	for _, test := range []struct {
		name, mime, body string
		target           error
	}{
		{"wrong MIME", "text/plain", "fictional-private-token", nil},
		{"overlarge body", "application/json", strings.Repeat("x", openAICompatibleResponseBytes+1), ErrModelOutputTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			provider := apiTestProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.mime)
				_, _ = io.WriteString(w, test.body)
			})
			_, err := provider.Generate(context.Background(), httpTestRequest())
			if err == nil || (test.target != nil && !errors.Is(err, test.target)) || strings.Contains(err.Error(), "fictional-private-token") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestAPIProviderCancellationCloseAndEarlyRejection(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	provider := apiTestProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, openAICompatibleResponseBytes))
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})
	// This cleanup runs before the helper's server.Close even when a test fails
	// before cancellation. An unread server request body must not hide a client
	// disconnect or cause the test server to wait forever.
	t.Cleanup(func() { close(release) })
	result := make(chan error, 1)
	go func() { _, err := provider.Generate(context.Background(), httpTestRequest()); result <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not start")
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("close did not cancel: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close leaked request")
	}
	if _, err := provider.Generate(context.Background(), httpTestRequest()); err == nil {
		t.Fatal("closed provider accepted request")
	}
	if _, err := provider.DiscoverModels(context.Background()); err == nil {
		t.Fatal("closed provider discovered models")
	}
	if err := provider.Close(); err != nil {
		t.Fatal(err)
	}
	var absent *APIProvider
	if absent.Close() != nil {
		t.Fatal("nil Close")
	}
	if _, err := absent.Generate(context.Background(), httpTestRequest()); err == nil {
		t.Fatal("nil provider")
	}
	if _, err := absent.BuildPrompt(PromptRequest{}); err == nil {
		t.Fatal("nil prompt provider")
	}
	if _, err := absent.DiscoverModels(context.Background()); err == nil {
		t.Fatal("nil discovery provider")
	}
	for _, mutation := range []func(*Request){func(r *Request) { r.Task = "unknown" }, func(r *Request) { r.Options.MaxOutputTokens = 65 }, func(r *Request) { r.Options.ContextTokens = -1 }} {
		request := httpTestRequest()
		mutation(&request)
		if _, err := provider.Generate(context.Background(), request); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
	if _, err := provider.Generate(nil, httpTestRequest()); err == nil {
		t.Fatal("nil context")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := provider.Generate(ctx, httpTestRequest()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestAPICredentialChecksNeverExposeValue(t *testing.T) {
	for _, value := range []string{"", "fictional-private-token\n", strings.Repeat("fictional-private-token", 500)} {
		t.Setenv("RKC_TEST_API_TOKEN", value)
		err := CheckCredential("RKC_TEST_API_TOKEN")
		if err == nil || strings.Contains(err.Error(), "fictional-private-token") {
			t.Fatalf("unsafe credential error: %v", err)
		}
	}
	if CheckCredential("INVALID=fictional-private-token") == nil {
		t.Fatal("credential name accepted a value")
	}
	if err := CheckCredential(""); err != nil {
		t.Fatal(err)
	}
}

func TestAPISchemaProjectionKeepsLocalChecksAndOwnDigest(t *testing.T) {
	schema := `{"type":"object","properties":{"claim":{"type":"string","const":"supported","maxLength":10},"items":{"type":"array","minItems":1,"maxItems":8,"uniqueItems":true}}}`
	for _, protocol := range []string{"openai", "anthropic", "gemini"} {
		profile, _ := NewProviderProfile(protocol, "fictional-model", true)
		provider, err := NewAPIProvider(profile, schema)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(provider.schema), `"const"`) || strings.Contains(string(provider.schema), `"uniqueItems"`) || provider.ResponseSchemaSHA256() == "" {
			t.Fatal(string(provider.schema))
		}
		if protocol == "anthropic" && (strings.Contains(string(provider.schema), `"maxLength"`) || strings.Contains(string(provider.schema), `"maxItems"`)) {
			t.Fatal("unsupported Anthropic constraints retained")
		}
		_ = provider.Close()
	}
	profile, _ := NewProviderProfile("openai", "fictional-model", true)
	for _, schema := range []string{"", "[]", `{"type":"string"}`, strings.Repeat("x", 65537)} {
		if _, err := NewAPIProvider(profile, schema); err == nil {
			t.Fatal("invalid schema accepted")
		}
	}
}
