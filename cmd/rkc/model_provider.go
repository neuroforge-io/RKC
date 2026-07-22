package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/neuroforge-io/RKC/internal/modelassets"
	"github.com/neuroforge-io/RKC/internal/modelruntime"
)

// qualifiedClaimResponseSchema matches the generation response contract used
// by models/qualification/rkc-local-model-v1.json. It deliberately permits only
// evidence-bound supported claims; inferred prose is not part of the qualified
// production profile.
const qualifiedClaimResponseSchema = `{"type":"object","additionalProperties":false,"required":["claims","unresolved_questions"],"properties":{"claims":{"type":"array","maxItems":8,"items":{"type":"object","additionalProperties":false,"required":["text","category","certainty","evidence_ids"],"properties":{"text":{"type":"string","maxLength":1200},"category":{"type":"string","enum":["purpose","signature","error","relationship","constraint"]},"certainty":{"const":"supported"},"evidence_ids":{"type":"array","minItems":1,"maxItems":8,"uniqueItems":true,"items":{"type":"string"}}}}},"unresolved_questions":{"type":"array","maxItems":8,"items":{"type":"string","maxLength":500}}}}`

type qualifiedGenerationRequest struct {
	Provider            string
	ModelPath           string
	LlamaCLI            string
	ModelLock           string
	ModelAsset          string
	RuntimeReceipt      string
	ContextTokens       int
	MaximumOutputTokens int
	MaximumRSSMiB       int64
	Threads             int
	BatchSize           int
	Timeout             time.Duration
	Temperature         float64
}

// qualifiedGenerationSession owns one verified provider and records every
// derived setting needed by callers' audit envelopes. It never owns repository
// or dataset paths.
type qualifiedGenerationSession struct {
	Provider             modelruntime.Provider
	ProviderName         string
	Descriptor           modelruntime.ModelDescriptor
	Binding              modelassets.GenerationBinding
	Budget               modelruntime.Budget
	Inference            modelruntime.InferenceOptions
	ResponseSchemaSHA256 string
}

func qualifiedStructuredGenerationArguments() []string {
	return []string{
		"--json-schema", qualifiedClaimResponseSchema,
		"--conversation", "--single-turn", "--jinja",
		"--chat-template-kwargs", `{"enable_thinking":false}`,
		"--reasoning", "off", "--reasoning-format", "none", "--reasoning-budget", "0",
		"--offline", "--no-perf", "--poll", "0", "--poll-batch", "0",
	}
}

func qualifiedResponseSchemaSHA256() string {
	digest := sha256.Sum256([]byte(qualifiedClaimResponseSchema))
	return hex.EncodeToString(digest[:])
}

func defaultSynthesisModelLockPath() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return ""
	}
	binDirectory := filepath.Dir(executable)
	candidates := []string{
		filepath.Join(filepath.Dir(binDirectory), "models", "models.lock.json"),
		filepath.Join(filepath.Dir(binDirectory), "share", "rkc", "models", "models.lock.json"),
	}
	for _, candidate := range candidates {
		info, err := os.Lstat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return candidate
		}
	}
	return ""
}

func openQualifiedGenerationProvider(request qualifiedGenerationRequest) (*qualifiedGenerationSession, error) {
	selectedProvider := strings.TrimSpace(request.Provider)
	if selectedProvider == "" || selectedProvider == "disabled" {
		if strings.TrimSpace(request.ModelPath) == "" {
			return nil, errors.New("model provider is disabled; configure a qualified model explicitly")
		}
		selectedProvider = "llama.cpp"
	}
	if selectedProvider != "llama.cpp" {
		return nil, fmt.Errorf("provider %q is not implemented", selectedProvider)
	}
	if strings.TrimSpace(request.ModelPath) == "" {
		return nil, errors.New("--model is required for the llama.cpp provider")
	}
	if request.Temperature != 0 {
		return nil, errors.New("model.temperature must be 0 for deterministic qualified generation")
	}
	if request.ContextTokens < 512 || request.ContextTokens > 262_144 {
		return nil, errors.New("context must be between 512 and 262144 tokens")
	}
	if request.MaximumOutputTokens < 1 || request.MaximumOutputTokens > request.ContextTokens {
		return nil, errors.New("max-output must be positive and no larger than context")
	}
	if request.MaximumRSSMiB < 256 || request.MaximumRSSMiB > modelMaximumRSSMiB {
		return nil, errors.New("max-rss-mib must be between 256 and the 2560 MiB safety ceiling")
	}
	if request.Threads < 0 || request.Threads > 64 {
		return nil, errors.New("threads must be between 0 and 64")
	}
	if request.BatchSize < 1 || request.BatchSize > 4_096 {
		return nil, errors.New("batch-size must be between 1 and 4096")
	}
	if request.Timeout <= 0 || request.Timeout > time.Hour {
		return nil, errors.New("timeout must be positive and no greater than 1h")
	}

	binding, err := modelassets.ResolveGeneration(modelassets.GenerationRequest{
		LockPath:           request.ModelLock,
		RuntimeReceiptPath: request.RuntimeReceipt,
		ExecutablePath:     request.LlamaCLI,
		ModelPath:          request.ModelPath,
		AssetID:            request.ModelAsset,
	})
	if err != nil {
		return nil, err
	}
	if request.ContextTokens > binding.NativeContextTokens {
		return nil, fmt.Errorf(
			"requested context %d exceeds locked model context %d",
			request.ContextTokens,
			binding.NativeContextTokens,
		)
	}
	budget := modelruntime.Budget{
		MaximumRSSBytes:   request.MaximumRSSMiB * 1024 * 1024,
		SafetyMarginBytes: 128 * 1024 * 1024,
	}
	provider, err := modelruntime.NewLlamaCPPProvider(modelruntime.LlamaCPPConfig{
		Executable:               binding.ExecutablePath,
		ModelPath:                binding.ModelPath,
		ExpectedExecutableSHA256: binding.RuntimeSHA256,
		ExpectedModelSHA256:      binding.ModelSHA256,
		ModelID:                  binding.AssetID,
		ModelRevision:            binding.ModelRevision,
		ModelLicense:             binding.ModelLicense,
		RuntimeRevision:          binding.RuntimeRevision,
		ContextLimit:             binding.NativeContextTokens,
		QuantizationBits:         binding.QuantizationBits,
		Threads:                  request.Threads,
		BatchSize:                request.BatchSize,
		Seed:                     1,
		Temperature:              0,
		Timeout:                  request.Timeout,
		Budget:                   budget,
		AdditionalArguments:      qualifiedStructuredGenerationArguments(),
	})
	if err != nil {
		return nil, err
	}
	descriptor := provider.Descriptor()
	if err := validateSynthesisDescriptor(binding, descriptor); err != nil {
		return nil, errors.Join(err, provider.Close())
	}
	return &qualifiedGenerationSession{
		Provider:     provider,
		ProviderName: "llamacpp-cli",
		Descriptor:   descriptor,
		Binding:      binding,
		Budget:       budget,
		Inference: modelruntime.InferenceOptions{
			ContextTokens:   request.ContextTokens,
			MaxOutputTokens: request.MaximumOutputTokens,
			Threads:         request.Threads,
			BatchSize:       request.BatchSize,
			Parallel:        1,
		},
		ResponseSchemaSHA256: qualifiedResponseSchemaSHA256(),
	}, nil
}

func (session *qualifiedGenerationSession) Close() error {
	if session == nil || session.Provider == nil {
		return nil
	}
	return session.Provider.Close()
}

func validateSynthesisDescriptor(binding modelassets.GenerationBinding, descriptor modelruntime.ModelDescriptor) error {
	if descriptor.ID != binding.AssetID || descriptor.Architecture != "gguf" || descriptor.WeightBytes != binding.ModelSizeBytes ||
		descriptor.ContextLimit != binding.NativeContextTokens || descriptor.QuantizationBits != binding.QuantizationBits ||
		descriptor.Digest != "sha256:"+binding.ModelSHA256 || descriptor.Revision != binding.ModelRevision || descriptor.License != binding.ModelLicense ||
		descriptor.Runtime != "llama.cpp-cli" || descriptor.RuntimeDigest != "sha256:"+binding.RuntimeSHA256 || descriptor.RuntimeRevision != binding.RuntimeRevision {
		return errors.New("llama.cpp provider descriptor does not match the resolved model/runtime binding")
	}
	return nil
}
