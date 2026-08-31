package flow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// goAnalyzer runs the bounded Go CFG and value-flow passes over the bundle's
// Go function nodes, re-parsing each admitted Go file exactly once.
type goAnalyzer struct {
	ctx       context.Context
	options   Options
	callGraph *callGraph
	limits    analysisLimits
	fragment  rkcmodel.Fragment
	stats     Stats

	seenNodes    map[string]struct{}
	seenEdges    map[string]struct{}
	nodeByID     map[string]rkcmodel.Node
	artifacts    map[string]rkcmodel.Artifact
	boundSeen    map[string]struct{}
	omissionSeen map[string]struct{}

	cfgBlockLimitHit   bool
	cfgEdgeLimitHit    bool
	valueLimitHit      bool
	flowEdgeLimitHit   bool
	envReadLimitHit    bool
	factRecordLimitHit bool
	factByteLimitHit   bool

	fileASTs            map[string]*ast.File
	fileSets            map[string]*token.FileSet
	fileMissing         map[string]struct{}
	pendingCallResults  []callResultValue
	flowEdgesByFunction map[string]int
	deferredBoundSeen   map[string]struct{}
}

func newGoAnalyzer(ctx context.Context, options Options, graph *callGraph, limits analysisLimits) *goAnalyzer {
	analyzer := &goAnalyzer{
		ctx: ctx, options: options, callGraph: graph,
		limits:    limits,
		seenNodes: map[string]struct{}{}, seenEdges: map[string]struct{}{},
		nodeByID: map[string]rkcmodel.Node{}, artifacts: map[string]rkcmodel.Artifact{},
		boundSeen: map[string]struct{}{}, omissionSeen: map[string]struct{}{},
		fileASTs: map[string]*ast.File{}, fileSets: map[string]*token.FileSet{},
		fileMissing:         map[string]struct{}{},
		flowEdgesByFunction: map[string]int{},
		deferredBoundSeen:   map[string]struct{}{},
	}
	for _, node := range options.Bundle.Nodes {
		analyzer.nodeByID[node.ID] = node
	}
	for _, artifact := range options.Artifacts {
		analyzer.artifacts[artifact.ID] = artifact
	}
	return analyzer
}

func (analyzer *goAnalyzer) analyze() error {
	artifactsByID := map[string]rkcmodel.Artifact{}
	for _, artifact := range analyzer.options.Artifacts {
		artifactsByID[artifact.ID] = artifact
	}
	// Deterministic order: process function nodes in ID order.
	var functions []rkcmodel.Node
	for _, node := range analyzer.options.Bundle.Nodes {
		if !isFunctionLike(node.Kind) || node.Language != "go" || node.Source == nil {
			continue
		}
		if _, ok := artifactsByID[node.ArtifactID]; !ok {
			continue
		}
		functions = append(functions, node)
	}
	sort.Slice(functions, func(i, j int) bool { return functions[i].ID < functions[j].ID })
	cfgFunctions := 0
	flowFunctions := 0
	for _, function := range functions {
		if err := analyzer.ctx.Err(); err != nil {
			return err
		}
		factBudgetAvailable := !analyzer.factBudgetHit()
		cfgAvailable := factBudgetAvailable && cfgFunctions < analyzer.limits.cfgFunctions && !analyzer.cfgBlockLimitHit && !analyzer.cfgEdgeLimitHit
		flowAvailable := factBudgetAvailable && flowFunctions < analyzer.limits.flowFunctions && !analyzer.valueLimitHit && !analyzer.flowEdgeLimitHit && !analyzer.envReadLimitHit
		if !cfgAvailable && cfgFunctions >= analyzer.limits.cfgFunctions {
			analyzer.noteBound("RKC-FLOW-2001", "bounded out at "+strconv.Itoa(analyzer.limits.cfgFunctions)+" Go functions for CFG analysis")
		}
		if !flowAvailable && flowFunctions >= analyzer.limits.flowFunctions {
			analyzer.noteBound("RKC-FLOW-2021", "bounded out at "+strconv.Itoa(analyzer.limits.flowFunctions)+" Go functions for value-flow analysis")
		}
		if !cfgAvailable && !flowAvailable {
			break
		}
		file, fset, ok := analyzer.fileFor(function)
		if !ok {
			continue
		}
		declaration := findFunctionDeclaration(fset, file, function)
		if declaration == nil || declaration.Body == nil {
			continue
		}
		if cfgAvailable {
			cfgFunctions++
			if analyzer.buildCFG(function, fset, declaration) {
				analyzer.stats.CFGFunctions++
			}
		}
		if flowAvailable {
			flowFunctions++
			if analyzer.buildValueFlow(function, fset, file, declaration) {
				analyzer.stats.ValueFunctions++
			}
		}
	}
	analyzer.emitCallBindings()
	return nil
}

func (analyzer *goAnalyzer) fileFor(function rkcmodel.Node) (*ast.File, *token.FileSet, bool) {
	if file, ok := analyzer.fileASTs[function.ArtifactID]; ok {
		return file, analyzer.fileSets[function.ArtifactID], true
	}
	if _, missing := analyzer.fileMissing[function.ArtifactID]; missing {
		return nil, nil, false
	}
	var artifact *rkcmodel.Artifact
	for index := range analyzer.options.Artifacts {
		if analyzer.options.Artifacts[index].ID == function.ArtifactID {
			artifact = &analyzer.options.Artifacts[index]
			break
		}
	}
	if artifact == nil {
		analyzer.fileMissing[function.ArtifactID] = struct{}{}
		return nil, nil, false
	}
	source, err := readArtifactSource(analyzer.options.Root, *artifact)
	if err != nil {
		analyzer.fileMissing[function.ArtifactID] = struct{}{}
		analyzer.addDiagnostic("RKC-FLOW-2002", "cannot read Go source for flow analysis: "+artifact.Path)
		return nil, nil, false
	}
	if !strings.HasSuffix(artifact.Path, ".go") {
		analyzer.fileMissing[function.ArtifactID] = struct{}{}
		return nil, nil, false
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, artifact.Path, source, parser.ParseComments)
	if err != nil {
		analyzer.fileMissing[function.ArtifactID] = struct{}{}
		analyzer.addDiagnostic("RKC-FLOW-2003", "cannot parse Go source for flow analysis: "+artifact.Path)
		return nil, nil, false
	}
	analyzer.fileASTs[function.ArtifactID] = file
	analyzer.fileSets[function.ArtifactID] = fset
	return file, fset, true
}

// findFunctionDeclaration locates the function by its recorded source span.
// The bundle stores 1-based start lines; the parser positions match.
func findFunctionDeclaration(fset *token.FileSet, file *ast.File, function rkcmodel.Node) *ast.FuncDecl {
	if function.Source == nil || function.Source.StartLine <= 0 {
		return nil
	}
	wantLine := function.Source.StartLine
	var found *ast.FuncDecl
	ast.Inspect(file, func(node ast.Node) bool {
		declaration, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}
		position := fset.PositionFor(declaration.Pos(), false)
		if position.Line == wantLine {
			found = declaration
			return false
		}
		return true
	})
	return found
}

func (analyzer *goAnalyzer) addNode(node rkcmodel.Node) bool {
	if _, exists := analyzer.seenNodes[node.ID]; exists {
		return true
	}
	switch node.Kind {
	case "cfg_block":
		if analyzer.stats.CFGBlocks >= analyzer.limits.cfgBlocksTotal {
			analyzer.cfgBlockLimitHit = true
			analyzer.noteBound("RKC-FLOW-2011", "bounded out at "+strconv.Itoa(analyzer.limits.cfgBlocksTotal)+" aggregate CFG blocks")
			return false
		}
	case "value":
		if analyzer.stats.ValueNodes >= analyzer.limits.valueNodesTotal {
			analyzer.valueLimitHit = true
			analyzer.noteBound("RKC-FLOW-2022", "bounded out at "+strconv.Itoa(analyzer.limits.valueNodesTotal)+" aggregate value nodes")
			return false
		}
	}
	node.Name = boundedFlowText(node.Name)
	node.QualifiedName = boundedFlowText(node.QualifiedName)
	node.Signature = boundedFlowText(node.Signature)
	node.Attributes = boundedFlowAttributes(node.Attributes)
	evidence := analyzer.newFactEvidence("node."+node.Kind, node.ID, analyzer.nodeSource(node))
	evidenceID := evidence.ID
	node.EvidenceIDs = appendUnique(node.EvidenceIDs, evidenceID)
	if !analyzer.admitFactPair(estimatedNodeBytes(node) + estimatedEvidenceBytes(evidence)) {
		return false
	}
	analyzer.seenNodes[node.ID] = struct{}{}
	analyzer.fragment.Nodes = append(analyzer.fragment.Nodes, node)
	analyzer.fragment.Evidence = append(analyzer.fragment.Evidence, evidence)
	analyzer.nodeByID[node.ID] = node
	if node.Kind == "cfg_block" {
		analyzer.stats.CFGBlocks++
	} else if node.Kind == "value" {
		analyzer.stats.ValueNodes++
	}
	return true
}

func (analyzer *goAnalyzer) addEdge(kind, from, to, resolution string, attributes map[string]any) bool {
	id := rkcmodel.StableID("edge", kind, from, to)
	return analyzer.addEdgeWithID(id, kind, from, to, resolution, attributes)
}

func (analyzer *goAnalyzer) addEdgeWithID(id, kind, from, to, resolution string, attributes map[string]any) bool {
	if _, exists := analyzer.seenEdges[id]; exists {
		return false
	}
	if !analyzer.edgeEndpointsAvailable(from, to) {
		return false
	}
	if kind == "precedes" {
		if analyzer.stats.CFGEdges >= analyzer.limits.cfgEdgesTotal {
			analyzer.cfgEdgeLimitHit = true
			analyzer.noteBound("RKC-FLOW-2012", "bounded out at "+strconv.Itoa(analyzer.limits.cfgEdgesTotal)+" aggregate CFG edges")
			return false
		}
	} else if flowEdgeKinds[kind] {
		if analyzer.flowEdgeCount() >= analyzer.limits.flowEdgesTotal {
			analyzer.flowEdgeLimitHit = true
			analyzer.noteBound("RKC-FLOW-2023", "bounded out at "+strconv.Itoa(analyzer.limits.flowEdgesTotal)+" aggregate value-flow edges")
			return false
		}
		if kind == "reads" && analyzer.stats.EnvReads >= analyzer.limits.envReads {
			analyzer.envReadLimitHit = true
			analyzer.noteBound("RKC-FLOW-2024", "bounded out at "+strconv.Itoa(analyzer.limits.envReads)+" environment reads")
			return false
		}
	}
	attributes = boundedFlowAttributes(attributes)
	evidence := analyzer.newFactEvidence("edge."+kind, id, analyzer.edgeSource(from, to))
	evidenceID := evidence.ID
	edge := rkcmodel.Edge{
		ID: id, Kind: kind, From: from, To: to, Resolution: resolution,
		Confidence: 1.0, Producer: PluginID, EvidenceIDs: []string{evidenceID}, Attributes: attributes,
	}
	if !analyzer.admitFactPair(estimatedEdgeBytes(edge) + estimatedEvidenceBytes(evidence)) {
		return false
	}
	analyzer.seenEdges[id] = struct{}{}
	analyzer.fragment.Edges = append(analyzer.fragment.Edges, edge)
	analyzer.fragment.Evidence = append(analyzer.fragment.Evidence, evidence)
	switch kind {
	case "precedes":
		analyzer.stats.CFGEdges++
	case "flows_to":
		analyzer.stats.ValueEdges++
	case "binds_to":
		analyzer.stats.BindsToEdges++
	case "returns_to":
		analyzer.stats.ReturnsToEdges++
	case "sanitizes":
		analyzer.stats.SanitizeEdges++
	case "reads":
		analyzer.stats.EnvReads++
	}
	return true
}

// addSanitizerHypothesis preserves the useful callee-name clue above the
// canonical truth layer. It deliberately emits related_to/syntax_inferred at
// low confidence, never a sanitizes edge and never a traversable flow fact.
func (analyzer *goAnalyzer) addSanitizerHypothesis(from, to string, attributes map[string]any) bool {
	if from == "" || to == "" || !analyzer.edgeEndpointsAvailable(from, to) {
		return false
	}
	id := rkcmodel.StableID("edge", "related_to", "sanitizer_name_hypothesis", from, to)
	if _, exists := analyzer.seenEdges[id]; exists {
		return false
	}
	copyAttributes := make(map[string]any, len(attributes)+3)
	for key, value := range attributes {
		copyAttributes[key] = value
	}
	copyAttributes["hypothesis"] = "sanitizer_name"
	copyAttributes["non_authoritative"] = true
	copyAttributes["basis"] = "callee_name_prefix"
	copyAttributes = boundedFlowAttributes(copyAttributes)
	evidence := analyzer.newFactEvidence("edge.related_to.sanitizer_name_hypothesis", id, analyzer.edgeSource(from, to))
	evidence.Confidence = 0.25
	edge := rkcmodel.Edge{
		ID: id, Kind: "related_to", From: from, To: to,
		Resolution: rkcmodel.ResolutionSyntaxInferred, Confidence: 0.25,
		Producer: PluginID, EvidenceIDs: []string{evidence.ID}, Attributes: copyAttributes,
	}
	if !analyzer.admitFactPair(estimatedEdgeBytes(edge) + estimatedEvidenceBytes(evidence)) {
		return false
	}
	analyzer.seenEdges[id] = struct{}{}
	analyzer.fragment.Edges = append(analyzer.fragment.Edges, edge)
	analyzer.fragment.Evidence = append(analyzer.fragment.Evidence, evidence)
	return true
}

func (analyzer *goAnalyzer) edgeEndpointsAvailable(from, to string) bool {
	if _, sourceExists := analyzer.nodeByID[from]; !sourceExists {
		analyzer.noteOmission("RKC-FLOW-2032", "omitted flow edge because its source node is unavailable")
		return false
	}
	if _, targetExists := analyzer.nodeByID[to]; !targetExists {
		analyzer.noteOmission("RKC-FLOW-2033", "omitted flow edge because its target node is unavailable")
		return false
	}
	return true
}

func (analyzer *goAnalyzer) noteOmission(code, message string) {
	if _, exists := analyzer.omissionSeen[code]; exists {
		return
	}
	analyzer.omissionSeen[code] = struct{}{}
	analyzer.addDiagnostic(code, message)
}

func (analyzer *goAnalyzer) addDiagnostic(code, message string) {
	if len(analyzer.fragment.Diagnostics) >= analyzer.limits.diagnosticsTotal {
		return
	}
	analyzer.fragment.Diagnostics = append(analyzer.fragment.Diagnostics, flowDiagnostic(code, boundedFlowText(message)))
}

func (analyzer *goAnalyzer) noteBound(code, message string) {
	if _, exists := analyzer.boundSeen[code]; exists {
		return
	}
	analyzer.boundSeen[code] = struct{}{}
	analyzer.stats.BoundedExceeded++
	analyzer.addDiagnostic(code, message)
}

func (analyzer *goAnalyzer) flowEdgeCount() int {
	return analyzer.stats.ValueEdges + analyzer.stats.BindsToEdges + analyzer.stats.ReturnsToEdges +
		analyzer.stats.SanitizeEdges + analyzer.stats.EnvReads
}

func (analyzer *goAnalyzer) nodeSource(node rkcmodel.Node) *rkcmodel.SourceRange {
	if node.Source != nil {
		result := *node.Source
		if result.ArtifactID == "" {
			result.ArtifactID = node.ArtifactID
		}
		if result.Path == "" {
			result.Path = analyzer.artifacts[node.ArtifactID].Path
		}
		return &result
	}
	if artifact, ok := analyzer.artifacts[node.ArtifactID]; ok {
		return &rkcmodel.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path}
	}
	return nil
}

func (analyzer *goAnalyzer) edgeSource(from, to string) *rkcmodel.SourceRange {
	if node, ok := analyzer.nodeByID[from]; ok {
		if source := analyzer.nodeSource(node); source != nil {
			return source
		}
	}
	if node, ok := analyzer.nodeByID[to]; ok {
		return analyzer.nodeSource(node)
	}
	return nil
}

func (analyzer *goAnalyzer) newFactEvidence(method, factID string, source *rkcmodel.SourceRange) rkcmodel.Evidence {
	id := rkcmodel.StableID("evidence", PluginID, PluginVersion, method, factID)
	evidence := rkcmodel.Evidence{
		ID: id, Kind: "syntax_inferred", Method: PluginID + "." + method, Confidence: 1,
		Source: source, Tool: PluginID, ToolVersion: PluginVersion,
	}
	if source != nil {
		evidence.InputDigest = analyzer.artifacts[source.ArtifactID].SHA256
	}
	return evidence
}

func (analyzer *goAnalyzer) factBudgetHit() bool {
	return analyzer.factRecordLimitHit || analyzer.factByteLimitHit
}

// admitFactPair reserves one generated graph fact and its mandatory evidence
// record atomically. The conservative byte estimate includes retained string,
// map, slice, and source-range storage; it intentionally overcounts shared
// strings so the ceiling remains safe under adversarial identifiers and paths.
func (analyzer *goAnalyzer) admitFactPair(estimatedBytes int64) bool {
	if analyzer.stats.FactRecords+2 > analyzer.limits.factRecordsTotal {
		analyzer.factRecordLimitHit = true
		analyzer.noteBound(
			"RKC-FLOW-2030",
			"bounded out at "+strconv.Itoa(analyzer.limits.factRecordsTotal)+" aggregate flow fact and evidence records",
		)
		return false
	}
	if estimatedBytes < 0 || analyzer.stats.EstimatedFactBytes > analyzer.limits.estimatedFactBytes-estimatedBytes {
		analyzer.factByteLimitHit = true
		analyzer.noteBound(
			"RKC-FLOW-2031",
			"bounded out at "+strconv.FormatInt(analyzer.limits.estimatedFactBytes, 10)+" estimated retained bytes for flow facts",
		)
		return false
	}
	analyzer.stats.FactRecords += 2
	analyzer.stats.EstimatedFactBytes += estimatedBytes
	return true
}

func boundedFlowText(value string) string {
	if len(value) <= maximumFlowTextBytes && utf8.ValidString(value) {
		return value
	}
	digest := sha256.Sum256([]byte(value))
	suffix := "...#" + hex.EncodeToString(digest[:8])
	if !utf8.ValidString(value) {
		return "invalid-utf8#" + hex.EncodeToString(digest[:8])
	}
	keep := maximumFlowTextBytes - len(suffix)
	for keep > 0 && !utf8.ValidString(value[:keep]) {
		keep--
	}
	return value[:keep] + suffix
}

func boundedFlowAttributes(attributes map[string]any) map[string]any {
	return boundedFlowAttributesDepth(attributes, 0)
}

func boundedFlowAttributesDepth(attributes map[string]any, depth int) map[string]any {
	if len(attributes) == 0 {
		return attributes
	}
	if depth >= 4 {
		return map[string]any{"bounded": true}
	}
	keys := make([]string, 0, len(attributes))
	for key := range attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > 32 {
		keys = keys[:32]
	}
	result := make(map[string]any, len(keys))
	for _, key := range keys {
		result[boundedFlowText(key)] = boundedFlowAttribute(attributes[key], depth+1)
	}
	return result
}

func boundedFlowAttribute(value any, depth int) any {
	if depth >= 4 {
		return "bounded"
	}
	switch typed := value.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return typed
	case string:
		return boundedFlowText(typed)
	case []string:
		limit := len(typed)
		if limit > 32 {
			limit = 32
		}
		result := make([]string, 0, limit)
		for _, item := range typed[:limit] {
			result = append(result, boundedFlowText(item))
		}
		return result
	case []any:
		limit := len(typed)
		if limit > 32 {
			limit = 32
		}
		result := make([]any, 0, limit)
		for _, item := range typed[:limit] {
			result = append(result, boundedFlowAttribute(item, depth+1))
		}
		return result
	case map[string]any:
		return boundedFlowAttributesDepth(typed, depth+1)
	default:
		return "unsupported"
	}
}

func estimatedNodeBytes(node rkcmodel.Node) int64 {
	return 512 + stringBytes(node.ID, node.LogicalID, node.Kind, node.Name, node.QualifiedName,
		node.Signature, node.Language, node.Visibility, node.Stability, node.ArtifactID,
		node.SemanticHash) + estimatedSourceBytes(node.Source) + estimatedStringsBytes(node.EvidenceIDs) +
		estimatedAttributesBytes(node.Attributes, 0)
}

func estimatedEdgeBytes(edge rkcmodel.Edge) int64 {
	return 384 + stringBytes(edge.ID, edge.Kind, edge.From, edge.To, edge.Resolution, edge.Producer,
		edge.Lifecycle) + estimatedStringsBytes(edge.EvidenceIDs) + estimatedAttributesBytes(edge.Attributes, 0)
}

func estimatedEvidenceBytes(evidence rkcmodel.Evidence) int64 {
	return 384 + stringBytes(evidence.ID, evidence.Kind, evidence.Method, evidence.Tool,
		evidence.ToolVersion, evidence.InputDigest, evidence.Detail) + estimatedSourceBytes(evidence.Source) +
		estimatedAttributesBytes(evidence.Attributes, 0)
}

func stringBytes(values ...string) int64 {
	var total int64
	for _, value := range values {
		total += int64(len(value)) + 16
	}
	return total
}

func estimatedStringsBytes(values []string) int64 {
	return 24 + stringBytes(values...)
}

func estimatedSourceBytes(source *rkcmodel.SourceRange) int64 {
	if source == nil {
		return 0
	}
	return 128 + stringBytes(source.ArtifactID, source.Path)
}

func estimatedAttributesBytes(attributes map[string]any, depth int) int64 {
	if len(attributes) == 0 || depth >= 4 {
		return 0
	}
	total := int64(64)
	for key, value := range attributes {
		total += int64(len(key)) + 32 + estimatedAttributeBytes(value, depth+1)
	}
	return total
}

func estimatedAttributeBytes(value any, depth int) int64 {
	if depth >= 4 {
		return 32
	}
	switch typed := value.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return 16
	case string:
		return int64(len(typed)) + 16
	case []string:
		return estimatedStringsBytes(typed)
	case []any:
		total := int64(24)
		for _, item := range typed {
			total += estimatedAttributeBytes(item, depth+1)
		}
		return total
	case map[string]any:
		return estimatedAttributesBytes(typed, depth+1)
	default:
		return 64
	}
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// cfgBlock is one bounded basic block. Blocks with no reachable predecessor
// are still recorded so the bounded graph never overstates reachability.
type cfgBlock struct {
	index       int
	kind        string
	unreachable bool
	successors  []cfgSuccessor
}

type cfgSuccessor struct {
	block      int
	kind       string
	condition  string
	unresolved bool
}

type cfgBuilder struct {
	analyzer    *goAnalyzer
	function    rkcmodel.Node
	fset        *token.FileSet
	declaration *ast.FuncDecl
	blocks      []cfgBlock
	current     int
	exit        int
	loopStack   []loopFrame
	labels      map[string]int
	nextBlock   int
	bounded     bool
}

type loopFrame struct {
	head  int
	after int
	kind  string
}

func (analyzer *goAnalyzer) buildCFG(function rkcmodel.Node, fset *token.FileSet, declaration *ast.FuncDecl) bool {
	builder := &cfgBuilder{
		analyzer: analyzer, function: function, fset: fset, declaration: declaration,
		labels: map[string]int{},
	}
	estimatedBlocks := estimateCFGBlocks(declaration, analyzer.limits.cfgBlocksPerFunc)
	if estimatedBlocks > analyzer.limits.cfgBlocksPerFunc {
		analyzer.stats.CFGBoundedFunctions++
		analyzer.stats.BoundedExceeded++
		analyzer.addDiagnostic("RKC-FLOW-2010", "function "+function.QualifiedName+" exceeds the per-function CFG block bound; CFG omitted")
		return false
	}
	if analyzer.stats.CFGBlocks+estimatedBlocks > analyzer.limits.cfgBlocksTotal {
		analyzer.cfgBlockLimitHit = true
		analyzer.noteBound("RKC-FLOW-2011", "bounded out at "+strconv.Itoa(analyzer.limits.cfgBlocksTotal)+" aggregate CFG blocks; remaining CFGs omitted")
		return false
	}
	// First pass: collect label targets.
	for _, statement := range declaration.Body.List {
		if labeled, ok := statement.(*ast.LabeledStmt); ok {
			builder.labels[labeled.Label.Name] = -1
		}
	}
	entry := builder.newBlock("entry")
	builder.current = entry
	builder.exit = builder.newBlock("exit")
	builder.emitStatements(declaration.Body.List)
	// A block with no successors reaches the exit.
	if len(builder.blocks[builder.current].successors) == 0 {
		builder.connect(builder.current, builder.exit, "exit", "")
	}
	return builder.emit()
}

// estimateCFGBlocks mirrors the block constructors below and stops as soon as
// the per-function ceiling is crossed. This prevents a deeply nested function
// from being partially published merely because its top-level statement count
// looked small.
func estimateCFGBlocks(declaration *ast.FuncDecl, limit int) int {
	count := 2 // entry and exit
	ast.Inspect(declaration.Body, func(node ast.Node) bool {
		if node == nil || count > limit {
			return false
		}
		switch value := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			count += 3
		case *ast.SwitchStmt:
			count++
			if value.Body != nil {
				count += len(value.Body.List)
			}
		case *ast.TypeSwitchStmt:
			count++
			if value.Body != nil {
				count += len(value.Body.List)
			}
		case *ast.SelectStmt:
			count++
			if value.Body != nil {
				count += len(value.Body.List)
			}
		case *ast.ReturnStmt:
			count++
		}
		return count <= limit
	})
	return count
}

func (builder *cfgBuilder) newBlock(kind string) int {
	index := len(builder.blocks)
	builder.blocks = append(builder.blocks, cfgBlock{index: index, kind: kind})
	return index
}

func (builder *cfgBuilder) connect(from, to int, kind, condition string) {
	if from < 0 || to < 0 || from >= len(builder.blocks) || to >= len(builder.blocks) {
		return
	}
	block := &builder.blocks[from]
	if len(block.successors) >= builder.analyzer.limits.successorsPerBlock {
		if !builder.bounded {
			builder.analyzer.stats.CFGBoundedFunctions++
		}
		builder.bounded = true
		return
	}
	block.successors = append(block.successors, cfgSuccessor{block: to, kind: kind, condition: condition})
}

func (builder *cfgBuilder) emitStatements(statements []ast.Stmt) {
	for _, statement := range statements {
		builder.emitStatement(statement)
	}
}

func (builder *cfgBuilder) emitStatement(statement ast.Stmt) {
	switch value := statement.(type) {
	case *ast.BlockStmt:
		builder.emitStatements(value.List)
	case *ast.IfStmt:
		builder.emitIf(value)
	case *ast.ForStmt:
		builder.emitFor(value)
	case *ast.RangeStmt:
		builder.emitRange(value)
	case *ast.SwitchStmt:
		builder.emitSwitch(value, value.Body)
	case *ast.TypeSwitchStmt:
		builder.emitSwitch(value, value.Body)
	case *ast.SelectStmt:
		builder.emitSelect(value)
	case *ast.ReturnStmt:
		builder.connect(builder.current, builder.exit, "return", "")
		builder.current = builder.newBlock("unreachable")
		builder.blocks[builder.current].unreachable = true
	case *ast.BranchStmt:
		builder.emitBranch(value)
	case *ast.LabeledStmt:
		if _, ok := builder.labels[value.Label.Name]; ok {
			builder.labels[value.Label.Name] = builder.current
		}
		builder.emitStatement(value.Stmt)
	case *ast.DeferStmt, *ast.GoStmt:
		// Spawned work does not change the enclosing control flow; the block
		// attribute records the spawn for runtime correlation.
		attributes := map[string]any{}
		_ = attributes
	default:
		// Assignments, expression statements, declarations, and sends stay in
		// the current block.
	}
}

func (builder *cfgBuilder) emitIf(value *ast.IfStmt) {
	thenBlock := builder.newBlock("branch")
	elseBlock := builder.newBlock("branch")
	join := builder.newBlock("join")
	builder.connect(builder.current, thenBlock, "branch", "true")
	builder.connect(builder.current, elseBlock, "branch", "false")
	previous := builder.current
	builder.current = thenBlock
	builder.emitStatement(value.Body)
	builder.connect(builder.current, join, "join", "")
	builder.current = elseBlock
	if value.Else != nil {
		builder.emitStatement(value.Else)
	}
	builder.connect(builder.current, join, "join", "")
	builder.current = join
	_ = previous
}

func (builder *cfgBuilder) emitFor(value *ast.ForStmt) {
	head := builder.newBlock("loop_head")
	body := builder.newBlock("loop_body")
	after := builder.newBlock("loop_after")
	builder.connect(builder.current, head, "loop", "")
	builder.connect(head, body, "loop_body", "condition")
	builder.loopStack = append(builder.loopStack, loopFrame{head: head, after: after, kind: "for"})
	builder.current = body
	builder.emitStatement(value.Body)
	builder.loopStack = builder.loopStack[:len(builder.loopStack)-1]
	builder.connect(builder.current, head, "loop_back", "")
	builder.connect(head, after, "loop_exit", "!condition")
	builder.current = after
}

func (builder *cfgBuilder) emitRange(value *ast.RangeStmt) {
	head := builder.newBlock("loop_head")
	body := builder.newBlock("loop_body")
	after := builder.newBlock("loop_after")
	builder.connect(builder.current, head, "loop", "")
	builder.connect(head, body, "loop_body", "next")
	builder.loopStack = append(builder.loopStack, loopFrame{head: head, after: after, kind: "range"})
	builder.current = body
	builder.emitStatement(value.Body)
	builder.loopStack = builder.loopStack[:len(builder.loopStack)-1]
	builder.connect(builder.current, head, "loop_back", "")
	builder.connect(head, after, "loop_exit", "exhausted")
	builder.current = after
}

func (builder *cfgBuilder) emitSwitch(statement ast.Stmt, body *ast.BlockStmt) {
	join := builder.newBlock("join")
	hasDefault := false
	first := true
	if body != nil {
		for _, clauseNode := range body.List {
			clause, ok := clauseNode.(*ast.CaseClause)
			if !ok {
				continue
			}
			if clause.List == nil {
				hasDefault = true
			}
			caseBlock := builder.newBlock("branch")
			condition := "case"
			if clause.List == nil {
				condition = "default"
			}
			builder.connect(builder.current, caseBlock, "branch", condition)
			previous := builder.current
			builder.current = caseBlock
			builder.emitStatements(clause.Body)
			// Explicit fallthrough continues into the next clause; the
			// fallthrough jump is emitted conservatively to the join.
			builder.connect(builder.current, join, "join", "")
			builder.current = previous
			first = false
		}
	}
	if !hasDefault {
		builder.connect(builder.current, join, "branch", "no_match")
	}
	builder.current = join
	_ = first
}

func (builder *cfgBuilder) emitSelect(value *ast.SelectStmt) {
	join := builder.newBlock("join")
	if value.Body != nil {
		for _, clauseNode := range value.Body.List {
			clause, ok := clauseNode.(*ast.CommClause)
			if !ok {
				continue
			}
			caseBlock := builder.newBlock("branch")
			builder.connect(builder.current, caseBlock, "branch", "comm")
			previous := builder.current
			builder.current = caseBlock
			builder.emitStatements(clause.Body)
			builder.connect(builder.current, join, "join", "")
			builder.current = previous
		}
	}
	builder.current = join
}

func (builder *cfgBuilder) emitBranch(value *ast.BranchStmt) {
	switch value.Tok.String() {
	case "break":
		if len(builder.loopStack) > 0 {
			frame := builder.loopStack[len(builder.loopStack)-1]
			builder.connect(builder.current, frame.after, "break", "")
		} else {
			builder.connect(builder.current, builder.exit, "break", "")
		}
	case "continue":
		if len(builder.loopStack) > 0 {
			frame := builder.loopStack[len(builder.loopStack)-1]
			builder.connect(builder.current, frame.head, "continue", "")
		} else {
			builder.connect(builder.current, builder.exit, "continue", "")
		}
	case "fallthrough":
		builder.connect(builder.current, builder.exit, "fallthrough", "")
	default:
		builder.connect(builder.current, builder.exit, "goto", "")
	}
}

// emit publishes the bounded CFG as nodes and precedes edges. Unreachable
// blocks are recorded with an explicit attribute instead of being discarded,
// so static possibility never claims them.
func (builder *cfgBuilder) emit() bool {
	if builder.bounded {
		builder.analyzer.stats.BoundedExceeded++
		builder.analyzer.addDiagnostic(
			"RKC-FLOW-2013",
			"function "+builder.function.QualifiedName+" exceeds the per-block successor bound; CFG omitted",
		)
		return false
	}
	edgeCount := 0
	for _, block := range builder.blocks {
		edgeCount += len(block.successors)
	}
	if builder.analyzer.stats.CFGEdges+edgeCount > builder.analyzer.limits.cfgEdgesTotal {
		builder.analyzer.cfgEdgeLimitHit = true
		builder.analyzer.noteBound("RKC-FLOW-2012", "bounded out at "+strconv.Itoa(builder.analyzer.limits.cfgEdgesTotal)+" aggregate CFG edges; remaining CFGs omitted")
		return false
	}
	for _, block := range builder.blocks {
		attributes := map[string]any{"kind": block.kind}
		if block.unreachable {
			attributes["unreachable"] = true
		}
		if !builder.analyzer.addNode(rkcmodel.Node{
			ID: cfgBlockID(builder.function.ID, block.index), Kind: "cfg_block",
			Name: blockName(builder.function, block.index), QualifiedName: builder.function.QualifiedName + "#block" + strconv.Itoa(block.index),
			Language: "go", Visibility: "internal", ArtifactID: builder.function.ArtifactID,
			Source:     blockSource(builder.function, builder.fset, builder.declaration),
			Attributes: attributes,
		}) {
			return false
		}
	}
	for _, block := range builder.blocks {
		for _, successor := range block.successors {
			attributes := map[string]any{"kind": successor.kind}
			if successor.condition != "" {
				attributes["condition"] = successor.condition
			}
			if successor.unresolved {
				attributes["unresolved"] = true
			}
			edgeID := rkcmodel.StableID("edge", "precedes", cfgBlockID(builder.function.ID, block.index), cfgBlockID(builder.function.ID, successor.block), successor.kind)
			if !builder.analyzer.addEdgeWithID(
				edgeID, "precedes", cfgBlockID(builder.function.ID, block.index),
				cfgBlockID(builder.function.ID, successor.block), "declared", attributes,
			) {
				return false
			}
		}
	}
	return true
}

func cfgBlockID(functionID string, index int) string {
	return rkcmodel.StableID("node", "cfg_block", functionID, strconv.Itoa(index))
}

func blockName(function rkcmodel.Node, index int) string {
	return function.Name + "#block" + strconv.Itoa(index)
}

func blockSource(function rkcmodel.Node, fset *token.FileSet, declaration *ast.FuncDecl) *rkcmodel.SourceRange {
	return function.Source
}
