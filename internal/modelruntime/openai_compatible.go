package modelruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	openAICompatiblePromptBytes   = 256 * 1024
	openAICompatibleResponseBytes = 1024 * 1024
)

// OpenAICompatibleConfig selects an independently managed local HTTP model.
// Endpoint must be an exact credential-free, IP-literal loopback chat-completions
// URL. The adapter does not manage, attest, or measure the server process.
type OpenAICompatibleConfig struct {
	Endpoint       string
	Model          string
	ContextLimit   int
	Timeout        time.Duration
	ResponseSchema string
	Profile        OpenAICompatibleProfile
}

// OpenAICompatibleProvider sends bounded evidence packets to a loopback model
// server. It never reads credentials, follows redirects, or uses HTTP proxies.
type OpenAICompatibleProvider struct {
	config    OpenAICompatibleConfig
	schema    json.RawMessage
	client    *http.Client
	transport *http.Transport
	mutex     sync.Mutex
	closed    bool
	cancel    context.CancelFunc
}

// NewOpenAICompatibleProvider validates configuration without making a request.
func NewOpenAICompatibleProvider(config OpenAICompatibleConfig) (*OpenAICompatibleProvider, error) {
	if config.Profile == "" {
		config.Profile = ProfileStructuredClaims
	}
	if config.Profile != ProfileStructuredClaims && config.Profile != ProfileNeuroForgeNativeExtractive {
		return nil, errors.New("HTTP model profile must be structured-claims or neuroforge-native-extractive")
	}
	if err := validateOpenAICompatibleEndpoint(config.Endpoint); err != nil {
		return nil, err
	}
	if strings.TrimSpace(config.Model) != config.Model || config.Model == "" ||
		len(config.Model) > 256 || !utf8.ValidString(config.Model) || strings.IndexFunc(config.Model, unicode.IsControl) >= 0 {
		return nil, errors.New("HTTP model ID must be nonempty printable text within 256 bytes")
	}
	if config.ContextLimit < 512 || config.ContextLimit > 262144 {
		return nil, errors.New("HTTP model context limit must be between 512 and 262144 tokens")
	}
	if config.Profile == ProfileNeuroForgeNativeExtractive && config.ContextLimit > nativeExtractiveContextTokens {
		return nil, errors.New("native extractive profile context must be explicitly configured to 512 tokens")
	}
	if config.Timeout <= 0 || config.Timeout > time.Hour {
		return nil, errors.New("HTTP model timeout must be positive and no greater than 1h")
	}
	if config.Profile == ProfileStructuredClaims {
		if len(config.ResponseSchema) == 0 || len(config.ResponseSchema) > 64*1024 || !utf8.ValidString(config.ResponseSchema) {
			return nil, errors.New("HTTP model response schema must be a bounded JSON object")
		}
		var schema map[string]json.RawMessage
		if err := json.Unmarshal([]byte(config.ResponseSchema), &schema); err != nil || schema == nil || string(schema["type"]) != `"object"` {
			return nil, errors.New("HTTP model response schema must declare a JSON object")
		}
	}
	dialer := &net.Dialer{Timeout: config.Timeout}
	transport := &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(address)
			ip := net.ParseIP(host)
			if err != nil || ip == nil || !ip.IsLoopback() {
				return nil, errors.New("HTTP model transport refuses non-loopback addresses")
			}
			return dialer.DialContext(ctx, network, address)
		},
		TLSHandshakeTimeout:    config.Timeout,
		MaxResponseHeaderBytes: 32 * 1024,
	}
	return &OpenAICompatibleProvider{
		config: config, schema: json.RawMessage(config.ResponseSchema), transport: transport,
		client: &http.Client{Transport: transport, Timeout: config.Timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("HTTP model redirects are disabled")
			}},
	}, nil
}

func validateOpenAICompatibleEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.Opaque != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery ||
		parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(endpoint, "#") || parsed.RawPath != "" ||
		!strings.HasSuffix(parsed.Path, "/chat/completions") || strings.Contains(parsed.Path, "//") ||
		strings.Contains(parsed.Path, "/../") || strings.Contains(parsed.Path, "/./") {
		return errors.New("HTTP model endpoint must be an exact credential-free loopback chat-completions URL")
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return errors.New("HTTP model endpoint must use an IP-literal loopback host")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return errors.New("HTTP model endpoint port must be between 1 and 65535")
		}
	} else if strings.HasSuffix(parsed.Host, ":") {
		return errors.New("HTTP model endpoint port cannot be empty")
	}
	return nil
}

// Descriptor reports the selected model and external HTTP transport. A zero
// weight size and absent digests mean server artifacts have not been attested.
func (provider *OpenAICompatibleProvider) Descriptor() ModelDescriptor {
	return ModelDescriptor{ID: provider.config.Model, Architecture: "external-http",
		ContextLimit: provider.config.ContextLimit, Runtime: "openai-compatible-http",
		RuntimeRevision: provider.Capabilities().ResponseProtocol}
}

// Capabilities reports the selected adapter profile. Native limits are
// conservative local admission rules, not tokenizer or upstream attestations.
func (provider *OpenAICompatibleProvider) Capabilities() ProviderCapabilities {
	if provider.config.Profile == ProfileNeuroForgeNativeExtractive {
		return ProviderCapabilities{Profile: string(provider.config.Profile), ResponseProtocol: nativeExtractiveProtocol,
			MaximumPromptBytes: nativeExtractivePromptBytes, MaximumOutputTokens: nativeExtractiveOutputTokens,
			MaximumContextTokens: nativeExtractiveContextTokens}
	}
	return ProviderCapabilities{Profile: string(provider.config.Profile), ResponseProtocol: structuredClaimsProtocol,
		JSONSchemaOutput: true, TemperatureControl: true,
		MaximumPromptBytes: openAICompatiblePromptBytes, MaximumOutputTokens: provider.config.ContextLimit,
		MaximumContextTokens: provider.config.ContextLimit}
}

// BuildPrompt constructs the exact deterministic text transmitted by Generate.
// Native extractive prompts contain only compact, validated candidate lines.
func (provider *OpenAICompatibleProvider) BuildPrompt(input PromptRequest) (string, error) {
	if provider == nil {
		return "", errors.New("HTTP model provider is required")
	}
	request := Request{Task: input.Task, Packet: input.Packet, ValidationPass: input.ValidationPass}
	if provider.config.Profile == ProfileNeuroForgeNativeExtractive {
		prompt, _, err := buildNativeExtractivePrompt(request)
		return prompt, err
	}
	return BuildPrompt(request)
}

// Supports reports whether the evidence-packet prompt supports this task.
func (provider *OpenAICompatibleProvider) Supports(task Task) bool {
	switch task {
	case TaskSymbolSummary, TaskModuleSummary, TaskExecutionExplanation, TaskGapAnalysis:
		return true
	default:
		return false
	}
}

// Close cancels in-flight work and closes idle connections. It is idempotent.
func (provider *OpenAICompatibleProvider) Close() error {
	if provider == nil {
		return nil
	}
	provider.mutex.Lock()
	defer provider.mutex.Unlock()
	if !provider.closed {
		provider.closed = true
		if provider.cancel != nil {
			provider.cancel()
		}
		provider.transport.CloseIdleConnections()
	}
	return nil
}

// Generate returns untrusted claims for the existing evidence validator. The
// configured context is a request ceiling, not proof of server memory control.
func (provider *OpenAICompatibleProvider) Generate(parent context.Context, request Request) (Response, error) {
	if provider == nil || parent == nil {
		return Response{}, errors.New("HTTP model provider and context are required")
	}
	if err := parent.Err(); err != nil {
		return Response{}, err
	}
	if !provider.Supports(request.Task) {
		return Response{}, ErrUnsupportedTask
	}
	options := request.Options
	if options.ContextTokens < 0 || options.MaxOutputTokens < 0 {
		return Response{}, errors.New("HTTP model context/output tokens cannot be negative")
	}
	if options.ContextTokens == 0 {
		options.ContextTokens = minInt(provider.config.ContextLimit, 4096)
	}
	if options.MaxOutputTokens == 0 {
		options.MaxOutputTokens = minInt(options.ContextTokens, 768)
		if provider.config.Profile == ProfileNeuroForgeNativeExtractive {
			options.MaxOutputTokens = nativeExtractiveOutputTokens
		}
	}
	if options.ContextTokens < 1 || options.ContextTokens > provider.config.ContextLimit ||
		options.MaxOutputTokens < 1 || options.MaxOutputTokens > options.ContextTokens {
		return Response{}, errors.New("HTTP model context/output tokens exceed configured limits")
	}
	if options.MaxOutputTokens > provider.Capabilities().MaximumOutputTokens {
		return Response{}, errors.New("HTTP model output tokens exceed selected profile limit")
	}
	prompt, err := BuildProviderPrompt(provider, request)
	if err != nil {
		return Response{}, err
	}
	if len(prompt) > provider.Capabilities().MaximumPromptBytes {
		return Response{}, errors.New("HTTP model prompt exceeds selected profile byte limit")
	}
	if !utf8.ValidString(prompt) {
		return Response{}, errors.New("HTTP model prompt must be valid UTF-8")
	}
	var candidates []nativeExtractiveCandidate
	if provider.config.Profile == ProfileNeuroForgeNativeExtractive {
		_, candidates, err = buildNativeExtractivePrompt(request)
		if err != nil {
			return Response{}, err
		}
		if len(candidates) == 0 {
			return Response{}, ErrNoExtractiveEvidence
		}
	}
	payload := map[string]any{"model": provider.config.Model,
		"messages":   []map[string]string{{"role": "user", "content": prompt}},
		"max_tokens": options.MaxOutputTokens, "stream": false}
	if provider.config.Profile == ProfileNeuroForgeNativeExtractive {
		payload["n"] = 1
	} else {
		payload["temperature"] = 0
		payload["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{
			"name": "rkc_claims", "strict": true, "schema": provider.schema}}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return Response{}, errors.New("cannot encode HTTP model request")
	}
	ctx, cancel := context.WithTimeout(parent, provider.config.Timeout)
	if request.Deadline != nil {
		var deadlineCancel context.CancelFunc
		ctx, deadlineCancel = context.WithDeadline(ctx, *request.Deadline)
		defer deadlineCancel()
	}
	defer cancel()
	provider.mutex.Lock()
	if provider.closed || provider.cancel != nil {
		provider.mutex.Unlock()
		return Response{}, errors.New("HTTP model provider is closed or already generating")
	}
	provider.cancel = cancel
	provider.mutex.Unlock()
	defer func() {
		provider.mutex.Lock()
		provider.cancel = nil
		provider.mutex.Unlock()
	}()
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, provider.config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return Response{}, errors.New("cannot construct HTTP model request")
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	started := time.Now()
	httpResponse, err := provider.client.Do(httpRequest)
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, fmt.Errorf("HTTP model request cancelled: %w", ctx.Err())
		}
		return Response{}, errors.New("HTTP model request failed; redirects and proxies are disabled")
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		return Response{}, fmt.Errorf("HTTP model returned status %d", httpResponse.StatusCode)
	}
	contentType, _, err := mime.ParseMediaType(httpResponse.Header.Get("Content-Type"))
	if err != nil || contentType != "application/json" {
		return Response{}, errors.New("HTTP model response must use application/json")
	}
	output, err := io.ReadAll(io.LimitReader(httpResponse.Body, openAICompatibleResponseBytes+1))
	if err != nil {
		if ctx.Err() != nil {
			return Response{}, fmt.Errorf("HTTP model response cancelled: %w", ctx.Err())
		}
		return Response{}, errors.New("cannot read HTTP model response")
	}
	if len(output) > openAICompatibleResponseBytes {
		return Response{}, ErrModelOutputTooLarge
	}
	var response Response
	if provider.config.Profile == ProfileNeuroForgeNativeExtractive {
		response, err = decodeNativeExtractiveResponse(output, provider.config.Model, options, request.Packet, candidates)
	} else {
		response, err = decodeOpenAICompatibleResponse(output, provider.config.Model, options)
	}
	if err != nil {
		return Response{}, err
	}
	response.RequestID = request.RequestID
	response.ModelID = provider.config.Model
	response.Usage.WallTimeMillis = time.Since(started).Milliseconds()
	return response, nil
}

func decodeOpenAICompatibleResponse(output []byte, model string, options InferenceOptions) (Response, error) {
	contentText, usage, err := decodeOpenAICompatibleCompletion(output, model, options)
	if err != nil {
		return Response{}, err
	}
	var content struct {
		Claims              []ClaimDraft `json:"claims"`
		UnresolvedQuestions []string     `json:"unresolved_questions"`
	}
	if err := decodeSingleJSONObject([]byte(contentText), &content, true); err != nil ||
		content.Claims == nil || content.UnresolvedQuestions == nil ||
		(len(content.Claims) == 0 && len(content.UnresolvedQuestions) == 0) {
		return Response{}, fmt.Errorf("%w: HTTP model content must be one nonempty strict claims object", ErrModelOutputInvalid)
	}
	return Response{Claims: content.Claims, UnresolvedQuestions: content.UnresolvedQuestions, Usage: usage}, nil
}

func decodeOpenAICompatibleCompletion(output []byte, model string, options InferenceOptions) (string, Usage, error) {
	var envelope struct {
		Model   string `json:"model"`
		Object  string `json:"object"`
		Choices []struct {
			Index   *int `json:"index"`
			Message struct {
				Role         string          `json:"role"`
				Content      string          `json:"content"`
				Refusal      json.RawMessage `json:"refusal"`
				ToolCalls    json.RawMessage `json:"tool_calls"`
				FunctionCall json.RawMessage `json:"function_call"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			Prompt     *int `json:"prompt_tokens"`
			Completion *int `json:"completion_tokens"`
			Total      *int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := decodeSingleJSONObject(output, &envelope, false); err != nil || envelope.Model != model || envelope.Object != "chat.completion" || len(envelope.Choices) != 1 {
		return "", Usage{}, fmt.Errorf("%w: invalid HTTP completion envelope", ErrModelOutputInvalid)
	}
	choice := envelope.Choices[0]
	if choice.Index == nil || *choice.Index != 0 || choice.Message.Role != "assistant" || choice.FinishReason != "stop" ||
		strings.TrimSpace(choice.Message.Content) == "" || nonNullJSON(choice.Message.Refusal) ||
		nonNullJSON(choice.Message.ToolCalls) || nonNullJSON(choice.Message.FunctionCall) {
		return "", Usage{}, fmt.Errorf("%w: empty, refused, truncated or unsupported HTTP completion", ErrModelOutputInvalid)
	}
	var telemetry Usage
	if usage := envelope.Usage; usage != nil {
		if usage.Prompt == nil || usage.Completion == nil || usage.Total == nil ||
			*usage.Prompt < 0 || *usage.Prompt > options.ContextTokens || *usage.Completion < 0 ||
			*usage.Completion > options.MaxOutputTokens || *usage.Total != *usage.Prompt+*usage.Completion ||
			*usage.Total > options.ContextTokens {
			return "", Usage{}, fmt.Errorf("%w: invalid HTTP token usage", ErrModelOutputInvalid)
		}
		telemetry.PromptTokens = *usage.Prompt
		telemetry.OutputTokens = *usage.Completion
	}
	return choice.Message.Content, telemetry, nil
}

func nonNullJSON(value json.RawMessage) bool {
	return len(value) > 0 && string(value) != "null"
}

func decodeSingleJSONObject(value []byte, target any, strict bool) error {
	trimmed := bytes.TrimSpace(value)
	if len(trimmed) == 0 || trimmed[0] != '{' || !utf8.Valid(trimmed) {
		return errors.New("JSON object required")
	}
	uniqueness := json.NewDecoder(bytes.NewReader(trimmed))
	uniqueness.UseNumber()
	if err := validateUniqueJSONKeys(uniqueness, 0); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	if strict {
		decoder.DisallowUnknownFields()
	}
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON or text is forbidden")
	}
	return nil
}

// validateUniqueJSONKeys rejects duplicate members and excessive nesting before
// Go's ordinary decoder can silently replace an earlier claim or model field.
func validateUniqueJSONKeys(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return errors.New("JSON nesting exceeds response limit")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, container := token.(json.Delim)
	if !container {
		return nil
	}
	switch delimiter {
	case '{':
		keys := map[string]struct{}{}
		for decoder.More() {
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return errors.New("JSON object key is invalid")
			}
			if _, exists := keys[key]; exists {
				return errors.New("duplicate JSON member is forbidden")
			}
			keys[key] = struct{}{}
			if err := validateUniqueJSONKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for decoder.More() {
			if err := validateUniqueJSONKeys(decoder, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = decoder.Token()
	return err
}
