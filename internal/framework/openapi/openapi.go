// Package openapi extracts deterministic API surfaces from JSON OpenAPI and
// Swagger documents. YAML support belongs in a parser-backed adapter rather
// than a collection of indentation guesses wearing a standards badge.
package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/repository-knowledge-compiler/rkc/pkg/pluginapi"
	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

const (
	PluginID      = "rkc.openapi-json"
	PluginVersion = "0.2.0"
)

var operations = []string{"get", "put", "post", "delete", "options", "head", "patch", "trace"}

type Options struct {
	Root  string
	Files []pluginapi.FileRef
}

// Extract parses candidate JSON files and ignores ordinary JSON documents. A
// malformed file whose name strongly suggests OpenAPI produces a diagnostic;
// unrelated malformed JSON remains someone else's problem.
func Extract(options Options) (rkcmodel.Fragment, error) {
	fragment := rkcmodel.Fragment{}
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, file := range files {
		path := filepath.Join(options.Root, filepath.FromSlash(file.Path))
		data, err := os.ReadFile(path)
		if err != nil {
			fragment.Diagnostics = append(fragment.Diagnostics, diagnostic(file, "RKC-API-1001", err.Error(), "error"))
			continue
		}
		var document map[string]any
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(&document); err != nil {
			if likelyOpenAPIPath(file.Path) {
				fragment.Diagnostics = append(fragment.Diagnostics, diagnostic(file, "RKC-API-1002", "invalid OpenAPI JSON: "+err.Error(), "error"))
			}
			continue
		}
		if !isOpenAPI(document) {
			continue
		}
		extractDocument(&fragment, file, document)
	}
	rkcmodel.SortFragment(&fragment)
	return fragment, nil
}

func extractDocument(fragment *rkcmodel.Fragment, file pluginapi.FileRef, document map[string]any) {
	version := stringValue(document["openapi"])
	if version == "" {
		version = stringValue(document["swagger"])
	}
	info := mapValue(document["info"])
	title := first(stringValue(info["title"]), strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path)))
	serviceQualified := "openapi:" + file.Path
	serviceID := rkcmodel.StableID("node", "api_service", serviceQualified)
	serviceEvidence := addEvidence(fragment, file, "openapi.document", "#", title)
	fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
		ID: serviceID, LogicalID: rkcmodel.StableID("logical", "api_service", title), Kind: "api_service",
		Name: title, QualifiedName: serviceQualified, Language: "openapi", Visibility: "public", PublicSurface: true,
		ArtifactID: file.ArtifactID, Source: source(file, "#"), EvidenceIDs: []string{serviceEvidence},
		Attributes: map[string]any{
			"openapi_version": version, "api_version": stringValue(info["version"]), "description": stringValue(info["description"]),
			"servers": stringSliceFromObjects(document["servers"], "url"), "base_path": stringValue(document["basePath"]),
		},
	})
	fragment.Edges = append(fragment.Edges, edge("derived_from", serviceID, file.ArtifactID, "declared", serviceEvidence, nil))

	schemas := extractSchemas(fragment, file, serviceID, document)
	security := extractSecuritySchemes(fragment, file, serviceID, document)
	paths := mapValue(document["paths"])
	pathNames := sortedKeys(paths)
	for _, pathName := range pathNames {
		pathItem := mapValue(paths[pathName])
		for _, method := range operations {
			operation, ok := pathItem[method].(map[string]any)
			if !ok {
				continue
			}
			extractOperation(fragment, file, serviceID, title, strings.ToUpper(method), pathName, pathItem, operation, schemas, security)
		}
	}
}

func extractSchemas(fragment *rkcmodel.Fragment, file pluginapi.FileRef, serviceID string, document map[string]any) map[string]string {
	items := mapValue(mapValue(document["components"])["schemas"])
	if len(items) == 0 {
		items = mapValue(document["definitions"])
	}
	result := map[string]string{}
	for _, name := range sortedKeys(items) {
		schema := mapValue(items[name])
		id := rkcmodel.StableID("node", "schema", file.Path, name)
		anchor := "#/components/schemas/" + escapeJSONPointer(name)
		if mapValue(document["components"])["schemas"] == nil {
			anchor = "#/definitions/" + escapeJSONPointer(name)
		}
		evidenceID := addEvidence(fragment, file, "openapi.schema", anchor, name)
		fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
			ID: id, LogicalID: rkcmodel.StableID("logical", "schema", name), Kind: "schema", Name: name,
			QualifiedName: file.Path + "#schema/" + name, Language: "openapi", Visibility: "public", PublicSurface: true,
			ArtifactID: file.ArtifactID, Source: source(file, anchor), EvidenceIDs: []string{evidenceID},
			Attributes: map[string]any{
				"type": stringValue(schema["type"]), "format": stringValue(schema["format"]),
				"description": stringValue(schema["description"]), "required": stringSlice(schema["required"]),
				"properties": sortedKeys(mapValue(schema["properties"])), "deprecated": boolValue(schema["deprecated"]),
			},
		})
		fragment.Edges = append(fragment.Edges, edge("declares", serviceID, id, "declared", evidenceID, nil))
		result["#/components/schemas/"+name] = id
		result["#/definitions/"+name] = id
	}
	for _, name := range sortedKeys(items) {
		from := result["#/components/schemas/"+name]
		if from == "" {
			from = result["#/definitions/"+name]
		}
		refs := collectRefs(items[name])
		for _, ref := range refs {
			to := resolveRef(fragment, file, ref, result)
			evidenceID := addEvidence(fragment, file, "openapi.ref", "#/components/schemas/"+escapeJSONPointer(name), ref)
			fragment.Edges = append(fragment.Edges, edge("references", from, to, resolutionForTarget(to, result), evidenceID, map[string]any{"ref": ref}))
		}
	}
	return result
}

func extractSecuritySchemes(fragment *rkcmodel.Fragment, file pluginapi.FileRef, serviceID string, document map[string]any) map[string]string {
	items := mapValue(mapValue(document["components"])["securitySchemes"])
	if len(items) == 0 {
		items = mapValue(document["securityDefinitions"])
	}
	result := map[string]string{}
	for _, name := range sortedKeys(items) {
		definition := mapValue(items[name])
		id := rkcmodel.StableID("node", "security_scheme", file.Path, name)
		anchor := "#/components/securitySchemes/" + escapeJSONPointer(name)
		evidenceID := addEvidence(fragment, file, "openapi.security_scheme", anchor, name)
		fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
			ID: id, LogicalID: rkcmodel.StableID("logical", "security_scheme", name), Kind: "security_scheme",
			Name: name, QualifiedName: file.Path + "#security/" + name, Language: "openapi", Visibility: "public",
			ArtifactID: file.ArtifactID, Source: source(file, anchor), EvidenceIDs: []string{evidenceID},
			Attributes: map[string]any{"type": stringValue(definition["type"]), "scheme": stringValue(definition["scheme"]), "in": stringValue(definition["in"]), "name": stringValue(definition["name"]), "description": stringValue(definition["description"])},
		})
		fragment.Edges = append(fragment.Edges, edge("declares", serviceID, id, "declared", evidenceID, nil))
		result[name] = id
	}
	return result
}

func extractOperation(fragment *rkcmodel.Fragment, file pluginapi.FileRef, serviceID, serviceName, method, pathName string, pathItem, operation map[string]any, schemas, securitySchemes map[string]string) {
	operationID := stringValue(operation["operationId"])
	name := method + " " + pathName
	qualified := serviceName + " " + name
	id := rkcmodel.StableID("node", "api_endpoint", file.Path, method, pathName)
	anchor := "#/paths/" + escapeJSONPointer(pathName) + "/" + strings.ToLower(method)
	evidenceID := addEvidence(fragment, file, "openapi.operation", anchor, name)
	parameters := append(arrayValue(pathItem["parameters"]), arrayValue(operation["parameters"])...)
	responses := mapValue(operation["responses"])
	fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
		ID: id, LogicalID: logicalOperationID(operationID, method, pathName), Kind: "api_endpoint", Name: name,
		QualifiedName: qualified, Signature: name, Language: "openapi", Visibility: "public", PublicSurface: true,
		Stability: stability(operation), ArtifactID: file.ArtifactID, Source: source(file, anchor), EvidenceIDs: []string{evidenceID},
		Attributes: map[string]any{
			"method": method, "path": pathName, "operation_id": operationID, "summary": stringValue(operation["summary"]),
			"description": stringValue(operation["description"]), "tags": stringSlice(operation["tags"]),
			"deprecated": boolValue(operation["deprecated"]), "response_codes": sortedKeys(responses),
		},
	})
	fragment.Edges = append(fragment.Edges, edge("exposes", serviceID, id, "declared", evidenceID, nil))

	for index, raw := range parameters {
		parameter := mapValue(raw)
		if ref := stringValue(parameter["$ref"]); ref != "" {
			to := resolveRef(fragment, file, ref, schemas)
			refEvidence := addEvidence(fragment, file, "openapi.ref", fmt.Sprintf("%s/parameters/%d", anchor, index), ref)
			fragment.Edges = append(fragment.Edges, edge("references", id, to, resolutionForTarget(to, schemas), refEvidence, map[string]any{"ref": ref, "role": "parameter"}))
			continue
		}
		parameterName := first(stringValue(parameter["name"]), fmt.Sprintf("parameter_%d", index+1))
		parameterID := rkcmodel.StableID("node", "parameter", id, parameterName, stringValue(parameter["in"]))
		parameterEvidence := addEvidence(fragment, file, "openapi.parameter", fmt.Sprintf("%s/parameters/%d", anchor, index), parameterName)
		schema := mapValue(parameter["schema"])
		fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
			ID: parameterID, LogicalID: rkcmodel.StableID("logical", "api_parameter", logicalOperationID(operationID, method, pathName), parameterName),
			Kind: "parameter", Name: parameterName, QualifiedName: qualified + " parameter " + parameterName,
			Signature: parameterSignature(parameter, schema), Language: "openapi", Visibility: "public", ArtifactID: file.ArtifactID,
			Source: source(file, fmt.Sprintf("%s/parameters/%d", anchor, index)), EvidenceIDs: []string{parameterEvidence},
			Attributes: map[string]any{"in": stringValue(parameter["in"]), "required": boolValue(parameter["required"]), "type": first(stringValue(schema["type"]), stringValue(parameter["type"])), "format": first(stringValue(schema["format"]), stringValue(parameter["format"])), "description": stringValue(parameter["description"])},
		})
		fragment.Edges = append(fragment.Edges, edge("declares", id, parameterID, "declared", parameterEvidence, nil))
		for _, ref := range collectRefs(parameter) {
			to := resolveRef(fragment, file, ref, schemas)
			fragment.Edges = append(fragment.Edges, edge("references", parameterID, to, resolutionForTarget(to, schemas), parameterEvidence, map[string]any{"ref": ref}))
		}
	}

	if requestBody := mapValue(operation["requestBody"]); len(requestBody) > 0 {
		for _, ref := range collectRefs(requestBody) {
			to := resolveRef(fragment, file, ref, schemas)
			refEvidence := addEvidence(fragment, file, "openapi.request_body", anchor+"/requestBody", ref)
			fragment.Edges = append(fragment.Edges, edge("deserializes", id, to, resolutionForTarget(to, schemas), refEvidence, map[string]any{"ref": ref}))
		}
	}
	for _, code := range sortedKeys(responses) {
		response := mapValue(responses[code])
		responseID := rkcmodel.StableID("node", "return_value", id, code)
		responseEvidence := addEvidence(fragment, file, "openapi.response", anchor+"/responses/"+escapeJSONPointer(code), code)
		fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{
			ID: responseID, LogicalID: rkcmodel.StableID("logical", "api_response", logicalOperationID(operationID, method, pathName), code),
			Kind: "return_value", Name: code, QualifiedName: qualified + " response " + code, Language: "openapi",
			Visibility: "public", ArtifactID: file.ArtifactID, Source: source(file, anchor+"/responses/"+escapeJSONPointer(code)),
			EvidenceIDs: []string{responseEvidence}, Attributes: map[string]any{"status_code": code, "description": stringValue(response["description"])},
		})
		fragment.Edges = append(fragment.Edges, edge("declares", id, responseID, "declared", responseEvidence, nil))
		for _, ref := range collectRefs(response) {
			to := resolveRef(fragment, file, ref, schemas)
			fragment.Edges = append(fragment.Edges, edge("serializes", responseID, to, resolutionForTarget(to, schemas), responseEvidence, map[string]any{"ref": ref}))
		}
	}

	security := arrayValue(operation["security"])
	if operation["security"] == nil {
		security = nil
	}
	for _, requirement := range security {
		for _, name := range sortedKeys(mapValue(requirement)) {
			to := securitySchemes[name]
			if to == "" {
				to = unresolved(fragment, file, "security_scheme", name)
			}
			securityEvidence := addEvidence(fragment, file, "openapi.security_requirement", anchor+"/security", name)
			fragment.Edges = append(fragment.Edges, edge("authenticates", id, to, resolutionForSecurity(to, securitySchemes), securityEvidence, nil))
		}
	}
}

func resolveRef(fragment *rkcmodel.Fragment, file pluginapi.FileRef, ref string, known map[string]string) string {
	if id := known[ref]; id != "" {
		return id
	}
	return unresolved(fragment, file, "schema_ref", ref)
}

func unresolved(fragment *rkcmodel.Fragment, file pluginapi.FileRef, namespace, name string) string {
	id := rkcmodel.StableID("node", "unresolved_symbol", "openapi", namespace, name)
	for _, node := range fragment.Nodes {
		if node.ID == id {
			return id
		}
	}
	fragment.Nodes = append(fragment.Nodes, rkcmodel.Node{ID: id, Kind: "unresolved_symbol", Name: name, QualifiedName: name, Language: "openapi", Visibility: "unknown", ArtifactID: file.ArtifactID, Attributes: map[string]any{"namespace": namespace, "placeholder": true}})
	return id
}

func addEvidence(fragment *rkcmodel.Fragment, file pluginapi.FileRef, method, anchor, detail string) string {
	id := rkcmodel.StableID("evidence", PluginID, file.ArtifactID, method, anchor, detail)
	fragment.Evidence = append(fragment.Evidence, rkcmodel.Evidence{
		ID: id, Kind: "manifest", Method: method, Confidence: 1, Source: source(file, anchor),
		Tool: PluginID, ToolVersion: PluginVersion, InputDigest: file.SHA256, Detail: detail,
	})
	return id
}

func edge(kind, from, to, resolution, evidence string, attributes map[string]any) rkcmodel.Edge {
	return rkcmodel.Edge{ID: rkcmodel.StableID("edge", kind, from, to, evidence), Kind: kind, From: from, To: to, Resolution: resolution, Confidence: confidence(resolution), Producer: PluginID, EvidenceIDs: []string{evidence}, Attributes: attributes}
}

func source(file pluginapi.FileRef, anchor string) *rkcmodel.SourceRange {
	return &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, Anchor: anchor}
}

func diagnostic(file pluginapi.FileRef, code, message, severity string) rkcmodel.Diagnostic {
	return rkcmodel.Diagnostic{ID: rkcmodel.StableID("diagnostic", PluginID, file.ArtifactID, code, message), Severity: severity, Code: code, Message: message, Stage: "framework_openapi", Plugin: PluginID, Source: source(file, "#")}
}

func isOpenAPI(document map[string]any) bool {
	return (stringValue(document["openapi"]) != "" || stringValue(document["swagger"]) != "") && len(mapValue(document["paths"])) > 0
}
func likelyOpenAPIPath(path string) bool {
	lower := strings.ToLower(filepath.Base(path))
	return strings.Contains(lower, "openapi") || strings.Contains(lower, "swagger")
}
func mapValue(value any) map[string]any { result, _ := value.(map[string]any); return result }
func arrayValue(value any) []any        { result, _ := value.([]any); return result }
func stringValue(value any) string      { result, _ := value.(string); return result }
func boolValue(value any) bool          { result, _ := value.(bool); return result }
func stringSlice(value any) []string {
	var out []string
	for _, item := range arrayValue(value) {
		if text := stringValue(item); text != "" {
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out
}
func stringSliceFromObjects(value any, key string) []string {
	var out []string
	for _, item := range arrayValue(value) {
		if text := stringValue(mapValue(item)[key]); text != "" {
			out = append(out, text)
		}
	}
	sort.Strings(out)
	return out
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
func escapeJSONPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
func logicalOperationID(operationID, method, path string) string {
	if operationID != "" {
		return rkcmodel.StableID("logical", "api_operation", operationID)
	}
	return rkcmodel.StableID("logical", "api_operation", method, path)
}
func stability(operation map[string]any) string {
	if boolValue(operation["deprecated"]) {
		return "deprecated"
	}
	return "unspecified"
}
func parameterSignature(parameter, schema map[string]any) string {
	return fmt.Sprintf("%s %s %s", first(stringValue(parameter["in"]), "unknown"), first(stringValue(parameter["name"]), "parameter"), first(stringValue(schema["type"]), stringValue(parameter["type"]), "any"))
}
func confidence(resolution string) float64 {
	if rkcmodel.IsResolvedResolution(resolution) {
		return 1
	}
	return 0.5
}
func resolutionForTarget(id string, known map[string]string) string {
	for _, candidate := range known {
		if candidate == id {
			return "declared"
		}
	}
	return "unresolved"
}
func resolutionForSecurity(id string, known map[string]string) string {
	for _, candidate := range known {
		if candidate == id {
			return "declared"
		}
	}
	return "unresolved"
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
