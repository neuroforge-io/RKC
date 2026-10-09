package modelruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestModelDiscoveryIsMetadataOnlyAndSurfacesIncompletePages(t *testing.T) {
	for _, protocol := range []string{"openai-compatible", "openai", "anthropic", "gemini"} {
		t.Run(protocol, func(t *testing.T) {
			calls := 0
			provider := apiTestProvider(t, protocol, func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/models") || r.URL.RawQuery != "" {
					t.Errorf("not a metadata GET: %s %s", r.Method, r.URL.String())
				}
				body, _ := io.ReadAll(r.Body)
				if len(body) != 0 {
					t.Error("metadata GET carried a prompt")
				}
				w.Header().Set("Content-Type", "application/json")
				if protocol == "gemini" {
					_, _ = io.WriteString(w, fmt.Sprintf(`{"models":[{"name":"models/fictional-model","displayName":"Fictional model","supportedGenerationMethods":["generateContent"]}],"nextPageToken":%q}`, "unfollowed-page"))
				} else {
					_, _ = io.WriteString(w, `{"data":[{"id":"fictional-model","display_name":"Fictional model"}],"has_more":true}`)
				}
			})
			catalog, err := provider.DiscoverModels(context.Background())
			if err != nil || calls != 1 || !catalog.MoreAvailable || len(catalog.Models) != 1 || catalog.Models[0].ID != "fictional-model" {
				t.Fatalf("catalog=%+v calls=%d error=%v", catalog, calls, err)
			}
			if _, err := provider.DiscoverModels(nil); err == nil {
				t.Fatal("nil context accepted")
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := provider.DiscoverModels(ctx); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func TestModelDiscoveryRejectsInvalidCatalogs(t *testing.T) {
	for _, protocol := range []string{"openai", "gemini"} {
		for _, body := range []string{`{}`, `{"data":null,"models":null}`, `{"data":[{"id":"fictional\nmodel"}],"models":[{"name":"models/fictional\nmodel"}]}`} {
			provider := apiTestProvider(t, protocol, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			})
			if _, err := provider.DiscoverModels(context.Background()); !errors.Is(err, ErrModelOutputInvalid) {
				t.Fatalf("invalid catalog accepted: %v", err)
			}
		}
	}
}

func TestAPIProviderTLSUsesCertificateVerification(t *testing.T) {
	t.Setenv("RKC_TEST_API_TOKEN", "fictional-token")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, apiTestCompletion("openai", httpTestClaims))
	}))
	defer server.Close()
	profile, _ := NewProviderProfile("openai", "fictional-model", true)
	profile.Endpoint, profile.APIKeyEnv = server.URL+"/v1/chat/completions", "RKC_TEST_API_TOKEN"
	profile.TimeoutSeconds, profile.MaxOutputTokens = 1, 64
	provider, err := NewAPIProvider(profile, httpTestSchema)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if _, err := provider.Generate(context.Background(), httpTestRequest()); err == nil || strings.Contains(err.Error(), "fictional-token") {
		t.Fatalf("untrusted TLS certificate accepted or leaked token: %v", err)
	}
	provider.transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	if _, err := provider.Generate(context.Background(), httpTestRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestNativeAPIResponsesRejectUnsupportedPartsAndSchemaConstraints(t *testing.T) {
	options := httpTestRequest().Options
	for _, test := range []struct{ protocol, body string }{
		{"anthropic", `{"type":"message","role":"assistant","model":"fictional","stop_reason":"end_turn","content":[{"type":"tool_use","text":"private"}]}`},
		{"anthropic", `{"type":"message","role":"assistant","model":"fictional","stop_reason":"end_turn","content":[{"type":"text","text":"{}"}],"usage":{"input_tokens":1,"output_tokens":32,"cache_read_input_tokens":999999}}`},
		{"gemini", `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"{}","functionCall":{}}]}}]}`},
		{"gemini", `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"{}"}]}}],"promptFeedback":{"blockReason":"SAFETY"}}`},
	} {
		if test.protocol == "anthropic" {
			if _, _, _, err := decodeAnthropicCompletion([]byte(test.body), options); err == nil {
				t.Fatal("unsupported Anthropic response accepted")
			}
		} else {
			if _, _, _, err := decodeGeminiCompletion([]byte(test.body), options); err == nil {
				t.Fatal("unsupported Gemini response accepted")
			}
		}
	}
	for _, content := range []string{
		`{"claims":[],"unresolved_questions":[]}`,
		strings.Replace(httpTestClaims, `"certainty":"supported"`, `"certainty":"inferred"`, 1),
		strings.Replace(httpTestClaims, `"category":"purpose"`, `"category":"guess"`, 1),
		strings.Replace(httpTestClaims, `"fictional-evidence"`, `"fictional-evidence","fictional-evidence"`, 1),
		`{"claims":[],"unresolved_questions":["` + strings.Repeat("x", 501) + `"]}`,
	} {
		profile, _ := NewProviderProfile("openai", "fictional-model", true)
		provider, err := NewAPIProvider(profile, httpTestSchema)
		if err != nil {
			t.Fatal(err)
		}
		_, err = provider.decodeResponse([]byte(apiTestCompletion("openai", content)), options, httpTestRequest().Packet, nil)
		_ = provider.Close()
		if !errors.Is(err, ErrModelOutputInvalid) {
			t.Fatalf("schema-invalid content accepted: %v", err)
		}
	}
}
