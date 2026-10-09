package main

import (
	"errors"
	"time"

	"github.com/neuroforge-io/RKC/internal/modelruntime"
)

// openAnswerGenerationProvider keeps endpoint execution explicit and separate
// from the qualified, digest-bound local process used by synthesize. HTTP
// descriptors report an external service; they never assert model qualification
// or a measured server memory ceiling.
func openAnswerGenerationProvider(request qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
	if request.Provider != "openai-compatible" {
		return openQualifiedGenerationProvider(request)
	}
	if request.Temperature != 0 {
		return nil, errors.New("model.temperature must be 0 for endpoint answers")
	}
	if request.ContextTokens < 512 || request.ContextTokens > 262_144 {
		return nil, errors.New("context must be between 512 and 262144 tokens")
	}
	if request.MaximumOutputTokens < 1 || request.MaximumOutputTokens > request.ContextTokens {
		return nil, errors.New("max-output must be positive and no larger than context")
	}
	if request.Timeout <= 0 || request.Timeout > time.Hour {
		return nil, errors.New("timeout must be positive and no greater than 1h")
	}
	provider, err := modelruntime.NewOpenAICompatibleProvider(modelruntime.OpenAICompatibleConfig{
		Endpoint: request.Endpoint, Model: request.HTTPModelName,
		Profile:      modelruntime.OpenAICompatibleProfile(request.HTTPProfile),
		ContextLimit: request.ContextTokens, Timeout: request.Timeout,
		ResponseSchema: qualifiedClaimResponseSchema,
	})
	if err != nil {
		return nil, err
	}
	if maximum := provider.Capabilities().MaximumOutputTokens; maximum > 0 && request.MaximumOutputTokens > maximum {
		_ = provider.Close()
		return nil, errors.New("max-output exceeds the explicitly selected endpoint profile limit")
	}
	schemaDigest := qualifiedResponseSchemaSHA256()
	if !provider.Capabilities().JSONSchemaOutput {
		schemaDigest = ""
	}
	return &qualifiedGenerationSession{
		Provider: provider, ProviderName: "openai-compatible-http",
		Descriptor: provider.Descriptor(),
		Inference: modelruntime.InferenceOptions{
			ContextTokens:   request.ContextTokens,
			MaxOutputTokens: request.MaximumOutputTokens, Parallel: 1,
		},
		ResponseSchemaSHA256: schemaDigest,
	}, nil
}
