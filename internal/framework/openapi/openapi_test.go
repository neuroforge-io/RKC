package openapi

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestExtractOpenAPIRichDocumentsAndDeterminism(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeOpenAPITestFile(t, root, "api/openapi.json", `{
  "openapi": "3.1.0",
  "info": {"title": "Inventory", "version": "2.0", "description": "inventory API"},
  "servers": [{"url": "https://z.example"}, {"url": "https://a.example"}],
  "components": {
    "schemas": {
      "Base": {"type": "object", "properties": {"id": {"type": "string"}}},
      "Item": {"allOf": [{"$ref": "#/components/schemas/Base"}], "deprecated": true}
    },
    "securitySchemes": {
      "BearerAuth": {"type": "http", "scheme": "bearer"}
    }
  },
  "paths": {
    "/items": {
      "parameters": [{"name": "trace", "in": "header", "type": "string"}],
      "get": {
        "operationId": "listItems",
        "summary": "List items",
        "tags": ["z", "a"],
        "parameters": [
          {"name": "limit", "in": "query", "schema": {"type": "integer"}},
          {"$ref": "#/components/parameters/Missing"}
        ],
        "requestBody": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
        "responses": {
          "200": {"description": "ok", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Item"}}}},
          "default": {"description": "error", "content": {"application/json": {"schema": {"$ref": "#/components/schemas/Missing"}}}}
        },
        "security": [{"BearerAuth": []}, {"MissingAuth": []}]
      },
      "post": {"deprecated": true, "responses": {"204": {"description": "done"}}}
    }
  }
}`)
	writeOpenAPITestFile(t, root, "api/swagger.json", `{
  "swagger": "2.0",
  "info": {"title": "Legacy", "version": "1"},
  "basePath": "/v1",
  "definitions": {"Old": {"type": "object", "required": ["id"], "properties": {"id": {"type": "integer"}}}},
  "securityDefinitions": {"Key": {"type": "apiKey", "in": "header", "name": "X-Key"}},
  "paths": {"/old": {"get": {"responses": {"200": {"description": "ok", "schema": {"$ref": "#/definitions/Old"}}}, "security": [{"Key": []}]}}}
}`)
	files := []pluginapi.FileRef{
		{ArtifactID: "swagger", Path: "api/swagger.json", SHA256: "sha-swagger"},
		{ArtifactID: "openapi", Path: "api/openapi.json", SHA256: "sha-openapi"},
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

	wanted := map[string]bool{
		"Inventory": false, "Legacy": false, "Base": false, "Item": false, "Old": false,
		"BearerAuth": false, "Key": false, "GET /items": false, "POST /items": false,
		"GET /old": false, "trace": false, "limit": false, "200": false, "default": false,
		"#/components/parameters/Missing": false, "#/components/schemas/Missing": false, "MissingAuth": false,
	}
	for _, node := range got.Nodes {
		if _, ok := wanted[node.Name]; ok {
			wanted[node.Name] = true
		}
		if node.Name == "Inventory" {
			if got := node.Attributes["servers"]; !reflect.DeepEqual(got, []string{"https://a.example", "https://z.example"}) {
				t.Errorf("servers = %#v, want sorted URLs", got)
			}
		}
		if node.Name == "POST /items" && node.Stability != "deprecated" {
			t.Errorf("deprecated endpoint stability = %q", node.Stability)
		}
	}
	for name, found := range wanted {
		if !found {
			t.Errorf("missing node %q", name)
		}
	}

	edgeKinds := map[string]int{}
	resolution := map[string]int{}
	for _, edge := range got.Edges {
		edgeKinds[edge.Kind]++
		resolution[edge.Resolution]++
		if edge.Resolution == "unresolved" && edge.Confidence != 0.5 {
			t.Errorf("unresolved edge confidence = %v, want 0.5", edge.Confidence)
		}
	}
	for _, kind := range []string{"exposes", "declares", "references", "deserializes", "serializes", "authenticates"} {
		if edgeKinds[kind] == 0 {
			t.Errorf("missing edge kind %q", kind)
		}
	}
	if resolution["declared"] == 0 || resolution["unresolved"] == 0 {
		t.Errorf("edge resolutions = %#v", resolution)
	}
}

func TestExtractOpenAPIDiagnosticsAndClassification(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeOpenAPITestFile(t, root, "broken.openapi.json", `{broken`)
	writeOpenAPITestFile(t, root, "broken.json", `{broken`)
	writeOpenAPITestFile(t, root, "ordinary.json", `{"openapi":"3.1.0","paths":{}}`)
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{
		{ArtifactID: "broken-api", Path: "broken.openapi.json", SHA256: "one"},
		{ArtifactID: "broken-data", Path: "broken.json", SHA256: "two"},
		{ArtifactID: "ordinary", Path: "ordinary.json", SHA256: "three"},
		{ArtifactID: "missing", Path: "missing.swagger.json", SHA256: "four"},
	}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	codes := map[string]int{}
	for _, diagnostic := range fragment.Diagnostics {
		codes[diagnostic.Code]++
	}
	if codes["RKC-API-1001"] != 1 || codes["RKC-API-1002"] != 1 {
		t.Fatalf("diagnostic codes = %#v, want one read and one parse error", codes)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("invalid/non-API files produced %d nodes", len(fragment.Nodes))
	}
}

func TestExtractOpenAPIRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeOpenAPITestFile(t, root, "openapi.json", `{"openapi":"3.1.0","paths":{"/":{"get":{"responses":{}}}}} {}`)
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "api", Path: "openapi.json", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("OpenAPI with trailing JSON produced %d nodes", len(fragment.Nodes))
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-API-1002" {
		t.Fatalf("diagnostics = %#v, want one RKC-API-1002", fragment.Diagnostics)
	}
}

func TestExtractOpenAPIRejectsPathsOutsideRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "repository")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeOpenAPITestFile(t, parent, "openapi.json", `{"openapi":"3.1.0","paths":{"/":{"get":{"responses":{}}}}}`)
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "outside", Path: "../openapi.json", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("outside-root path produced %d nodes", len(fragment.Nodes))
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-API-1001" {
		t.Fatalf("diagnostics = %#v, want one RKC-API-1001", fragment.Diagnostics)
	}
}

func TestOpenAPIHelpersBoundaries(t *testing.T) {
	t.Parallel()
	if !likelyOpenAPIPath("SWAGGER.JSON") || !likelyOpenAPIPath("my-openapi.json") || likelyOpenAPIPath("api.json") {
		t.Error("likelyOpenAPIPath classification mismatch")
	}
	if isOpenAPI(map[string]any{"openapi": "3.1.0", "paths": map[string]any{}}) {
		t.Error("document with no paths classified as OpenAPI surface")
	}
	if got := parameterSignature(map[string]any{"name": "id", "in": "path"}, map[string]any{"type": "integer"}); got != "path id integer" {
		t.Errorf("parameterSignature() = %q", got)
	}
	if got := parameterSignature(nil, nil); got != "unknown parameter any" {
		t.Errorf("empty parameterSignature() = %q", got)
	}
	if got := confidence("declared"); got != 1 {
		t.Errorf("declared confidence = %v", got)
	}
	if got := confidence("unresolved"); got != 0.5 {
		t.Errorf("unresolved confidence = %v", got)
	}
	if got := stability(map[string]any{"deprecated": true}); got != "deprecated" {
		t.Errorf("stability(deprecated) = %q", got)
	}
	known := map[string]string{"#/known": "node-known"}
	if got := resolutionForTarget("node-known", known); got != "declared" {
		t.Errorf("known resolution = %q", got)
	}
	if got := resolutionForSecurity("unknown", known); got != "unresolved" {
		t.Errorf("unknown security resolution = %q", got)
	}
	fragment := rkcmodel.Fragment{}
	file := pluginapi.FileRef{ArtifactID: "artifact", Path: "api.json"}
	firstID := unresolved(&fragment, file, "schema", "Missing")
	secondID := unresolved(&fragment, file, "schema", "Missing")
	if firstID != secondID || len(fragment.Nodes) != 1 {
		t.Errorf("unresolved node deduplication failed: ids=%q/%q nodes=%d", firstID, secondID, len(fragment.Nodes))
	}
	refs := collectRefs(map[string]any{"b": map[string]any{"$ref": "#/b"}, "a": []any{map[string]any{"$ref": "#/a"}, map[string]any{"$ref": "#/b"}}})
	if !reflect.DeepEqual(refs, []string{"#/a", "#/b"}) {
		t.Errorf("collectRefs() = %#v", refs)
	}
}

func writeOpenAPITestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
