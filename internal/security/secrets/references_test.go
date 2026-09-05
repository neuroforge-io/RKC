package secrets

import (
	"strings"
	"testing"
)

func TestConfigReferencesAreNotCredentialLiterals(t *testing.T) {
	key := strings.Join([]string{"api", "key"}, "_")
	for _, value := range []string{"config.high_level.api_key", "config.vision_language.api_key", "env.AUDIO_STUDY_TURNSTILE_SECRET", "settings.service_token", "process.env.SERVICE_TOKEN"} {
		for _, separator := range []string{" = ", ": "} {
			if findings := Scan([]byte(key + separator + value + ",")); len(findings) != 0 {
				t.Fatalf("source reference treated as a credential: %q", value)
			}
			if findings := Scan([]byte(key + separator + "\"" + value + "\"")); len(findings) != 1 {
				t.Fatalf("quoted credential-shaped value was suppressed: %q", value)
			}
		}
	}
	if findings := Scan([]byte(key + " = " + "other.longcredentialvalue")); len(findings) != 1 {
		t.Fatal("arbitrary dotted credential was suppressed")
	}
}
