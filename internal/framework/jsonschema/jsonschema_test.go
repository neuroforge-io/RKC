package jsonschema

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

func TestExtractJSONSchemaRichDocumentAndDeterminism(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	document := `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:example:root",
  "title": "Root",
  "type": "object",
  "required": ["id"],
  "properties": {
    "child": {"$ref": "#/$defs/Thing~1A~0B"},
    "id": {"type": ["string", "null"], "format": "uuid", "default": "none", "readOnly": true},
    "missing": {"allOf": [{"$ref": "#/missing"}]},
    "options": {"enum": ["a", "b"], "default": {"unsafe": "object"}}
  },
  "$defs": {
    "Thing/A~B": {
      "type": "object",
      "properties": {"value": {"const": 3}}
    }
  }
}`
	writeSchemaTestFile(t, root, "schemas/root.schema.json", document)
	writeSchemaTestFile(t, root, "schemas/ignored.json", `{"ordinary":true}`)
	files := []pluginapi.FileRef{
		{ArtifactID: "ignored", Path: "schemas/ignored.json", SHA256: "sha-ignored"},
		{ArtifactID: "schema", Path: "schemas/root.schema.json", SHA256: "sha-schema"},
	}

	got, err := Extract(Options{Root: root, Files: files})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	reversed, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{files[1], files[0]}})
	if err != nil {
		t.Fatalf("Extract(reversed) error = %v", err)
	}
	if !reflect.DeepEqual(got, reversed) {
		t.Fatal("Extract() output depends on input file order")
	}
	if len(got.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", got.Diagnostics)
	}

	byName := map[string]map[string]any{}
	for _, node := range got.Nodes {
		byName[node.Name] = node.Attributes
	}
	for _, name := range []string{"Root", "Thing/A~B", "child", "id", "missing", "options", "value", "#/missing"} {
		if _, ok := byName[name]; !ok {
			t.Errorf("missing node %q", name)
		}
	}
	if got := byName["id"]["required"]; got != true {
		t.Errorf("id.required = %#v, want true", got)
	}
	if got := byName["id"]["type"]; !reflect.DeepEqual(got, []string{"null", "string"}) {
		t.Errorf("id.type = %#v, want sorted nullable type", got)
	}
	if got := byName["options"]["default"]; got != nil {
		t.Errorf("object default retained as %#v, want nil", got)
	}

	resolved, unresolved := 0, 0
	for _, edge := range got.Edges {
		if edge.Kind != "references" {
			continue
		}
		switch edge.Resolution {
		case "declared":
			resolved++
		case "unresolved":
			unresolved++
			if edge.Confidence != 0.5 {
				t.Errorf("unresolved edge confidence = %v, want 0.5", edge.Confidence)
			}
		}
	}
	if resolved == 0 || unresolved == 0 {
		t.Errorf("reference resolution counts: declared=%d unresolved=%d", resolved, unresolved)
	}
}

func TestExtractJSONSchemaDiagnosticsAndClassification(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSchemaTestFile(t, root, "broken.schema.json", `{not-json`)
	writeSchemaTestFile(t, root, "broken.json", `{not-json`)
	writeSchemaTestFile(t, root, "openapi.schema.json", `{"openapi":"3.1.0","paths":{},"properties":{"x":{"type":"string"}}}`)

	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{
		{ArtifactID: "ordinary", Path: "broken.json", SHA256: "one"},
		{ArtifactID: "schema", Path: "broken.schema.json", SHA256: "two"},
		{ArtifactID: "openapi", Path: "openapi.schema.json", SHA256: "three"},
		{ArtifactID: "missing", Path: "missing.schema.json", SHA256: "four"},
	}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	codes := map[string]int{}
	for _, diagnostic := range fragment.Diagnostics {
		codes[diagnostic.Code]++
	}
	if codes["RKC-SCH-1001"] != 1 || codes["RKC-SCH-1002"] != 1 {
		t.Fatalf("diagnostic codes = %#v, want one read and one parse error", codes)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("malformed, ordinary, and OpenAPI inputs produced %d schema nodes", len(fragment.Nodes))
	}
}

func TestExtractJSONSchemaRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSchemaTestFile(t, root, "trailing.schema.json", `{"$schema":"urn:test","properties":{}} {"second":true}`)

	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "schema", Path: "trailing.schema.json", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("JSON with trailing document produced %d nodes", len(fragment.Nodes))
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-SCH-1002" {
		t.Fatalf("diagnostics = %#v, want one RKC-SCH-1002", fragment.Diagnostics)
	}
}

func TestExtractJSONSchemaRejectsPathsOutsideRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "repository")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSchemaTestFile(t, parent, "outside.schema.json", `{"$schema":"urn:test","title":"Outside"}`)

	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "outside", Path: "../outside.schema.json", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("outside-root path produced %d nodes", len(fragment.Nodes))
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-SCH-1001" {
		t.Fatalf("diagnostics = %#v, want one RKC-SCH-1001", fragment.Diagnostics)
	}
}

func TestJSONSchemaHelpersBoundaries(t *testing.T) {
	t.Parallel()
	if got := escape("a~/b"); got != "a~0~1b" {
		t.Errorf("escape() = %q", got)
	}
	if got := schemaType(map[string]any{"const": json.Number("1")}); got != "const" {
		t.Errorf("const schema type = %#v", got)
	}
	if got := schemaType(map[string]any{"enum": []any{"x"}}); got != "enum" {
		t.Errorf("enum schema type = %#v", got)
	}
	if got := schemaType(nil); got != "any" {
		t.Errorf("empty schema type = %#v", got)
	}
	if got := propertySignature("field", map[string]any{"type": "integer"}, false); got != "field?: integer" {
		t.Errorf("optional signature = %q", got)
	}
	if got := safeScalar([]any{"not", "scalar"}); got != nil {
		t.Errorf("safeScalar(array) = %#v", got)
	}
	refs := collectRefs(map[string]any{
		"z": []any{map[string]any{"$ref": "#/z"}, map[string]any{"$ref": "#/a"}},
		"a": map[string]any{"$ref": "#/z"},
	})
	if !reflect.DeepEqual(refs, []string{"#/a", "#/z"}) {
		t.Errorf("collectRefs() = %#v", refs)
	}
	if !likelySchemaPath("SCHEMA.JSON") || likelySchemaPath("data.json") {
		t.Error("likelySchemaPath classification mismatch")
	}
}

func writeSchemaTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func fragmentContainsText(fragment any, text string) bool {
	encoded, _ := json.Marshal(fragment)
	return strings.Contains(string(encoded), text)
}
