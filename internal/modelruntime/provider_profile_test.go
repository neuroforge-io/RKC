package modelruntime

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func TestProviderPresetsRequireExplicitModelAndRemoteConsent(t *testing.T) {
	for _, preset := range ProviderPresets() {
		profile, err := NewProviderProfile(preset.ID, "fictional-model", preset.Remote)
		if err != nil {
			t.Fatalf("preset %s: %v", preset.ID, err)
		}
		if err := profile.Validate(); err != nil {
			t.Fatal(err)
		}
		data, err := MarshalProviderProfile(profile)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := ReadProviderProfile(strings.NewReader(string(data)))
		if err != nil || decoded != profile {
			t.Fatalf("round trip=%+v error=%v", decoded, err)
		}
		if preset.Remote {
			profile.AllowRemote = false
			if err := profile.Validate(); err == nil {
				t.Fatalf("%s inferred remote consent", preset.ID)
			}
		}
		if _, err := NewProviderProfile(preset.ID, "", preset.Remote); err == nil {
			t.Fatalf("%s selected an implicit model", preset.ID)
		}
	}
	if _, err := NewProviderProfile("guess", "fictional", true); err == nil {
		t.Fatal("unknown preset accepted")
	}
	first := ProviderPresets()
	first[0].ID = "changed"
	if ProviderPresets()[0].ID == "changed" {
		t.Fatal("mutable shared catalog")
	}
}

func TestProviderProfileRejectsInvalidPolicy(t *testing.T) {
	base, _ := NewProviderProfile("openai", "fictional-model", true)
	tests := []struct {
		name   string
		mutate func(*ProviderProfile)
	}{
		{"version", func(p *ProviderProfile) { p.SchemaVersion = "future" }},
		{"provider", func(p *ProviderProfile) { p.Provider = "guess" }},
		{"model whitespace", func(p *ProviderProfile) { p.Model = " fictional" }},
		{"model control", func(p *ProviderProfile) { p.Model = "fictional\n" }},
		{"model size", func(p *ProviderProfile) { p.Model = strings.Repeat("x", 257) }},
		{"profile", func(p *ProviderProfile) { p.Profile = "plaintext" }},
		{"native wire", func(p *ProviderProfile) { p.Profile = string(ProfileNeuroForgeNativeExtractive) }},
		{"context", func(p *ProviderProfile) { p.ContextTokens = 511 }},
		{"output", func(p *ProviderProfile) { p.MaxOutputTokens = 0 }},
		{"output context", func(p *ProviderProfile) { p.MaxOutputTokens = 4097 }},
		{"timeout", func(p *ProviderProfile) { p.TimeoutSeconds = 0 }},
		{"timeout overflow", func(p *ProviderProfile) { p.TimeoutSeconds = math.MaxInt }},
		{"credential value", func(p *ProviderProfile) { p.APIKeyEnv = "sk-fake-secret" }},
		{"credential omitted", func(p *ProviderProfile) { p.APIKeyEnv = "" }},
		{"remote consent", func(p *ProviderProfile) { p.AllowRemote = false }},
		{"remote HTTP", func(p *ProviderProfile) { p.Endpoint = "http://example.test/v1/chat/completions" }},
		{"URL credential", func(p *ProviderProfile) { p.Endpoint = "https://user:private@example.test/v1/chat/completions" }},
		{"URL query", func(p *ProviderProfile) { p.Endpoint += "?key=private" }},
		{"URL fragment", func(p *ProviderProfile) { p.Endpoint += "#" }},
		{"URL encoded path", func(p *ProviderProfile) { p.Endpoint = "https://example.test/v1/%63hat/completions" }},
		{"URL relative", func(p *ProviderProfile) { p.Endpoint = "https://example.test/../chat/completions" }},
		{"URL wrong path", func(p *ProviderProfile) { p.Endpoint = "https://example.test/v1/completions" }},
		{"URL duplicate slash", func(p *ProviderProfile) { p.Endpoint = "https://example.test//v1/chat/completions" }},
		{"URL port", func(p *ProviderProfile) { p.Endpoint = "https://example.test:65536/v1/chat/completions" }},
		{"URL empty port", func(p *ProviderProfile) { p.Endpoint = "https://example.test:/v1/chat/completions" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			profile := base
			test.mutate(&profile)
			if profile.Validate() == nil {
				t.Fatal("invalid policy accepted")
			}
		})
	}
	local, _ := NewProviderProfile("ollama", "fictional-model", false)
	local.Endpoint = "http://localhost:11434/v1/chat/completions"
	if local.Validate() == nil {
		t.Fatal("local hostname bypass accepted")
	}
	local.Endpoint = "http://[::1]:11434/v1/chat/completions"
	if err := local.Validate(); err != nil {
		t.Fatal(err)
	}
	native, _ := NewProviderProfile("neuroforge", "fictional-model", true)
	native.MaxOutputTokens = 129
	if native.Validate() == nil {
		t.Fatal("native output cap ignored")
	}
	gemini, _ := NewProviderProfile("gemini", "fictional-model", true)
	gemini.Model = "models/fictional-model"
	if gemini.Validate() == nil {
		t.Fatal("Gemini model path accepted")
	}
}

type brokenProfileReader struct{}

func (brokenProfileReader) Read([]byte) (int, error) { return 0, errors.New("synthetic read failure") }

func TestProviderProfileReadsOnlyOneBoundedStrictObject(t *testing.T) {
	profile, _ := NewProviderProfile("ollama", "fictional-model", false)
	data, _ := MarshalProviderProfile(profile)
	for _, invalid := range []string{
		"[]", "{}", "null", string(data) + "{}",
		strings.Replace(string(data), `"provider": "openai-compatible"`, `"provider": "openai-compatible", "provider": "openai"`, 1),
		strings.Replace(string(data), `"schema_version"`, `"api_key": "fictional-secret", "schema_version"`, 1),
		strings.Repeat(" ", 32*1024+1),
	} {
		if _, err := ReadProviderProfile(strings.NewReader(invalid)); err == nil {
			t.Errorf("accepted invalid input %.80s", invalid)
		}
	}
	if _, err := ReadProviderProfile(nil); err == nil {
		t.Fatal("nil reader accepted")
	}
	if _, err := ReadProviderProfile(brokenProfileReader{}); err == nil {
		t.Fatal("read failure ignored")
	}
	for _, name := range []string{"RKC_API_KEY", "_key2", "lower_key"} {
		if !validCredentialEnvName(name) {
			t.Fatal(name)
		}
	}
	for _, name := range []string{"", "1KEY", "KEY=secret", "KEY\n", strings.Repeat("x", 129)} {
		if validCredentialEnvName(name) {
			t.Fatal(fmt.Sprintf("invalid name %q", name))
		}
	}
}
