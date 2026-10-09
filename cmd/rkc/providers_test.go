package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/modelruntime"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/server"
)

func TestProvidersInitDoctorAndNoClobber(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider.json")
	var output, diagnostics bytes.Buffer
	err := runProvidersWithIO(context.Background(), []string{"init", "--preset", "ollama", "--model", "fictional-model", "--out", path}, &output, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := loadProviderProfile(path)
	if err != nil || profile.AllowRemote || profile.Model != "fictional-model" {
		t.Fatalf("profile=%+v error=%v", profile, err)
	}
	before, _ := os.ReadFile(path)
	if err := runProvidersWithIO(context.Background(), []string{"init", "--preset", "ollama", "--model", "replacement-model", "--out", path}, &output, &diagnostics); err == nil {
		t.Fatal("existing profile was overwritten")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("existing profile changed")
	}
	output.Reset()
	if err := runProvidersWithIO(context.Background(), []string{"doctor", "--file", path, "--json"}, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	var report struct {
		Ready          bool `json:"ready"`
		NetworkChecked bool `json:"network_checked"`
	}
	if json.Unmarshal(output.Bytes(), &report) != nil || !report.Ready || report.NetworkChecked {
		t.Fatalf("doctor report=%s", output.String())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("staging files left behind: %+v", entries)
	}
}

func TestProvidersHostedInitRequiresConsentAndKeepsSecretOutOfProfiles(t *testing.T) {
	t.Setenv("RKC_TEST_CREDENTIAL", "fictional-private-token")
	var output, diagnostics bytes.Buffer
	if err := runProvidersWithIO(context.Background(), []string{"init", "--preset", "openai", "--model", "fictional-model"}, &output, &diagnostics); err == nil || !strings.Contains(err.Error(), "allow_remote") {
		t.Fatalf("missing consent accepted: %v", err)
	}
	if err := runProvidersWithIO(context.Background(), []string{"init", "--preset", "openai-compatible", "--model", "fictional-model", "--endpoint", "https://api.example.test/v1/chat/completions", "--api-key-env", "RKC_TEST_CREDENTIAL", "--allow-remote"}, &output, &diagnostics); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String()+diagnostics.String(), "fictional-private-token") {
		t.Fatal("secret was written to output")
	}
	profile, err := modelruntime.ReadProviderProfile(bytes.NewReader(output.Bytes()))
	if err != nil || !profile.AllowRemote || profile.APIKeyEnv != "RKC_TEST_CREDENTIAL" {
		t.Fatalf("profile=%+v error=%v", profile, err)
	}
	path := filepath.Join(t.TempDir(), "provider.json")
	if err := publishProviderProfile(context.Background(), path, output.Bytes()); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	t.Setenv("RKC_TEST_CREDENTIAL", "")
	err = runProvidersWithIO(context.Background(), []string{"doctor", "--file", path}, &output, &diagnostics)
	if err == nil || !strings.Contains(output.String(), "RKC_TEST_CREDENTIAL is not set") || strings.Contains(output.String(), "fictional-private-token") {
		t.Fatalf("doctor missing credential=%v output=%s", err, output.String())
	}
}

func TestProviderProfilePublicationCancellationAndConcurrentNoClobber(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "provider.json")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := publishProviderProfile(ctx, path, []byte("cancelled")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("cancelled operation published a profile")
	}
	var group sync.WaitGroup
	results := make(chan error, 2)
	for _, data := range [][]byte{bytes.Repeat([]byte("a"), 32000), bytes.Repeat([]byte("b"), 32000)} {
		group.Add(1)
		go func(data []byte) {
			defer group.Done()
			results <- publishProviderProfile(context.Background(), path, data)
		}(data)
	}
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || successes != 1 || len(data) != 32000 || (!bytes.Equal(data, bytes.Repeat([]byte("a"), 32000)) && !bytes.Equal(data, bytes.Repeat([]byte("b"), 32000))) {
		t.Fatalf("partial/clobbered profile: successes=%d length=%d err=%v", successes, len(data), err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatal("temporary profile leaked")
	}
	if err := publishProviderProfile(nil, path, nil); err == nil {
		t.Fatal("nil context")
	}
	if err := publishProviderProfile(context.Background(), filepath.Join(root, "missing", "provider.json"), nil); err == nil {
		t.Fatal("missing parent")
	}
}

func TestProvidersCatalogLoginGuideAndValidation(t *testing.T) {
	for _, args := range [][]string{{}, {"help"}, {"list"}, {"list", "--json"}, {"login-guide"}, {"login-guide", "--client", "codex"}, {"login-guide", "--client", "claude"}, {"login-guide", "--client", "gemini"}} {
		var output, diagnostics bytes.Buffer
		if err := runProvidersWithIO(context.Background(), args, &output, &diagnostics); err != nil || output.Len() == 0 {
			t.Fatalf("args=%v output=%s err=%v", args, output.String(), err)
		}
	}
	for _, args := range [][]string{
		{"guess"}, {"list", "extra"}, {"list", "--unknown"}, {"init", "extra"}, {"init", "--preset", "guess", "--model", "fictional"},
		{"init", "--preset", "ollama", "--model", "fictional", "--api-key-env", "fake-key-value"}, {"doctor"}, {"doctor", "extra"}, {"models"}, {"login-guide", "--client", "guess"}, {"login-guide", "extra"},
	} {
		var output, diagnostics bytes.Buffer
		if err := runProvidersWithIO(context.Background(), args, &output, &diagnostics); err == nil {
			t.Fatalf("args=%v accepted", args)
		}
	}
	if err := runProvidersWithIO(nil, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("nil context")
	}
	if _, err := loadProviderProfile(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing profile accepted")
	}
	if err := runProvidersWithIO(context.Background(), []string{"list"}, failingProviderWriter{}, io.Discard); err == nil {
		t.Fatal("output failure ignored")
	}
}

type failingProviderWriter struct{}

func (failingProviderWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic output failure")
}

func TestProvidersModelsMakesOnlyMetadataGET(t *testing.T) {
	calls := 0
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"id":"fictional-model"}],"has_more":true}`)
	}))
	defer endpoint.Close()
	profile, _ := modelruntime.NewProviderProfile("ollama", "fictional-model", false)
	profile.Endpoint = endpoint.URL + "/v1/chat/completions"
	data, _ := modelruntime.MarshalProviderProfile(profile)
	path := filepath.Join(t.TempDir(), "provider.json")
	if err := publishProviderProfile(context.Background(), path, data); err != nil {
		t.Fatal(err)
	}
	for _, jsonOutput := range []bool{false, true} {
		var output, diagnostics bytes.Buffer
		args := []string{"models", "--file", path}
		if jsonOutput {
			args = append(args, "--json")
		}
		if err := runProvidersWithIO(context.Background(), args, &output, &diagnostics); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(output.String(), "fictional-model") {
			t.Fatalf("missing model ID: %s", output.String())
		}
	}
	if calls != 2 {
		t.Fatal(calls)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runProvidersWithIO(ctx, []string{"models", "--file", path}, io.Discard, io.Discard); !errors.Is(err, context.Canceled) || calls != 2 {
		t.Fatalf("cancelled metadata call: %v calls=%d", err, calls)
	}
}

func TestAnswerPortableProfileAndExplicitOverrides(t *testing.T) {
	profile, _ := modelruntime.NewProviderProfile("openai", "fictional-model", true)
	data, _ := modelruntime.MarshalProviderProfile(profile)
	path := filepath.Join(t.TempDir(), "provider.json")
	if err := publishProviderProfile(context.Background(), path, data); err != nil {
		t.Fatal(err)
	}
	var received qualifiedGenerationRequest
	provider := &cliAnswerProvider{descriptor: modelruntime.ModelDescriptor{ID: "fictional-model"}}
	bundle := cliAnswerBundle()
	var output bytes.Buffer
	err := runAnswerContext(context.Background(), []string{"--provider-config", path, "--context", "2048", "--max-output", "256", "--timeout", "30s", "--json", "What is Alpha?"}, answerDependencies{
		loadDataset: func(string) (*server.Dataset, error) {
			return &server.Dataset{Bundle: bundle, Search: search.BuildFromBundle(bundle), Graph: graph.Build(bundle.Nodes, bundle.Edges)}, nil
		},
		openProvider: func(request qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
			received = request
			return &qualifiedGenerationSession{Provider: provider, Descriptor: provider.descriptor}, nil
		},
		stdout: &output, now: time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if received.Provider != "openai" || received.HTTPModelName != "fictional-model" || received.Endpoint != profile.Endpoint || received.APIKeyEnv != "OPENAI_API_KEY" || !received.AllowRemote || received.ContextTokens != 2048 || received.MaximumOutputTokens != 256 || received.Timeout != 30*time.Second {
		t.Fatalf("profile or overrides lost: %+v", received)
	}
}

func TestAnswerAPIProviderSetupAndInvalidMixedFlags(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "fictional-private-token")
	t.Setenv("ANTHROPIC_API_KEY", "fictional-private-token")
	t.Setenv("GEMINI_API_KEY", "fictional-private-token")
	for _, protocol := range []string{"openai", "anthropic", "gemini"} {
		request := qualifiedGenerationRequest{Provider: protocol, HTTPModelName: "fictional-model", ContextTokens: 4096, MaximumOutputTokens: 768, Timeout: time.Minute, AllowRemote: true}
		session, err := openAnswerGenerationProvider(request)
		if err != nil {
			t.Fatal(err)
		}
		if session.ProviderName != protocol+"-http" || session.ResponseSchemaSHA256 == "" || session.Descriptor.Digest != "" {
			t.Fatalf("session=%+v", session)
		}
		_ = session.Close()
		request.AllowRemote = false
		if _, err := openAnswerGenerationProvider(request); err == nil {
			t.Fatal("remote consent inferred")
		}
	}
	for _, args := range [][]string{{"--api-key-env", "OPENAI_API_KEY", "Alpha"}, {"--allow-remote", "Alpha"}, {"--provider", "openai", "--threads", "1", "Alpha"}, {"--provider", "gemini", "--model", "fictional.gguf", "Alpha"}} {
		var output bytes.Buffer
		err := runAnswerContext(context.Background(), args, answerDependencies{loadDataset: func(string) (*server.Dataset, error) {
			t.Fatal("invalid mixed flags loaded a dataset")
			return nil, nil
		}, openProvider: openAnswerGenerationProvider, stdout: &output, now: time.Now})
		if err == nil || !strings.Contains(err.Error(), "require --provider") {
			t.Fatalf("args=%v error=%v", args, err)
		}
	}
}
