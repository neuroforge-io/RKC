// Package jsonschema extracts JSON Schema documents, definitions, properties,
// and references into the repository graph.
package jsonschema

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	PluginID      = "rkc.json-schema"
	PluginVersion = "0.2.0"
)

type Options struct {
	Root  string
	Files []pluginapi.FileRef
}

type collector struct {
	fragment rkcmodel.Fragment
	seenNode map[string]struct{}
	seenEdge map[string]struct{}
	file     pluginapi.FileRef
}

func Extract(options Options) (rkcmodel.Fragment, error) {
	fragment := rkcmodel.Fragment{}
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, file := range files {
		data, err := sourcepath.ReadFile(options.Root, file.Path)
		if err != nil {
			fragment.Diagnostics = append(fragment.Diagnostics, diagnostic(file, "RKC-SCH-1001", err.Error()))
			continue
		}
		var document map[string]any
		if err := decodeJSONObject(data, &document); err != nil {
			if likelySchemaPath(file.Path) {
				fragment.Diagnostics = append(fragment.Diagnostics, diagnostic(file, "RKC-SCH-1002", "invalid JSON Schema: "+err.Error()))
			}
			continue
		}
		if !isSchema(document) || isOpenAPI(document) {
			continue
		}
		c := collector{seenNode: map[string]struct{}{}, seenEdge: map[string]struct{}{}, file: file}
		c.document(document)
		fragment.Nodes = append(fragment.Nodes, c.fragment.Nodes...)
		fragment.Edges = append(fragment.Edges, c.fragment.Edges...)
		fragment.Evidence = append(fragment.Evidence, c.fragment.Evidence...)
		fragment.Diagnostics = append(fragment.Diagnostics, c.fragment.Diagnostics...)
	}
	rkcmodel.SortFragment(&fragment)
	return fragment, nil
}

func decodeJSONObject(data []byte, document *map[string]any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(document); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err == nil {
		return errors.New("multiple JSON values")
	} else if !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid trailing data: %w", err)
	}
	return nil
}

func (c *collector) document(document map[string]any) {
	title := first(stringValue(document["title"]), strings.TrimSuffix(filepath.Base(c.file.Path), filepath.Ext(c.file.Path)))
	rootID := c.schemaNode(title, "#", document, true)
	c.addEdge("derived_from", rootID, c.file.ArtifactID, "declared", c.evidence("jsonschema.document", "#", title), nil)
	definitions := mapValue(document["$defs"])
	prefix := "#/$defs/"
	if len(definitions) == 0 {
		definitions = mapValue(document["definitions"])
		prefix = "#/definitions/"
	}
	known := map[string]string{"#": rootID}
	for _, name := range sortedKeys(definitions) {
		anchor := prefix + escape(name)
		known[anchor] = c.schemaNode(name, anchor, mapValue(definitions[name]), true)
		c.addEdge("declares", rootID, known[anchor], "declared", c.evidence("jsonschema.definition", anchor, name), nil)
	}
	c.properties(rootID, "#", document, known)
	for _, name := range sortedKeys(definitions) {
		anchor := prefix + escape(name)
		c.properties(known[anchor], anchor, mapValue(definitions[name]), known)
	}
	c.refs(rootID, "#", document, known)
	for _, name := range sortedKeys(definitions) {
		anchor := prefix + escape(name)
		c.refs(known[anchor], anchor, mapValue(definitions[name]), known)
	}
}

func (c *collector) properties(parentID, anchor string, schema map[string]any, known map[string]string) {
	properties := mapValue(schema["properties"])
	requiredSet := setOf(stringSlice(schema["required"]))
	for _, name := range sortedKeys(properties) {
		property := mapValue(properties[name])
		propertyAnchor := anchor + "/properties/" + escape(name)
		id := rkcmodel.StableID("node", "field", c.file.Path, propertyAnchor)
		evidence := c.evidence("jsonschema.property", propertyAnchor, name)
		c.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", "jsonschema-field", c.file.Path, propertyAnchor), Kind: "field", Name: name, QualifiedName: c.file.Path + propertyAnchor, Signature: propertySignature(name, property, requiredSet[name]), Language: "jsonschema", Visibility: "public", ArtifactID: c.file.ArtifactID, Source: source(c.file, propertyAnchor), EvidenceIDs: []string{evidence}, Attributes: map[string]any{"type": schemaType(property), "format": stringValue(property["format"]), "description": stringValue(property["description"]), "required": requiredSet[name], "default": safeScalar(property["default"]), "read_only": boolValue(property["readOnly"]), "write_only": boolValue(property["writeOnly"]), "deprecated": boolValue(property["deprecated"])}})
		c.addEdge("declares", parentID, id, "declared", evidence, nil)
		if ref := stringValue(property["$ref"]); ref != "" {
			to, resolution := resolve(c, ref, known)
			c.addEdge("references", id, to, resolution, evidence, map[string]any{"ref": ref})
		}
		for _, ref := range collectRefs(property) {
			if ref == stringValue(property["$ref"]) {
				continue
			}
			to, resolution := resolve(c, ref, known)
			c.addEdge("references", id, to, resolution, evidence, map[string]any{"ref": ref})
		}
	}
}
func (c *collector) refs(from, anchor string, schema map[string]any, known map[string]string) {
	for _, ref := range collectRefs(schema) {
		to, resolution := resolve(c, ref, known)
		evidence := c.evidence("jsonschema.ref", anchor, ref)
		c.addEdge("references", from, to, resolution, evidence, map[string]any{"ref": ref})
	}
}
func (c *collector) schemaNode(name, anchor string, schema map[string]any, public bool) string {
	id := rkcmodel.StableID("node", "schema", c.file.Path, anchor)
	evidence := c.evidence("jsonschema.schema", anchor, name)
	c.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", "jsonschema", first(stringValue(schema["$id"]), c.file.Path+anchor)), Kind: "schema", Name: name, QualifiedName: first(stringValue(schema["$id"]), c.file.Path+anchor), Language: "jsonschema", Visibility: "public", PublicSurface: public, ArtifactID: c.file.ArtifactID, Source: source(c.file, anchor), EvidenceIDs: []string{evidence}, Attributes: map[string]any{"schema_uri": stringValue(schema["$schema"]), "id": stringValue(schema["$id"]), "type": schemaType(schema), "description": stringValue(schema["description"]), "required": stringSlice(schema["required"]), "additional_properties": safeScalar(schema["additionalProperties"])}})
	return id
}
func resolve(c *collector, ref string, known map[string]string) (string, string) {
	if id := known[ref]; id != "" {
		return id, "declared"
	}
	id := rkcmodel.StableID("node", "unresolved_symbol", "jsonschema", c.file.Path, ref)
	c.addNode(rkcmodel.Node{ID: id, Kind: "unresolved_symbol", Name: ref, QualifiedName: ref, Language: "jsonschema", Visibility: "unknown", ArtifactID: c.file.ArtifactID, Attributes: map[string]any{"placeholder": true, "namespace": "jsonschema_ref"}})
	return id, "unresolved"
}
func (c *collector) evidence(method, anchor, detail string) string {
	id := rkcmodel.StableID("evidence", PluginID, c.file.ArtifactID, method, anchor, detail)
	c.fragment.Evidence = append(c.fragment.Evidence, rkcmodel.Evidence{ID: id, Kind: "manifest", Method: method, Confidence: 1, Source: source(c.file, anchor), Tool: PluginID, ToolVersion: PluginVersion, InputDigest: c.file.SHA256, Detail: detail})
	return id
}
func (c *collector) addNode(node rkcmodel.Node) {
	if _, ok := c.seenNode[node.ID]; ok {
		return
	}
	c.seenNode[node.ID] = struct{}{}
	c.fragment.Nodes = append(c.fragment.Nodes, node)
}
func (c *collector) addEdge(kind, from, to, resolution, evidence string, attributes map[string]any) {
	id := rkcmodel.StableID("edge", kind, from, to, evidence)
	if _, ok := c.seenEdge[id]; ok {
		return
	}
	c.seenEdge[id] = struct{}{}
	confidence := 1.0
	if resolution == "unresolved" {
		confidence = .5
	}
	c.fragment.Edges = append(c.fragment.Edges, rkcmodel.Edge{ID: id, Kind: kind, From: from, To: to, Resolution: resolution, Confidence: confidence, Producer: PluginID, EvidenceIDs: []string{evidence}, Attributes: attributes})
}
func source(file pluginapi.FileRef, anchor string) *rkcmodel.SourceRange {
	return &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, Anchor: anchor}
}
func diagnostic(file pluginapi.FileRef, code, message string) rkcmodel.Diagnostic {
	return rkcmodel.Diagnostic{ID: rkcmodel.StableID("diagnostic", PluginID, file.ArtifactID, code, message), Severity: "error", Code: code, Message: message, Stage: "framework_jsonschema", Plugin: PluginID, Source: source(file, "#")}
}
func isSchema(document map[string]any) bool {
	return stringValue(document["$schema"]) != "" || stringValue(document["$id"]) != "" || len(mapValue(document["properties"])) > 0 || len(mapValue(document["$defs"])) > 0
}
func isOpenAPI(document map[string]any) bool {
	return stringValue(document["openapi"]) != "" || stringValue(document["swagger"]) != ""
}
func likelySchemaPath(path string) bool {
	lower := strings.ToLower(filepath.Base(path))
	return strings.Contains(lower, "schema")
}
func mapValue(value any) map[string]any { result, _ := value.(map[string]any); return result }
func arrayValue(value any) []any        { result, _ := value.([]any); return result }
func stringValue(value any) string      { result, _ := value.(string); return result }
func boolValue(value any) bool          { result, _ := value.(bool); return result }
func stringSlice(value any) []string {
	var output []string
	for _, item := range arrayValue(value) {
		if text := stringValue(item); text != "" {
			output = append(output, text)
		}
	}
	sort.Strings(output)
	return output
}
func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
func escape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
func setOf(values []string) map[string]bool {
	result := map[string]bool{}
	for _, value := range values {
		result[value] = true
	}
	return result
}
func schemaType(schema map[string]any) any {
	if text := stringValue(schema["type"]); text != "" {
		return text
	}
	if values := stringSlice(schema["type"]); len(values) > 0 {
		return values
	}
	if schema["const"] != nil {
		return "const"
	}
	if schema["enum"] != nil {
		return "enum"
	}
	return "any"
}
func propertySignature(name string, schema map[string]any, required bool) string {
	suffix := "?"
	if required {
		suffix = ""
	}
	return fmt.Sprintf("%s%s: %v", name, suffix, schemaType(schema))
}
func safeScalar(value any) any {
	switch value.(type) {
	case nil, string, bool, json.Number, float64:
		return value
	default:
		return nil
	}
}
func collectRefs(value any) []string {
	seen := map[string]struct{}{}
	var walk func(any)
	walk = func(current any) {
		switch typed := current.(type) {
		case map[string]any:
			if ref := stringValue(typed["$ref"]); ref != "" {
				seen[ref] = struct{}{}
			}
			for _, key := range sortedKeys(typed) {
				walk(typed[key])
			}
		case []any:
			for _, item := range typed {
				walk(item)
			}
		}
	}
	walk(value)
	refs := make([]string, 0, len(seen))
	for ref := range seen {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	return refs
}
