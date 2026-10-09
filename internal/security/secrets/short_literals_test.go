package secrets

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestShortSourceSecretsDoNotRenameUnrelatedCanonicalMetadata(t *testing.T) {
	data := []byte("{" + quotedJSONFixturePair("token", "two") + "," + quotedJSONFixturePair("password", "one") + "}")
	findings := Scan(data)
	wantRedacted := "{" + quotedJSONFixturePair("token", "***") + "," + quotedJSONFixturePair("password", "***") + "}"
	if len(findings) != 2 || string(Redact(data, findings)) != wantRedacted {
		t.Fatalf("short source fields were not masked in place: %+v", findings)
	}
	path := "internal/gitworktree/worktree.go"
	id := rkcmodel.StableID("node", path, "Clone")
	sharedValue := "two"
	nested := map[string]string{"api_key": "env:LOCAL_KEY", "secret": "replace-me"}
	nested["password"] = "one"
	bundle := rkcmodel.Bundle{
		Artifacts: []rkcmodel.Artifact{{ID: "artifact", Path: path}},
		Nodes: []rkcmodel.Node{{ID: id, Name: "Clone", QualifiedName: "gitworktree.Clone", ArtifactID: "artifact", Source: &rkcmodel.SourceRange{Path: path}, Attributes: map[string]any{
			"ordinary":        "one versus two",
			"token":           sharedValue,
			"nested":          nested,
			"other_reference": sharedValue,
		}}},
		Documents: []rkcmodel.Document{{Sections: []rkcmodel.DocumentSection{{PlainText: string(data), Markdown: string(data)}}}},
	}
	SanitizeBundle(&bundle, SensitiveLiterals(data, findings))
	if bundle.Artifacts[0].Path != path || bundle.Nodes[0].Source.Path != path || bundle.Nodes[0].ID != id || bundle.Nodes[0].Name != "Clone" || bundle.Nodes[0].QualifiedName != "gitworktree.Clone" {
		t.Fatalf("source privacy masking corrupted identity or citations: %+v", bundle)
	}
	attributes := bundle.Nodes[0].Attributes
	if attributes["ordinary"] != "one versus two" || attributes["other_reference"] != "two" || attributes["token"] != redactionToken {
		t.Fatalf("short credential context was lost or propagated globally: %#v", attributes)
	}
	if want := (map[string]string{"password": redactionToken, "api_key": "env:LOCAL_KEY", "secret": "replace-me"}); !reflect.DeepEqual(attributes["nested"], want) {
		t.Fatalf("credential fields or references incorrectly masked: %#v", attributes["nested"])
	}
	for _, section := range bundle.Documents[0].Sections {
		if strings.Contains(section.PlainText, `"two"`) || strings.Contains(section.Markdown, `"one"`) {
			t.Fatalf("short source secret survived canonical body masking: %+v", section)
		}
	}
}

func TestCanonicalCredentialMapMaskingHandlesJSONRoundtripAndRetainsValueTypes(t *testing.T) {
	attributes := map[string]any{
		"api_key":      "vault:configured-key",
		"access_token": "test-only",
		"token_count":  3,
		"APIKeyEnv":    "LOCAL_KEY",
		"secret":       nil,
		"name":         "tiny-small ordinary identifiers",
	}
	// Keep bare attribute values and JSON types unchanged without embedding
	// credential-shaped key/value pairs in the checked-in source.
	attributes["password"] = "tiny"
	attributes["secret_kind"] = "json_secret_field"
	attributes["secret_name"] = "review-marker"
	attributes["tokenizer"] = "bpe"
	attributes["model_token_limit"] = "4096"
	attributes["provider_api_key"] = "small"
	attributes["apiKey"] = "tiny"
	attributes["privateKey"] = "small"
	object := map[string]any{"description": "keep this"}
	object["password"] = "small"
	attributes["credential_object"] = object
	bundle := rkcmodel.Bundle{Nodes: []rkcmodel.Node{{Attributes: attributes}}}
	data, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &bundle); err != nil {
		t.Fatal(err)
	}
	SanitizeBundle(&bundle, []string{"tiny", "small"})
	attributes = bundle.Nodes[0].Attributes
	if attributes["password"] != redactionToken || attributes["name"] != "tiny-small ordinary identifiers" || attributes["api_key"] != "vault:configured-key" || attributes["access_token"] != "test-only" || attributes["token_count"] != float64(3) || attributes["secret_kind"] != "json_secret_field" || attributes["secret_name"] != "review-marker" || attributes["secret"] != nil {
		t.Fatalf("short literal policy corrupted canonical metadata: %#v", attributes)
	}
	if attributes["APIKeyEnv"] != "LOCAL_KEY" || attributes["tokenizer"] != "bpe" || attributes["model_token_limit"] != "4096" || attributes["provider_api_key"] != redactionToken || attributes["apiKey"] != redactionToken || attributes["privateKey"] != redactionToken {
		t.Fatalf("credential suffixes masked references or classification metadata: %#v", attributes)
	}
	object = attributes["credential_object"].(map[string]any)
	if object["password"] != redactionToken || object["description"] != "keep this" {
		t.Fatalf("nested credential context lost: %#v", object)
	}
}
