package modelruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
)

// APIProvider implements bounded native and compatible HTTP protocols. It owns
// client resources only; its descriptor never attests remote weights or memory.
// Subscription CLI credentials are not read. Secrets are resolved from the one
// configured environment variable immediately before each request.
type APIProvider struct {
	profile   ProviderProfile
	schema    json.RawMessage
	client    *http.Client
	transport *http.Transport
	mutex     sync.Mutex
	closed    bool
	cancel    context.CancelFunc
}

// NewAPIProvider validates connection policy without network or credential I/O.
// The original OpenAICompatibleProvider remains available for its strict local
// protocol; this separate constructor adds explicit portable connection policy.
func NewAPIProvider(profile ProviderProfile, responseSchema string) (*APIProvider, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	var schema map[string]any
	if profile.Profile == string(ProfileStructuredClaims) {
		if len(responseSchema) == 0 || len(responseSchema) > 64*1024 || decodeSingleJSONObject([]byte(responseSchema), &schema, false) != nil || schema["type"] != "object" {
			return nil, errors.New("API response schema must be a bounded JSON object schema")
		}
		if profile.Provider != "openai-compatible" {
			projectAPISchema(schema, profile.Provider)
		}
	}
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, errors.New("cannot encode API response schema")
	}
	transport := newAPITransport(profile)
	return &APIProvider{profile: profile, schema: encoded, transport: transport,
		client: &http.Client{Transport: transport, Timeout: time.Duration(profile.TimeoutSeconds) * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("model API redirects are disabled") }}}, nil
}

func newAPITransport(profile ProviderProfile) *http.Transport {
	timeout := time.Duration(profile.TimeoutSeconds) * time.Second
	dialer := &net.Dialer{Timeout: timeout}
	parsed, _ := url.Parse(profile.Endpoint) // profile.Validate has checked this URL.
	ip := net.ParseIP(parsed.Hostname())
	local := ip != nil && ip.IsLoopback()
	return &http.Transport{Proxy: nil, TLSHandshakeTimeout: timeout,
		MaxResponseHeaderBytes: 32 * 1024, MaxConnsPerHost: 1, MaxIdleConnsPerHost: 1,
		IdleConnTimeout: time.Minute,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			if local {
				host, _, err := net.SplitHostPort(address)
				addressIP := net.ParseIP(host)
				if err != nil || addressIP == nil || !addressIP.IsLoopback() {
					return nil, errors.New("local API transport refuses non-loopback addresses")
				}
			}
			return dialer.DialContext(ctx, network, address)
		}}
}

// CheckCredential checks only a named environment variable. Errors contain the
// variable name, never its contents. Empty names denote an anonymous connection.
func CheckCredential(name string) error {
	_, err := readAPICredential(name)
	return err
}

func readAPICredential(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if !validCredentialEnvName(name) {
		return "", errors.New("api_key_env must name an environment variable")
	}
	value := os.Getenv(name)
	if value == "" {
		return "", fmt.Errorf("credential environment variable %s is not set; set it in the process running RKC", name)
	}
	if len(value) > 8192 {
		return "", fmt.Errorf("credential environment variable %s exceeds the length limit", name)
	}
	for _, character := range value {
		if character < 33 || character > 126 {
			return "", fmt.Errorf("credential environment variable %s must contain a printable token without whitespace", name)
		}
	}
	return value, nil
}

// Descriptor records requested model and wire protocol, without server identity
// or resource attestation. Response.ReportedModelID retains an upstream revision.
func (provider *APIProvider) Descriptor() ModelDescriptor {
	return ModelDescriptor{ID: provider.profile.Model, Architecture: "external-http",
		ContextLimit: provider.profile.ContextTokens, Runtime: provider.profile.Provider + "-http",
		RuntimeRevision: provider.Capabilities().ResponseProtocol}
}

// Capabilities reports the actual request schema and conservative local bounds.
func (provider *APIProvider) Capabilities() ProviderCapabilities {
	if provider.profile.Profile == string(ProfileNeuroForgeNativeExtractive) {
		return ProviderCapabilities{Profile: provider.profile.Profile, ResponseProtocol: nativeExtractiveProtocol,
			MaximumPromptBytes: nativeExtractivePromptBytes, MaximumOutputTokens: nativeExtractiveOutputTokens,
			MaximumContextTokens: nativeExtractiveContextTokens}
	}
	protocol := structuredClaimsProtocol
	if provider.profile.Provider != "openai-compatible" {
		protocol += "/" + provider.profile.Provider + "-v1"
	}
	return ProviderCapabilities{Profile: provider.profile.Profile, ResponseProtocol: protocol,
		JSONSchemaOutput: true, TemperatureControl: provider.profile.Provider == "openai-compatible",
		MaximumPromptBytes: openAICompatiblePromptBytes, MaximumOutputTokens: provider.profile.MaxOutputTokens,
		MaximumContextTokens: provider.profile.ContextTokens}
}

// ResponseSchemaSHA256 hashes the exact projected schema sent on this wire.
// Empty means the selected extractive profile sends no JSON schema.
func (provider *APIProvider) ResponseSchemaSHA256() string {
	if !provider.Capabilities().JSONSchemaOutput {
		return ""
	}
	digest := sha256.Sum256(provider.schema)
	return hex.EncodeToString(digest[:])
}

// BuildPrompt constructs exactly the same evidence text used by Generate.
func (provider *APIProvider) BuildPrompt(input PromptRequest) (string, error) {
	if provider == nil {
		return "", errors.New("API provider is required")
	}
	request := Request{Task: input.Task, Packet: input.Packet, ValidationPass: input.ValidationPass}
	if provider.profile.Profile == string(ProfileNeuroForgeNativeExtractive) {
		prompt, _, err := buildNativeExtractivePrompt(request)
		return prompt, err
	}
	return BuildPrompt(request)
}

// Supports reports the four evidence-packet synthesis tasks.
func (provider *APIProvider) Supports(task Task) bool {
	return task == TaskSymbolSummary || task == TaskModuleSummary || task == TaskExecutionExplanation || task == TaskGapAnalysis
}

// Close cancels in-flight requests and closes idle connections, idempotently.
func (provider *APIProvider) Close() error {
	if provider == nil {
		return nil
	}
	provider.mutex.Lock()
	defer provider.mutex.Unlock()
	provider.closed = true
	if provider.cancel != nil {
		provider.cancel()
	}
	provider.transport.CloseIdleConnections()
	return nil
}

// Generate sends one bounded non-streaming request without retries. Structured
// responses remain untrusted and must pass the caller's canonical validator.
func (provider *APIProvider) Generate(parent context.Context, request Request) (Response, error) {
	if provider == nil || parent == nil {
		return Response{}, errors.New("API provider and context are required")
	}
	if err := parent.Err(); err != nil {
		return Response{}, err
	}
	if !provider.Supports(request.Task) {
		return Response{}, ErrUnsupportedTask
	}
	options := request.Options
	if options.ContextTokens == 0 {
		options.ContextTokens = provider.profile.ContextTokens
	}
	if options.MaxOutputTokens == 0 {
		options.MaxOutputTokens = provider.profile.MaxOutputTokens
	}
	if options.ContextTokens < 1 || options.ContextTokens > provider.profile.ContextTokens || options.MaxOutputTokens < 1 ||
		options.MaxOutputTokens > provider.profile.MaxOutputTokens || options.MaxOutputTokens > options.ContextTokens {
		return Response{}, errors.New("API context/output tokens exceed configured limits")
	}
	prompt, err := BuildProviderPrompt(provider, request)
	if err != nil {
		return Response{}, err
	}
	if len(prompt) > provider.Capabilities().MaximumPromptBytes {
		return Response{}, errors.New("API prompt exceeds selected profile byte limit")
	}
	var candidates []nativeExtractiveCandidate
	if provider.profile.Profile == string(ProfileNeuroForgeNativeExtractive) {
		_, candidates, err = buildNativeExtractivePrompt(request)
		if err != nil {
			return Response{}, err
		}
		if len(candidates) == 0 {
			return Response{}, ErrNoExtractiveEvidence
		}
	}
	body, err := provider.encodeRequest(prompt, options)
	if err != nil {
		return Response{}, err
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(provider.profile.TimeoutSeconds)*time.Second)
	defer cancel()
	if request.Deadline != nil {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithDeadline(ctx, *request.Deadline)
		defer deadlineCancel()
	}
	provider.mutex.Lock()
	if provider.closed || provider.cancel != nil {
		provider.mutex.Unlock()
		return Response{}, errors.New("API provider is closed or already generating")
	}
	provider.cancel = cancel
	provider.mutex.Unlock()
	defer func() { provider.mutex.Lock(); provider.cancel = nil; provider.mutex.Unlock() }()
	started := time.Now()
	output, err := provider.requestJSON(ctx, http.MethodPost, provider.profile.Endpoint, body)
	if err != nil {
		return Response{}, err
	}
	response, err := provider.decodeResponse(output, options, request.Packet, candidates)
	if err != nil {
		return Response{}, err
	}
	response.RequestID = request.RequestID
	response.Usage.WallTimeMillis = time.Since(started).Milliseconds()
	return response, nil
}

func (provider *APIProvider) requestJSON(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	credential, err := readAPICredential(provider.profile.APIKeyEnv)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("cannot construct API request")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if credential != "" {
		switch provider.profile.Provider {
		case "anthropic":
			request.Header.Set("x-api-key", credential)
		case "gemini":
			request.Header.Set("x-goog-api-key", credential)
		default:
			request.Header.Set("Authorization", "Bearer "+credential)
		}
	}
	if provider.profile.Provider == "anthropic" {
		request.Header.Set("anthropic-version", "2023-06-01")
	}
	response, err := provider.client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("API request cancelled: %w", ctx.Err())
		}
		return nil, errors.New("API request failed; check HTTPS certificate and endpoint; redirects and proxies are disabled")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Response bodies and URL-bearing transport errors can contain secrets.
		return nil, fmt.Errorf("model API returned HTTP %d; check access, model support, quota and endpoint", response.StatusCode)
	}
	contentType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return nil, errors.New("model API response must use application/json")
	}
	output, err := io.ReadAll(io.LimitReader(response.Body, openAICompatibleResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("API response cancelled: %w", ctx.Err())
		}
		return nil, errors.New("cannot read API response")
	}
	if len(output) > openAICompatibleResponseBytes {
		return nil, ErrModelOutputTooLarge
	}
	return output, nil
}
