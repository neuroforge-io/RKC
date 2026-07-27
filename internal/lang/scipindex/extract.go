// Package scipindex imports compiler-produced SCIP code-intelligence indexes
// without executing repository code or adding a protobuf runtime dependency.
package scipindex

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	PluginID      = "rkc.scip"
	PluginVersion = "1.0.0"

	maximumDocuments   = 200_000
	maximumSymbols     = 500_000
	maximumOccurrences = 500_000
)

const (
	roleDefinition = int32(1)
	roleImport     = int32(2)
	roleWrite      = int32(4)
	roleRead       = int32(8)
	roleGenerated  = int32(16)
	roleTest       = int32(32)
)

type Options struct {
	Root      string
	Inputs    []Input
	Files     []pluginapi.FileRef
	Artifacts []rkcmodel.Artifact
}

type extractor struct {
	ctx       context.Context
	root      string
	files     map[string]pluginapi.FileRef
	artifacts map[string]rkcmodel.Artifact

	fragment    rkcmodel.Fragment
	nodes       map[string]rkcmodel.Node
	edges       map[string]rkcmodel.Edge
	evidence    map[string]rkcmodel.Evidence
	diagnostics map[string]rkcmodel.Diagnostic
	parsed      map[string]map[string]struct{}
	counts      indexCounts

	input    Input
	metadata metadata
}

type indexCounts struct {
	documents   int
	symbols     int
	occurrences int
}

type definitionContext struct {
	symbolID     string
	rangePos     sourcePosition
	enclosing    sourcePosition
	hasEnclosing bool
}

func Extract(ctx context.Context, options Options) (rkcmodel.Fragment, error) {
	if ctx == nil {
		return rkcmodel.Fragment{}, errors.New("SCIP extraction context is required")
	}
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return rkcmodel.Fragment{}, fmt.Errorf("resolve repository root: %w", err)
	}
	value := &extractor{
		ctx: ctx, root: root,
		files: map[string]pluginapi.FileRef{}, artifacts: map[string]rkcmodel.Artifact{},
		nodes: map[string]rkcmodel.Node{}, edges: map[string]rkcmodel.Edge{},
		evidence: map[string]rkcmodel.Evidence{}, diagnostics: map[string]rkcmodel.Diagnostic{},
		parsed: map[string]map[string]struct{}{},
	}
	for _, file := range options.Files {
		value.files[file.Path] = file
	}
	for _, artifact := range options.Artifacts {
		value.artifacts[artifact.Path] = artifact
	}
	inputs := append([]Input(nil), options.Inputs...)
	sort.Slice(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	for _, input := range inputs {
		if err := value.extractIndex(input); err != nil {
			return rkcmodel.Fragment{}, err
		}
	}
	value.finish()
	return value.fragment, nil
}

func (extractor *extractor) extractIndex(input Input) error {
	if err := extractor.ctx.Err(); err != nil {
		return err
	}
	before, err := os.Lstat(input.Path)
	if err != nil {
		return fmt.Errorf("inspect SCIP index %q: %w", input.Path, err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() ||
		before.Size() != input.SizeBytes {
		return fmt.Errorf("SCIP index %q no longer matches its prepared input", input.Path)
	}
	file, err := os.Open(input.Path)
	if err != nil {
		return fmt.Errorf("open SCIP index %q: %w", input.Path, err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !sameFileSnapshot(before, opened) {
		return fmt.Errorf("SCIP index %q changed while opening", input.Path)
	}
	hasher := sha256.New()
	reader := newWireReader(
		io.TeeReader(&contextReader{ctx: extractor.ctx, reader: file}, hasher),
		input.SizeBytes,
	)
	extractor.input = input
	extractor.metadata = metadata{}
	metadataSeen := false
	firstField := true
	for {
		field, wire, done, err := reader.next()
		if err != nil {
			return fmt.Errorf("decode SCIP index %q: %w", input.Path, err)
		}
		if done {
			break
		}
		if firstField && field != 1 {
			return fmt.Errorf("decode SCIP index %q: metadata must be the first field", input.Path)
		}
		firstField = false
		switch field {
		case 1:
			if metadataSeen {
				return fmt.Errorf("decode SCIP index %q: metadata appears more than once", input.Path)
			}
			if err := requireWire(field, wire, 2); err != nil {
				return err
			}
			message, err := reader.bytes(maximumMessageBytes)
			if err != nil {
				return fmt.Errorf("decode SCIP metadata in %q: %w", input.Path, err)
			}
			extractor.metadata, err = parseMetadata(message)
			if err != nil {
				return fmt.Errorf("decode SCIP metadata in %q: %w", input.Path, err)
			}
			metadataSeen = true
		case 2:
			if !metadataSeen {
				return fmt.Errorf("decode SCIP index %q: document precedes metadata", input.Path)
			}
			if err := requireWire(field, wire, 2); err != nil {
				return err
			}
			message, err := reader.bytes(maximumDocumentBytes)
			if err != nil {
				return fmt.Errorf("decode SCIP document in %q: %w", input.Path, err)
			}
			document, err := parseDocument(message)
			if err != nil {
				return fmt.Errorf("decode SCIP document in %q: %w", input.Path, err)
			}
			if err := extractor.extractDocument(document); err != nil {
				return fmt.Errorf("import SCIP document %q from %q: %w", document.path, input.Path, err)
			}
		case 3:
			if !metadataSeen {
				return fmt.Errorf("decode SCIP index %q: external symbol precedes metadata", input.Path)
			}
			if err := requireWire(field, wire, 2); err != nil {
				return err
			}
			message, err := reader.bytes(maximumMessageBytes)
			if err != nil {
				return fmt.Errorf("decode SCIP external symbol in %q: %w", input.Path, err)
			}
			symbol, err := parseSymbolInformation(message)
			if err != nil {
				return fmt.Errorf("decode SCIP external symbol in %q: %w", input.Path, err)
			}
			extractor.counts.symbols++
			if extractor.counts.symbols > maximumSymbols {
				return fmt.Errorf("SCIP inputs exceed the %d-symbol limit", maximumSymbols)
			}
			extractor.addSymbol("", "", nil, symbol, nil)
		default:
			if err := reader.skip(wire); err != nil {
				return fmt.Errorf("skip SCIP field %d in %q: %w", field, input.Path, err)
			}
		}
	}
	if !metadataSeen {
		return fmt.Errorf("decode SCIP index %q: metadata is missing", input.Path)
	}
	actualDigest := hex.EncodeToString(hasher.Sum(nil))
	if actualDigest != input.SHA256 {
		return fmt.Errorf(
			"SCIP index %q digest changed: got %s, want %s",
			input.Path, actualDigest, input.SHA256,
		)
	}
	after, err := os.Lstat(input.Path)
	if err != nil || !sameFileSnapshot(before, after) {
		return fmt.Errorf("SCIP index %q changed while importing", input.Path)
	}
	return nil
}

func (extractor *extractor) extractDocument(document document) error {
	if err := extractor.ctx.Err(); err != nil {
		return err
	}
	extractor.counts.documents++
	extractor.counts.symbols += len(document.symbols)
	extractor.counts.occurrences += len(document.occurrences)
	if extractor.counts.documents > maximumDocuments {
		return fmt.Errorf("SCIP inputs exceed the %d-document limit", maximumDocuments)
	}
	if extractor.counts.symbols > maximumSymbols {
		return fmt.Errorf("SCIP inputs exceed the %d-symbol limit", maximumSymbols)
	}
	if extractor.counts.occurrences > maximumOccurrences {
		return fmt.Errorf("SCIP inputs exceed the %d-occurrence limit", maximumOccurrences)
	}
	canonical, err := sourcepath.ResolveRelative("", document.path)
	if err != nil || canonical != document.path {
		return fmt.Errorf("relative_path is not canonical and repository-contained")
	}
	file, ok := extractor.files[document.path]
	if !ok {
		return fmt.Errorf("relative_path does not identify an inventoried text artifact")
	}
	artifact, ok := extractor.artifacts[document.path]
	if !ok || artifact.ID != file.ArtifactID {
		return errors.New("relative_path artifact identity is unavailable")
	}
	source, err := sourcepath.ReadFile(extractor.root, document.path)
	if err != nil {
		return err
	}
	if int64(len(source)) != file.SizeBytes {
		return errors.New("source size changed after inventory")
	}
	mapper, err := newPositionMapper(source, document.positionEncoding)
	if err != nil && len(document.occurrences) > 0 {
		return err
	}
	language := normalizeLanguage(document.language, file.Language)
	definitions := make([]definitionContext, 0)
	definitionBySymbol := map[string]occurrence{}
	for _, value := range document.occurrences {
		if value.roles&roleDefinition == 0 || strings.TrimSpace(value.symbol) == "" {
			continue
		}
		definitionBySymbol[value.symbol] = value
		rangePos, hasRange, err := occurrenceRange(value)
		if err != nil {
			return err
		}
		if !hasRange {
			continue
		}
		enclosing, hasEnclosing, err := occurrenceEnclosingRange(value)
		if err != nil {
			return err
		}
		definitions = append(definitions, definitionContext{
			symbolID: extractor.nodeID(document.path, value.symbol),
			rangePos: rangePos, enclosing: enclosing, hasEnclosing: hasEnclosing,
		})
	}
	for _, symbol := range document.symbols {
		definition, present := definitionBySymbol[symbol.symbol]
		var definitionPointer *occurrence
		if present {
			definitionPointer = &definition
		}
		extractor.addSymbol(document.path, language, mapper, symbol, definitionPointer)
	}
	for _, value := range document.occurrences {
		if err := extractor.addOccurrence(document.path, language, mapper, definitions, value); err != nil {
			return err
		}
	}
	copyArtifact := artifact
	copyArtifact.Status = "semantic_parsed"
	if copyArtifact.Attributes == nil {
		copyArtifact.Attributes = map[string]string{}
	} else {
		attributes := make(map[string]string, len(copyArtifact.Attributes)+3)
		for key, value := range copyArtifact.Attributes {
			attributes[key] = value
		}
		copyArtifact.Attributes = attributes
	}
	copyArtifact.Attributes["semantic_parser"] = "scip"
	copyArtifact.Attributes["semantic_indexer"] = extractor.toolName()
	copyArtifact.Attributes["semantic_index_sha256"] = extractor.input.SHA256
	extractor.fragment.Artifacts = append(extractor.fragment.Artifacts, copyArtifact)
	if extractor.parsed[artifact.ID] == nil {
		extractor.parsed[artifact.ID] = map[string]struct{}{}
	}
	extractor.parsed[artifact.ID][extractor.input.SHA256] = struct{}{}
	return nil
}

func (extractor *extractor) addSymbol(
	path, language string,
	mapper *positionMapper,
	symbol symbolInformation,
	definition *occurrence,
) string {
	if strings.TrimSpace(symbol.symbol) == "" {
		return ""
	}
	id := extractor.nodeID(path, symbol.symbol)
	source := (*rkcmodel.SourceRange)(nil)
	evidenceIDs := []string{}
	roles := int32(0)
	if definition != nil {
		roles = definition.roles
		rangePos, present, err := occurrenceRange(*definition)
		if err == nil && present {
			source, _ = mapper.sourceRange(path, extractor.files[path].ArtifactID, rangePos)
		}
		evidenceID := extractor.addEvidence(
			"scip.symbol.definition", path, symbol.symbol, source,
			map[string]any{"symbol_roles": definition.roles},
		)
		evidenceIDs = append(evidenceIDs, evidenceID)
	} else {
		evidenceID := extractor.addEvidence(
			"scip.symbol.information", path, symbol.symbol, nil, nil,
		)
		evidenceIDs = append(evidenceIDs, evidenceID)
	}
	kind := mapSymbolKind(symbol.kind)
	name := strings.TrimSpace(symbol.displayName)
	if name == "" {
		name = symbolDisplayName(symbol.symbol)
	}
	attributes := map[string]any{
		"scip_symbol":           symbol.symbol,
		"scip_kind":             symbol.kind,
		"compiler_indexer":      extractor.toolName(),
		"compiler_index_sha256": extractor.input.SHA256,
	}
	if documentation := strings.TrimSpace(strings.Join(symbol.documentation, "\n\n")); documentation != "" {
		attributes["documentation"] = documentation
	}
	if symbol.signatureLang != "" {
		attributes["signature_language"] = normalizeLanguage(symbol.signatureLang, language)
	}
	if roles&roleGenerated != 0 {
		attributes["generated"] = true
	}
	if roles&roleTest != 0 {
		attributes["test"] = true
	}
	node := rkcmodel.Node{
		ID: id, LogicalID: rkcmodel.StableID("logical", "scip", symbol.symbol),
		Kind: kind, Name: name, QualifiedName: symbol.symbol,
		Signature: strings.TrimSpace(symbol.signature), Language: language,
		Visibility: "compiler_indexed", ArtifactID: extractor.files[path].ArtifactID,
		Source: source, EvidenceIDs: evidenceIDs, Attributes: attributes,
	}
	extractor.upsertNode(node)
	parent := extractor.files[path].ArtifactID
	if symbol.enclosingSymbol != "" {
		parent = extractor.ensureSymbol(path, language, symbol.enclosingSymbol, 0)
	}
	if parent != "" && parent != id {
		extractor.addEdge("contains", parent, id, evidenceIDs, map[string]any{
			"source": "scip_symbol_hierarchy",
		})
	}
	for _, relation := range symbol.relationships {
		target := extractor.ensureSymbol(path, language, relation.symbol, 0)
		if target == "" || target == id {
			continue
		}
		if relation.isImplementation {
			extractor.addEdge("implements", id, target, evidenceIDs, map[string]any{
				"scip_relationship": "implementation",
			})
		}
		if relation.isReference {
			extractor.addEdge("related_to", id, target, evidenceIDs, map[string]any{
				"scip_relationship": "reference_equivalence",
			})
		}
		if relation.isTypeDefinition {
			extractor.addEdge("related_to", id, target, evidenceIDs, map[string]any{
				"scip_relationship": "type_definition",
			})
		}
		if relation.isDefinition {
			extractor.addEdge("aliases", id, target, evidenceIDs, map[string]any{
				"scip_relationship": "definition",
			})
		}
	}
	return id
}

func (extractor *extractor) addOccurrence(
	path, language string,
	mapper *positionMapper,
	definitions []definitionContext,
	value occurrence,
) error {
	rangePos, hasRange, err := occurrenceRange(value)
	if err != nil {
		return err
	}
	var source *rkcmodel.SourceRange
	if hasRange {
		source, err = mapper.sourceRange(path, extractor.files[path].ArtifactID, rangePos)
		if err != nil {
			return err
		}
	}
	for _, diagnostic := range value.diagnostics {
		extractor.addDiagnostic(path, source, diagnostic)
	}
	if strings.TrimSpace(value.symbol) == "" {
		return nil
	}
	target := extractor.ensureSymbol(path, language, value.symbol, value.syntaxKind)
	if target == "" {
		return nil
	}
	evidenceID := extractor.addEvidence(
		"scip.occurrence", path, value.symbol, source,
		map[string]any{
			"symbol_roles": value.roles, "syntax_kind": value.syntaxKind,
			"override_documentation": append([]string(nil), value.overrideDocumentation...),
		},
	)
	node := extractor.nodes[target]
	node.EvidenceIDs = appendUnique(node.EvidenceIDs, evidenceID)
	if node.Source == nil && value.roles&roleDefinition != 0 {
		node.Source = source
		node.ArtifactID = extractor.files[path].ArtifactID
	}
	if len(value.overrideDocumentation) > 0 && node.Attributes != nil {
		node.Attributes["occurrence_documentation"] = strings.Join(value.overrideDocumentation, "\n\n")
	}
	extractor.nodes[target] = node
	if value.roles&roleDefinition != 0 {
		return nil
	}
	from := extractor.files[path].ArtifactID
	if hasRange {
		from = owningDefinition(definitions, rangePos, from)
	}
	kinds := occurrenceEdgeKinds(value.roles)
	for _, kind := range kinds {
		extractor.addEdge(kind, from, target, []string{evidenceID}, map[string]any{
			"symbol_roles": value.roles, "syntax_kind": value.syntaxKind,
		})
	}
	return nil
}

func (extractor *extractor) ensureSymbol(path, language, symbol string, syntaxKind int32) string {
	symbol = strings.TrimSpace(symbol)
	if symbol == "" {
		return ""
	}
	id := extractor.nodeID(path, symbol)
	if _, ok := extractor.nodes[id]; ok {
		return id
	}
	kind := mapSyntaxKind(syntaxKind)
	node := rkcmodel.Node{
		ID: id, LogicalID: rkcmodel.StableID("logical", "scip", symbol),
		Kind: kind, Name: symbolDisplayName(symbol), QualifiedName: symbol,
		Language: language, Visibility: "compiler_indexed",
		Attributes: map[string]any{
			"scip_symbol": symbol, "syntax_kind": syntaxKind,
			"compiler_indexer":      extractor.toolName(),
			"compiler_index_sha256": extractor.input.SHA256,
		},
	}
	extractor.upsertNode(node)
	return id
}

func (extractor *extractor) nodeID(path, symbol string) string {
	key := symbol
	if strings.HasPrefix(symbol, "local ") {
		key = path + "\x00" + symbol
	}
	return rkcmodel.StableID("node", "scip", key)
}

func (extractor *extractor) upsertNode(node rkcmodel.Node) {
	current, ok := extractor.nodes[node.ID]
	if !ok {
		extractor.nodes[node.ID] = node
		return
	}
	if current.Kind == "unresolved_symbol" && node.Kind != "unresolved_symbol" {
		current.Kind = node.Kind
	}
	if current.Name == "" || current.Name == current.QualifiedName {
		current.Name = node.Name
	}
	if current.Language == "" {
		current.Language = node.Language
	}
	if current.Signature == "" {
		current.Signature = node.Signature
	}
	if current.ArtifactID == "" {
		current.ArtifactID = node.ArtifactID
	}
	if current.Source == nil {
		current.Source = node.Source
	}
	current.EvidenceIDs = appendUnique(current.EvidenceIDs, node.EvidenceIDs...)
	if current.Attributes == nil {
		current.Attributes = map[string]any{}
	}
	for key, value := range node.Attributes {
		current.Attributes[key] = value
	}
	extractor.nodes[node.ID] = current
}

func (extractor *extractor) addEvidence(
	method, path, detail string,
	source *rkcmodel.SourceRange,
	attributes map[string]any,
) string {
	rangeIdentity := ""
	if source != nil {
		rangeIdentity = fmt.Sprintf(
			"%d:%d:%d:%d",
			source.StartLine, source.StartColumn, source.EndLine, source.EndColumn,
		)
	}
	id := rkcmodel.StableID(
		"evidence", PluginID, extractor.input.SHA256, method, path, detail, rangeIdentity,
	)
	extractor.evidence[id] = rkcmodel.Evidence{
		ID: id, Kind: "compiler_resolved", Method: method, Confidence: 1,
		Source: source, Tool: extractor.toolName(), ToolVersion: extractor.metadata.toolVersion,
		InputDigest: extractor.input.SHA256, Detail: detail, Attributes: attributes,
	}
	return id
}

func (extractor *extractor) addEdge(
	kind, from, to string,
	evidenceIDs []string,
	attributes map[string]any,
) {
	if from == "" || to == "" || from == to {
		return
	}
	id := rkcmodel.StableID("edge", kind, from, to)
	current, ok := extractor.edges[id]
	if !ok {
		current = rkcmodel.Edge{
			ID: id, Kind: kind, From: from, To: to,
			Resolution: rkcmodel.ResolutionCompilerResolved,
			Confidence: 1, Producer: PluginID,
		}
	}
	current.EvidenceIDs = appendUnique(current.EvidenceIDs, evidenceIDs...)
	if current.Attributes == nil {
		current.Attributes = map[string]any{}
	}
	for key, value := range attributes {
		current.Attributes[key] = value
	}
	extractor.edges[id] = current
}

func (extractor *extractor) addDiagnostic(
	path string,
	source *rkcmodel.SourceRange,
	diagnostic compilerDiagnostic,
) {
	severity := "note"
	switch diagnostic.severity {
	case 1:
		severity = "error"
	case 2:
		severity = "warning"
	case 3, 4:
		severity = "note"
	}
	code := strings.TrimSpace(diagnostic.code)
	if code == "" {
		code = "SCIP"
	}
	id := rkcmodel.StableID(
		"diagnostic", PluginID, extractor.input.SHA256, path, code,
		diagnostic.message, fmt.Sprint(source),
	)
	extractor.diagnostics[id] = rkcmodel.Diagnostic{
		ID: id, Severity: severity, Code: code, Message: diagnostic.message,
		Source: source, Stage: "semantic_parse",
		Plugin: extractor.toolName() + "@" + extractor.metadata.toolVersion,
		Attributes: map[string]any{
			"compiler_source":   diagnostic.source,
			"scip_index_sha256": extractor.input.SHA256,
		},
	}
}

func (extractor *extractor) finish() {
	for _, node := range extractor.nodes {
		extractor.fragment.Nodes = append(extractor.fragment.Nodes, node)
	}
	for _, edge := range extractor.edges {
		extractor.fragment.Edges = append(extractor.fragment.Edges, edge)
	}
	for _, evidence := range extractor.evidence {
		extractor.fragment.Evidence = append(extractor.fragment.Evidence, evidence)
	}
	for _, diagnostic := range extractor.diagnostics {
		extractor.fragment.Diagnostics = append(extractor.fragment.Diagnostics, diagnostic)
	}
	rkcmodel.SortFragment(&extractor.fragment)
}

func (extractor *extractor) toolName() string {
	name := strings.TrimSpace(extractor.metadata.toolName)
	if name == "" {
		return PluginID
	}
	return name
}

func occurrenceEdgeKinds(roles int32) []string {
	var kinds []string
	if roles&roleImport != 0 {
		kinds = append(kinds, "imports")
	}
	if roles&roleWrite != 0 {
		kinds = append(kinds, "writes")
	}
	if roles&roleRead != 0 {
		kinds = append(kinds, "reads")
	}
	if len(kinds) == 0 {
		kinds = append(kinds, "references")
	}
	return kinds
}

func owningDefinition(definitions []definitionContext, position sourcePosition, fallback string) string {
	owner := fallback
	var best sourcePosition
	found := false
	for _, definition := range definitions {
		container := definition.rangePos
		if definition.hasEnclosing {
			container = definition.enclosing
		}
		if !containsPosition(container, position) {
			continue
		}
		if !found || rangeSmaller(container, best) {
			owner = definition.symbolID
			best = container
			found = true
		}
	}
	return owner
}

func containsPosition(container, value sourcePosition) bool {
	return comparePosition(container.startLine, container.startCharacter, value.startLine, value.startCharacter) <= 0 &&
		comparePosition(container.endLine, container.endCharacter, value.endLine, value.endCharacter) >= 0
}

func rangeSmaller(left, right sourcePosition) bool {
	if left.endLine-left.startLine != right.endLine-right.startLine {
		return left.endLine-left.startLine < right.endLine-right.startLine
	}
	return left.endCharacter-left.startCharacter < right.endCharacter-right.startCharacter
}

func comparePosition(leftLine, leftCharacter, rightLine, rightCharacter int32) int {
	if leftLine < rightLine || leftLine == rightLine && leftCharacter < rightCharacter {
		return -1
	}
	if leftLine == rightLine && leftCharacter == rightCharacter {
		return 0
	}
	return 1
}

func mapSymbolKind(kind int32) string {
	switch kind {
	case 7, 62, 75:
		return "class"
	case 21, 42, 56, 86:
		return "interface"
	case 53, 85:
		return "trait"
	case 11:
		return "enum"
	case 12:
		return "enum_member"
	case 17, 40:
		return "function"
	case 9:
		return "constructor"
	case 15, 77, 79:
		return "field"
	case 22, 41, 81:
		return "property"
	case 8:
		return "constant"
	case 27, 37, 38, 44, 52:
		return "parameter"
	case 13, 78:
		return "event"
	case 28:
		return "message"
	case 29:
		return "module"
	case 30:
		return "namespace"
	case 35, 36:
		return "package"
	case 16:
		return "file"
	case 1, 3, 6, 10, 31, 32, 33, 46, 48, 49, 54, 55, 57, 58, 59, 63:
		return "type"
	case 4, 14, 20, 60, 61, 82:
		return "variable"
	case 2, 5, 19, 23, 24, 25, 34, 39, 43, 50, 51, 64, 65:
		return "function"
	case 18, 26, 45, 47, 66, 67, 68, 69, 70, 71, 72, 74, 76, 80:
		return "method"
	case 73:
		return "type"
	default:
		return "unresolved_symbol"
	}
}

func mapSyntaxKind(kind int32) string {
	switch kind {
	case 14:
		return "namespace"
	case 15, 16:
		return "function"
	case 17, 18:
		return "function"
	case 19, 20:
		return "type"
	case 9:
		return "constant"
	case 10, 11, 12, 13:
		return "variable"
	default:
		return "unresolved_symbol"
	}
}

func normalizeLanguage(primary, fallback string) string {
	value := strings.TrimSpace(primary)
	if value == "" {
		value = strings.TrimSpace(fallback)
	}
	switch strings.ToLower(value) {
	case "cpp", "c++":
		return "cpp"
	case "csharp", "c#":
		return "csharp"
	case "javascriptreact":
		return "javascript"
	case "typescriptreact":
		return "typescript"
	case "objective_c":
		return "objective-c"
	case "objective_cpp":
		return "objective-cpp"
	case "shellscript":
		return "shell"
	default:
		return strings.ToLower(value)
	}
}

func symbolDisplayName(symbol string) string {
	symbol = strings.TrimSpace(symbol)
	if strings.HasPrefix(symbol, "local ") {
		return strings.TrimSpace(strings.TrimPrefix(symbol, "local "))
	}
	last := symbol
	if index := strings.LastIndex(last, " "); index >= 0 {
		last = last[index+1:]
	}
	last = strings.Trim(last, "./#!:[]()")
	if index := strings.LastIndexAny(last, "/#.!:"); index >= 0 {
		last = last[index+1:]
	}
	last = strings.Trim(last, "`[]()")
	if last == "" {
		return symbol
	}
	return strings.ReplaceAll(last, "``", "`")
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range additions {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

type positionMapper struct {
	source     []byte
	lineStarts []int
	encoding   int32
}

func newPositionMapper(source []byte, encoding int32) (*positionMapper, error) {
	if encoding < 1 || encoding > 3 {
		return nil, fmt.Errorf("SCIP document position_encoding %d is ambiguous or unsupported", encoding)
	}
	starts := []int{0}
	for index, value := range source {
		if value == '\n' {
			starts = append(starts, index+1)
		}
	}
	return &positionMapper{source: source, lineStarts: starts, encoding: encoding}, nil
}

func (mapper *positionMapper) sourceRange(
	path, artifactID string,
	position sourcePosition,
) (*rkcmodel.SourceRange, error) {
	if mapper == nil {
		return nil, errors.New("SCIP position mapper is unavailable")
	}
	start, err := mapper.byteOffset(position.startLine, position.startCharacter)
	if err != nil {
		return nil, fmt.Errorf("map SCIP start position: %w", err)
	}
	end, err := mapper.byteOffset(position.endLine, position.endCharacter)
	if err != nil {
		return nil, fmt.Errorf("map SCIP end position: %w", err)
	}
	if end < start {
		return nil, errors.New("mapped SCIP range end precedes its start")
	}
	return &rkcmodel.SourceRange{
		ArtifactID: artifactID, Path: path,
		StartByte: int64(start), EndByte: int64(end),
		StartLine: int(position.startLine) + 1, StartColumn: int(position.startCharacter),
		EndLine: int(position.endLine) + 1, EndColumn: int(position.endCharacter),
	}, nil
}

func (mapper *positionMapper) byteOffset(line, character int32) (int, error) {
	if line < 0 || int(line) >= len(mapper.lineStarts) || character < 0 {
		return 0, errors.New("position is outside the source file")
	}
	start := mapper.lineStarts[line]
	end := len(mapper.source)
	if int(line)+1 < len(mapper.lineStarts) {
		end = mapper.lineStarts[line+1]
	}
	contentEnd := end
	for contentEnd > start && (mapper.source[contentEnd-1] == '\n' || mapper.source[contentEnd-1] == '\r') {
		contentEnd--
	}
	lineBytes := mapper.source[start:contentEnd]
	switch mapper.encoding {
	case 1:
		if int(character) > len(lineBytes) ||
			int(character) < len(lineBytes) && !utf8.RuneStart(lineBytes[character]) {
			return 0, errors.New("UTF-8 byte position is not on a code-point boundary")
		}
		return start + int(character), nil
	case 2:
		units := int32(0)
		for offset, runeValue := range string(lineBytes) {
			if units == character {
				return start + offset, nil
			}
			width := int32(1)
			if runeValue > 0xffff {
				width = 2
			}
			if units+width > character {
				return 0, errors.New("UTF-16 position splits a surrogate pair")
			}
			units += width
		}
		if units == character {
			return contentEnd, nil
		}
	case 3:
		units := int32(0)
		for offset := range string(lineBytes) {
			if units == character {
				return start + offset, nil
			}
			units++
		}
		if units == character {
			return contentEnd, nil
		}
	}
	return 0, errors.New("character position is outside the source line")
}
