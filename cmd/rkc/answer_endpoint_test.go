package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/groundedanswer"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/server"
)

func TestAnswerEndpointUsesCanonicalGrounding(t *testing.T) {
	for _, test := range []struct {
		name, evidence string
		answered       bool
	}{
		{"known citation", "e-alpha", true},
		{"invented citation", "e-not-in-the-atlas", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/v1/chat/completions" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
					t.Errorf("unexpected request route or authentication")
				}
				var payload struct {
					Model string `json:"model"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Model != "fictional-test-model" {
					t.Errorf("model selection: %+v, %v", payload, err)
				}
				content, _ := json.Marshal(map[string]any{
					"claims":               []map[string]any{{"text": "Alpha is the entry point.", "category": "purpose", "certainty": "supported", "evidence_ids": []string{test.evidence}}},
					"unresolved_questions": []string{},
				})
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"object": "chat.completion", "model": "fictional-test-model", "choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": string(content)}}}})
			}))
			defer endpoint.Close()
			bundle := cliAnswerBundle()
			var output bytes.Buffer
			err := runAnswerContext(context.Background(), []string{
				"--provider", "openai-compatible", "--endpoint", endpoint.URL + "/v1/chat/completions",
				"--model-name", "fictional-test-model", "--json", "What is Alpha?",
			}, answerDependencies{
				loadDataset: func(string) (*server.Dataset, error) {
					return &server.Dataset{Bundle: bundle, Search: search.BuildFromBundle(bundle), Graph: graph.Build(bundle.Nodes, bundle.Edges)}, nil
				},
				openProvider: openAnswerGenerationProvider, stdout: &output, now: time.Now,
			})
			if err != nil {
				t.Fatal(err)
			}
			var result groundedanswer.Result
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if (result.Status == groundedanswer.StatusAnswered) != test.answered || calls < 1 || calls > 3 {
				t.Fatalf("status=%s calls=%d claims=%+v", result.Status, calls, result.Claims)
			}
			if test.answered && (len(result.Citations) != 1 || result.Citations[0].EvidenceID != "e-alpha") {
				t.Fatalf("citations=%+v", result.Citations)
			}
			if !test.answered && len(result.Claims) != 0 {
				t.Fatalf("invented citation published: %+v", result.Claims)
			}
			if result.Provenance.Provider.Runtime != "openai-compatible-http" || result.Provenance.Usage.PeakRSSBytes != 0 {
				t.Fatalf("misleading endpoint provenance: %+v", result.Provenance)
			}
		})
	}
}

func TestAnswerEndpointRejectsMixedProviderOptionsBeforeLoading(t *testing.T) {
	for _, args := range [][]string{
		{"--endpoint", "http://127.0.0.1:1/v1/chat/completions", "Alpha"},
		{"--endpoint-profile", "neuroforge-native-extractive", "Alpha"},
		{"--provider", "openai-compatible", "--model", "/fictional.gguf", "Alpha"},
		{"--provider", "openai-compatible", "--runtime-receipt", "/fictional.json", "Alpha"},
		{"--provider", "openai-compatible", "--max-rss-mib", "256", "Alpha"},
		{"--provider", "openai-compatible", "--threads", "1", "Alpha"},
		{"--provider", "openai-compatible", "--batch-size", "32", "Alpha"},
	} {
		var output bytes.Buffer
		err := runAnswerContext(context.Background(), args, answerDependencies{
			loadDataset:  func(string) (*server.Dataset, error) { t.Fatal("loaded dataset for invalid flags"); return nil, nil },
			openProvider: openAnswerGenerationProvider, stdout: &output, now: time.Now,
		})
		if err == nil || !strings.Contains(err.Error(), "require --provider") {
			t.Fatalf("args=%v err=%v", args, err)
		}
	}
}

func TestAnswerEndpointProfilesAreExplicitAndBounded(t *testing.T) {
	request := qualifiedGenerationRequest{Provider: "openai-compatible", Endpoint: "http://127.0.0.1:1/v1/chat/completions",
		HTTPModelName: "fictional-native", HTTPProfile: "neuroforge-native-extractive", ContextTokens: 512,
		MaximumOutputTokens: 128, Timeout: time.Second}
	session, err := openAnswerGenerationProvider(request)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.ResponseSchemaSHA256 != "" || !strings.Contains(session.Descriptor.RuntimeRevision, "native-extractive") {
		t.Fatalf("native profile reports structured qualification: %+v", session)
	}
	request.MaximumOutputTokens = 129
	if _, err := openAnswerGenerationProvider(request); err == nil {
		t.Fatal("native output cap ignored")
	}
	request.MaximumOutputTokens = 128
	request.ContextTokens = 4096
	if _, err := openAnswerGenerationProvider(request); err == nil {
		t.Fatal("native context cap ignored")
	}
	request.ContextTokens = 512
	request.HTTPProfile = "guess-remote-capabilities"
	if _, err := openAnswerGenerationProvider(request); err == nil {
		t.Fatal("unknown capability profile accepted")
	}
}
