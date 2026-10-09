package modelruntime

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProviderProfileVersion identifies the standalone, portable model connection
// format. Profiles do not change deterministic extraction configuration.
const ProviderProfileVersion = "rkc.provider.v1"

// ProviderProfile stores connection policy, never the credential value. Remote
// execution is enabled only by AllowRemote; APIKeyEnv names a process environment
// variable. Model selection is explicit and does not assert account entitlement.
type ProviderProfile struct {
	SchemaVersion   string `json:"schema_version"`
	Provider        string `json:"provider"`
	Endpoint        string `json:"endpoint"`
	Model           string `json:"model"`
	Profile         string `json:"profile"`
	APIKeyEnv       string `json:"api_key_env,omitempty"`
	AllowRemote     bool   `json:"allow_remote"`
	ContextTokens   int    `json:"context_tokens"`
	MaxOutputTokens int    `json:"max_output_tokens"`
	TimeoutSeconds  int    `json:"timeout_seconds"`
}

// ProviderPreset describes a connection template. It deliberately has no model
// default: availability, schema support, account access, and cost vary by host.
type ProviderPreset struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Endpoint    string `json:"endpoint"`
	APIKeyEnv   string `json:"api_key_env,omitempty"`
	Profile     string `json:"profile"`
	Remote      bool   `json:"remote"`
	Description string `json:"description"`
}

// ProviderPresets returns a fresh catalog of built-in transport templates.
// A template is not a tested deployment, recommended model, or price promise.
func ProviderPresets() []ProviderPreset {
	return []ProviderPreset{
		{ID: "ollama", Name: "Ollama", Provider: "openai-compatible", Endpoint: "http://127.0.0.1:11434/v1/chat/completions", Profile: string(ProfileStructuredClaims), Description: "Existing local server; the selected model must support strict JSON schema."},
		{ID: "lm-studio", Name: "LM Studio", Provider: "openai-compatible", Endpoint: "http://127.0.0.1:1234/v1/chat/completions", Profile: string(ProfileStructuredClaims), Description: "Existing local server; enable its OpenAI-compatible API first."},
		{ID: "openai-compatible", Name: "Your model API", Provider: "openai-compatible", Endpoint: "http://127.0.0.1:8080/v1/chat/completions", Profile: string(ProfileStructuredClaims), Description: "Replace the endpoint with your exact chat-completions URL."},
		{ID: "openai", Name: "OpenAI API", Provider: "openai", Endpoint: "https://api.openai.com/v1/chat/completions", APIKeyEnv: "OPENAI_API_KEY", Profile: string(ProfileStructuredClaims), Remote: true, Description: "API credentials and a model supporting Chat Completions structured output."},
		{ID: "anthropic", Name: "Claude API", Provider: "anthropic", Endpoint: "https://api.anthropic.com/v1/messages", APIKeyEnv: "ANTHROPIC_API_KEY", Profile: string(ProfileStructuredClaims), Remote: true, Description: "Native Messages API with output_config JSON schema."},
		{ID: "gemini", Name: "Gemini API", Provider: "gemini", Endpoint: "https://generativelanguage.googleapis.com/v1beta/models/{model}:generateContent", APIKeyEnv: "GEMINI_API_KEY", Profile: string(ProfileStructuredClaims), Remote: true, Description: "Native GenerateContent API with JSON schema; API credentials are distinct from browser login."},
		{ID: "neuroforge", Name: "NeuroForge native API", Provider: "openai-compatible", Endpoint: "https://neuroforge.io/v1/chat/completions", APIKeyEnv: "NEUROFORGE_API_KEY", Profile: string(ProfileNeuroForgeNativeExtractive), Remote: true, Description: "Restricted exact source quotation profile; live access and price are not assumed."},
	}
}

// NewProviderProfile creates a template for an explicitly named model. Consent
// is never inferred from the preset; the caller chooses allowRemote explicitly.
func NewProviderProfile(presetID, model string, allowRemote bool) (ProviderProfile, error) {
	for _, preset := range ProviderPresets() {
		if preset.ID != presetID {
			continue
		}
		profile := ProviderProfile{SchemaVersion: ProviderProfileVersion, Provider: preset.Provider,
			Endpoint: strings.ReplaceAll(preset.Endpoint, "{model}", url.PathEscape(model)), Model: model,
			Profile: preset.Profile, APIKeyEnv: preset.APIKeyEnv, AllowRemote: allowRemote,
			ContextTokens: 4096, MaxOutputTokens: 768, TimeoutSeconds: 60}
		if preset.Profile == string(ProfileNeuroForgeNativeExtractive) {
			profile.ContextTokens, profile.MaxOutputTokens = 512, 128
		}
		// A template can be created before remote consent is supplied. All other
		// properties must already be valid; execution validates consent again.
		validation := profile
		validation.AllowRemote = preset.Remote || allowRemote
		if err := validation.Validate(); err != nil {
			return ProviderProfile{}, err
		}
		return profile, nil
	}
	return ProviderProfile{}, fmt.Errorf("unknown provider preset %q; use rkc providers list", presetID)
}

// ReadProviderProfile reads one bounded, duplicate-free strict JSON object.
func ReadProviderProfile(reader io.Reader) (ProviderProfile, error) {
	if reader == nil {
		return ProviderProfile{}, errors.New("provider profile reader is required")
	}
	data, err := io.ReadAll(io.LimitReader(reader, 32*1024+1))
	if err != nil || len(data) > 32*1024 {
		return ProviderProfile{}, errors.New("provider profile must be readable and no larger than 32 KiB")
	}
	var profile ProviderProfile
	if err := decodeSingleJSONObject(data, &profile, true); err != nil {
		return ProviderProfile{}, errors.New("provider profile must be one valid JSON object with only documented fields")
	}
	if err := profile.Validate(); err != nil {
		return ProviderProfile{}, err
	}
	return profile, nil
}

// Validate checks transport and request policy without reading credentials or
// opening a connection. Use CheckCredential for a separate local readiness check.
func (profile ProviderProfile) Validate() error {
	if profile.SchemaVersion != ProviderProfileVersion {
		return errors.New("provider profile schema_version must be rkc.provider.v1")
	}
	switch profile.Provider {
	case "openai-compatible", "openai", "anthropic", "gemini":
	default:
		return errors.New("provider must be openai-compatible, openai, anthropic, or gemini")
	}
	if !validModelID(profile.Model) {
		return errors.New("model must be nonempty printable text within 256 bytes")
	}
	if profile.Profile != string(ProfileStructuredClaims) && profile.Profile != string(ProfileNeuroForgeNativeExtractive) {
		return errors.New("profile must be structured-claims or neuroforge-native-extractive")
	}
	if profile.Profile == string(ProfileNeuroForgeNativeExtractive) && profile.Provider != "openai-compatible" {
		return errors.New("native extractive profile requires the openai-compatible provider")
	}
	if profile.ContextTokens < 512 || profile.ContextTokens > 262144 || profile.MaxOutputTokens < 1 || profile.MaxOutputTokens > profile.ContextTokens {
		return errors.New("context_tokens must be 512..262144 and max_output_tokens must be positive and no larger than context")
	}
	if profile.Profile == string(ProfileNeuroForgeNativeExtractive) && (profile.ContextTokens != 512 || profile.MaxOutputTokens > 128) {
		return errors.New("native extractive profile requires context_tokens 512 and max_output_tokens at most 128")
	}
	if profile.TimeoutSeconds < 1 || profile.TimeoutSeconds > 3600 {
		return errors.New("timeout_seconds must be between 1 and 3600")
	}
	if profile.APIKeyEnv != "" && !validCredentialEnvName(profile.APIKeyEnv) {
		return errors.New("api_key_env must name an environment variable, not contain a credential")
	}
	if profile.Provider != "openai-compatible" && profile.APIKeyEnv == "" {
		return errors.New("this provider requires an api_key_env environment variable name")
	}
	return validateAPIEndpoint(profile.Endpoint, profile.Provider, profile.Model, profile.AllowRemote)
}

func validModelID(model string) bool {
	return strings.TrimSpace(model) == model && model != "" && len(model) <= 256 && utf8.ValidString(model) && strings.IndexFunc(model, unicode.IsControl) < 0
}

func validCredentialEnvName(name string) bool {
	if len(name) == 0 || len(name) > 128 {
		return false
	}
	for i, character := range name {
		if character != '_' && !(character >= 'A' && character <= 'Z') && !(character >= 'a' && character <= 'z') && !(i > 0 && character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

func validateAPIEndpoint(endpoint, provider, model string, allowRemote bool) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || len(endpoint) > 4096 || !utf8.ValidString(endpoint) || strings.IndexFunc(endpoint, unicode.IsControl) >= 0 ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || parsed.RawFragment != "" || strings.Contains(endpoint, "#") ||
		parsed.RawPath != "" || strings.Contains(parsed.Path, "//") || strings.Contains(parsed.Path, "\\") {
		return errors.New("endpoint must be an exact HTTP(S) URL without credentials, query, fragment, or encoded path")
	}
	for _, component := range strings.Split(parsed.Path, "/") {
		if component == "." || component == ".." {
			return errors.New("endpoint path cannot contain relative components")
		}
	}
	suffix := "/chat/completions"
	switch provider {
	case "anthropic":
		suffix = "/messages"
	case "gemini":
		if strings.ContainsAny(model, "/\\:?#% ") {
			return errors.New("Gemini model must be a bare model ID without path or URL characters")
		}
		suffix = "/models/" + model + ":generateContent"
	}
	if !strings.HasSuffix(parsed.Path, suffix) {
		return fmt.Errorf("endpoint path must end with %s", suffix)
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return errors.New("endpoint port must be between 1 and 65535")
		}
	} else if strings.HasSuffix(parsed.Host, ":") {
		return errors.New("endpoint port cannot be empty")
	}
	ip := net.ParseIP(parsed.Hostname())
	if ip != nil && ip.IsLoopback() {
		return nil
	}
	if !allowRemote {
		return errors.New("remote endpoint requires explicit allow_remote consent; local endpoints must use an IP-literal loopback host")
	}
	if parsed.Scheme != "https" {
		return errors.New("remote endpoint requires HTTPS")
	}
	return nil
}

// MarshalProviderProfile returns readable JSON suitable for reviewing and
// copying between computers. APIKeyEnv is a reference, never a secret value.
func MarshalProviderProfile(profile ProviderProfile) ([]byte, error) {
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}
