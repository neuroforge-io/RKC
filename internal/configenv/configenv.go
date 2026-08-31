// Package configenv compiles build configuration and environment contracts
// into the canonical graph: Go build tags, CI workflow definitions, Terraform
// resources, and environment-variable declarations. Together with the
// value-flow stage's code-level environment reads, it answers "what calls what
// in what environment when feature X is enabled or disabled".
//
// The analysis is deterministic and bounded. YAML and HCL files are parsed
// with a conservative indentation-based key extractor, never a full language
// parser. Environment defaults and CI command bodies are represented only by
// domain-separated fingerprints and coarse classifications; no repository code
// or toolchain is executed.
package configenv

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// PluginID is the stable producer identity attached to every config-env fact.
const PluginID = "rkc.configenv"

// PluginVersion identifies the bounded config-environment semantics.
const PluginVersion = "1.2.0"

const (
	maximumConfigFileBytes     = 1 << 20
	maximumWorkflowLines       = 20000
	maximumConfigNodes         = 262144
	maximumEnvNodes            = 65536
	maximumTerraformStatements = 4096
	maximumRetainedTextBytes   = 4096
	maximumRetainedBytes       = 64 << 20
	maximumRetainedFacts       = 65536
	maximumDiagnostics         = 256
	retainedFactOverheadBytes  = 512
)

type extractionLimits struct {
	configFileBytes     int
	workflowLines       int
	configNodes         int
	environmentNodes    int
	terraformStatements int
	retainedTextBytes   int
	retainedBytes       int
	retainedFacts       int
}

var defaultExtractionLimits = extractionLimits{
	configFileBytes:     maximumConfigFileBytes,
	workflowLines:       maximumWorkflowLines,
	configNodes:         maximumConfigNodes,
	environmentNodes:    maximumEnvNodes,
	terraformStatements: maximumTerraformStatements,
	retainedTextBytes:   maximumRetainedTextBytes,
	retainedBytes:       maximumRetainedBytes,
	retainedFacts:       maximumRetainedFacts,
}

func mergeUniqueStrings(existing []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(existing)+len(additions))
	merged := make([]string, 0, len(existing)+len(additions))
	for _, value := range append(append([]string(nil), existing...), additions...) {
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		merged = append(merged, value)
	}
	sort.Strings(merged)
	return merged
}

func mergeEnvironmentDeclarations(existing *rkcmodel.Node, incoming rkcmodel.Node) {
	if existing.Attributes == nil {
		existing.Attributes = map[string]any{}
	}
	existingDeclarations, _ := existing.Attributes["declarations"].([]map[string]any)
	incomingDeclarations, _ := incoming.Attributes["declarations"].([]map[string]any)
	for _, declaration := range incomingDeclarations {
		existingDeclarations = append(existingDeclarations, declaration)
	}
	existing.Attributes["declarations"] = existingDeclarations
	existing.Attributes["declaration_count"] = len(existingDeclarations)
	for _, key := range []string{"ci_sources", "source_files"} {
		existingValues, _ := existing.Attributes[key].([]string)
		incomingValues, _ := incoming.Attributes[key].([]string)
		existing.Attributes[key] = mergeUniqueStrings(existingValues, incomingValues...)
	}
}

func retainedTextUsage(value reflect.Value, maximumTextBytes, depth int) (int, bool) {
	if !value.IsValid() {
		return 0, false
	}
	if depth > 16 {
		return 0, true
	}
	if value.Kind() == reflect.Interface || value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return 0, false
		}
		return retainedTextUsage(value.Elem(), maximumTextBytes, depth+1)
	}
	if value.Kind() == reflect.String {
		length := value.Len()
		return length, length > maximumTextBytes
	}
	total := 0
	add := func(next int) bool {
		maximumInt := int(^uint(0) >> 1)
		if next > maximumInt-total {
			total = maximumInt
			return false
		}
		total += next
		return true
	}
	switch value.Kind() {
	case reflect.Struct:
		for index := 0; index < value.NumField(); index++ {
			count, oversized := retainedTextUsage(value.Field(index), maximumTextBytes, depth+1)
			if oversized || !add(count) {
				return total, true
			}
		}
	case reflect.Slice, reflect.Array:
		for index := 0; index < value.Len(); index++ {
			count, oversized := retainedTextUsage(value.Index(index), maximumTextBytes, depth+1)
			if oversized || !add(count) {
				return total, true
			}
		}
	case reflect.Map:
		iterator := value.MapRange()
		for iterator.Next() {
			for _, item := range []reflect.Value{iterator.Key(), iterator.Value()} {
				count, oversized := retainedTextUsage(item, maximumTextBytes, depth+1)
				if oversized || !add(count) {
					return total, true
				}
			}
		}
	}
	return total, false
}

// Options supplies the trusted repository root and admitted file references.
type Options struct {
	Root  string
	Files []pluginapi.FileRef
}

// Extract compiles bounded configuration and environment facts from the
// admitted files. The fragment is deterministic for identical inputs.
func Extract(ctx context.Context, options Options) (rkcmodel.Fragment, error) {
	return extractWithLimits(ctx, options, defaultExtractionLimits)
}

func extractWithLimits(ctx context.Context, options Options, limits extractionLimits) (rkcmodel.Fragment, error) {
	if ctx == nil {
		return rkcmodel.Fragment{}, errors.New("config-env analysis context is required")
	}
	if strings.TrimSpace(options.Root) == "" {
		return rkcmodel.Fragment{}, errors.New("config-env analysis root is required")
	}
	if limits.configFileBytes <= 0 || limits.workflowLines <= 0 || limits.configNodes <= 0 ||
		limits.environmentNodes <= 0 || limits.terraformStatements <= 0 ||
		limits.retainedTextBytes <= 0 || limits.retainedBytes <= 0 || limits.retainedFacts <= 0 {
		return rkcmodel.Fragment{}, errors.New("config-env analysis limits must be positive")
	}
	fragment := rkcmodel.Fragment{}
	nodeIndexes := map[string]int{}
	edgeIndexes := map[string]int{}
	environmentNodes := 0
	configNodeLimitReached := false
	environmentNodeLimitReached := false
	retainedTextLimitReached := false
	retainedBudgetReached := false
	retainedBytes := 0
	retainedFacts := 0
	diagnosticLimitReached := false
	seenDiagnostics := map[string]struct{}{}
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	reserveFact := func(value any) bool {
		textBytes, oversized := retainedTextUsage(reflect.ValueOf(value), limits.retainedTextBytes, 0)
		if oversized {
			retainedTextLimitReached = true
			return false
		}
		cost := textBytes + retainedFactOverheadBytes
		if retainedFacts >= limits.retainedFacts || cost > limits.retainedBytes-retainedBytes {
			retainedBudgetReached = true
			return false
		}
		retainedFacts++
		retainedBytes += cost
		return true
	}
	addNode := func(node rkcmodel.Node) bool {
		if index, exists := nodeIndexes[node.ID]; exists {
			existing := &fragment.Nodes[index]
			mergedEvidence := mergeUniqueStrings(existing.EvidenceIDs, node.EvidenceIDs...)
			if len(mergedEvidence) == len(existing.EvidenceIDs) {
				return true
			}
			// Count every distinct declaration conservatively against the retained
			// fact budget before expanding a deduplicated canonical node.
			if !reserveFact(node) {
				return false
			}
			existing.EvidenceIDs = mergedEvidence
			if existing.Kind == "environment_variable" && node.Kind == existing.Kind {
				mergeEnvironmentDeclarations(existing, node)
			}
			return true
		}
		if len(nodeIndexes) >= limits.configNodes {
			configNodeLimitReached = true
			return false
		}
		if node.Kind == "environment_variable" && environmentNodes >= limits.environmentNodes {
			environmentNodeLimitReached = true
			return false
		}
		if !reserveFact(node) {
			return false
		}
		nodeIndexes[node.ID] = len(fragment.Nodes)
		if node.Kind == "environment_variable" {
			environmentNodes++
		}
		fragment.Nodes = append(fragment.Nodes, node)
		return true
	}
	addEdge := func(kind, from, to string, attributes map[string]any, evidenceIDs ...string) {
		id := rkcmodel.StableID("edge", kind, from, to)
		evidenceIDs = mergeUniqueStrings(nil, evidenceIDs...)
		if len(evidenceIDs) == 0 {
			return
		}
		if index, exists := edgeIndexes[id]; exists {
			existing := &fragment.Edges[index]
			mergedEvidence := mergeUniqueStrings(existing.EvidenceIDs, evidenceIDs...)
			if len(mergedEvidence) == len(existing.EvidenceIDs) {
				return
			}
			candidate := *existing
			candidate.EvidenceIDs = mergedEvidence
			if !reserveFact(candidate) {
				return
			}
			existing.EvidenceIDs = mergedEvidence
			return
		}
		edge := rkcmodel.Edge{
			ID: id, Kind: kind, From: from, To: to, Resolution: "declared",
			Confidence: 1.0, Producer: PluginID, Attributes: attributes, EvidenceIDs: evidenceIDs,
		}
		if !reserveFact(edge) {
			return
		}
		edgeIndexes[id] = len(fragment.Edges)
		fragment.Edges = append(fragment.Edges, edge)
	}
	addEvidence := func(method string, file pluginapi.FileRef, line int, detail string) string {
		id := rkcmodel.StableID("evidence", PluginID, method, file.Path, fmt.Sprint(line), detail)
		evidence := rkcmodel.Evidence{
			ID: id, Kind: "declared", Method: method, Confidence: 1.0,
			Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: line, EndLine: line},
			Tool:   PluginID, ToolVersion: PluginVersion, InputDigest: file.SHA256, Detail: detail,
		}
		if !reserveFact(evidence) {
			return ""
		}
		fragment.Evidence = append(fragment.Evidence, evidence)
		return id
	}
	addDiagnostic := func(code, message string) {
		if len(fragment.Diagnostics) >= maximumDiagnostics || len(message) > limits.retainedTextBytes {
			diagnosticLimitReached = true
			return
		}
		identity := code + "\x00" + message
		if _, duplicate := seenDiagnostics[identity]; duplicate {
			return
		}
		seenDiagnostics[identity] = struct{}{}
		fragment.Diagnostics = append(fragment.Diagnostics, rkcmodel.Diagnostic{
			ID: rkcmodel.StableID("diagnostic", PluginID, code, message), Severity: "note", Code: code,
			Message: message, Stage: "config_env", Plugin: PluginID + "@" + PluginVersion,
		})
	}
	addSummaryDiagnostic := func(code, message string) {
		identity := code + "\x00" + message
		if _, duplicate := seenDiagnostics[identity]; duplicate {
			return
		}
		seenDiagnostics[identity] = struct{}{}
		fragment.Diagnostics = append(fragment.Diagnostics, rkcmodel.Diagnostic{
			ID: rkcmodel.StableID("diagnostic", PluginID, code, message), Severity: "note", Code: code,
			Message: message, Stage: "config_env", Plugin: PluginID + "@" + PluginVersion,
		})
	}

	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return rkcmodel.Fragment{}, err
		}
		if retainedBudgetReached {
			break
		}
		if len(nodeIndexes) >= limits.configNodes {
			configNodeLimitReached = true
			break
		}
		switch {
		case strings.HasSuffix(file.Path, ".go"):
			extractBuildTags(ctx, options.Root, file, addNode, addEdge, addEvidence)
		case isWorkflowFile(file.Path):
			if err := extractWorkflow(ctx, options.Root, file, limits, addNode, addEdge, addEvidence, addDiagnostic); err != nil {
				return rkcmodel.Fragment{}, err
			}
		case strings.HasSuffix(file.Path, ".tf"):
			extractTerraform(ctx, options.Root, file, limits, addNode, addEdge, addEvidence, addDiagnostic)
		}
	}
	if configNodeLimitReached {
		addDiagnostic("RKC-CFG-3001", "bounded out at "+fmt.Sprint(limits.configNodes)+" config-env nodes")
	}
	if environmentNodeLimitReached {
		addDiagnostic("RKC-CFG-3005", "bounded out at "+fmt.Sprint(limits.environmentNodes)+" environment-variable nodes")
	}
	if retainedTextLimitReached {
		addSummaryDiagnostic("RKC-CFG-3006", "config-env retained text bound reached ("+fmt.Sprint(limits.retainedTextBytes)+" bytes)")
	}
	if retainedBudgetReached {
		addSummaryDiagnostic("RKC-CFG-3007", "config-env retained output budget reached")
	}
	if diagnosticLimitReached {
		// This fixed, final summary is allowed beyond the ordinary diagnostic cap;
		// otherwise adversarial per-file failures could suppress notice of truncation.
		addSummaryDiagnostic("RKC-CFG-3008", "bounded out at the config-env diagnostic limit")
	}
	// Evidence is created immediately before its candidate node. If a hard node
	// bound rejects that candidate, discard the now-orphaned record rather than
	// publishing evidence that no accepted fact references.
	referencedEvidence := make(map[string]struct{}, len(fragment.Evidence))
	for _, node := range fragment.Nodes {
		for _, evidenceID := range node.EvidenceIDs {
			referencedEvidence[evidenceID] = struct{}{}
		}
	}
	for _, edge := range fragment.Edges {
		for _, evidenceID := range edge.EvidenceIDs {
			referencedEvidence[evidenceID] = struct{}{}
		}
	}
	filteredEvidence := fragment.Evidence[:0]
	for _, evidence := range fragment.Evidence {
		if _, referenced := referencedEvidence[evidence.ID]; referenced {
			filteredEvidence = append(filteredEvidence, evidence)
		}
	}
	fragment.Evidence = filteredEvidence
	rkcmodel.SortBundle(&rkcmodel.Bundle{Nodes: fragment.Nodes, Edges: fragment.Edges})
	return fragment, nil
}

var goBuildConstraint = regexp.MustCompile(`(?m)^//go:build\s+(\S.*)$`)
var legacyBuildConstraint = regexp.MustCompile(`(?m)^//\s*\+build\s+(\S.*)$`)

// extractBuildTags records Go build constraints as build_target nodes bound to
// their files. Only the first constraint block is read (bounded), and only tag
// names are retained; the raw constraint text is kept for provenance.
func extractBuildTags(
	ctx context.Context,
	root string,
	file pluginapi.FileRef,
	addNode func(rkcmodel.Node) bool,
	addEdge func(string, string, string, map[string]any, ...string),
	addEvidence func(string, pluginapi.FileRef, int, string) string,
) {
	input, err := sourcepath.OpenRegular(root, file.Path)
	if err != nil {
		return
	}
	defer input.Close()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), 8*1024)
	prefix := make([]string, 0, 64)
	for scanner.Scan() && len(prefix) < 64 {
		line := scanner.Text()
		if strings.HasPrefix(line, "package ") {
			break
		}
		prefix = append(prefix, line)
	}
	text := strings.Join(prefix, "\n")
	constraint := ""
	if match := goBuildConstraint.FindStringSubmatch(text); match != nil {
		constraint = strings.TrimSpace(match[1])
	} else if match := legacyBuildConstraint.FindStringSubmatch(text); match != nil {
		constraint = strings.TrimSpace(match[1])
	}
	if constraint == "" {
		return
	}
	tags := buildTagNames(constraint)
	fileID := file.ArtifactID
	targetID := rkcmodel.StableID("node", "build_target", "go", file.Path)
	attributes := map[string]any{"constraint": constraint, "kind": "build_constraint"}
	if len(tags) > 0 {
		attributes["build_tags"] = tags
	}
	evidence := addEvidence("configenv.build_tag", file, 1, constraint)
	if evidence == "" {
		return
	}
	if !addNode(rkcmodel.Node{
		ID: targetID, Kind: "build_target", Name: filepath.Base(file.Path) + " [" + constraint + "]",
		QualifiedName: file.Path + " build " + constraint, Language: "go", Visibility: "internal",
		ArtifactID: file.ArtifactID, EvidenceIDs: []string{evidence}, Attributes: attributes,
	}) {
		return
	}
	addEdge("builds", targetID, fileID, nil, evidence)
	_ = ctx
}

// buildTagNames extracts identifier-like tokens from a Go build expression:
// platform names and feature tags, excluding operators and keywords.
var buildTagExcluded = map[string]bool{
	"true": true, "false": true, "ignore": true,
}

func buildTagNames(constraint string) []string {
	tokens := strings.FieldsFunc(constraint, func(character rune) bool {
		return !((character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '_' || character == '.' || character == '/')
	})
	seen := map[string]struct{}{}
	var tags []string
	for _, token := range tokens {
		if token == "" || buildTagExcluded[token] || strings.ContainsAny(token, "!&|()") {
			continue
		}
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		tags = append(tags, token)
	}
	sort.Strings(tags)
	return tags
}

// isWorkflowFile identifies CI workflow definitions from their conventional
// paths.
func isWorkflowFile(path string) bool {
	base := filepath.Base(path)
	if strings.HasSuffix(base, ".yml") || strings.HasSuffix(base, ".yaml") {
		slash := filepath.ToSlash(path)
		if strings.Contains(slash, ".github/workflows/") || base == ".gitlab-ci.yml" ||
			base == "bitbucket-pipelines.yml" || base == "azure-pipelines.yml" ||
			base == "buildkite.yml" || base == "Jenkinsfile" {
			return true
		}
	}
	return base == "Jenkinsfile"
}

func workflowSystem(path string) string {
	slash := filepath.ToSlash(path)
	switch {
	case strings.Contains(slash, ".github/workflows/"):
		return "github"
	case filepath.Base(path) == ".gitlab-ci.yml":
		return "gitlab"
	case filepath.Base(path) == "bitbucket-pipelines.yml":
		return "bitbucket"
	case filepath.Base(path) == "azure-pipelines.yml":
		return "azure"
	case filepath.Base(path) == "buildkite.yml":
		return "buildkite"
	case filepath.Base(path) == "Jenkinsfile":
		return "jenkins"
	default:
		return "ci"
	}
}

type workflowLine struct {
	indent int
	text   string
	number int
}

// readBoundedConfigFile enforces the limit against the opened file's actual
// size and against the bytes read. The second check closes the small race in
// which a file grows after the initial stat. No partial configuration facts are
// emitted for an oversized input.
func readBoundedConfigFile(root string, file pluginapi.FileRef, maximumBytes int) ([]byte, int64, bool, error) {
	input, err := sourcepath.OpenRegular(root, file.Path)
	if err != nil {
		return nil, 0, false, err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return nil, 0, false, err
	}
	if info.Size() > int64(maximumBytes) {
		return nil, info.Size(), true, nil
	}
	data, err := io.ReadAll(io.LimitReader(input, int64(maximumBytes)+1))
	if err != nil {
		return nil, 0, false, err
	}
	actual := int64(len(data))
	if len(data) > maximumBytes {
		return nil, actual, true, nil
	}
	return data, actual, false, nil
}

// extractWorkflow compiles a bounded view of a CI workflow: the workflow node,
// its jobs and steps, and declared environment variables. YAML structure is
// approximated by indentation. Environment defaults and command bodies are
// fingerprinted and classified, never copied into the fragment.
func extractWorkflow(
	ctx context.Context,
	root string,
	file pluginapi.FileRef,
	limits extractionLimits,
	addNode func(rkcmodel.Node) bool,
	addEdge func(string, string, string, map[string]any, ...string),
	addEvidence func(string, pluginapi.FileRef, int, string) string,
	addDiagnostic func(string, string),
) error {
	data, actualBytes, oversized, err := readBoundedConfigFile(root, file, limits.configFileBytes)
	if err != nil {
		return nil
	}
	if oversized {
		addDiagnostic("RKC-CFG-3003", fmt.Sprintf(
			"skipped oversized configuration file %s: %d bytes exceeds the %d-byte limit",
			file.Path, actualBytes, limits.configFileBytes,
		))
		return nil
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), limits.configFileBytes+1)
	var lines []workflowLine
	physicalLine := 0
	workflowLineLimitReached := false
	for scanner.Scan() {
		physicalLine++
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := scanner.Text()
		if strings.TrimSpace(raw) == "" || strings.HasPrefix(strings.TrimSpace(raw), "#") {
			continue
		}
		if len(lines) >= limits.workflowLines {
			workflowLineLimitReached = true
			continue
		}
		indent := 0
		for indent < len(raw) && raw[indent] == ' ' {
			indent++
		}
		if indent%2 == 1 {
			indent++
		}
		lines = append(lines, workflowLine{indent: indent, text: strings.TrimSpace(raw), number: physicalLine})
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan workflow %q: %w", file.Path, err)
	}
	if workflowLineLimitReached {
		addDiagnostic("RKC-CFG-3004", "bounded out at "+fmt.Sprint(limits.workflowLines)+" significant workflow lines in "+file.Path)
	}
	if len(lines) == 0 {
		return nil
	}
	system := workflowSystem(file.Path)
	workflowID := rkcmodel.StableID("node", "build_target", "ci", file.Path)
	evidence := addEvidence("configenv.workflow", file, lines[0].number, system)
	if evidence == "" {
		return nil
	}
	if !addNode(rkcmodel.Node{
		ID: workflowID, Kind: "build_target", Name: filepath.Base(file.Path),
		QualifiedName: file.Path, Language: "yaml", Visibility: "repository",
		ArtifactID: file.ArtifactID, EvidenceIDs: []string{evidence},
		Attributes: map[string]any{"ci_system": system, "kind": "workflow"},
	}) {
		return nil
	}
	addEdge("configures", workflowID, file.ArtifactID, nil, evidence)
	// Indentation guide: workflow keys at indent 0, jobs at indent 2, job
	// fields at indent 4, steps at indent 6, step fields at indent 8.
	jobID := ""
	stepID := ""
	inJobs := false
	for index := range lines {
		line := lines[index]
		key, value := splitKeyValue(line.text)
		switch {
		case line.indent == 0 && key == "jobs":
			inJobs = true
		case line.indent == 0 && key != "":
			inJobs = false
			if key == "env" {
				extractEnvBlock(lines, index, file, addNode, addEdge, addEvidence, workflowID)
			}
		case inJobs && line.indent == 2:
			jobName := strings.TrimSuffix(key, ":")
			jobID = rkcmodel.StableID("node", "build_target", "ci", file.Path, "job", jobName)
			jobEvidence := addEvidence("configenv.job", file, line.number, jobName)
			if jobEvidence == "" {
				jobID = ""
				stepID = ""
				continue
			}
			if !addNode(rkcmodel.Node{
				ID: jobID, Kind: "build_target", Name: jobName,
				QualifiedName: file.Path + " job " + jobName, Language: "yaml",
				Visibility: "repository", ArtifactID: file.ArtifactID,
				EvidenceIDs: []string{jobEvidence},
				Attributes:  map[string]any{"ci_system": system, "kind": "job"},
			}) {
				jobID = ""
				stepID = ""
				continue
			}
			addEdge("configures", workflowID, jobID, nil, jobEvidence)
		case inJobs && jobID != "" && line.indent == 4 && key == "env":
			extractEnvBlock(lines, index, file, addNode, addEdge, addEvidence, jobID)
		case inJobs && jobID != "" && line.indent == 6 && strings.HasPrefix(line.text, "-"):
			stepName := value
			stepKey := strings.TrimSuffix(key, ":")
			stepDetail := "named workflow step"
			if stepKey == "run" {
				command := workflowCommand(lines, index, value)
				metadata := commandMetadata(command)
				stepName = metadata["command_class"].(string) + " command"
				stepKey = "run"
				stepDetail = commandEvidenceDetail(metadata)
			}
			if stepName == "" {
				stepName = "workflow step"
			}
			stepID = rkcmodel.StableID("node", "config_key", "ci", file.Path, "step", stepKey, fmt.Sprint(line.number))
			stepEvidence := addEvidence("configenv.step", file, line.number, stepDetail)
			if stepEvidence == "" {
				stepID = ""
				continue
			}
			stepAttributes := map[string]any{"ci_system": system, "kind": "step"}
			if stepKey == "run" && value != "" {
				for key, metadataValue := range commandMetadata(workflowCommand(lines, index, value)) {
					stepAttributes[key] = metadataValue
				}
			}
			qualifiedName := file.Path + " step " + stepName
			if stepKey == "run" {
				qualifiedName = fmt.Sprintf("%s run step at line %d", file.Path, line.number)
			}
			if !addNode(rkcmodel.Node{
				ID: stepID, Kind: "config_key", Name: stepName,
				QualifiedName: qualifiedName, Language: "yaml",
				Visibility: "repository", ArtifactID: file.ArtifactID,
				EvidenceIDs: []string{stepEvidence},
				Attributes:  stepAttributes,
			}) {
				stepID = ""
				continue
			}
			addEdge("configures", jobID, stepID, nil, stepEvidence)
		case inJobs && jobID != "" && stepID != "" && line.indent == 8 && key == "run" && value != "":
			metadata := commandMetadata(workflowCommand(lines, index, value))
			runEvidence := addEvidence("configenv.step_run", file, line.number, commandEvidenceDetail(metadata))
			if runEvidence != "" {
				addEdge("configures", stepID, file.ArtifactID, metadata, runEvidence)
			}
		}
	}
	return nil
}

func workflowCommand(lines []workflowLine, index int, inline string) string {
	if !isYAMLBlockScalar(inline) {
		return inline
	}
	indent := lines[index].indent
	parts := make([]string, 0, 8)
	for _, line := range lines[index+1:] {
		if line.indent <= indent {
			break
		}
		parts = append(parts, line.text)
	}
	return strings.Join(parts, "\n")
}

func isYAMLBlockScalar(value string) bool {
	return value == "|" || value == "|-" || value == "|+" ||
		value == ">" || value == ">-" || value == ">+"
}

func privateValueDigest(domain, value string) string {
	// Domain separation prevents a workflow command and an environment default
	// with identical text from becoming a cross-context correlation identifier.
	digest := sha256.Sum256([]byte(PluginID + "\x00" + domain + "\x00" + value))
	return fmt.Sprintf("%x", digest)
}

func scalarClassification(value string) string {
	trimmed := strings.TrimSpace(value)
	lower := strings.ToLower(trimmed)
	switch {
	case trimmed == "":
		return "empty"
	case strings.Contains(trimmed, "${{") || strings.Contains(trimmed, "${") ||
		strings.Contains(trimmed, "$(") || strings.Contains(trimmed, "{{"):
		return "expression"
	case lower == "true" || lower == "false" || lower == "yes" || lower == "no" ||
		lower == "on" || lower == "off":
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

func commandClassification(command string) string {
	lower := strings.ToLower(command)
	switch {
	case strings.Contains(lower, " test") || strings.HasPrefix(strings.TrimSpace(lower), "test ") ||
		strings.Contains(lower, "pytest") || strings.Contains(lower, "go test"):
		return "test"
	case strings.Contains(lower, "lint") || strings.Contains(lower, "vet") || strings.Contains(lower, "ruff"):
		return "quality"
	case strings.Contains(lower, "deploy") || strings.Contains(lower, "publish") || strings.Contains(lower, "release"):
		return "deploy"
	case strings.Contains(lower, "build") || strings.Contains(lower, "compile"):
		return "build"
	case strings.TrimSpace(command) == "":
		return "empty"
	default:
		return "command"
	}
}

func commandMetadata(command string) map[string]any {
	return map[string]any{
		"command_sha256": privateValueDigest("ci-run", command),
		"command_class":  commandClassification(command),
		"command_dynamic": strings.Contains(command, "${{") || strings.Contains(command, "${") ||
			strings.Contains(command, "$(") || strings.Contains(command, "{{"),
	}
}

func commandEvidenceDetail(metadata map[string]any) string {
	return fmt.Sprintf("run command: class=%s sha256=%s", metadata["command_class"], metadata["command_sha256"])
}

func splitKeyValue(text string) (key, value string) {
	text = strings.TrimSpace(text)
	text = strings.TrimPrefix(text, "-")
	text = strings.TrimSpace(text)
	key, value, _ = strings.Cut(text, ":")
	key = strings.TrimSpace(key)
	value = strings.Trim(strings.TrimSpace(value), `"'`)
	return key, value
}

func extractEnvBlock(
	lines []workflowLine,
	startIndex int,
	file pluginapi.FileRef,
	addNode func(rkcmodel.Node) bool,
	addEdge func(string, string, string, map[string]any, ...string),
	addEvidence func(string, pluginapi.FileRef, int, string) string,
	parent string,
) {
	blockIndent := lines[startIndex].indent
	for _, line := range lines[startIndex+1:] {
		if line.indent <= blockIndent {
			break
		}
		key, value := splitKeyValue(line.text)
		if key == "" || strings.ContainsAny(key, " \t") {
			continue
		}
		envID := rkcmodel.StableID("node", "environment_variable", key)
		secretLike := secretLikeName(key)
		declaration := map[string]any{
			"ci_source": parent, "source_file": file.Path, "source_line": line.number,
			"name_only": true,
		}
		attributes := map[string]any{
			"name_only": true, "declaration_count": 1,
			"declarations": []map[string]any{declaration},
			"ci_sources":   []string{parent}, "source_files": []string{file.Path},
		}
		if secretLike {
			attributes["secret_like"] = true
			declaration["secret_like"] = true
		} else if value != "" {
			attributes["has_default"] = true
			attributes["default_sha256"] = privateValueDigest("environment-default", value)
			attributes["default_class"] = scalarClassification(value)
			declaration["has_default"] = true
			declaration["default_sha256"] = attributes["default_sha256"]
			declaration["default_class"] = attributes["default_class"]
		}
		evidence := addEvidence("configenv.env", file, line.number, key)
		if evidence == "" {
			continue
		}
		declaration["evidence_id"] = evidence
		if !addNode(rkcmodel.Node{
			ID: envID, Kind: "environment_variable", Name: key, QualifiedName: key,
			Language: "yaml", Visibility: "deployment",
			EvidenceIDs: []string{evidence}, Attributes: attributes,
		}) {
			continue
		}
		addEdge("configures", parent, envID, nil, evidence)
	}
}

var secretLikeName = regexp.MustCompile(`(?i)(secret|token|password|passwd|private_key|api_key|credential|auth|key$|pwd$)`).MatchString

var terraformResource = regexp.MustCompile(`^\s*(resource|variable|output)\s+"([^"]+)"\s+"?([^"{]*)`)

// extractTerraform records bounded Terraform resources, variables, and outputs
// as config entities. Only the declaration line is parsed; block bodies are
// not evaluated.
func extractTerraform(
	ctx context.Context,
	root string,
	file pluginapi.FileRef,
	limits extractionLimits,
	addNode func(rkcmodel.Node) bool,
	addEdge func(string, string, string, map[string]any, ...string),
	addEvidence func(string, pluginapi.FileRef, int, string) string,
	addDiagnostic func(string, string),
) {
	data, actualBytes, oversized, err := readBoundedConfigFile(root, file, limits.configFileBytes)
	if err != nil {
		return
	}
	if oversized {
		addDiagnostic("RKC-CFG-3003", fmt.Sprintf(
			"skipped oversized configuration file %s: %d bytes exceeds the %d-byte limit",
			file.Path, actualBytes, limits.configFileBytes,
		))
		return
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64*1024), limits.configFileBytes+1)
	lineNumber := 0
	count := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return
		}
		lineNumber++
		line := scanner.Text()
		match := terraformResource.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		count++
		if count > limits.terraformStatements {
			addDiagnostic("RKC-CFG-3002", "bounded out at "+fmt.Sprint(limits.terraformStatements)+" terraform declarations in "+file.Path)
			return
		}
		kind, typeName, name := match[1], match[2], strings.Trim(strings.TrimSpace(match[3]), `"`)
		nodeKind := "config_key"
		if kind == "resource" {
			nodeKind = "build_target"
		}
		nodeID := rkcmodel.StableID("node", nodeKind, "terraform", file.Path, kind, typeName, name)
		attributes := map[string]any{"terraform_kind": kind}
		if typeName != "" {
			attributes["terraform_type"] = typeName
		}
		display := name
		if kind == "resource" && typeName != "" {
			display = typeName + "." + name
		}
		evidence := addEvidence("configenv.terraform", file, lineNumber, kind+" "+typeName+" "+name)
		if evidence == "" {
			continue
		}
		if !addNode(rkcmodel.Node{
			ID: nodeID, Kind: nodeKind, Name: display, QualifiedName: file.Path + " " + kind + " " + display,
			Language: "hcl", Visibility: "repository", ArtifactID: file.ArtifactID,
			EvidenceIDs: []string{evidence}, Attributes: attributes,
		}) {
			return
		}
		addEdge("configures", nodeID, file.ArtifactID, nil, evidence)
	}
}
