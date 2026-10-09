package modelruntime

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

func (provider *APIProvider) encodeRequest(prompt string, options InferenceOptions) ([]byte, error) {
	var payload map[string]any
	switch provider.profile.Provider {
	case "anthropic":
		payload = map[string]any{"model": provider.profile.Model, "max_tokens": options.MaxOutputTokens,
			"messages": []map[string]string{{"role": "user", "content": prompt}}, "stream": false,
			"output_config": map[string]any{"format": map[string]any{"type": "json_schema", "schema": provider.schema}}}
	case "gemini":
		payload = map[string]any{"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]string{"text": prompt}}}},
			"generationConfig": map[string]any{"maxOutputTokens": options.MaxOutputTokens, "candidateCount": 1,
				"responseMimeType": "application/json", "responseJsonSchema": provider.schema}}
	default:
		payload = map[string]any{"model": provider.profile.Model,
			"messages": []map[string]string{{"role": "user", "content": prompt}}, "stream": false}
		if provider.profile.Provider == "openai" {
			// The current OpenAI field also works with reasoning models; older
			// compatible servers retain their documented max_tokens contract.
			payload["max_completion_tokens"] = options.MaxOutputTokens
		} else {
			payload["max_tokens"] = options.MaxOutputTokens
		}
		if provider.profile.Profile == string(ProfileNeuroForgeNativeExtractive) {
			payload["n"] = 1
		} else {
			if provider.profile.Provider == "openai-compatible" {
				payload["temperature"] = 0
			}
			payload["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{
				"name": "rkc_claims", "strict": true, "schema": provider.schema}}
		}
	}
	return json.Marshal(payload)
}

// projectAPISchema translates unsupported wire constraints without weakening
// RKC's response checks. The projected bytes have their own provenance digest.
func projectAPISchema(value any, protocol string) {
	switch current := value.(type) {
	case map[string]any:
		if constant, exists := current["const"]; exists {
			current["enum"] = []any{constant}
			delete(current, "const")
		}
		delete(current, "uniqueItems")
		if protocol == "anthropic" || protocol == "gemini" {
			delete(current, "maxLength")
			delete(current, "minLength")
		}
		if protocol == "anthropic" {
			delete(current, "maxItems")
			delete(current, "minimum")
			delete(current, "maximum")
		}
		for _, nested := range current {
			projectAPISchema(nested, protocol)
		}
	case []any:
		for _, nested := range current {
			projectAPISchema(nested, protocol)
		}
	}
}

func (provider *APIProvider) decodeResponse(output []byte, options InferenceOptions, packet EvidencePacket, candidates []nativeExtractiveCandidate) (Response, error) {
	if provider.profile.Provider == "openai-compatible" {
		var response Response
		var err error
		if provider.profile.Profile == string(ProfileNeuroForgeNativeExtractive) {
			response, err = decodeNativeExtractiveResponse(output, provider.profile.Model, options, packet, candidates)
		} else {
			response, err = decodeOpenAICompatibleResponse(output, provider.profile.Model, options)
		}
		response.ModelID = provider.profile.Model
		return response, err
	}
	var content, reportedModel string
	var usage Usage
	var err error
	switch provider.profile.Provider {
	case "anthropic":
		content, reportedModel, usage, err = decodeAnthropicCompletion(output, options)
	case "gemini":
		content, reportedModel, usage, err = decodeGeminiCompletion(output, options)
	case "openai":
		// Providers may resolve an alias to a versioned model ID. Record the
		// reported ID instead of treating it as verified artifact identity.
		var envelope map[string]json.RawMessage
		if decodeSingleJSONObject(output, &envelope, false) != nil || json.Unmarshal(envelope["model"], &reportedModel) != nil || !validModelID(reportedModel) {
			return Response{}, invalidAPIOutput("invalid OpenAI completion envelope")
		}
		envelope["model"], _ = json.Marshal(provider.profile.Model)
		normalized, _ := json.Marshal(envelope)
		content, usage, err = decodeOpenAICompatibleCompletion(normalized, provider.profile.Model, options)
	}
	if err != nil {
		return Response{}, err
	}
	var decoded struct {
		Claims              []ClaimDraft `json:"claims"`
		UnresolvedQuestions []string     `json:"unresolved_questions"`
	}
	if decodeSingleJSONObject([]byte(content), &decoded, true) != nil || decoded.Claims == nil || decoded.UnresolvedQuestions == nil ||
		len(decoded.Claims)+len(decoded.UnresolvedQuestions) == 0 {
		return Response{}, invalidAPIOutput("content must be one nonempty strict claims object")
	}
	if len(decoded.Claims) > 8 || len(decoded.UnresolvedQuestions) > 8 {
		return Response{}, invalidAPIOutput("claims or unresolved questions exceed the schema count limit")
	}
	for _, claim := range decoded.Claims {
		if strings.TrimSpace(claim.Text) == "" || utf8.RuneCountInString(claim.Text) > 1200 || claim.Certainty != "supported" || len(claim.EvidenceIDs) < 1 || len(claim.EvidenceIDs) > 8 {
			return Response{}, invalidAPIOutput("claim violates the bounded supported-claim schema")
		}
		if claim.Category != "purpose" && claim.Category != "signature" && claim.Category != "error" && claim.Category != "relationship" && claim.Category != "constraint" {
			return Response{}, invalidAPIOutput("claim category is outside the response schema")
		}
		seen := map[string]bool{}
		for _, evidenceID := range claim.EvidenceIDs {
			if seen[evidenceID] {
				return Response{}, invalidAPIOutput("claim contains duplicate evidence IDs")
			}
			seen[evidenceID] = true
		}
	}
	for _, question := range decoded.UnresolvedQuestions {
		if utf8.RuneCountInString(question) > 500 {
			return Response{}, invalidAPIOutput("unresolved question exceeds the schema character limit")
		}
	}
	return Response{Claims: decoded.Claims, UnresolvedQuestions: decoded.UnresolvedQuestions,
		ModelID: provider.profile.Model, ReportedModelID: reportedModel, Usage: usage}, nil
}

func invalidAPIOutput(reason string) error {
	return fmt.Errorf("%w: %s", ErrModelOutputInvalid, reason)
}

func decodeAnthropicCompletion(output []byte, options InferenceOptions) (string, string, Usage, error) {
	var envelope struct {
		Type       string            `json:"type"`
		Role       string            `json:"role"`
		Model      string            `json:"model"`
		StopReason string            `json:"stop_reason"`
		Content    []json.RawMessage `json:"content"`
		Usage      *struct {
			Input        *int `json:"input_tokens"`
			Output       *int `json:"output_tokens"`
			CacheCreated int  `json:"cache_creation_input_tokens"`
			CacheRead    int  `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if decodeSingleJSONObject(output, &envelope, false) != nil || envelope.Type != "message" || envelope.Role != "assistant" ||
		!validModelID(envelope.Model) || envelope.StopReason != "end_turn" || len(envelope.Content) != 1 {
		return "", "", Usage{}, invalidAPIOutput("invalid, refused, truncated or unsupported Anthropic completion")
	}
	var block struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if decodeSingleJSONObject(envelope.Content[0], &block, true) != nil || block.Type != "text" || strings.TrimSpace(block.Text) == "" {
		return "", "", Usage{}, invalidAPIOutput("Anthropic completion must contain exactly one text block")
	}
	var usage Usage
	if values := envelope.Usage; values != nil {
		if values.Input == nil || values.Output == nil || values.CacheCreated < 0 || values.CacheRead < 0 || *values.Input < 0 ||
			values.CacheCreated > options.ContextTokens || values.CacheRead > options.ContextTokens || *values.Input > options.ContextTokens {
			return "", "", Usage{}, invalidAPIOutput("invalid Anthropic token usage")
		}
		usage = Usage{PromptTokens: *values.Input + values.CacheCreated + values.CacheRead, OutputTokens: *values.Output}
		if !boundedAPIUsage(usage, options) {
			return "", "", Usage{}, invalidAPIOutput("Anthropic token usage exceeds request limits")
		}
	}
	return block.Text, envelope.Model, usage, nil
}

func decodeGeminiCompletion(output []byte, options InferenceOptions) (string, string, Usage, error) {
	var envelope struct {
		ModelVersion string `json:"modelVersion"`
		Candidates   []struct {
			FinishReason string `json:"finishReason"`
			Content      struct {
				Role  string            `json:"role"`
				Parts []json.RawMessage `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
		PromptFeedback json.RawMessage `json:"promptFeedback"`
		Usage          *struct {
			Prompt   *int `json:"promptTokenCount"`
			Output   *int `json:"candidatesTokenCount"`
			Thoughts int  `json:"thoughtsTokenCount"`
			Total    *int `json:"totalTokenCount"`
		} `json:"usageMetadata"`
	}
	if decodeSingleJSONObject(output, &envelope, false) != nil || len(envelope.Candidates) != 1 ||
		(envelope.ModelVersion != "" && !validModelID(envelope.ModelVersion)) {
		return "", "", Usage{}, invalidAPIOutput("invalid Gemini completion envelope")
	}
	if nonNullJSON(envelope.PromptFeedback) {
		var feedback map[string]json.RawMessage
		if decodeSingleJSONObject(envelope.PromptFeedback, &feedback, false) != nil || nonNullJSON(feedback["blockReason"]) {
			return "", "", Usage{}, invalidAPIOutput("Gemini prompt was blocked")
		}
	}
	candidate := envelope.Candidates[0]
	if candidate.FinishReason != "STOP" || candidate.Content.Role != "model" || len(candidate.Content.Parts) != 1 {
		return "", "", Usage{}, invalidAPIOutput("Gemini completion was refused, truncated or unsupported")
	}
	var part struct {
		Text string `json:"text"`
	}
	if decodeSingleJSONObject(candidate.Content.Parts[0], &part, true) != nil || strings.TrimSpace(part.Text) == "" {
		return "", "", Usage{}, invalidAPIOutput("Gemini completion must contain exactly one text part")
	}
	var usage Usage
	if values := envelope.Usage; values != nil {
		if values.Prompt == nil || values.Output == nil || values.Total == nil || values.Thoughts < 0 || *values.Output < 0 ||
			values.Thoughts > options.MaxOutputTokens || *values.Output > options.MaxOutputTokens {
			return "", "", Usage{}, invalidAPIOutput("invalid Gemini token usage")
		}
		usage = Usage{PromptTokens: *values.Prompt, OutputTokens: *values.Output + values.Thoughts}
		if !boundedAPIUsage(usage, options) || *values.Total != usage.PromptTokens+usage.OutputTokens {
			return "", "", Usage{}, invalidAPIOutput("Gemini token usage exceeds request limits")
		}
	}
	return part.Text, envelope.ModelVersion, usage, nil
}

func boundedAPIUsage(usage Usage, options InferenceOptions) bool {
	return usage.PromptTokens >= 0 && usage.PromptTokens <= options.ContextTokens && usage.OutputTokens >= 0 &&
		usage.OutputTokens <= options.MaxOutputTokens && usage.PromptTokens+usage.OutputTokens <= options.ContextTokens
}
