package openapi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
	"gopkg.in/yaml.v3"
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
      "parameters": [{"name": "trace", "in": "header", "type": "string", "schema": {"$ref": "#/components/schemas/Base"}}],
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

func TestOpenAPIJSONIsBoundedAndRejectsDuplicateMembers(t *testing.T) {
	t.Parallel()
	var document map[string]any
	duplicate := []byte(`{"openapi":"3.1.0","openapi":"3.0.0","paths":{}}`)
	if err := decodeJSONObject(duplicate, &document); err == nil ||
		!strings.Contains(err.Error(), "duplicate JSON object member") {
		t.Fatalf("duplicate JSON error = %v", err)
	}
	if err := decodeJSONObject(
		make([]byte, maximumOpenAPIDocumentBytes+1),
		&document,
	); err == nil || !strings.Contains(err.Error(), "byte safety limit") {
		t.Fatalf("oversized JSON error = %v", err)
	}
	if err := decodeJSONObject([]byte(`["not an object"]`), &document); err == nil {
		t.Fatal("array-root JSON was accepted as an object")
	}
	root := t.TempDir()
	writeOpenAPITestFile(t, root, "oversized.openapi.json", strings.Repeat("x", maximumOpenAPIDocumentBytes+1))
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "oversized", Path: "oversized.openapi.json", SHA256: "sha"}}})
	if err != nil || len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-API-1001" {
		t.Fatalf("oversized OpenAPI diagnostic = %+v, %v", fragment.Diagnostics, err)
	}
}

func TestEquivalentJSONAndYAMLProduceIdenticalFragments(t *testing.T) {
	t.Parallel()
	jsonData := []byte(`{
  "openapi": "3.1.0",
  "info": {"title": "Parity API", "version": "1.0"},
  "servers": [{"url": "https://api.example"}],
  "components": {
    "schemas": {
      "Item": {
        "type": "object",
        "required": ["id"],
        "properties": {"id": {"type": "integer"}},
        "deprecated": false
      }
    }
  },
  "paths": {
    "/items": {
      "get": {
        "operationId": "listItems",
        "tags": ["items"],
        "responses": {
          "200": {
            "description": "ok",
            "content": {
              "application/json": {
                "schema": {"$ref": "#/components/schemas/Item"}
              }
            }
          }
        }
      }
    }
  }
}`)
	yamlData := []byte(`openapi: 3.1.0
info:
  title: Parity API
  version: "1.0"
servers:
  - url: https://api.example
components:
  schemas:
    Item:
      type: object
      required: [id]
      properties:
        id:
          type: integer
      deprecated: false
paths:
  /items:
    get:
      operationId: listItems
      tags: [items]
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                $ref: "#/components/schemas/Item"
`)

	var jsonDocument, yamlDocument map[string]any
	if err := decodeOpenAPIDocument(jsonData, documentFormatJSON, &jsonDocument); err != nil {
		t.Fatalf("decode JSON: %v", err)
	}
	if err := decodeOpenAPIDocument(yamlData, documentFormatYAML, &yamlDocument); err != nil {
		t.Fatalf("decode YAML: %v", err)
	}
	if !reflect.DeepEqual(jsonDocument, yamlDocument) {
		t.Fatalf("canonical documents differ:\nJSON: %#v\nYAML: %#v", jsonDocument, yamlDocument)
	}

	file := pluginapi.FileRef{ArtifactID: "api", Path: "api/openapi.spec", SHA256: "sha"}
	jsonFragment := rkcmodel.Fragment{}
	yamlFragment := rkcmodel.Fragment{}
	extractDocument(&jsonFragment, file, jsonDocument)
	extractDocument(&yamlFragment, file, yamlDocument)
	rkcmodel.SortFragment(&jsonFragment)
	rkcmodel.SortFragment(&yamlFragment)
	if !reflect.DeepEqual(jsonFragment, yamlFragment) {
		t.Fatalf("equivalent JSON and YAML fragments differ:\nJSON: %#v\nYAML: %#v", jsonFragment, yamlFragment)
	}
}

func TestExtractOpenAPIYAMLAndExternalReferenceIsolation(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeOpenAPITestFile(t, root, "api/openapi.yaml", `openapi: 3.1.0
info:
  title: YAML API
  version: "1"
paths:
  /items:
    get:
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                $ref: https://example.invalid/schemas.json#/Item
`)
	fragment, err := Extract(Options{
		Root: root,
		Files: []pluginapi.FileRef{{
			ArtifactID: "yaml-api",
			Path:       "api/openapi.yaml",
			SHA256:     "sha-yaml",
		}},
	})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", fragment.Diagnostics)
	}
	var serviceFound, endpointFound, externalUnresolved bool
	for _, node := range fragment.Nodes {
		serviceFound = serviceFound || node.Name == "YAML API"
		endpointFound = endpointFound || node.Name == "GET /items"
		if node.Kind == "unresolved_symbol" &&
			node.Name == "https://example.invalid/schemas.json#/Item" {
			externalUnresolved = true
		}
	}
	if !serviceFound || !endpointFound || !externalUnresolved {
		t.Fatalf(
			"YAML extraction incomplete: service=%t endpoint=%t external unresolved=%t",
			serviceFound,
			endpointFound,
			externalUnresolved,
		)
	}
}

func TestExtractOpenAPIRejectsUnsafeYAML(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		reason  string
	}{
		{
			name:    "malformed",
			content: "openapi: [\n",
			reason:  "invalid OpenAPI YAML",
		},
		{
			name: "duplicate key",
			content: `openapi: 3.1.0
openapi: 3.0.0
paths: {}
`,
			reason: "mapping key",
		},
		{
			name: "explicit tag",
			content: `openapi: !!str 3.1.0
paths: {}
`,
			reason: "explicit YAML tags are not supported",
		},
		{
			name: "explicit non-specific tag",
			content: `openapi: ! 3.1.0
paths: {}
`,
			reason: "explicit YAML tags are not supported",
		},
		{
			name: "alias bomb",
			content: `openapi: 3.1.0
seed: &seed [x, x, x, x]
paths: *seed
`,
			reason: "YAML aliases are not supported",
		},
		{
			name: "non-string key",
			content: `openapi: 3.1.0
1: value
paths: {}
`,
			reason: "mapping keys must be strings",
		},
		{
			name: "multiple documents",
			content: `openapi: 3.1.0
paths: {}
---
openapi: 3.0.0
paths: {}
`,
			reason: "multiple YAML documents",
		},
		{
			name: "non-finite number",
			content: `openapi: 3.1.0
extension: .inf
paths: {}
`,
			reason: "non-finite floating-point scalars are not supported",
		},
	}

	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeOpenAPITestFile(t, root, "openapi.yaml", test.content)
			fragment, err := Extract(Options{
				Root: root,
				Files: []pluginapi.FileRef{{
					ArtifactID: "unsafe-yaml",
					Path:       "openapi.yaml",
					SHA256:     "sha",
				}},
			})
			if err != nil {
				t.Fatalf("Extract() error = %v", err)
			}
			if len(fragment.Nodes) != 0 {
				t.Fatalf("unsafe YAML produced %d nodes", len(fragment.Nodes))
			}
			if len(fragment.Diagnostics) != 1 ||
				fragment.Diagnostics[0].Code != "RKC-API-1002" ||
				!strings.Contains(fragment.Diagnostics[0].Message, test.reason) {
				t.Fatalf(
					"diagnostics = %#v, want RKC-API-1002 containing %q",
					fragment.Diagnostics,
					test.reason,
				)
			}
		})
	}
}

func TestYAMLConversionSafetyBoundaries(t *testing.T) {
	t.Parallel()
	var document map[string]any
	if err := decodeYAMLObject([]byte("openapi: \"unterminated\npaths: {}\n"), &document); err == nil {
		t.Fatal("unterminated YAML scalar was accepted")
	}
	if err := decodeYAMLObject(make([]byte, maximumYAMLBytes+1), &document); err == nil ||
		!strings.Contains(err.Error(), "byte safety limit") {
		t.Fatalf("oversized YAML error = %v", err)
	}
	if err := decodeYAMLObject([]byte("---\n"), &document); err == nil {
		t.Fatal("empty YAML document was accepted")
	}
	if err := decodeYAMLObject([]byte("- item\n"), &document); err == nil ||
		!strings.Contains(err.Error(), "root must be a mapping") {
		t.Fatalf("sequence-root YAML error = %v", err)
	}
	if err := decodeOpenAPIDocument(nil, documentFormat("TOML"), &document); err == nil ||
		!strings.Contains(err.Error(), "unsupported document format") {
		t.Fatalf("unsupported document format error = %v", err)
	}

	converter := yamlConverter{nodes: maximumYAMLNodes}
	if _, err := converter.convert(&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str"}, 1); err == nil ||
		!strings.Contains(err.Error(), "node safety limit") {
		t.Fatalf("node-limit error = %v", err)
	}
	if _, err := (&yamlConverter{}).convert(nil, 1); err == nil {
		t.Fatal("nil YAML node was accepted")
	}
	if _, err := (&yamlConverter{}).convert(
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str"},
		maximumYAMLDepth+1,
	); err == nil || !strings.Contains(err.Error(), "nesting limit") {
		t.Fatalf("depth-limit error = %v", err)
	}
	if _, err := (&yamlConverter{}).convert(&yaml.Node{Kind: yaml.DocumentNode}, 1); err == nil ||
		!strings.Contains(err.Error(), "unsupported YAML node") {
		t.Fatalf("unsupported-node error = %v", err)
	}
	if _, err := (&yamlConverter{}).convert(
		&yaml.Node{
			Kind:    yaml.MappingNode,
			Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "key"}},
		},
		1,
	); err == nil || !strings.Contains(err.Error(), "unmatched key") {
		t.Fatalf("unmatched-key error = %v", err)
	}
	sequenceError := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{{Kind: yaml.DocumentNode}}}
	if _, err := (&yamlConverter{}).convert(sequenceError, 1); err == nil || !strings.Contains(err.Error(), "unsupported YAML node") {
		t.Fatalf("sequence child error = %v", err)
	}
	keyWithTag := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Style: yaml.TaggedStyle, Value: "key"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "value"},
	}}
	if _, err := (&yamlConverter{}).convert(keyWithTag, 1); err == nil || !strings.Contains(err.Error(), "explicit YAML tags") {
		t.Fatalf("tagged mapping key error = %v", err)
	}
	if _, err := (&yamlConverter{nodes: maximumYAMLNodes - 1}).convert(
		&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Tag: "!!str", Value: "a"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: "b"}}}, 1,
	); err == nil || !strings.Contains(err.Error(), "node safety limit") {
		t.Fatalf("sequence node-limit error = %v", err)
	}
	if _, err := (&yamlConverter{nodes: maximumYAMLNodes - 1}).convertMapping(
		&yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "a"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: "one"},
			{Kind: yaml.ScalarNode, Tag: "!!str", Value: "b"}, {Kind: yaml.ScalarNode, Tag: "!!str", Value: "two"},
		}}, 1,
	); err == nil || !strings.Contains(err.Error(), "node safety limit") {
		t.Fatalf("mapping node-limit error = %v", err)
	}
	deepFlow := []byte(strings.Repeat("[", maximumYAMLDepth+1) +
		"0" + strings.Repeat("]", maximumYAMLDepth+1))
	if err := decodeYAMLObject(deepFlow, &document); err == nil ||
		!strings.Contains(err.Error(), "flow nesting limit") {
		t.Fatalf("deep-flow preflight error = %v", err)
	}
	deepIndent := strings.Builder{}
	for depth := 0; depth <= maximumYAMLDepth; depth++ {
		deepIndent.WriteString(strings.Repeat(" ", depth))
		deepIndent.WriteString("key:\n")
	}
	if err := decodeYAMLObject([]byte(deepIndent.String()), &document); err == nil ||
		!strings.Contains(err.Error(), "nesting limit") {
		t.Fatalf("deep-indent preflight error = %v", err)
	}
}

func TestYAMLScalarCanonicalization(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		node    yaml.Node
		want    any
		wantErr string
	}{
		{name: "string", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "value"}, want: "value"},
		{name: "null", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, want: nil},
		{name: "boolean", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "TRUE"}, want: true},
		{name: "integer", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "0x10"}, want: json.Number("16")},
		{name: "leading-zero-decimal", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "0123"}, want: json.Number("123")},
		{name: "timestamp-as-string", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!timestamp", Value: "2026-07-27"}, want: "2026-07-27"},
		{name: "float-leading-dot", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: "+.5"}, want: json.Number("0.5")},
		{name: "float-trailing-dot", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: "1."}, want: json.Number("1.0")},
		{name: "negative-leading-dot", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: "-.5"}, want: json.Number("-0.5")},
		{name: "invalid-boolean", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "sometimes"}, wantErr: "invalid boolean"},
		{name: "invalid-integer", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "0xno"}, wantErr: "invalid integer"},
		{name: "overflowing-float", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: "1e999"}, wantErr: "invalid floating-point"},
		{name: "non-json-float", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!!float", Value: "01.5"}, wantErr: "not JSON-compatible"},
		{name: "custom-tag", node: yaml.Node{Kind: yaml.ScalarNode, Tag: "!custom", Value: "value"}, wantErr: "unsupported scalar tag"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := (&yamlConverter{}).convertScalar(&test.node)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("convertScalar() error = %v, want %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("convertScalar() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("convertScalar() = %#v, want %#v", got, test.want)
			}
		})
	}
	for _, test := range []struct {
		raw, want string
	}{
		{"-0x10", "-16"}, {"+0X10", "16"}, {"0o10", "8"}, {"0B11", "3"}, {"1_000", "1000"},
	} {
		if got, err := canonicalYAMLInteger(test.raw); err != nil || got != test.want {
			t.Errorf("canonicalYAMLInteger(%q) = %q, %v; want %q", test.raw, got, err, test.want)
		}
	}
	for _, raw := range []string{"", "+", "0x"} {
		if _, err := canonicalYAMLInteger(raw); err == nil {
			t.Errorf("canonicalYAMLInteger(%q) accepted invalid input", raw)
		}
	}
}

func TestOpenAPIEscapedComponentPointersResolve(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeOpenAPITestFile(t, root, "openapi.json", `{
  "openapi": "3.1.0",
  "info": {"title": "Pointer API", "version": "1"},
  "paths": {
    "/item": {
      "get": {
        "responses": {
          "200": {
            "description": "ok",
            "content": {
              "application/json": {
                "schema": {"$ref": "#/components/schemas/A~1B~0C"}
              }
            }
          }
        }
      }
    }
  },
  "components": {
    "schemas": {
      "A/B~C": {"type": "object"}
    }
  }
}`)
	fragment, err := Extract(Options{
		Root: root,
		Files: []pluginapi.FileRef{{
			ArtifactID: "pointer-api", Path: "openapi.json", SHA256: "sha",
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range fragment.Edges {
		if edge.Kind == "serializes" {
			if edge.Resolution != "declared" {
				t.Fatalf("escaped pointer edge = %+v", edge)
			}
			return
		}
	}
	t.Fatal("escaped component pointer produced no serialization edge")
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
	if !likelyOpenAPIPath("SWAGGER.YAML") || !likelyOpenAPIPath("my-openapi.json") || likelyOpenAPIPath("api.json") {
		t.Error("likelyOpenAPIPath classification mismatch")
	}
	if got := documentFormatForPath("api/openapi.YML"); got != documentFormatYAML {
		t.Errorf("YML document format = %q, want YAML", got)
	}
	if got := documentFormatForPath("api/openapi.json"); got != documentFormatJSON {
		t.Errorf("JSON document format = %q, want JSON", got)
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
