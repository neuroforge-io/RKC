// Package tssyntax provides a deterministic, dependency-free Tier-1 extractor
// for JavaScript, JSX, TypeScript, and TSX. It intentionally stops short of
// compiler semantics. Declarations and source ranges are syntax facts; calls,
// route handlers, and symbol targets remain syntax-inferred or unresolved until
// a TypeScript compiler adapter contributes a higher-precision GraphPatch.
package tssyntax

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	PluginID      = "rkc.typescript-syntax"
	PluginVersion = "0.2.0"
)

type Options struct {
	Root       string
	SnapshotID string
	Files      []pluginapi.FileRef
}

type tokenKind uint8

const (
	tokenIdentifier tokenKind = iota + 1
	tokenString
	tokenNumber
	tokenPunctuation
)

type token struct {
	kind                  tokenKind
	text                  string
	start, end            int
	line, column, endLine int
	endColumn             int
}

type extractor struct {
	root        string
	snapshotID  string
	packageName string
	fragment    rkcmodel.Fragment
	seenNodes   map[string]struct{}
	seenEdges   map[string]struct{}
	modules     map[string]string
	projects    map[string]string
}

func Extract(options Options) (rkcmodel.Fragment, error) {
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return rkcmodel.Fragment{}, err
	}
	e := &extractor{
		root: root, snapshotID: options.SnapshotID, packageName: readPackageName(root),
		seenNodes: map[string]struct{}{}, seenEdges: map[string]struct{}{}, modules: map[string]string{}, projects: map[string]string{},
	}
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, file := range files {
		if file.Language != "typescript" && file.Language != "javascript" {
			continue
		}
		e.extractFile(file)
	}
	sort.Slice(e.fragment.Nodes, func(i, j int) bool { return e.fragment.Nodes[i].ID < e.fragment.Nodes[j].ID })
	sort.Slice(e.fragment.Edges, func(i, j int) bool { return e.fragment.Edges[i].ID < e.fragment.Edges[j].ID })
	sort.Slice(e.fragment.Evidence, func(i, j int) bool { return e.fragment.Evidence[i].ID < e.fragment.Evidence[j].ID })
	sort.Slice(e.fragment.Diagnostics, func(i, j int) bool { return e.fragment.Diagnostics[i].ID < e.fragment.Diagnostics[j].ID })
	return e.fragment, nil
}

func (e *extractor) extractFile(file pluginapi.FileRef) {
	data, err := sourcepath.ReadFile(e.root, file.Path)
	if err != nil {
		e.error(file, "RKC-TS-1001", err.Error(), 1, 0)
		return
	}
	tokens, diagnostics := lex(data, file)
	e.fragment.Diagnostics = append(e.fragment.Diagnostics, diagnostics...)
	if len(tokens) == 0 {
		return
	}
	moduleQualified := moduleQualifiedName(e.packageName, file.Path)
	moduleID := rkcmodel.StableID("node", "module", file.Language, moduleQualified)
	moduleEvidence := e.evidence(file, "declared", "typescript.syntax.module", tokens[0], tokens[len(tokens)-1], moduleQualified, 1)
	e.addNode(rkcmodel.Node{
		ID: moduleID, LogicalID: rkcmodel.StableID("logical", file.Language, "module", moduleQualified), Kind: "module",
		Name: strings.TrimSuffix(filepath.Base(file.Path), filepath.Ext(file.Path)), QualifiedName: moduleQualified,
		Language: file.Language, Visibility: "repository", ArtifactID: file.ArtifactID,
		Source: sourceRange(file, tokens[0], tokens[len(tokens)-1]), EvidenceIDs: []string{moduleEvidence},
		Attributes: map[string]any{"module_system": detectModuleSystem(tokens), "package_name": e.packageName},
	})
	e.addEdge("derived_from", moduleID, file.ArtifactID, "declared", []string{moduleEvidence}, nil)
	if projectID := e.projectFor(file, moduleEvidence); projectID != "" {
		e.addEdge("contains", projectID, moduleID, "declared", []string{moduleEvidence}, nil)
	}
	e.modules[file.Path] = moduleID
	parser := parser{extractor: e, file: file, data: data, tokens: tokens, moduleID: moduleID, moduleQualified: moduleQualified}
	parser.parseRange(0, len(tokens), "", "")
}

func (e *extractor) projectFor(file pluginapi.FileRef, evidenceID string) string {
	if e.packageName == "" {
		return ""
	}
	if id, ok := e.projects[e.packageName]; ok {
		return id
	}
	manifestPath := "package.json"
	id := rkcmodel.StableID("node", "project", manifestPath, "npm", e.packageName)
	e.projects[e.packageName] = id
	e.addNode(rkcmodel.Node{
		ID: id, LogicalID: rkcmodel.StableID("logical", "project", "npm", e.packageName), Kind: "project",
		Name: e.packageName, QualifiedName: "npm:" + e.packageName, Language: "npm", Visibility: "repository",
		ArtifactID: file.ArtifactID, EvidenceIDs: []string{evidenceID},
		Attributes: map[string]any{"ecosystem": "npm", "manifest": manifestPath, "synthetic_reference": true, "inferred_from": file.Path},
	})
	return id
}

type parser struct {
	extractor       *extractor
	file            pluginapi.FileRef
	data            []byte
	tokens          []token
	moduleID        string
	moduleQualified string
}

func (p *parser) parseRange(start, end int, parentID, parentQualified string) {
	for i := start; i < end; {
		if p.tokens[i].text == "import" {
			i = p.parseImport(i, end)
			continue
		}
		if p.isRequireDeclaration(i, end) {
			i = p.parseRequire(i, end)
			continue
		}
		declarationStart := i
		exported, defaultExport, next := p.consumeExportModifiers(i, end)
		if next != i {
			i = next
		}
		for i < end && isModifier(p.tokens[i].text) {
			if parentID != "" && (p.tokens[i].text == "get" || p.tokens[i].text == "set") && i+1 < end && p.tokens[i+1].text == "(" {
				break
			}
			i++
		}
		asyncModifier := hasToken(p.tokens[declarationStart:i], "async")
		if i >= end {
			break
		}
		switch p.tokens[i].text {
		case "function":
			i = p.parseFunction(i, end, parentID, parentQualified, exported, defaultExport, "function", asyncModifier)
		case "class":
			i = p.parseClass(i, end, parentID, parentQualified, exported, defaultExport)
		case "interface":
			i = p.parseNamedType(i, end, parentID, parentQualified, exported, "interface")
		case "type":
			i = p.parseNamedType(i, end, parentID, parentQualified, exported, "type")
		case "enum":
			i = p.parseEnum(i, end, parentID, parentQualified, exported)
		case "const", "let", "var":
			i = p.parseVariable(i, end, parentID, parentQualified, exported)
		default:
			if parentID != "" && p.looksLikeMethod(i, end) {
				i = p.parseMethod(i, end, parentID, parentQualified)
			} else {
				i++
			}
		}
	}
}

func (p *parser) consumeExportModifiers(i, end int) (exported, defaultExport bool, next int) {
	next = i
	if next < end && p.tokens[next].text == "export" {
		exported = true
		next++
		if next < end && p.tokens[next].text == "default" {
			defaultExport = true
			next++
		}
	}
	return
}

func (p *parser) parseImport(i, end int) int {
	start := i
	i++
	depth := 0
	target := ""
	alias := ""
	for i < end {
		t := p.tokens[i]
		if t.text == "{" || t.text == "(" || t.text == "[" {
			depth++
		}
		if t.text == "}" || t.text == ")" || t.text == "]" {
			if depth > 0 {
				depth--
			}
		}
		if t.kind == tokenString && (target == "" || (i > start && p.tokens[i-1].text == "from")) {
			target = unquoteToken(t.text)
		}
		if alias == "" && t.kind == tokenIdentifier && t.text != "from" && t.text != "type" {
			alias = t.text
		}
		if depth == 0 && (t.text == ";" || (i+1 < end && p.tokens[i+1].line > t.line && target != "")) {
			i++
			break
		}
		i++
	}
	if target != "" {
		dependencyID, resolution := p.importTarget(target)
		evKind, confidence := "declared", 1.0
		if resolution == "unresolved" {
			evKind, confidence = "syntax_inferred", 0.75
		}
		ev := p.extractor.evidence(p.file, evKind, "typescript.syntax.import", p.tokens[start], p.tokens[max(start, i-1)], target, confidence)
		p.extractor.addEdge("imports", p.moduleID, dependencyID, resolution, []string{ev}, map[string]any{"module": target, "alias": alias})
	}
	return max(i, start+1)
}

func (p *parser) isRequireDeclaration(i, end int) bool {
	if i+4 >= end || (p.tokens[i].text != "const" && p.tokens[i].text != "let" && p.tokens[i].text != "var") {
		return false
	}
	for j := i + 1; j < min(end, i+12); j++ {
		if p.tokens[j].text == "require" && j+2 < end && p.tokens[j+1].text == "(" && p.tokens[j+2].kind == tokenString {
			return true
		}
		if p.tokens[j].text == ";" {
			break
		}
	}
	return false
}

func (p *parser) parseRequire(i, end int) int {
	start := i
	alias := ""
	if i+1 < end && p.tokens[i+1].kind == tokenIdentifier {
		alias = p.tokens[i+1].text
	}
	for i < end {
		if p.tokens[i].text == "require" && i+2 < end && p.tokens[i+1].text == "(" && p.tokens[i+2].kind == tokenString {
			target := unquoteToken(p.tokens[i+2].text)
			dependencyID, resolution := p.importTarget(target)
			evKind, confidence := "declared", 1.0
			if resolution == "unresolved" {
				evKind, confidence = "syntax_inferred", 0.75
			}
			ev := p.extractor.evidence(p.file, evKind, "javascript.syntax.require", p.tokens[start], p.tokens[i+2], target, confidence)
			p.extractor.addEdge("imports", p.moduleID, dependencyID, resolution, []string{ev}, map[string]any{"module": target, "alias": alias, "style": "commonjs"})
			return p.statementEnd(i+3, end)
		}
		i++
	}
	return start + 1
}

func (p *parser) parseFunction(i, end int, parentID, parentQualified string, exported, defaultExport bool, declaredKind string, asyncModifier bool) int {
	start := i
	i++
	if i < end && p.tokens[i].text == "*" {
		i++
	}
	name := "default"
	if i < end && p.tokens[i].kind == tokenIdentifier {
		name = p.tokens[i].text
		i++
	} else if !defaultExport {
		return start + 1
	}
	if i < end && p.tokens[i].text == "<" {
		i = p.skipBalanced(i, end, "<", ">")
	}
	if i >= end || p.tokens[i].text != "(" {
		return start + 1
	}
	close := p.matching(i, end, "(", ")")
	if close < 0 {
		return start + 1
	}
	parameters := p.parameters(i+1, close)
	j := close + 1
	returnType := ""
	if j < end && p.tokens[j].text == ":" {
		typeStart := j + 1
		j = p.untilBodyOrTerminator(typeStart, end)
		returnType = p.render(typeStart, j)
	}
	bodyStart := j
	for bodyStart < end && p.tokens[bodyStart].text != "{" && p.tokens[bodyStart].text != ";" {
		bodyStart++
	}
	bodyEnd := bodyStart
	if bodyStart < end && p.tokens[bodyStart].text == "{" {
		bodyEnd = p.matching(bodyStart, end, "{", "}")
		if bodyEnd < 0 {
			bodyEnd = end - 1
		}
	}
	qualified := qualify(p.moduleQualified, parentQualified, name)
	kind := declaredKind
	if parentID != "" {
		kind = "method"
	}
	if strings.HasSuffix(p.file.Path, ".test.ts") || strings.HasSuffix(p.file.Path, ".spec.ts") || strings.HasSuffix(p.file.Path, ".test.js") || strings.HasSuffix(p.file.Path, ".spec.js") {
		if strings.HasPrefix(name, "test") || strings.HasPrefix(name, "it") {
			kind = "test"
		}
	}
	signature := p.render(start, max(close+1, j))
	id := rkcmodel.StableID("node", kind, qualified, signature)
	ev := p.extractor.evidence(p.file, "declared", "typescript.syntax."+kind, p.tokens[start], p.tokens[max(start, max(bodyEnd, close))], qualified, 1)
	visibility := "private"
	if exported || parentID != "" {
		visibility = "public"
	}
	node := rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", p.file.Language, kind, qualified), Kind: kind, Name: name, QualifiedName: qualified, Signature: signature, Language: p.file.Language, Visibility: visibility, PublicSurface: exported, ArtifactID: p.file.ArtifactID, Source: sourceRange(p.file, p.tokens[start], p.tokens[max(start, max(bodyEnd, close))]), EvidenceIDs: []string{ev}, Attributes: map[string]any{"arguments": parameters, "return_type": returnType, "async": asyncModifier, "generator": hasToken(p.tokens[start:i], "*")}}
	p.extractor.addNode(node)
	owner := p.moduleID
	if parentID != "" {
		owner = parentID
	}
	p.extractor.addEdge("declares", owner, id, "declared", []string{ev}, nil)
	if exported {
		p.extractor.addEdge("exports", p.moduleID, id, "declared", []string{ev}, map[string]any{"default": defaultExport})
	}
	if bodyStart < end && p.tokens[bodyStart].text == "{" {
		p.parseCalls(id, bodyStart+1, bodyEnd)
	}
	if bodyEnd > bodyStart {
		return bodyEnd + 1
	}
	return p.statementEnd(close+1, end)
}

func (p *parser) parseClass(i, end int, parentID, parentQualified string, exported, defaultExport bool) int {
	start := i
	if i+1 >= end || p.tokens[i+1].kind != tokenIdentifier {
		return i + 1
	}
	name := p.tokens[i+1].text
	i += 2
	qualified := qualify(p.moduleQualified, parentQualified, name)
	bodyStart := -1
	var extends, implements []string
	for i < end {
		if p.tokens[i].text == "extends" && i+1 < end {
			extends = append(extends, p.readTypeName(i+1, end))
			i++
		}
		if p.tokens[i].text == "implements" {
			implements, i = p.readTypeList(i+1, end)
			if i >= end {
				break
			}
		}
		if p.tokens[i].text == "{" {
			bodyStart = i
			break
		}
		if p.tokens[i].text == ";" {
			break
		}
		i++
	}
	if bodyStart < 0 {
		return max(i, start+1)
	}
	bodyEnd := p.matching(bodyStart, end, "{", "}")
	if bodyEnd < 0 {
		bodyEnd = end - 1
	}
	id := rkcmodel.StableID("node", "class", qualified)
	ev := p.extractor.evidence(p.file, "declared", "typescript.syntax.class", p.tokens[start], p.tokens[bodyEnd], qualified, 1)
	visibility := "private"
	if exported {
		visibility = "public"
	}
	p.extractor.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", p.file.Language, "class", qualified), Kind: "class", Name: name, QualifiedName: qualified, Signature: p.render(start, bodyStart), Language: p.file.Language, Visibility: visibility, PublicSurface: exported, ArtifactID: p.file.ArtifactID, Source: sourceRange(p.file, p.tokens[start], p.tokens[bodyEnd]), EvidenceIDs: []string{ev}, Attributes: map[string]any{"extends": extends, "implements": implements, "default_export": defaultExport}})
	owner := p.moduleID
	if parentID != "" {
		owner = parentID
	}
	p.extractor.addEdge("declares", owner, id, "declared", []string{ev}, nil)
	if exported {
		p.extractor.addEdge("exports", p.moduleID, id, "declared", []string{ev}, map[string]any{"default": defaultExport})
	}
	for _, base := range extends {
		p.unresolvedEdge("inherits", id, base, ev)
	}
	for _, contract := range implements {
		p.unresolvedEdge("implements", id, contract, ev)
	}
	p.parseRange(bodyStart+1, bodyEnd, id, qualified)
	return bodyEnd + 1
}

func (p *parser) parseNamedType(i, end int, parentID, parentQualified string, exported bool, kind string) int {
	start := i
	if i+1 >= end || p.tokens[i+1].kind != tokenIdentifier {
		return i + 1
	}
	name := p.tokens[i+1].text
	qualified := qualify(p.moduleQualified, parentQualified, name)
	j := i + 2
	bodyStart := -1
	for j < end {
		if p.tokens[j].text == "{" {
			bodyStart = j
			break
		}
		if p.tokens[j].text == ";" {
			break
		}
		j++
	}
	finish := j
	if bodyStart >= 0 {
		if close := p.matching(bodyStart, end, "{", "}"); close >= 0 {
			finish = close
		}
	}
	id := rkcmodel.StableID("node", kind, qualified)
	ev := p.extractor.evidence(p.file, "declared", "typescript.syntax."+kind, p.tokens[start], p.tokens[max(start, finish)], qualified, 1)
	visibility := "private"
	if exported {
		visibility = "public"
	}
	p.extractor.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", p.file.Language, kind, qualified), Kind: kind, Name: name, QualifiedName: qualified, Signature: p.render(start, max(start+1, func() int {
		if bodyStart >= 0 {
			return bodyStart
		}
		return finish + 1
	}())), Language: p.file.Language, Visibility: visibility, PublicSurface: exported, ArtifactID: p.file.ArtifactID, Source: sourceRange(p.file, p.tokens[start], p.tokens[max(start, finish)]), EvidenceIDs: []string{ev}})
	owner := p.moduleID
	if parentID != "" {
		owner = parentID
	}
	p.extractor.addEdge("declares", owner, id, "declared", []string{ev}, nil)
	if exported {
		p.extractor.addEdge("exports", p.moduleID, id, "declared", []string{ev}, nil)
	}
	if bodyStart >= 0 {
		if kind == "interface" && finish > bodyStart {
			p.parseRange(bodyStart+1, finish, id, qualified)
		}
		return finish + 1
	}
	return p.statementEnd(finish+1, end)
}

func (p *parser) parseEnum(i, end int, parentID, parentQualified string, exported bool) int {
	start := i
	if i+1 >= end || p.tokens[i+1].kind != tokenIdentifier {
		return i + 1
	}
	name := p.tokens[i+1].text
	qualified := qualify(p.moduleQualified, parentQualified, name)
	bodyStart := i + 2
	for bodyStart < end && p.tokens[bodyStart].text != "{" {
		bodyStart++
	}
	if bodyStart >= end {
		return i + 1
	}
	bodyEnd := p.matching(bodyStart, end, "{", "}")
	if bodyEnd < 0 {
		bodyEnd = end - 1
	}
	id := rkcmodel.StableID("node", "enum", qualified)
	ev := p.extractor.evidence(p.file, "declared", "typescript.syntax.enum", p.tokens[start], p.tokens[bodyEnd], qualified, 1)
	visibility := "private"
	if exported {
		visibility = "public"
	}
	p.extractor.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", p.file.Language, "enum", qualified), Kind: "enum", Name: name, QualifiedName: qualified, Signature: p.render(start, bodyStart), Language: p.file.Language, Visibility: visibility, PublicSurface: exported, ArtifactID: p.file.ArtifactID, Source: sourceRange(p.file, p.tokens[start], p.tokens[bodyEnd]), EvidenceIDs: []string{ev}})
	owner := p.moduleID
	if parentID != "" {
		owner = parentID
	}
	p.extractor.addEdge("declares", owner, id, "declared", []string{ev}, nil)
	if exported {
		p.extractor.addEdge("exports", p.moduleID, id, "declared", []string{ev}, nil)
	}
	segmentStart := bodyStart + 1
	depth := 0
	for j := bodyStart + 1; j <= bodyEnd; j++ {
		atEnd := j == bodyEnd
		if !atEnd {
			switch p.tokens[j].text {
			case "(", "[", "{", "<":
				depth++
			case ")", "]", "}", ">":
				if depth > 0 {
					depth--
				}
			case ">>":
				depth = max(0, depth-2)
			}
		}
		if !atEnd && !(depth == 0 && p.tokens[j].text == ",") {
			continue
		}
		memberIndex := -1
		for k := segmentStart; k < j; k++ {
			if p.tokens[k].kind == tokenIdentifier || p.tokens[k].kind == tokenString {
				memberIndex = k
				break
			}
		}
		if memberIndex >= 0 {
			member := strings.Trim(p.tokens[memberIndex].text, "'\"")
			memberQualified := qualified + "." + member
			memberID := rkcmodel.StableID("node", "enum_member", memberQualified)
			mev := p.extractor.evidence(p.file, "declared", "typescript.syntax.enum_member", p.tokens[memberIndex], p.tokens[memberIndex], memberQualified, 1)
			p.extractor.addNode(rkcmodel.Node{ID: memberID, LogicalID: rkcmodel.StableID("logical", p.file.Language, "enum_member", memberQualified), Kind: "enum_member", Name: member, QualifiedName: memberQualified, Language: p.file.Language, Visibility: visibility, PublicSurface: exported, ArtifactID: p.file.ArtifactID, Source: sourceRange(p.file, p.tokens[memberIndex], p.tokens[memberIndex]), EvidenceIDs: []string{mev}})
			p.extractor.addEdge("declares", id, memberID, "declared", []string{mev}, nil)
		}
		segmentStart = j + 1
	}
	return bodyEnd + 1
}

func (p *parser) parseVariable(i, end int, parentID, parentQualified string, exported bool) int {
	start := i
	declarationKind := p.tokens[i].text
	i++
	if i >= end || p.tokens[i].kind != tokenIdentifier {
		return start + 1
	}
	name := p.tokens[i].text
	nameIndex := i
	statementEnd := p.statementEnd(i+1, end)
	arrow := -1
	depth := 0
	for j := i + 1; j < statementEnd; j++ {
		switch p.tokens[j].text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			if depth > 0 {
				depth--
			}
		case "=>":
			if depth == 0 || p.tokens[j-1].text == ")" {
				arrow = j
			}
		}
		if arrow >= 0 {
			break
		}
	}
	qualified := qualify(p.moduleQualified, parentQualified, name)
	kind := "variable"
	signature := p.render(start, statementEnd)
	attributes := map[string]any{"declaration_kind": declarationKind}
	bodyStart, bodyEnd := -1, -1
	if arrow >= 0 {
		kind = "function"
		attributes["arrow_function"] = true
		open := nameIndex + 1
		for open < arrow && p.tokens[open].text != "(" {
			open++
		}
		if open < arrow {
			if close := p.matching(open, arrow+1, "(", ")"); close >= 0 {
				attributes["arguments"] = p.parameters(open+1, close)
			}
		}
		if arrow+1 < statementEnd && p.tokens[arrow+1].text == "{" {
			bodyStart = arrow + 1
			bodyEnd = p.matching(bodyStart, statementEnd, "{", "}")
		}
	}
	id := rkcmodel.StableID("node", kind, qualified, signature)
	ev := p.extractor.evidence(p.file, "declared", "typescript.syntax."+kind, p.tokens[start], p.tokens[max(start, statementEnd-1)], qualified, 1)
	visibility := "private"
	if exported {
		visibility = "public"
	}
	p.extractor.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", p.file.Language, kind, qualified), Kind: kind, Name: name, QualifiedName: qualified, Signature: signature, Language: p.file.Language, Visibility: visibility, PublicSurface: exported, ArtifactID: p.file.ArtifactID, Source: sourceRange(p.file, p.tokens[start], p.tokens[max(start, statementEnd-1)]), EvidenceIDs: []string{ev}, Attributes: attributes})
	owner := p.moduleID
	if parentID != "" {
		owner = parentID
	}
	p.extractor.addEdge("declares", owner, id, "declared", []string{ev}, nil)
	if exported {
		p.extractor.addEdge("exports", p.moduleID, id, "declared", []string{ev}, nil)
	}
	if bodyStart >= 0 && bodyEnd > bodyStart {
		p.parseCalls(id, bodyStart+1, bodyEnd)
	}
	return max(statementEnd, start+1)
}

func (p *parser) looksLikeMethod(i, end int) bool {
	j := i
	if j+1 < end && (p.tokens[j].text == "get" || p.tokens[j].text == "set") && p.tokens[j+1].text == "(" {
		return true
	}
	for j < end && isModifier(p.tokens[j].text) {
		j++
	}
	if j >= end {
		return false
	}
	if p.tokens[j].text == "constructor" {
		return j+1 < end && p.tokens[j+1].text == "("
	}
	if p.tokens[j].kind != tokenIdentifier && p.tokens[j].kind != tokenString {
		return false
	}
	j++
	if j < end && p.tokens[j].text == "?" {
		j++
	}
	if j < end && p.tokens[j].text == "<" {
		j = p.skipBalanced(j, end, "<", ">")
	}
	return j < end && p.tokens[j].text == "("
}

func (p *parser) parseMethod(i, end int, parentID, parentQualified string) int {
	start := i
	visibility := "public"
	async := false
	static := false
	for i < end && isModifier(p.tokens[i].text) {
		if (p.tokens[i].text == "get" || p.tokens[i].text == "set") && i+1 < end && p.tokens[i+1].text == "(" {
			break
		}
		switch p.tokens[i].text {
		case "private", "protected", "public":
			visibility = p.tokens[i].text
		case "async":
			async = true
		case "static":
			static = true
		}
		i++
	}
	if i >= end {
		return start + 1
	}
	name := strings.Trim(p.tokens[i].text, "'\"")
	i++
	if i < end && p.tokens[i].text == "?" {
		i++
	}
	if i < end && p.tokens[i].text == "<" {
		i = p.skipBalanced(i, end, "<", ">")
	}
	if i >= end || p.tokens[i].text != "(" {
		return start + 1
	}
	close := p.matching(i, end, "(", ")")
	if close < 0 {
		return start + 1
	}
	params := p.parameters(i+1, close)
	j := close + 1
	returnType := ""
	if j < end && p.tokens[j].text == ":" {
		typeStart := j + 1
		j = p.untilBodyOrTerminator(typeStart, end)
		returnType = p.render(typeStart, j)
	}
	bodyStart := j
	for bodyStart < end && p.tokens[bodyStart].text != "{" && p.tokens[bodyStart].text != ";" {
		bodyStart++
	}
	bodyEnd := bodyStart
	if bodyStart < end && p.tokens[bodyStart].text == "{" {
		bodyEnd = p.matching(bodyStart, end, "{", "}")
		if bodyEnd < 0 {
			bodyEnd = end - 1
		}
	}
	kind := "method"
	if name == "constructor" {
		kind = "constructor"
	}
	qualified := parentQualified + "." + name
	signature := p.render(start, max(close+1, j))
	id := rkcmodel.StableID("node", kind, qualified, signature)
	ev := p.extractor.evidence(p.file, "declared", "typescript.syntax."+kind, p.tokens[start], p.tokens[max(start, max(bodyEnd, close))], qualified, 1)
	p.extractor.addNode(rkcmodel.Node{ID: id, LogicalID: rkcmodel.StableID("logical", p.file.Language, kind, qualified), Kind: kind, Name: name, QualifiedName: qualified, Signature: signature, Language: p.file.Language, Visibility: visibility, PublicSurface: visibility == "public", ArtifactID: p.file.ArtifactID, Source: sourceRange(p.file, p.tokens[start], p.tokens[max(start, max(bodyEnd, close))]), EvidenceIDs: []string{ev}, Attributes: map[string]any{"arguments": params, "return_type": returnType, "async": async, "static": static}})
	p.extractor.addEdge("declares", parentID, id, "declared", []string{ev}, nil)
	if bodyStart < end && p.tokens[bodyStart].text == "{" {
		p.parseCalls(id, bodyStart+1, bodyEnd)
	}
	if bodyEnd > bodyStart {
		return bodyEnd + 1
	}
	return p.statementEnd(close+1, end)
}

func (p *parser) parseCalls(ownerID string, start, end int) {
	for i := start; i < end; i++ {
		if p.tokens[i].kind != tokenIdentifier || isCallKeyword(p.tokens[i].text) {
			continue
		}
		chainEnd := i + 1
		for chainEnd+1 < end && p.tokens[chainEnd].text == "." && p.tokens[chainEnd+1].kind == tokenIdentifier {
			chainEnd += 2
		}
		if chainEnd >= end || p.tokens[chainEnd].text != "(" {
			continue
		}
		spelling := p.renderCompact(i, chainEnd)
		close := p.matching(chainEnd, end, "(", ")")
		if close < 0 {
			continue
		}
		ev := p.extractor.evidence(p.file, "syntax_inferred", "typescript.syntax.call", p.tokens[i], p.tokens[close], spelling, 0.7)
		target := p.extractor.placeholder(p.file.Language, "call", spelling)
		p.extractor.addEdge("calls", ownerID, target, "unresolved", []string{ev}, map[string]any{"spelling": spelling})
		if method, route, ok := routeCall(spelling, p.tokens, chainEnd+1, close); ok {
			endpointQualified := strings.ToUpper(method) + " " + route
			endpointID := rkcmodel.StableID("node", "api_endpoint", p.moduleQualified, endpointQualified)
			p.extractor.addNode(rkcmodel.Node{ID: endpointID, LogicalID: rkcmodel.StableID("logical", "http", endpointQualified), Kind: "api_endpoint", Name: endpointQualified, QualifiedName: p.moduleQualified + " " + endpointQualified, Signature: endpointQualified, Language: p.file.Language, Visibility: "public", PublicSurface: true, ArtifactID: p.file.ArtifactID, Source: sourceRange(p.file, p.tokens[i], p.tokens[close]), EvidenceIDs: []string{ev}, Attributes: map[string]any{"method": strings.ToUpper(method), "path": route, "framework_inference": "javascript-router-call"}})
			p.extractor.addEdge("exposes", p.moduleID, endpointID, "syntax_inferred", []string{ev}, nil)
			p.extractor.addEdge("handles", endpointID, ownerID, "syntax_inferred", []string{ev}, nil)
		}
		i = chainEnd
	}
}

func (p *parser) importTarget(target string) (string, string) {
	if strings.HasPrefix(target, ".") || strings.HasPrefix(target, "/") {
		qualified := p.moduleQualified + "->" + target
		id := rkcmodel.StableID("node", "unresolved_symbol", p.file.Language, "module", qualified)
		p.extractor.addNode(rkcmodel.Node{ID: id, Kind: "unresolved_symbol", Name: target, QualifiedName: qualified, Language: p.file.Language, Visibility: "repository", Attributes: map[string]any{"placeholder": true, "namespace": "module", "import_path": target}})
		return id, "unresolved"
	}
	id := rkcmodel.StableID("node", "external_dependency", "npm", target)
	p.extractor.addNode(rkcmodel.Node{ID: id, LogicalID: id, Kind: "external_dependency", Name: dependencyName(target), QualifiedName: "npm:" + target, Language: "npm", Visibility: "external"})
	return id, "declared"
}

func (p *parser) unresolvedEdge(kind, from, spelling, evidence string) {
	if strings.TrimSpace(spelling) == "" {
		return
	}
	target := p.extractor.placeholder(p.file.Language, kind, spelling)
	p.extractor.addEdge(kind, from, target, "unresolved", []string{evidence}, map[string]any{"spelling": spelling})
}

func (p *parser) parameters(start, end int) []any {
	var out []any
	segment := start
	depth := 0
	for i := start; i <= end; i++ {
		atEnd := i == end
		if !atEnd {
			switch p.tokens[i].text {
			case "(", "[", "{", "<":
				depth++
			case ")", "]", "}", ">":
				if depth > 0 {
					depth--
				}
			case ">>":
				depth = max(0, depth-2)
			}
		}
		if atEnd || (depth == 0 && p.tokens[i].text == ",") {
			if segment < i {
				raw := p.render(segment, i)
				name := ""
				optional := false
				rest := false
				typ := ""
				defaultValue := ""
				for j := segment; j < i; j++ {
					if p.tokens[j].text == "..." {
						rest = true
						continue
					}
					if name == "" && p.tokens[j].kind == tokenIdentifier {
						name = p.tokens[j].text
					}
					if p.tokens[j].text == "?" {
						optional = true
					}
					if p.tokens[j].text == ":" {
						typeEnd := i
						for k := j + 1; k < i; k++ {
							if p.tokens[k].text == "=" {
								typeEnd = k
								break
							}
						}
						typ = p.render(j+1, typeEnd)
					}
					if p.tokens[j].text == "=" {
						defaultValue = p.render(j+1, i)
						optional = true
						break
					}
				}
				out = append(out, map[string]any{"name": name, "type": typ, "required": !optional, "optional": optional, "rest": rest, "default": defaultValue, "source": raw})
			}
			segment = i + 1
		}
	}
	return out
}
func (p *parser) matching(start, end int, open, close string) int {
	if start >= end || p.tokens[start].text != open {
		return -1
	}
	depth := 0
	for i := start; i < end; i++ {
		if p.tokens[i].text == open {
			depth++
		}
		if open == "<" && close == ">" && p.tokens[i].text == ">>" {
			depth -= 2
			if depth <= 0 {
				return i
			}
		}
		if p.tokens[i].text == close {
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}
func (p *parser) skipBalanced(start, end int, open, close string) int {
	if finish := p.matching(start, end, open, close); finish >= 0 {
		return finish + 1
	}
	return start
}
func (p *parser) statementEnd(start, end int) int {
	depth := 0
	startLine := 0
	if start < end {
		startLine = p.tokens[start].line
	}
	for i := start; i < end; i++ {
		switch p.tokens[i].text {
		case "(", "[", "{":
			depth++
		case ")", "]", "}":
			if depth > 0 {
				depth--
			}
		case ";":
			if depth == 0 {
				return i + 1
			}
		}
		if depth == 0 && i > start && p.tokens[i].line > startLine && startsDeclaration(p.tokens[i].text) {
			return i
		}
	}
	return end
}
func (p *parser) untilBodyOrTerminator(start, end int) int {
	depth := 0
	for i := start; i < end; i++ {
		switch p.tokens[i].text {
		case "(", "[", "<":
			depth++
		case ")", "]", ">":
			if depth > 0 {
				depth--
			}
		case ">>":
			depth = max(0, depth-2)
		case "{", ";", "=>":
			if depth == 0 {
				return i
			}
		}
	}
	return end
}
func (p *parser) render(start, end int) string {
	if start < 0 || start >= len(p.tokens) || end <= start {
		return ""
	}
	if end > len(p.tokens) {
		end = len(p.tokens)
	}
	return strings.TrimSpace(string(p.data[p.tokens[start].start:p.tokens[end-1].end]))
}
func (p *parser) renderCompact(start, end int) string {
	var b strings.Builder
	for i := start; i < end; i++ {
		b.WriteString(p.tokens[i].text)
	}
	return b.String()
}
func (p *parser) readTypeName(start, end int) string {
	finish := start
	for finish < end && (p.tokens[finish].kind == tokenIdentifier || p.tokens[finish].text == "." || p.tokens[finish].text == "<" || p.tokens[finish].text == ">" || p.tokens[finish].text == ">>" || p.tokens[finish].text == ",") {
		if p.tokens[finish].text == "," {
			break
		}
		finish++
	}
	return p.renderCompact(start, finish)
}
func (p *parser) readTypeList(start, end int) ([]string, int) {
	var out []string
	segment := start
	depth := 0
	i := start
	for ; i < end; i++ {
		switch p.tokens[i].text {
		case "<":
			depth++
		case ">":
			if depth > 0 {
				depth--
			}
		case ">>":
			depth = max(0, depth-2)
		case ",":
			if depth == 0 {
				out = append(out, p.renderCompact(segment, i))
				segment = i + 1
			}
		case "{":
			if depth == 0 {
				if segment < i {
					out = append(out, p.renderCompact(segment, i))
				}
				return cleanStrings(out), i
			}
		}
	}
	if segment < end {
		out = append(out, p.renderCompact(segment, end))
	}
	return cleanStrings(out), i
}

func lex(data []byte, file pluginapi.FileRef) ([]token, []rkcmodel.Diagnostic) {
	var tokens []token
	var diagnostics []rkcmodel.Diagnostic
	i, line, column := 0, 1, 0
	for i < len(data) {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			diagnostics = append(diagnostics, rkcmodel.Diagnostic{ID: rkcmodel.StableID("diagnostic", PluginID, file.Path, fmt.Sprint(i)), Severity: "warning", Code: "RKC-TS-1002", Message: "invalid UTF-8 byte in source", Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartByte: int64(i), EndByte: int64(i + 1), StartLine: line, StartColumn: column}, Stage: "syntax_parse", Plugin: PluginID + "@" + PluginVersion})
			i++
			column++
			continue
		}
		if r == '\n' {
			i += size
			line++
			column = 0
			continue
		}
		if unicode.IsSpace(r) {
			i += size
			column++
			continue
		}
		if r == '/' && i+1 < len(data) && data[i+1] == '/' {
			i += 2
			column += 2
			for i < len(data) && data[i] != '\n' {
				i++
				column++
			}
			continue
		}
		if r == '/' && i+1 < len(data) && data[i+1] == '*' {
			i += 2
			column += 2
			closed := false
			for i < len(data) {
				if i+1 < len(data) && data[i] == '*' && data[i+1] == '/' {
					i += 2
					column += 2
					closed = true
					break
				}
				if data[i] == '\n' {
					i++
					line++
					column = 0
				} else {
					i++
					column++
				}
			}
			if !closed {
				diagnostics = append(diagnostics, rkcmodel.Diagnostic{ID: rkcmodel.StableID("diagnostic", PluginID, file.Path, "unterminated-comment"), Severity: "warning", Code: "RKC-TS-1003", Message: "unterminated block comment", Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: line, StartColumn: column}, Stage: "syntax_parse", Plugin: PluginID + "@" + PluginVersion})
			}
			continue
		}
		start, startLine, startColumn := i, line, column
		if r == '\'' || r == '"' || r == '`' {
			quote := byte(r)
			i += size
			column++
			escaped := false
			for i < len(data) {
				c := data[i]
				if c == '\n' && quote != '`' {
					break
				}
				if c == '\n' {
					i++
					line++
					column = 0
					escaped = false
					continue
				}
				i++
				column++
				if escaped {
					escaped = false
					continue
				}
				if c == '\\' {
					escaped = true
					continue
				}
				if c == quote {
					break
				}
			}
			tokens = append(tokens, token{kind: tokenString, text: string(data[start:i]), start: start, end: i, line: startLine, column: startColumn, endLine: line, endColumn: column})
			continue
		}
		if isIdentifierStart(r) {
			i += size
			column++
			for i < len(data) {
				rr, ss := utf8.DecodeRune(data[i:])
				if !isIdentifierPart(rr) {
					break
				}
				i += ss
				column++
			}
			tokens = append(tokens, token{kind: tokenIdentifier, text: string(data[start:i]), start: start, end: i, line: startLine, column: startColumn, endLine: line, endColumn: column})
			continue
		}
		if unicode.IsDigit(r) {
			i += size
			column++
			for i < len(data) {
				rr, ss := utf8.DecodeRune(data[i:])
				if !(unicode.IsDigit(rr) || unicode.IsLetter(rr) || rr == '.' || rr == '_') {
					break
				}
				i += ss
				column++
			}
			tokens = append(tokens, token{kind: tokenNumber, text: string(data[start:i]), start: start, end: i, line: startLine, column: startColumn, endLine: line, endColumn: column})
			continue
		}
		punct := string(r)
		if i+3 <= len(data) && string(data[i:i+3]) == "..." {
			punct = "..."
			i += 3
			column += 3
		} else if i+2 <= len(data) {
			two := string(data[i : i+2])
			if isTwoCharPunctuation(two) {
				punct = two
				i += 2
				column += 2
			} else {
				i += size
				column++
			}
		} else {
			i += size
			column++
		}
		tokens = append(tokens, token{kind: tokenPunctuation, text: punct, start: start, end: i, line: startLine, column: startColumn, endLine: line, endColumn: column})
	}
	return tokens, diagnostics
}

func (e *extractor) evidence(file pluginapi.FileRef, kind, method string, start, end token, detail string, confidence float64) string {
	id := rkcmodel.StableID("evidence", PluginID, method, file.Path, fmt.Sprint(start.start), fmt.Sprint(end.end), detail)
	e.fragment.Evidence = append(e.fragment.Evidence, rkcmodel.Evidence{ID: id, Kind: kind, Method: method, Confidence: confidence, Source: sourceRange(file, start, end), Tool: PluginID, ToolVersion: PluginVersion, InputDigest: file.SHA256, Detail: detail})
	return id
}
func (e *extractor) addNode(node rkcmodel.Node) {
	if _, ok := e.seenNodes[node.ID]; ok {
		return
	}
	e.seenNodes[node.ID] = struct{}{}
	e.fragment.Nodes = append(e.fragment.Nodes, node)
}
func (e *extractor) addEdge(kind, from, to, resolution string, evidence []string, attributes map[string]any) {
	id := rkcmodel.StableID("edge", kind, from, to, strings.Join(evidence, ","))
	if _, ok := e.seenEdges[id]; ok {
		return
	}
	e.seenEdges[id] = struct{}{}
	confidence := 1.0
	if resolution == "unresolved" {
		confidence = .7
	} else if resolution == "syntax_inferred" {
		confidence = .75
	}
	e.fragment.Edges = append(e.fragment.Edges, rkcmodel.Edge{ID: id, Kind: kind, From: from, To: to, Resolution: resolution, Confidence: confidence, Producer: PluginID, EvidenceIDs: append([]string(nil), evidence...), Attributes: attributes})
}
func (e *extractor) placeholder(language, namespace, name string) string {
	id := rkcmodel.StableID("node", "unresolved_symbol", language, namespace, name)
	e.addNode(rkcmodel.Node{ID: id, Kind: "unresolved_symbol", Name: name, QualifiedName: name, Language: language, Visibility: "unknown", Attributes: map[string]any{"placeholder": true, "namespace": namespace}})
	return id
}
func (e *extractor) error(file pluginapi.FileRef, code, message string, line, column int) {
	e.fragment.Diagnostics = append(e.fragment.Diagnostics, rkcmodel.Diagnostic{ID: rkcmodel.StableID("diagnostic", PluginID, file.Path, code, message), Severity: "error", Code: code, Message: message, Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: line, StartColumn: column}, Stage: "syntax_parse", Plugin: PluginID + "@" + PluginVersion})
}

func sourceRange(file pluginapi.FileRef, start, end token) *rkcmodel.SourceRange {
	return &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartByte: int64(start.start), EndByte: int64(end.end), StartLine: start.line, StartColumn: start.column, EndLine: end.endLine, EndColumn: end.endColumn}
}
func moduleQualifiedName(packageName, path string) string {
	path = strings.TrimSuffix(filepath.ToSlash(path), filepath.Ext(path))
	path = strings.TrimPrefix(path, "./")
	if packageName == "" {
		return strings.ReplaceAll(path, "/", ".")
	}
	if path == "index" || path == "src/index" {
		return packageName
	}
	return packageName + "/" + path
}
func readPackageName(root string) string {
	data, err := sourcepath.ReadFile(root, "package.json")
	if err != nil {
		return ""
	}
	text := string(data)
	needle := "\"name\""
	index := strings.Index(text, needle)
	if index < 0 {
		return ""
	}
	rest := text[index+len(needle):]
	colon := strings.Index(rest, ":")
	if colon < 0 {
		return ""
	}
	rest = strings.TrimSpace(rest[colon+1:])
	if len(rest) < 2 || (rest[0] != '\'' && rest[0] != '"') {
		return ""
	}
	quote := rest[0]
	finish := strings.IndexByte(rest[1:], quote)
	if finish < 0 {
		return ""
	}
	return rest[1 : finish+1]
}
func detectModuleSystem(tokens []token) string {
	for _, t := range tokens {
		if t.text == "import" || t.text == "export" {
			return "esm"
		}
		if t.text == "require" || t.text == "module" {
			return "commonjs"
		}
	}
	return "script"
}
func dependencyName(value string) string {
	value = strings.TrimSuffix(value, "/")
	if strings.HasPrefix(value, ".") || strings.HasPrefix(value, "/") {
		base := filepath.Base(value)
		if base == "." || base == "/" || base == "" {
			return value
		}
		return base
	}
	if strings.HasPrefix(value, "@") {
		parts := strings.Split(value, "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
	}
	parts := strings.Split(value, "/")
	return parts[0]
}
func routeCall(spelling string, tokens []token, start, end int) (string, string, bool) {
	parts := strings.Split(spelling, ".")
	if len(parts) < 2 {
		return "", "", false
	}
	receiver, method := parts[0], strings.ToLower(parts[len(parts)-1])
	if receiver != "app" && receiver != "router" && receiver != "server" && receiver != "fastify" {
		return "", "", false
	}
	switch method {
	case "get", "post", "put", "patch", "delete", "options", "head":
	default:
		return "", "", false
	}
	for i := start; i < end; i++ {
		if tokens[i].kind == tokenString {
			route := unquoteToken(tokens[i].text)
			if strings.HasPrefix(route, "/") {
				return method, route, true
			}
			return "", "", false
		}
	}
	return "", "", false
}
func unquoteToken(value string) string {
	if len(value) >= 2 && (value[0] == '\'' || value[0] == '"' || value[0] == '`') {
		return value[1 : len(value)-1]
	}
	return value
}
func qualify(module, parent, name string) string {
	if parent != "" {
		return parent + "." + name
	}
	if module != "" {
		return module + "." + name
	}
	return name
}
func cleanStrings(values []string) []string {
	out := values[:0]
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			out = append(out, value)
		}
	}
	return out
}
func hasToken(tokens []token, value string) bool {
	for _, t := range tokens {
		if t.text == value {
			return true
		}
	}
	return false
}
func startsDeclaration(value string) bool {
	switch value {
	case "export", "import", "function", "class", "interface", "type", "enum", "const", "let", "var":
		return true
	}
	return false
}
func isModifier(value string) bool {
	switch value {
	case "async", "abstract", "declare", "public", "private", "protected", "readonly", "static", "override", "get", "set":
		return true
	}
	return false
}
func isCallKeyword(value string) bool {
	switch value {
	case "if", "for", "while", "switch", "catch", "function", "return", "new", "typeof", "delete", "void", "await", "super", "this":
		return true
	}
	return false
}
func isIdentifierStart(r rune) bool { return r == '_' || r == '$' || unicode.IsLetter(r) }
func isIdentifierPart(r rune) bool  { return isIdentifierStart(r) || unicode.IsDigit(r) }
func isTwoCharPunctuation(value string) bool {
	switch value {
	case "=>", "?.", "??", "==", "!=", "<=", ">=", "++", "--", "&&", "||", "**", "+=", "-=", "*=", "/=", "%=", "<<", ">>":
		return true
	}
	return false
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
