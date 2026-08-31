// Package manifest extracts projects, dependencies, build targets, and
// container stages from deterministic repository manifests.
package manifest

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/internal/security/secrets"
	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	// PluginID is the stable producer identity attached to manifest facts.
	PluginID = "rkc.manifest"
	// PluginVersion identifies the extraction semantics used by this adapter.
	PluginVersion            = "0.3.0"
	maxDockerInstructionSize = 4 * 1024 * 1024
)

// Options supplies the materialized repository root and digest-bound manifest
// candidates. Extract recognizes only its explicit, inert manifest formats.
type Options struct {
	Root  string
	Files []pluginapi.FileRef
}

type collector struct {
	fragment rkcmodel.Fragment
	seenNode map[string]struct{}
	seenEdge map[string]struct{}
}

// Extract deterministically parses supported package, module, dependency, and
// container manifests without executing scripts or repository build logic.
// Read or parse failures are retained as diagnostics in the sorted fragment.
func Extract(options Options) (rkcmodel.Fragment, error) {
	collector := collector{seenNode: map[string]struct{}{}, seenEdge: map[string]struct{}{}}
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, file := range files {
		base := strings.ToLower(filepath.Base(file.Path))
		switch base {
		case "package.json":
			collector.packageJSON(options.Root, file)
		case "go.mod":
			collector.goMod(options.Root, file)
		case "dockerfile":
			collector.dockerfile(options.Root, file)
		default:
			if strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt") {
				collector.requirements(options.Root, file)
			}
		}
	}
	rkcmodel.SortFragment(&collector.fragment)
	return collector.fragment, nil
}

func (c *collector) packageJSON(root string, file pluginapi.FileRef) {
	data, err := sourcepath.ReadFile(root, file.Path)
	if err != nil {
		c.error(file, "RKC-MAN-1001", err.Error())
		return
	}
	var document map[string]any
	if err := decodeJSONObject(data, &document); err != nil {
		c.error(file, "RKC-MAN-1002", "invalid package.json: "+err.Error())
		return
	}
	name := stringValue(document["name"])
	if name == "" {
		name = filepath.Base(filepath.Dir(file.Path))
		if name == "." || name == "" {
			name = "npm-project"
		}
	}
	projectID := c.project(file, "npm", name, stringValue(document["version"]), "package.json")
	for _, group := range []struct {
		name                                    string
		production, development, optional, peer bool
	}{
		{"dependencies", true, false, false, false}, {"devDependencies", false, true, false, false},
		{"optionalDependencies", false, false, true, false}, {"peerDependencies", false, false, false, true},
	} {
		dependencies := mapValue(document[group.name])
		for _, dependency := range sortedKeys(dependencies) {
			version := stringValue(dependencies[dependency])
			dependencyID := c.dependency(file, "npm", dependency, version, "#/"+group.name+"/"+escapeJSONPointer(dependency))
			c.addEdge("depends_on", projectID, dependencyID, "declared", file, "package.json.dependency", "#/"+group.name+"/"+escapeJSONPointer(dependency), map[string]any{
				"scope": group.name, "production": group.production, "development": group.development, "optional": group.optional, "peer": group.peer, "constraint": version,
			})
		}
	}
	scripts := mapValue(document["scripts"])
	for _, script := range sortedKeys(scripts) {
		command := stringValue(scripts[script])
		id := rkcmodel.StableID("node", "build_target", file.Path, script)
		evidence := c.addEvidence(file, "package.json.script", "#/scripts/"+escapeJSONPointer(script), script)
		attributes := commandMetadata("npm-script", command)
		attributes["ecosystem"] = "npm"
		c.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", "npm-script", name, script), Kind: "build_target", Name: script, QualifiedName: name + " script " + script, Language: "shell", Visibility: "repository", ArtifactID: file.ArtifactID, Source: source(file, "#/scripts/"+escapeJSONPointer(script)), EvidenceIDs: []string{evidence}, Attributes: attributes})
		c.addEdgeWithEvidence("builds", projectID, id, "declared", evidence, nil)
	}
	binaries := mapValue(document["bin"])
	if path := stringValue(document["bin"]); path != "" {
		binaries = map[string]any{name: path}
	}
	for _, bin := range sortedKeys(binaries) {
		path := stringValue(binaries[bin])
		if path == "" {
			continue
		}
		id := rkcmodel.StableID("node", "cli_command", file.Path, bin)
		evidence := c.addEvidence(file, "package.json.bin", "#/bin/"+escapeJSONPointer(bin), bin)
		c.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", "cli", bin), Kind: "cli_command", Name: bin, QualifiedName: name + " CLI " + bin, Signature: bin, Language: "javascript", Visibility: "public", PublicSurface: true, ArtifactID: file.ArtifactID, Source: source(file, "#/bin/"+escapeJSONPointer(bin)), EvidenceIDs: []string{evidence}, Attributes: map[string]any{"entrypoint": path, "ecosystem": "npm"}})
		c.addEdgeWithEvidence("exposes", projectID, id, "declared", evidence, nil)
	}
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

func (c *collector) goMod(root string, file pluginapi.FileRef) {
	data, err := sourcepath.ReadFile(root, file.Path)
	if err != nil {
		c.error(file, "RKC-MAN-1101", err.Error())
		return
	}
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	module := "go-module"
	version := ""
	for _, raw := range lines {
		fields := strings.Fields(stripLineComment(raw))
		if len(fields) >= 2 && fields[0] == "module" {
			module = fields[1]
		}
		if len(fields) >= 2 && fields[0] == "go" {
			version = fields[1]
		}
	}
	projectID := c.project(file, "go", module, version, "go.mod")
	inRequire := false
	inReplace := false
	for index, raw := range lines {
		line := strings.TrimSpace(stripLineComment(raw))
		if line == "" {
			continue
		}
		if line == "require (" {
			inRequire = true
			continue
		}
		if line == "replace (" {
			inReplace = true
			continue
		}
		if line == ")" {
			inRequire = false
			inReplace = false
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		requireLine := inRequire
		inlineReplace := false
		if fields[0] == "require" {
			fields = fields[1:]
			requireLine = true
		} else if fields[0] == "replace" {
			fields = fields[1:]
			inlineReplace = true
		}
		if requireLine && len(fields) >= 2 {
			dependencyID := c.dependency(file, "go", fields[0], fields[1], fmt.Sprintf("line:%d", index+1))
			c.addEdge("depends_on", projectID, dependencyID, "declared", file, "go.mod.require", fmt.Sprintf("line:%d", index+1), map[string]any{"constraint": fields[1], "indirect": strings.Contains(raw, "// indirect")})
		}
		if inReplace || inlineReplace {
			arrow := indexOf(fields, "=>")
			if arrow > 0 && arrow+1 < len(fields) {
				from, to := fields[0], fields[arrow+1]
				fromID := c.dependency(file, "go", from, "", fmt.Sprintf("line:%d", index+1))
				toID := c.dependency(file, "go", to, joinAfter(fields, arrow+2), fmt.Sprintf("line:%d", index+1))
				c.addEdge("supersedes", toID, fromID, "declared", file, "go.mod.replace", fmt.Sprintf("line:%d", index+1), nil)
			}
		}
	}
}

var requirementName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*`)

func (c *collector) requirements(root string, file pluginapi.FileRef) {
	input, err := sourcepath.OpenRegular(root, file.Path)
	if err != nil {
		c.error(file, "RKC-MAN-1201", err.Error())
		return
	}
	defer input.Close()
	projectName := filepath.Base(filepath.Dir(file.Path))
	if projectName == "." || projectName == "" {
		projectName = "python-project"
	}
	projectID := c.project(file, "pypi", projectName, "", "requirements")
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "-") {
			continue
		}
		line = strings.SplitN(line, " #", 2)[0]
		name := requirementName.FindString(line)
		if name == "" {
			continue
		}
		constraint := strings.TrimSpace(strings.TrimPrefix(line, name))
		dependencyID := c.dependency(file, "pypi", name, constraint, fmt.Sprintf("line:%d", lineNumber))
		c.addEdge("depends_on", projectID, dependencyID, "declared", file, "requirements.entry", fmt.Sprintf("line:%d", lineNumber), map[string]any{"constraint": constraint})
	}
	if err := scanner.Err(); err != nil {
		c.error(file, "RKC-MAN-1202", err.Error())
	}
}

func (c *collector) dockerfile(root string, file pluginapi.FileRef) {
	input, err := sourcepath.OpenRegular(root, file.Path)
	if err != nil {
		c.error(file, "RKC-MAN-1301", err.Error())
		return
	}
	defer input.Close()
	projectName := filepath.Base(filepath.Dir(file.Path))
	if projectName == "." || projectName == "" {
		projectName = "container-build"
	}
	projectID := c.project(file, "docker", projectName, "", "Dockerfile")
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), maxDockerInstructionSize)
	currentStage := projectID
	err = scanDockerInstructions(scanner, maxDockerInstructionSize, func(instruction dockerInstruction) {
		line := instruction.text
		fields := strings.Fields(line)
		if len(fields) == 0 {
			return
		}
		lineNumber := instruction.startLine
		sourceRange := dockerSourceRange(file, instruction)
		directive := strings.ToUpper(fields[0])
		switch directive {
		case "FROM":
			if len(fields) < 2 {
				return
			}
			image := fields[1]
			stageName := fmt.Sprintf("stage-%d", lineNumber)
			for i := 2; i+1 < len(fields); i++ {
				if strings.EqualFold(fields[i], "AS") {
					stageName = fields[i+1]
					break
				}
			}
			stageID := rkcmodel.StableID("node", "build_target", file.Path, stageName)
			evidence := c.addDockerEvidence(file, "dockerfile.from", instruction, line)
			c.addNode(rkcmodel.Node{ID: stageID, LogicalID: rkcmodel.StableID("logical", "docker-stage", projectName, stageName), Kind: "build_target", Name: stageName, QualifiedName: file.Path + " stage " + stageName, Language: "dockerfile", Visibility: "repository", ArtifactID: file.ArtifactID, Source: sourceRange, EvidenceIDs: []string{evidence}, Attributes: map[string]any{"base_image": image, "line": lineNumber}})
			c.addEdgeWithEvidence("builds", projectID, stageID, "declared", evidence, nil)
			imageID := rkcmodel.StableID("node", "container_image", image)
			c.addNode(rkcmodel.Node{ID: imageID, LogicalID: imageID, Kind: "container_image", Name: image, QualifiedName: image, Language: "oci", Visibility: "external", EvidenceIDs: []string{evidence}, Attributes: map[string]any{"reference": image}})
			c.addEdgeWithEvidence("depends_on", stageID, imageID, "declared", evidence, nil)
			currentStage = stageID
		case "EXPOSE":
			for _, port := range fields[1:] {
				id := rkcmodel.StableID("node", "config_key", file.Path, "port", port)
				evidence := c.addDockerEvidence(file, "dockerfile.expose", instruction, port)
				c.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", "container-port", projectName, port), Kind: "config_key", Name: port, QualifiedName: file.Path + " exposed port " + port, Language: "dockerfile", Visibility: "public", PublicSurface: true, ArtifactID: file.ArtifactID, Source: sourceRange, EvidenceIDs: []string{evidence}, Attributes: map[string]any{"kind": "port"}})
				c.addEdgeWithEvidence("exposes", currentStage, id, "declared", evidence, nil)
			}
		case "ENV", "ARG":
			for _, assignment := range dockerAssignments(fields[1:]) {
				name := assignment.name
				if name == "" {
					continue
				}
				kind := "environment_variable"
				if directive == "ARG" {
					kind = "config_key"
				}
				secretLike := secrets.IsSecretName(name)
				id := rkcmodel.StableID("node", kind, file.Path, name)
				evidence := c.addDockerEvidence(file, "dockerfile."+strings.ToLower(directive), instruction, name)
				attributes := map[string]any{"directive": directive, "has_default": assignment.value != "", "secret_like": secretLike}
				if assignment.value != "" && !secretLike {
					attributes["default_sha256"] = manifestValueDigest("docker-default", assignment.value)
					attributes["default_class"] = scalarClassification(assignment.value)
					attributes["default_length_bytes"] = len(assignment.value)
				}
				c.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", kind, projectName, name), Kind: kind, Name: name, QualifiedName: file.Path + " " + name, Language: "dockerfile", Visibility: "repository", ArtifactID: file.ArtifactID, Source: sourceRange, EvidenceIDs: []string{evidence}, Attributes: attributes})
				c.addEdgeWithEvidence("configures", currentStage, id, "declared", evidence, nil)
			}
		}
	})
	if err != nil {
		c.error(file, "RKC-MAN-1302", err.Error())
	}
}

type dockerInstruction struct {
	text      string
	startLine int
	endLine   int
}

// scanDockerInstructions joins Docker's backslash-continuation lines without
// retaining the whole file. The explicit logical-instruction limit prevents a
// file containing indefinitely many continued physical lines from growing the
// builder without bound.
func scanDockerInstructions(scanner *bufio.Scanner, limit int, visit func(dockerInstruction)) error {
	var logical strings.Builder
	lineNumber := 0
	startLine := 0
	endLine := 0
	for scanner.Scan() {
		lineNumber++
		physical := strings.TrimSpace(scanner.Text())
		if physical == "" || strings.HasPrefix(physical, "#") {
			continue
		}
		if startLine == 0 {
			startLine = lineNumber
		}
		part, continued := dockerContinuation(physical)
		separatorSize := 0
		if logical.Len() > 0 && part != "" {
			separatorSize = 1
		}
		if limit < 1 || separatorSize > limit-logical.Len() || len(part) > limit-logical.Len()-separatorSize {
			return fmt.Errorf("Dockerfile instruction beginning at line %d exceeds %d bytes", startLine, limit)
		}
		if separatorSize != 0 {
			logical.WriteByte(' ')
		}
		logical.WriteString(part)
		endLine = lineNumber
		if continued {
			continue
		}
		if text := strings.TrimSpace(logical.String()); text != "" {
			visit(dockerInstruction{text: text, startLine: startLine, endLine: endLine})
		}
		logical.Reset()
		startLine = 0
		endLine = 0
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if text := strings.TrimSpace(logical.String()); text != "" {
		visit(dockerInstruction{text: text, startLine: startLine, endLine: endLine})
	}
	return nil
}

func dockerContinuation(line string) (string, bool) {
	backslashes := 0
	for index := len(line) - 1; index >= 0 && line[index] == '\\'; index-- {
		backslashes++
	}
	if backslashes%2 == 0 {
		return line, false
	}
	return strings.TrimSpace(line[:len(line)-1]), true
}

func dockerLineAnchor(instruction dockerInstruction) string {
	if instruction.startLine == instruction.endLine {
		return fmt.Sprintf("line:%d", instruction.startLine)
	}
	return fmt.Sprintf("line:%d-%d", instruction.startLine, instruction.endLine)
}

func dockerSourceRange(file pluginapi.FileRef, instruction dockerInstruction) *rkcmodel.SourceRange {
	return &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: instruction.startLine, EndLine: instruction.endLine}
}

func (c *collector) addDockerEvidence(file pluginapi.FileRef, method string, instruction dockerInstruction, detail string) string {
	anchor := dockerLineAnchor(instruction)
	id := rkcmodel.StableID("evidence", PluginID, file.ArtifactID, method, anchor, detail)
	evidenceSource := dockerSourceRange(file, instruction)
	evidenceSource.Anchor = anchor
	c.fragment.Evidence = append(c.fragment.Evidence, rkcmodel.Evidence{ID: id, Kind: "manifest", Method: method, Confidence: 1, Source: evidenceSource, Tool: PluginID, ToolVersion: PluginVersion, InputDigest: file.SHA256, Detail: detail})
	return id
}

type dockerAssignment struct {
	name  string
	value string
}

func dockerAssignments(fields []string) []dockerAssignment {
	if len(fields) == 0 {
		return nil
	}
	if !strings.Contains(fields[0], "=") {
		return []dockerAssignment{{name: fields[0], value: strings.Join(fields[1:], " ")}}
	}
	result := make([]dockerAssignment, 0, len(fields))
	for _, field := range fields {
		parts := strings.SplitN(field, "=", 2)
		value := ""
		if len(parts) == 2 {
			value = parts[1]
		}
		result = append(result, dockerAssignment{name: parts[0], value: value})
	}
	return result
}

func (c *collector) project(file pluginapi.FileRef, ecosystem, name, version, method string) string {
	id := rkcmodel.StableID("node", "project", file.Path, ecosystem, name)
	evidence := c.addEvidence(file, method, "#", name)
	c.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", "project", ecosystem, name), Kind: "project", Name: name, QualifiedName: ecosystem + ":" + name, Language: ecosystem, Visibility: "repository", ArtifactID: file.ArtifactID, Source: source(file, "#"), EvidenceIDs: []string{evidence}, Attributes: map[string]any{"ecosystem": ecosystem, "version": version, "manifest": file.Path}})
	c.addEdgeWithEvidence("derived_from", id, file.ArtifactID, "declared", evidence, nil)
	return id
}
func (c *collector) dependency(file pluginapi.FileRef, ecosystem, name, version, anchor string) string {
	id := rkcmodel.StableID("node", "external_dependency", ecosystem, name)
	evidence := c.addEvidence(file, "manifest.dependency", anchor, name)
	c.addNode(rkcmodel.Node{ID: id, LogicalID: id, Kind: "external_dependency", Name: name, QualifiedName: ecosystem + ":" + name, Language: ecosystem, Visibility: "external", EvidenceIDs: []string{evidence}, Attributes: map[string]any{"ecosystem": ecosystem, "constraint": version}})
	return id
}
func (c *collector) addEvidence(file pluginapi.FileRef, method, anchor, detail string) string {
	id := rkcmodel.StableID("evidence", PluginID, file.ArtifactID, method, anchor, detail)
	c.fragment.Evidence = append(c.fragment.Evidence, rkcmodel.Evidence{ID: id, Kind: "manifest", Method: method, Confidence: 1, Source: source(file, anchor), Tool: PluginID, ToolVersion: PluginVersion, InputDigest: file.SHA256, Detail: detail})
	return id
}
func (c *collector) addEdge(kind, from, to, resolution string, file pluginapi.FileRef, method, anchor string, attributes map[string]any) {
	evidence := c.addEvidence(file, method, anchor, kind)
	c.addEdgeWithEvidence(kind, from, to, resolution, evidence, attributes)
}
func (c *collector) addEdgeWithEvidence(kind, from, to, resolution, evidence string, attributes map[string]any) {
	id := rkcmodel.StableID("edge", kind, from, to, evidence)
	if _, ok := c.seenEdge[id]; ok {
		return
	}
	c.seenEdge[id] = struct{}{}
	c.fragment.Edges = append(c.fragment.Edges, rkcmodel.Edge{ID: id, Kind: kind, From: from, To: to, Resolution: resolution, Confidence: 1, Producer: PluginID, EvidenceIDs: []string{evidence}, Attributes: attributes})
}
func (c *collector) addNode(node rkcmodel.Node) {
	if _, ok := c.seenNode[node.ID]; ok {
		return
	}
	c.seenNode[node.ID] = struct{}{}
	c.fragment.Nodes = append(c.fragment.Nodes, node)
}
func (c *collector) error(file pluginapi.FileRef, code, message string) {
	c.fragment.Diagnostics = append(c.fragment.Diagnostics, rkcmodel.Diagnostic{ID: rkcmodel.StableID("diagnostic", PluginID, file.ArtifactID, code, message), Severity: "error", Code: code, Message: message, Stage: "framework_manifest", Plugin: PluginID, Source: source(file, "#")})
}
func source(file pluginapi.FileRef, anchor string) *rkcmodel.SourceRange {
	return &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, Anchor: anchor}
}
func mapValue(value any) map[string]any { result, _ := value.(map[string]any); return result }
func stringValue(value any) string      { result, _ := value.(string); return result }
func sortedKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
func escapeJSONPointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
func stripLineComment(value string) string {
	if index := strings.Index(value, "//"); index >= 0 {
		return value[:index]
	}
	return value
}
func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
func joinAfter(values []string, index int) string {
	if index >= len(values) {
		return ""
	}
	return strings.Join(values[index:], " ")
}

func manifestValueDigest(domain, value string) string {
	digest := sha256.Sum256([]byte(PluginID + "\x00" + domain + "\x00" + value))
	return fmt.Sprintf("%x", digest)
}

func scalarClassification(value string) string {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	switch {
	case trimmed == "":
		return "empty"
	case strings.Contains(trimmed, "${") || strings.Contains(trimmed, "$(") || strings.Contains(trimmed, "{{"):
		return "expression"
	case lower == "true" || lower == "false" || lower == "yes" || lower == "no" || lower == "on" || lower == "off":
		return "boolean"
	case func() bool { _, err := strconv.ParseFloat(trimmed, 64); return err == nil }():
		return "number"
	case strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://"):
		return "url"
	case strings.HasPrefix(trimmed, "/") || strings.HasPrefix(trimmed, "./") || strings.HasPrefix(trimmed, "../"):
		return "path"
	default:
		return "literal"
	}
}

func commandMetadata(domain, command string) map[string]any {
	lower := strings.ToLower(command)
	class := "command"
	switch {
	case strings.TrimSpace(command) == "":
		class = "empty"
	case strings.Contains(lower, " test") || strings.HasPrefix(strings.TrimSpace(lower), "test ") ||
		strings.Contains(lower, "pytest") || strings.Contains(lower, "go test"):
		class = "test"
	case strings.Contains(lower, "lint") || strings.Contains(lower, "vet") || strings.Contains(lower, "ruff"):
		class = "quality"
	case strings.Contains(lower, "deploy") || strings.Contains(lower, "publish") || strings.Contains(lower, "release"):
		class = "deploy"
	case strings.Contains(lower, "build") || strings.Contains(lower, "compile"):
		class = "build"
	}
	return map[string]any{
		"command_sha256":       manifestValueDigest(domain, command),
		"command_class":        class,
		"command_length_bytes": len(command),
		"command_dynamic": strings.Contains(command, "${") || strings.Contains(command, "$(") ||
			strings.Contains(command, "{{"),
	}
}
