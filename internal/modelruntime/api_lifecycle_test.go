package modelruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAPIExtractiveProfileUsesCredentialAndExactQuotation(t *testing.T) {
	t.Setenv("RKC_TEST_API_TOKEN", "fictional-private-token")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer fictional-private-token" {
			t.Error("missing named bearer credential")
		}
		var payload map[string]json.RawMessage
		if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload) != 5 || payload["response_format"] != nil || payload["temperature"] != nil || string(payload["n"]) != "1" {
			t.Errorf("native subset changed: %+v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, httpTestCompletion(nativeFictionalRule))
	}))
	defer server.Close()
	profile, _ := NewProviderProfile("neuroforge", "fictional-model", true)
	profile.Endpoint, profile.APIKeyEnv = server.URL+"/v1/chat/completions", "RKC_TEST_API_TOKEN"
	provider, err := NewAPIProvider(profile, "")
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	if provider.Capabilities().JSONSchemaOutput || provider.ResponseSchemaSHA256() != "" {
		t.Fatal("extractive profile claimed schema enforcement")
	}
	request := nativeTestRequest()
	request.Options = InferenceOptions{} // Profile bounds supply both defaults.
	response, err := provider.Generate(context.Background(), request)
	if err != nil || len(response.Claims) != 1 || response.Claims[0].Text != nativeFictionalRule || calls != 1 {
		t.Fatalf("response=%+v calls=%d err=%v", response, calls, err)
	}
	request.Packet.SourceExcerpts = nil
	if _, err := provider.Generate(context.Background(), request); err == nil || calls != 1 {
		t.Fatalf("missing evidence caused API invocation: %v calls=%d", err, calls)
	}
}

func TestAPIMetadataRequestIsCancelledAndBusyRequestsFailClosed(t *testing.T) {
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
	t.Cleanup(func() { close(release) })
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := provider.DiscoverModels(ctx); result <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("metadata request did not start")
	}
	if _, err := provider.DiscoverModels(context.Background()); err == nil {
		t.Fatal("concurrent metadata request admitted")
	}
	if _, err := provider.Generate(context.Background(), httpTestRequest()); err == nil {
		t.Fatal("concurrent generation request admitted")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("metadata cancellation lost: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("metadata request leaked after cancellation")
	}
}

func TestAPIRequestDeadlineAndMissingCredentialPreventNetwork(t *testing.T) {
	calls := 0
	provider := apiTestProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, apiTestCompletion("openai", httpTestClaims))
	})
	request := httpTestRequest()
	deadline := time.Now().Add(-time.Second)
	request.Deadline = &deadline
	if _, err := provider.Generate(context.Background(), request); !errors.Is(err, context.DeadlineExceeded) || calls != 0 {
		t.Fatalf("expired request invoked endpoint: %v calls=%d", err, calls)
	}
	request.Deadline = nil
	t.Setenv("RKC_TEST_API_TOKEN", "")
	if _, err := provider.Generate(context.Background(), request); err == nil || !strings.Contains(err.Error(), "RKC_TEST_API_TOKEN") || calls != 0 {
		t.Fatalf("missing credential invoked endpoint: %v calls=%d", err, calls)
	}
	if _, err := provider.transport.DialContext(context.Background(), "tcp", "192.0.2.1:80"); err == nil {
		t.Fatal("local transport dialed outside loopback")
	}
	request.Packet.SourceExcerpts[0].Text = strings.Repeat("x", openAICompatiblePromptBytes)
	if _, err := provider.Generate(context.Background(), request); err == nil || calls != 0 {
		t.Fatalf("overlarge prompt invoked endpoint: %v calls=%d", err, calls)
	}
}
