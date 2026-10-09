package secrets

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestQuotedJSONKeysEscapesShortValuesAndReferences(t *testing.T) {
	t.Parallel()
	// Assemble raw JSON pairs at runtime so the checked-in source has no
	// credential-shaped key/value pair, preserving every escape and source byte.
	data := []byte("{" + strings.Join([]string{
		quotedJSONFixturePair("password", "short"),
		`"nested":{` + quotedJSONFixturePair("accessToken", `a\"secret\\value`) + "}",
		quotedJSONFixturePair(`\u0061pi_key`, "escaped-field-private"),
		quotedJSONFixturePair("description", "useful retained description"),
		quotedJSONFixturePair("token", "env:TOKEN"),
		quotedJSONFixturePair("secret", "replace-me"),
	}, ",") + "}")
	findings := Scan(data)
	if len(findings) != 3 {
		t.Fatalf("quoted-key findings = %+v, want 3", findings)
	}
	redacted := Redact(data, findings)
	if len(redacted) != len(data) || !bytes.Contains(redacted, []byte("useful retained description")) || !bytes.Contains(redacted, []byte("env:TOKEN")) {
		t.Fatalf("quoted JSON redaction damaged source identity or safe data: %q", redacted)
	}
	for _, value := range []string{"short", "secret\\\\value", "escaped-field-private"} {
		if strings.Contains(string(redacted), value) {
			t.Fatalf("quoted JSON field survived redaction: %q", value)
		}
	}
	for _, finding := range findings {
		if finding.Kind != "json_secret_field" || finding.StartByte < 0 || finding.EndByte > len(data) || finding.KeyName == "" {
			t.Fatalf("invalid quoted-field receipt: %+v", finding)
		}
	}
}

func TestContextualSecretScanningAndRedactionPreserveCoordinates(t *testing.T) {
	t.Parallel()
	data := []byte("a\r\n" + quotedJSONFixturePair("password", "value-private-8429") + "\n")
	findings, err := ScanContext(context.Background(), data)
	if err != nil || len(findings) != 1 {
		t.Fatalf("contextual findings = %+v, %v", findings, err)
	}
	start := strings.Index(string(data), "value-private-8429")
	want := makeFinding(data, start, start+len("value-private-8429"), "json_secret_field", .9, "password")
	if findings[0] != want {
		t.Fatalf("indexed line coordinates differ: got %+v want %+v", findings[0], want)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := ScanContext(ctx, data); err != context.Canceled || got != nil {
		t.Fatalf("cancelled scan = %+v, %v", got, err)
	}
	if got, err := RedactContext(ctx, data, findings); err != context.Canceled || got != nil {
		t.Fatalf("cancelled redaction exposed partial bytes: %q, %v", got, err)
	}
	if _, err := ScanContext(nil, data); err == nil {
		t.Fatal("nil scan context succeeded")
	}
	if _, err := RedactContext(nil, data, findings); err == nil {
		t.Fatal("nil redaction context succeeded")
	}
}

// Keys and values are already JSON-escaped fragments. Deliberately avoid
// reserialization: the Unicode-key and escaped-value tests bind raw ranges.
func quotedJSONFixturePair(rawKey, rawValue string) string {
	return `"` + rawKey + `":"` + rawValue + `"`
}
