package flow

import (
	"crypto/sha256"
	"fmt"
	"go/ast"
	"go/token"
	"regexp"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

// Value-flow roles are deterministic, documented markers attached to flow
// value entities. They answer lineage questions without executing code.
const (
	roleSource     = "source"
	roleSink       = "sink"
	roleSanitizer  = "sanitizer"
	roleLiteral    = "literal"
	roleCallResult = "call_result"
	roleParameter  = "parameter"
	roleReturn     = "return"
	roleField      = "field"
	roleComputed   = "computed"
	roleWrite      = "write"
	roleRange      = "range"
	roleExternal   = "external"
)

// envReadFunctions map a resolved or spelled callee to its environment-name
// argument position. RKC only records the variable NAME, never its value.
var envReadFunctions = map[string]int{
	"Getenv":    0,
	"LookupEnv": 0,
}

// sinkAPIs is deliberately package-qualified. A repository-local function
// named Run, Exec, Query, or Create is not a sink merely because its basename
// resembles a sensitive standard-library operation.
var sinkAPIs = map[string]map[string]bool{
	"database/sql": {
		"Exec": true, "ExecContext": true, "Query": true, "QueryContext": true,
		"QueryRow": true, "QueryRowContext": true, "Prepare": true, "PrepareContext": true,
	},
	"os/exec": {
		"Command": true, "CommandContext": true, "CombinedOutput": true,
		"Output": true, "Run": true, "Start": true,
	},
	"os":      {"WriteFile": true, "OpenFile": true, "Create": true},
	"syscall": {"Exec": true},
}

var sanitizerPattern = regexp.MustCompile(`(?i)^(sanitize|escape|validate|quote|clean|scrub|normalize|hash|encrypt|decrypt|redact|strip|purge|mask|sanitise)`)

// valueFlowBuilder performs the bounded, path-insensitive intraprocedural
// value-flow pass over one Go function body.
type valueFlowBuilder struct {
	analyzer    *goAnalyzer
	function    rkcmodel.Node
	fset        *token.FileSet
	declaration *ast.FuncDecl

	currentValues map[string]string
	paramValues   map[string]string
	requestParams map[string]bool
	valueCounter  int
	valueNodes    int
	flowEdges     int
	returnIndex   int
	root          string
	imports       map[string]string
	bounded       bool
	boundReported bool
}

type callResultValue struct {
	callerID string
	valueID  string
	targetID string
	spelling string
	args     []argValue
}

type argValue struct {
	position int
	valueID  string
}

func (analyzer *goAnalyzer) buildValueFlow(function rkcmodel.Node, fset *token.FileSet, file *ast.File, declaration *ast.FuncDecl) bool {
	builder := &valueFlowBuilder{
		analyzer: analyzer, function: function, fset: fset, declaration: declaration,
		currentValues: map[string]string{}, paramValues: map[string]string{},
		root: analyzer.options.Root, imports: goImportAliases(file),
	}
	builder.run()
	analyzer.flowEdgesByFunction[function.ID] = builder.flowEdges
	if builder.boundReported {
		analyzer.deferredBoundSeen[function.ID] = struct{}{}
	}
	return !builder.bounded
}

func (builder *valueFlowBuilder) run() {
	builder.registerParameters()
	builder.walkStatements(builder.declaration.Body.List)
	// Call bindings are published after every function has been analyzed so
	// callee return values always exist. The collected results live on the
	// analyzer because the builder is discarded per function.
}

func (builder *valueFlowBuilder) registerParameters() {
	sourceParams := builder.httpRequestParameterNames()
	builder.requestParams = sourceParams
	arguments := builder.function.Attributes["arguments"]
	if arguments == nil {
		return
	}
	list, ok := arguments.([]any)
	if !ok {
		return
	}
	for position, raw := range list {
		if builder.bounded {
			return
		}
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["name"].(string)
		displayName := name
		if displayName == "" {
			displayName = "parameter_" + strconv.Itoa(position)
		}
		attributes := map[string]any{
			"parameter": name, "position": position,
			"parameter_type": entry["type"],
		}
		if name == "" {
			attributes["unnamed"] = true
		}
		if sourceParams[name] {
			attributes["flow_role"] = roleSource
		}
		valueID := builder.newValueWithID(
			parameterID(builder.function.ID, position),
			builder.function.QualifiedName+"#parameter"+strconv.Itoa(position),
			roleParameter, displayName, attributes,
		)
		if valueID == "" {
			return
		}
		if sourceParams[name] {
			builder.analyzer.stats.Sources++
		}
		if name != "" {
			builder.paramValues[name] = valueID
			builder.currentValues[name] = valueID
		}
	}
}

// httpRequestParameterNames identifies only parameters whose AST type is the
// Request type of an import bound to the exact net/http package. A function
// name such as ServeHTTP, or a repository-local type named http.Request, is
// never sufficient source authority.
func (builder *valueFlowBuilder) httpRequestParameterNames() map[string]bool {
	result := map[string]bool{}
	if builder.declaration == nil || builder.declaration.Type == nil || builder.declaration.Type.Params == nil {
		return result
	}
	for _, field := range builder.declaration.Type.Params.List {
		if !builder.isNetHTTPRequestType(field.Type) {
			continue
		}
		for _, name := range field.Names {
			if name != nil && name.Name != "" {
				result[name.Name] = true
			}
		}
	}
	return result
}

func (builder *valueFlowBuilder) isNetHTTPRequestType(expression ast.Expr) bool {
	if pointer, ok := expression.(*ast.StarExpr); ok {
		expression = pointer.X
	}
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil || selector.Sel.Name != "Request" {
		return false
	}
	alias, ok := selector.X.(*ast.Ident)
	return ok && builder.imports[alias.Name] == "net/http"
}

func goImportAliases(file *ast.File) map[string]string {
	aliases := map[string]string{}
	if file == nil {
		return aliases
	}
	for _, declaration := range file.Imports {
		if declaration == nil || declaration.Path == nil {
			continue
		}
		path, err := strconv.Unquote(declaration.Path.Value)
		if err != nil || path == "" {
			continue
		}
		alias := path
		if slash := strings.LastIndexByte(alias, '/'); slash >= 0 {
			alias = alias[slash+1:]
		}
		if declaration.Name != nil {
			alias = declaration.Name.Name
		}
		if alias == "" || alias == "." || alias == "_" {
			continue
		}
		aliases[alias] = path
	}
	return aliases
}

func (builder *valueFlowBuilder) walkStatements(statements []ast.Stmt) {
	for _, statement := range statements {
		if builder.bounded {
			return
		}
		builder.walkStatement(statement)
	}
}

func (builder *valueFlowBuilder) walkStatement(statement ast.Stmt) {
	if builder.bounded {
		return
	}
	switch value := statement.(type) {
	case *ast.AssignStmt:
		builder.walkAssign(value)
	case *ast.ExprStmt:
		builder.analyzeCallExpr(value.X)
	case *ast.ReturnStmt:
		builder.walkReturn(value)
	case *ast.RangeStmt:
		key := builder.newValue(roleRange, "range_key", map[string]any{"variable": rangeName(value.Key)})
		item := builder.newValue(roleRange, "range_item", map[string]any{"variable": rangeName(value.Value)})
		if name := rangeName(value.Key); name != "" && key != "" {
			builder.currentValues[name] = key
		}
		if name := rangeName(value.Value); name != "" && item != "" {
			builder.currentValues[name] = item
		}
		if value.Body != nil {
			builder.walkStatements(value.Body.List)
		}
	case *ast.ForStmt:
		if value.Body != nil {
			builder.walkStatements(value.Body.List)
		}
	case *ast.IfStmt:
		if value.Body != nil {
			builder.walkStatements(value.Body.List)
		}
		if value.Else != nil {
			builder.walkStatement(value.Else)
		}
	case *ast.SwitchStmt:
		builder.walkCaseClauses(value.Body)
	case *ast.TypeSwitchStmt:
		builder.walkCaseClauses(value.Body)
	case *ast.SelectStmt:
		builder.walkCommClauses(value.Body)
	case *ast.BlockStmt:
		builder.walkStatements(value.List)
	case *ast.DeclStmt:
		if specification, ok := value.Decl.(*ast.GenDecl); ok {
			for _, rawSpec := range specification.Specs {
				if values, ok := rawSpec.(*ast.ValueSpec); ok {
					for index, name := range values.Names {
						var originID string
						if index < len(values.Values) {
							originID = builder.origin(values.Values[index])
						}
						targetID := builder.newValue(roleWrite, name.Name, map[string]any{"variable": name.Name, "declaration": true})
						if originID != "" {
							builder.flow(originID, targetID)
						}
						builder.currentValues[name.Name] = targetID
					}
				}
			}
		}
	case *ast.LabeledStmt:
		builder.walkStatement(value.Stmt)
	case *ast.DeferStmt:
		builder.analyzeCallExpr(value.Call)
	case *ast.GoStmt:
		builder.analyzeCallExpr(value.Call)
	case *ast.IncDecStmt:
		if identifier, ok := value.X.(*ast.Ident); ok {
			targetID := builder.newValue(roleWrite, identifier.Name, map[string]any{"variable": identifier.Name, "update": value.Tok.String()})
			if current, ok := builder.currentValues[identifier.Name]; ok {
				builder.flow(current, targetID)
			}
			builder.currentValues[identifier.Name] = targetID
		}
	}
}

func (builder *valueFlowBuilder) walkCaseClauses(body *ast.BlockStmt) {
	if body == nil {
		return
	}
	for _, rawClause := range body.List {
		if clause, ok := rawClause.(*ast.CaseClause); ok {
			builder.walkStatements(clause.Body)
		}
	}
}

func (builder *valueFlowBuilder) walkCommClauses(body *ast.BlockStmt) {
	if body == nil {
		return
	}
	for _, rawClause := range body.List {
		if clause, ok := rawClause.(*ast.CommClause); ok {
			builder.walkStatements(clause.Body)
		}
	}
}

func (builder *valueFlowBuilder) walkAssign(value *ast.AssignStmt) {
	for index, left := range value.Lhs {
		name := identifierName(left)
		if name == "" {
			continue
		}
		var originID string
		if index < len(value.Rhs) {
			originID = builder.origin(value.Rhs[index])
		} else if len(value.Rhs) == 1 && len(value.Lhs) > 1 {
			// Multi-assignment with a single RHS (for example a function
			// returning several values) is recorded conservatively: the
			// originating call's result flows into every target.
			originID = builder.origin(value.Rhs[0])
		}
		targetID := builder.newValue(roleWrite, name, map[string]any{
			"variable": name, "assign": value.Tok.String(),
		})
		if originID != "" {
			builder.flow(originID, targetID)
		}
		builder.currentValues[name] = targetID
	}
}

func (builder *valueFlowBuilder) walkReturn(value *ast.ReturnStmt) {
	for _, expression := range value.Results {
		originID := builder.origin(expression)
		returnID := builder.newValueWithID(
			returnValueID(builder.function.ID, builder.returnIndex),
			builder.function.QualifiedName+"#return"+strconv.Itoa(builder.returnIndex),
			roleReturn, "return", map[string]any{
				"return_index": builder.returnIndex, "function": builder.function.QualifiedName,
			},
		)
		builder.returnIndex++
		if originID != "" {
			builder.flow(originID, returnID)
		}
	}
}

// origin resolves an expression to the value node it produces or derives from.
func (builder *valueFlowBuilder) origin(expression ast.Expr) string {
	switch value := expression.(type) {
	case nil:
		return ""
	case *ast.Ident:
		if current, ok := builder.currentValues[value.Name]; ok {
			return current
		}
		return builder.newValue(roleExternal, value.Name, map[string]any{"variable": value.Name})
	case *ast.BasicLit:
		return builder.newValue(roleLiteral, "literal", literalMetadata(value))
	case *ast.ParenExpr:
		return builder.origin(value.X)
	case *ast.CallExpr:
		return builder.analyzeCallExpr(value)
	case *ast.SelectorExpr:
		base := builder.origin(value.X)
		field := builder.newValue(roleField, "field", map[string]any{"selector": value.Sel.Name})
		if base != "" {
			builder.flow(base, field)
		}
		return field
	case *ast.BinaryExpr:
		left := builder.origin(value.X)
		right := builder.origin(value.Y)
		computed := builder.newValue(roleComputed, "computed", map[string]any{"operator": value.Op.String()})
		if left != "" {
			builder.flow(left, computed)
		}
		if right != "" {
			builder.flow(right, computed)
		}
		return computed
	case *ast.UnaryExpr:
		operand := builder.origin(value.X)
		computed := builder.newValue(roleComputed, "computed", map[string]any{"operator": value.Op.String()})
		if operand != "" {
			builder.flow(operand, computed)
		}
		return computed
	case *ast.StarExpr:
		operand := builder.origin(value.X)
		derived := builder.newValue(roleComputed, "deref", nil)
		if operand != "" {
			builder.flow(operand, derived)
		}
		return derived
	case *ast.IndexExpr:
		base := builder.origin(value.X)
		derived := builder.newValue(roleField, "index", nil)
		if base != "" {
			builder.flow(base, derived)
		}
		return derived
	case *ast.SliceExpr:
		base := builder.origin(value.X)
		derived := builder.newValue(roleComputed, "slice", nil)
		if base != "" {
			builder.flow(base, derived)
		}
		return derived
	case *ast.CompositeLit:
		return builder.newValue(roleLiteral, "composite", nil)
	case *ast.FuncLit:
		return builder.newValue(roleExternal, "closure", nil)
	case *ast.TypeAssertExpr:
		base := builder.origin(value.X)
		derived := builder.newValue(roleComputed, "type_assert", nil)
		if base != "" {
			builder.flow(base, derived)
		}
		return derived
	default:
		return builder.newValue(roleExternal, "unknown", nil)
	}
}

// analyzeCallExpr records call-result values, argument binding, environment
// reads, sinks, and sanitizer relationships for one call expression.
func (builder *valueFlowBuilder) analyzeCallExpr(expression ast.Expr) string {
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return ""
	}
	// Method chains like db.QueryRow(...).Scan(...) hide the receiver call in
	// the selector base; analyze it so its sinks and return flow are recorded.
	if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
		builder.analyzeCallExpr(selector.X)
	}
	resultID := builder.newValue(roleCallResult, "call_result", nil)
	if resultID == "" {
		return ""
	}
	spelling := callSpelling(call.Fun)
	targetID, resolved := builder.resolveTarget(call, spelling)
	args := make([]argValue, 0, len(call.Args))
	for position, argument := range call.Args {
		argID := builder.origin(argument)
		if argID == "" {
			continue
		}
		args = append(args, argValue{position: position, valueID: argID})
	}
	if resolved && targetID != "" {
		builder.analyzer.pendingCallResults = append(builder.analyzer.pendingCallResults, callResultValue{
			callerID: builder.function.ID, valueID: resultID, targetID: targetID, spelling: spelling, args: args,
		})
	}
	if envName, isEnv := builder.environmentName(targetID, resolved, call); isEnv {
		if !builder.canAddFlowEdge() {
			return resultID
		}
		if builder.analyzer.stats.EnvReads >= builder.analyzer.limits.envReads {
			builder.analyzer.envReadLimitHit = true
			builder.analyzer.noteBound("RKC-FLOW-2024", "bounded out at "+strconv.Itoa(builder.analyzer.limits.envReads)+" environment reads")
			builder.stopBounded()
			return resultID
		}
		envID := rkcmodel.StableID("node", "environment_variable", envName)
		if !builder.analyzer.addNode(rkcmodel.Node{
			ID: envID, Kind: "environment_variable", Name: envName, QualifiedName: envName,
			Language: "go", Visibility: "internal", ArtifactID: builder.function.ArtifactID,
			Source:     builder.function.Source,
			Attributes: map[string]any{"name_only": true, "flow_role": roleSource},
		}) {
			builder.stopBounded()
			return resultID
		}
		builder.addFlowEdge("reads", envID, builder.function.ID, map[string]any{"spelling": spelling})
		resultID = builder.newValue(roleSource, "env_read", map[string]any{"environment_variable": envName})
		if resultID != "" {
			builder.analyzer.stats.Sources++
		}
	}
	if sourceKind := builder.requestSourceKind(targetID, resolved, call); sourceKind != "" {
		resultID = builder.newValue(roleSource, "request_source", map[string]any{
			"source_kind": sourceKind, "source_spelling": spelling,
		})
		if resultID != "" {
			builder.analyzer.stats.Sources++
		}
	}
	if sinkName, isSink := builder.sinkName(targetID, resolved, spelling); isSink {
		sinkValueID := builder.newValue(roleSink, "sink", map[string]any{
			"sink_via": sinkName, "sink_spelling": spelling,
		})
		// Every argument of a sink call flows into the sink: an unsanitized
		// value in any position can reach the sink operation.
		for _, argument := range args {
			builder.flow(argument.valueID, sinkValueID)
		}
		builder.flow(sinkValueID, resultID)
		if sinkValueID != "" {
			builder.analyzer.stats.Sinks++
		}
	}
	if builder.isSanitizerHypothesis(targetID, resolved) {
		if len(args) > 0 {
			builder.analyzer.addSanitizerHypothesis(targetID, args[0].valueID, map[string]any{
				"spelling": spelling, "position": 0,
			})
		}
	}
	return resultID
}

func parameterID(functionID string, position int) string {
	return rkcmodel.StableID("node", "parameter", "go", functionID, strconv.Itoa(position))
}

// resolveTarget finds the resolved callee for one exact call expression. A
// spelling is only a candidate selector: when several resolved targets share
// it, compiler/syntax source coordinates must identify one exact call site.
// Residual ambiguity stays outside the canonical flow graph.
func (builder *valueFlowBuilder) resolveTarget(call *ast.CallExpr, spelling string) (string, bool) {
	if spelling == "" {
		return "", false
	}
	sites := builder.analyzer.callGraph.byFunction[builder.function.ID]
	start := builder.fset.PositionFor(call.Pos(), false)
	end := builder.fset.PositionFor(call.End(), false)
	resolvedTargets := map[string]struct{}{}
	exactTargets := map[string]struct{}{}
	hasExactCoordinates := false
	for _, site := range sites {
		if site.Spelling != spelling {
			continue
		}
		if _, resolved := builder.analyzer.callGraph.functionByID[site.TargetID]; !resolved {
			continue
		}
		resolvedTargets[site.TargetID] = struct{}{}
		hasExactCoordinates = hasExactCoordinates || hasExactCallCoordinates(site.Span)
		if exactCallSpan(site.Span, start, end) {
			exactTargets[site.TargetID] = struct{}{}
		}
	}
	if len(exactTargets) == 1 {
		for targetID := range exactTargets {
			return targetID, true
		}
	}
	if len(exactTargets) > 1 {
		builder.noteAmbiguousCall(spelling, start)
		return "", false
	}
	if len(resolvedTargets) == 0 {
		return "", false
	}
	// Once the authority supplied exact coordinates, a non-match is evidence
	// that this syntax call is not the resolved site. Never fall back to a
	// spelling-only confidence-1 binding in that case.
	if hasExactCoordinates {
		builder.noteAmbiguousCall(spelling, start)
		return "", false
	}
	if len(resolvedTargets) == 1 {
		for targetID := range resolvedTargets {
			return targetID, true
		}
	}
	builder.noteAmbiguousCall(spelling, start)
	return "", false
}

func exactCallSpan(span *rkcmodel.SourceRange, start, end token.Position) bool {
	if !hasExactCallCoordinates(span) {
		return false
	}
	// Byte offsets are the primary identity because they are unambiguous even
	// when multiple calls occupy one source line. A non-empty Go call always
	// has EndByte > StartByte when the producer supplied byte coordinates.
	if span.EndByte > span.StartByte {
		return span.StartByte == int64(start.Offset) && span.EndByte == int64(end.Offset)
	}
	// Some compiler indexes provide exact editor coordinates without bytes.
	// Columns are zero based in SourceRange and one based in token.Position.
	if span.StartLine > 0 && span.EndLine > 0 && (span.StartColumn > 0 || span.EndColumn > 0) {
		return span.StartLine == start.Line && span.StartColumn == max(0, start.Column-1) &&
			span.EndLine == end.Line && span.EndColumn == max(0, end.Column-1)
	}
	return false
}

func hasExactCallCoordinates(span *rkcmodel.SourceRange) bool {
	return span != nil && (span.EndByte > span.StartByte ||
		(span.StartLine > 0 && span.EndLine > 0 && (span.StartColumn > 0 || span.EndColumn > 0)))
}

func (builder *valueFlowBuilder) noteAmbiguousCall(spelling string, position token.Position) {
	location := strconv.Itoa(position.Line) + ":" + strconv.Itoa(max(0, position.Column-1))
	builder.analyzer.addDiagnostic(
		"RKC-FLOW-2025",
		"ambiguous call "+spelling+" at "+builder.function.QualifiedName+":"+location+"; value-flow binding omitted",
	)
}

func (builder *valueFlowBuilder) environmentName(targetID string, resolved bool, call *ast.CallExpr) (string, bool) {
	if !resolved || targetID == "" {
		return "", false
	}
	target, ok := builder.analyzer.callGraph.functionByID[targetID]
	if !ok || !qualifiedPackageMatch(target.QualifiedName, "os") {
		return "", false
	}
	position, ok := envReadFunctions[target.Name]
	if !ok || position < 0 || position >= len(call.Args) {
		return "", false
	}
	literal, ok := call.Args[position].(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil || value == "" {
		return "", false
	}
	if strings.IndexByte(value, 0) >= 0 {
		return "", false
	}
	return value, true
}

func literalMetadata(literal *ast.BasicLit) map[string]any {
	kind := literal.Kind.String()
	value := literal.Value
	if literal.Kind == token.STRING || literal.Kind == token.CHAR {
		if decoded, err := strconv.Unquote(literal.Value); err == nil {
			value = decoded
		}
	}
	digest := sha256.Sum256([]byte(PluginID + "\x00go-literal\x00" + kind + "\x00" + value))
	return map[string]any{
		"literal_type":         kind,
		"literal_length_bytes": len([]byte(value)),
		"literal_sha256":       fmt.Sprintf("%x", digest),
	}
}

// requestSourceKinds identify APIs on compiler-resolved net/http symbols.
// Basenames alone are never source authority: database Query, arbitrary Body,
// or repository-local Header methods must not become HTTP input.
var requestSourceKinds = map[string]string{
	"FormValue": "http_request", "PostFormValue": "http_request",
	"Cookie": "http_request", "PathValue": "http_request",
}

func (builder *valueFlowBuilder) requestSourceKind(targetID string, resolved bool, call *ast.CallExpr) string {
	if resolved && targetID != "" {
		target, ok := builder.analyzer.callGraph.functionByID[targetID]
		if ok && qualifiedPackageMatch(target.QualifiedName, "net/http") {
			return requestSourceKinds[target.Name]
		}
	}
	// Syntax can also provide exact receiver authority inside the current
	// declaration: the receiver identifier must be a parameter proven above to
	// have type *<alias>.Request where alias imports exactly net/http.
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel == nil {
		return ""
	}
	receiver, ok := selector.X.(*ast.Ident)
	if !ok || !builder.requestParams[receiver.Name] {
		return ""
	}
	return requestSourceKinds[selector.Sel.Name]
}

func (builder *valueFlowBuilder) sinkName(targetID string, resolved bool, spelling string) (string, bool) {
	if !resolved || targetID == "" {
		return "", false
	}
	target, ok := builder.analyzer.callGraph.functionByID[targetID]
	if !ok || target.QualifiedName == "" {
		return "", false
	}
	qualified := target.QualifiedName
	base := target.Name
	for packagePath, names := range sinkAPIs {
		if !names[base] || !qualifiedPackageMatch(qualified, packagePath) {
			continue
		}
		return qualified, true
	}
	return "", false
}

func qualifiedPackageMatch(qualified, packagePath string) bool {
	if strings.HasPrefix(qualified, packagePath+".") ||
		strings.HasPrefix(qualified, packagePath+"(") {
		return true
	}
	// SCIP symbols end in a package-relative descriptor after their scheme,
	// manager, package, and version fields. Matching that complete final token
	// retains compiler-qualified APIs without accepting a longer lookalike path.
	descriptor := qualified
	if index := strings.LastIndexByte(qualified, ' '); index >= 0 {
		descriptor = qualified[index+1:]
	}
	return strings.HasPrefix(descriptor, packagePath+"/")
}

func (builder *valueFlowBuilder) isSanitizerHypothesis(targetID string, resolved bool) bool {
	if !resolved || targetID == "" {
		return false
	}
	target, ok := builder.analyzer.callGraph.functionByID[targetID]
	return ok && sanitizerPattern.MatchString(target.Name)
}

// emitCallBindings publishes returns_to edges from analyzed callee return
// values into every call-result value node recorded at call sites. It runs
// after every function body has been analyzed so callee return nodes exist.
func (analyzer *goAnalyzer) emitCallBindings() {
	for _, callResult := range analyzer.pendingCallResults {
		if analyzer.flowEdgeLimitHit || analyzer.factBudgetHit() {
			return
		}
		// Callee parameters may be analyzed after their callers, or omitted by
		// a configured bound. Publish a binding only after the complete pass has
		// established that the canonical parameter node actually exists.
		for _, argument := range callResult.args {
			parameter, exists := analyzer.callParameterID(callResult.targetID, argument.position)
			if !exists {
				continue
			}
			analyzer.addDeferredCallEdge(callResult, "binds_to", argument.valueID, parameter, map[string]any{
				"position": argument.position, "spelling": callResult.spelling,
			})
		}
		for index := 0; index < analyzer.limits.valueNodesPerFunc; index++ {
			returnID := returnValueID(callResult.targetID, index)
			if _, exists := analyzer.seenNodes[returnID]; exists {
				analyzer.addDeferredCallEdge(callResult, "returns_to", returnID, callResult.valueID, map[string]any{
					"spelling": callResult.spelling, "return_index": index,
				})
			}
		}
	}
}

func (analyzer *goAnalyzer) callParameterID(targetID string, actualPosition int) (string, bool) {
	target, exists := analyzer.callGraph.functionByID[targetID]
	if !exists || actualPosition < 0 {
		return "", false
	}
	arguments, ok := target.Attributes["arguments"].([]any)
	if !ok || len(arguments) == 0 {
		return "", false
	}
	formalPosition := actualPosition
	if formalPosition >= len(arguments) {
		variadic, _ := target.Attributes["variadic"].(bool)
		if !variadic {
			return "", false
		}
		formalPosition = len(arguments) - 1
	}
	parameter := parameterID(targetID, formalPosition)
	if _, exists := analyzer.nodeByID[parameter]; !exists {
		return "", false
	}
	return parameter, true
}

func (analyzer *goAnalyzer) addDeferredCallEdge(call callResultValue, kind, from, to string, attributes map[string]any) bool {
	if analyzer.flowEdgesByFunction[call.callerID] >= analyzer.limits.flowEdgesPerFunc {
		analyzer.noteDeferredFunctionBound(call.callerID)
		return false
	}
	if analyzer.addEdge(kind, from, to, "declared", attributes) {
		analyzer.flowEdgesByFunction[call.callerID]++
		return true
	}
	return false
}

func (analyzer *goAnalyzer) noteDeferredFunctionBound(functionID string) {
	if _, seen := analyzer.deferredBoundSeen[functionID]; seen {
		return
	}
	analyzer.deferredBoundSeen[functionID] = struct{}{}
	analyzer.stats.BoundedExceeded++
	qualifiedName := functionID
	if function, exists := analyzer.nodeByID[functionID]; exists && function.QualifiedName != "" {
		qualifiedName = function.QualifiedName
	}
	analyzer.addDiagnostic(
		"RKC-FLOW-2020",
		"function "+qualifiedName+" exceeded a per-function value-flow bound; remaining facts omitted",
	)
}

func returnValueID(functionID string, index int) string {
	return rkcmodel.StableID("node", "return_value", "go", functionID, strconv.Itoa(index))
}

func (builder *valueFlowBuilder) newValue(role, name string, attributes map[string]any) string {
	if builder.bounded {
		return ""
	}
	valueID := rkcmodel.StableID("node", "value", "go", builder.function.ID, strconv.Itoa(builder.valueCounter))
	builder.valueCounter++
	return builder.newValueWithID(valueID, builder.function.QualifiedName+"#value"+strconv.Itoa(builder.valueCounter-1), role, name, attributes)
}

// newValueWithID creates a value entity with an explicit identity, used by
// parameters and return values whose IDs are stable targets for binds_to and
// returns_to edges.
func (builder *valueFlowBuilder) newValueWithID(id, qualifiedName, role, name string, attributes map[string]any) string {
	if builder.bounded {
		return ""
	}
	if builder.valueNodes >= builder.analyzer.limits.valueNodesPerFunc {
		builder.markBounded()
		return ""
	}
	merged := map[string]any{"flow_role": role}
	for key, value := range attributes {
		merged[key] = value
	}
	if !builder.analyzer.addNode(rkcmodel.Node{
		ID: id, Kind: "value", Name: name, QualifiedName: qualifiedName,
		Language: "go", Visibility: "internal", ArtifactID: builder.function.ArtifactID,
		Source: builder.function.Source, Attributes: merged,
	}) {
		builder.stopBounded()
		return ""
	}
	builder.valueNodes++
	return id
}

func (builder *valueFlowBuilder) flow(from, to string) {
	if from == "" || to == "" || from == to {
		return
	}
	builder.addFlowEdge("flows_to", from, to, nil)
}

func (builder *valueFlowBuilder) canAddFlowEdge() bool {
	if builder.bounded {
		return false
	}
	if builder.flowEdges >= builder.analyzer.limits.flowEdgesPerFunc {
		builder.markBounded()
		return false
	}
	if builder.analyzer.flowEdgeLimitHit {
		builder.stopBounded()
		return false
	}
	return true
}

func (builder *valueFlowBuilder) addFlowEdge(kind, from, to string, attributes map[string]any) bool {
	if from == "" || to == "" || !builder.canAddFlowEdge() {
		return false
	}
	if builder.analyzer.addEdge(kind, from, to, "declared", attributes) {
		builder.flowEdges++
		return true
	}
	if builder.analyzer.flowEdgeLimitHit || builder.analyzer.envReadLimitHit {
		builder.stopBounded()
	}
	return false
}

func (builder *valueFlowBuilder) markBounded() {
	builder.bounded = true
	if builder.boundReported {
		return
	}
	builder.boundReported = true
	builder.analyzer.stats.BoundedExceeded++
	builder.analyzer.addDiagnostic(
		"RKC-FLOW-2020",
		"function "+builder.function.QualifiedName+" exceeded a per-function value-flow bound; remaining facts omitted",
	)
}

func (builder *valueFlowBuilder) stopBounded() {
	builder.bounded = true
}

func callSpelling(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		prefix := callSpelling(value.X)
		if prefix == "" {
			return value.Sel.Name
		}
		return prefix + "." + value.Sel.Name
	case *ast.IndexExpr:
		return callSpelling(value.X)
	case *ast.IndexListExpr:
		return callSpelling(value.X)
	case *ast.ParenExpr:
		return callSpelling(value.X)
	default:
		return ""
	}
}

func identifierName(expression ast.Expr) string {
	if identifier, ok := expression.(*ast.Ident); ok {
		return identifier.Name
	}
	return ""
}

func rangeName(expression ast.Expr) string {
	return identifierName(expression)
}
