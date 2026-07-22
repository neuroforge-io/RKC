// Package goast extracts deterministic syntax facts from Go source using the
// standard library parser. It is a Tier-1/Tier-2 bridge: declarations, types,
// imports, and source positions are exact syntax facts, while call targets stay
// unresolved until go/types or another semantic adapter resolves them.
package goast

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/neuroforge-io/RKC/internal/sourcepath"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

const (
	PluginID      = "rkc.go-ast"
	PluginVersion = "0.2.0"
)

type Options struct {
	Root       string
	SnapshotID string
	Files      []pluginapi.FileRef
}

type extractor struct {
	root       string
	snapshotID string
	modulePath string
	fileset    *token.FileSet
	fragment   rkcmodel.Fragment
	packageIDs map[string]string
	seenNodes  map[string]struct{}
}

func Extract(options Options) (rkcmodel.Fragment, error) {
	root, err := filepath.Abs(options.Root)
	if err != nil {
		return rkcmodel.Fragment{}, err
	}
	extractor := &extractor{
		root: root, snapshotID: options.SnapshotID, modulePath: readModulePath(root), fileset: token.NewFileSet(),
		packageIDs: map[string]string{}, seenNodes: map[string]struct{}{},
	}
	files := append([]pluginapi.FileRef(nil), options.Files...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, file := range files {
		if file.Language != "go" {
			continue
		}
		extractor.extractFile(file)
	}
	rkcmodel.SortBundle(&rkcmodel.Bundle{
		Artifacts:   extractor.fragment.Artifacts,
		Nodes:       extractor.fragment.Nodes,
		Edges:       extractor.fragment.Edges,
		Evidence:    extractor.fragment.Evidence,
		Diagnostics: extractor.fragment.Diagnostics,
	})
	sort.Slice(extractor.fragment.Nodes, func(i, j int) bool { return extractor.fragment.Nodes[i].ID < extractor.fragment.Nodes[j].ID })
	sort.Slice(extractor.fragment.Edges, func(i, j int) bool { return extractor.fragment.Edges[i].ID < extractor.fragment.Edges[j].ID })
	sort.Slice(extractor.fragment.Evidence, func(i, j int) bool { return extractor.fragment.Evidence[i].ID < extractor.fragment.Evidence[j].ID })
	sort.Slice(extractor.fragment.Diagnostics, func(i, j int) bool {
		return extractor.fragment.Diagnostics[i].ID < extractor.fragment.Diagnostics[j].ID
	})
	return extractor.fragment, nil
}

func (extractor *extractor) extractFile(file pluginapi.FileRef) {
	input, err := sourcepath.OpenRegular(extractor.root, file.Path)
	if err != nil {
		extractor.fragment.Diagnostics = append(extractor.fragment.Diagnostics, rkcmodel.Diagnostic{
			ID: rkcmodel.StableID("diagnostic", PluginID, file.Path, err.Error()), Severity: "error", Code: "RKC-GO-1001",
			Message: err.Error(), Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path}, Stage: "syntax_parse", Plugin: PluginID + "@" + PluginVersion,
		})
		return
	}
	parsed, err := parser.ParseFile(extractor.fileset, file.Path, input, parser.ParseComments|parser.AllErrors)
	_ = input.Close()
	if err != nil {
		extractor.fragment.Diagnostics = append(extractor.fragment.Diagnostics, rkcmodel.Diagnostic{
			ID: rkcmodel.StableID("diagnostic", PluginID, file.Path, err.Error()), Severity: "error", Code: "RKC-GO-1001",
			Message: err.Error(), Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path}, Stage: "syntax_parse", Plugin: PluginID + "@" + PluginVersion,
		})
		if parsed == nil {
			return
		}
	}
	packageQualified := extractor.packageQualified(file.Path, parsed.Name.Name)
	packageID := extractor.packageNode(packageQualified, parsed.Name.Name, file)
	fileEvidence := extractor.addEvidence("declared", "go.ast.file", file, parsed, packageQualified, 1)
	extractor.addEdge("contains", file.ArtifactID, packageID, "declared", []string{fileEvidence}, nil)

	for _, declaration := range parsed.Decls {
		switch value := declaration.(type) {
		case *ast.GenDecl:
			extractor.extractGenDecl(file, packageID, packageQualified, value)
		case *ast.FuncDecl:
			extractor.extractFuncDecl(file, packageID, packageQualified, value)
		}
	}
}

func (extractor *extractor) packageNode(qualified, name string, file pluginapi.FileRef) string {
	if id, ok := extractor.packageIDs[qualified]; ok {
		return id
	}
	id := rkcmodel.StableID("node", "package", qualified)
	extractor.packageIDs[qualified] = id
	evidenceID := rkcmodel.StableID("evidence", PluginID, "package", qualified)
	extractor.fragment.Evidence = append(extractor.fragment.Evidence, rkcmodel.Evidence{
		ID: evidenceID, Kind: "declared", Method: "go.ast.package", Confidence: 1,
		Source: &rkcmodel.SourceRange{ArtifactID: file.ArtifactID, Path: file.Path, StartLine: 1}, Tool: PluginID, ToolVersion: PluginVersion, Detail: qualified,
	})
	extractor.addNode(rkcmodel.Node{
		ID: id, LogicalID: rkcmodel.StableID("logical", "go", "package", qualified), Kind: "package", Name: name,
		QualifiedName: qualified, Language: "go", Visibility: "public", PublicSurface: true,
		EvidenceIDs: []string{evidenceID}, Attributes: map[string]any{"module_path": extractor.modulePath},
	})
	return id
}

func (extractor *extractor) extractGenDecl(file pluginapi.FileRef, packageID, packageQualified string, declaration *ast.GenDecl) {
	if declaration.Tok == token.IMPORT {
		for _, spec := range declaration.Specs {
			importSpec, ok := spec.(*ast.ImportSpec)
			if !ok {
				continue
			}
			path, _ := strconv.Unquote(importSpec.Path.Value)
			dependencyID := rkcmodel.StableID("node", "external_dependency", "go", path)
			extractor.addNode(rkcmodel.Node{ID: dependencyID, LogicalID: dependencyID, Kind: "external_dependency", Name: filepath.Base(path), QualifiedName: path, Language: "go", Visibility: "external"})
			evidenceID := extractor.addEvidence("declared", "go.ast.import", file, importSpec, path, 1)
			attributes := map[string]any{}
			if importSpec.Name != nil {
				attributes["alias"] = importSpec.Name.Name
			}
			extractor.addEdge("imports", packageID, dependencyID, "declared", []string{evidenceID}, attributes)
		}
		return
	}
	for _, spec := range declaration.Specs {
		switch value := spec.(type) {
		case *ast.TypeSpec:
			extractor.extractType(file, packageID, packageQualified, value, declaration.Doc)
		case *ast.ValueSpec:
			extractor.extractValues(file, packageID, packageQualified, value, declaration.Tok, declaration.Doc)
		}
	}
}

func (extractor *extractor) extractType(file pluginapi.FileRef, packageID, packageQualified string, spec *ast.TypeSpec, inheritedDoc *ast.CommentGroup) {
	kind := "type"
	switch spec.Type.(type) {
	case *ast.StructType:
		kind = "class"
	case *ast.InterfaceType:
		kind = "interface"
	}
	qualified := packageQualified + "." + spec.Name.Name
	id := rkcmodel.StableID("node", kind, qualified)
	evidenceID := extractor.addEvidence("declared", "go.ast.type", file, spec, qualified, 1)
	visibility := goVisibility(spec.Name.Name)
	attributes := map[string]any{
		"underlying_type": extractor.render(spec.Type),
		"alias":           spec.Assign.IsValid(),
		"docstring":       commentText(firstComment(spec.Doc, inheritedDoc)),
	}
	node := rkcmodel.Node{
		ID: id, LogicalID: rkcmodel.StableID("logical", "go", kind, qualified), Kind: kind, Name: spec.Name.Name,
		QualifiedName: qualified, Signature: "type " + spec.Name.Name + " " + extractor.render(spec.Type), Language: "go",
		Visibility: visibility, PublicSurface: visibility == "public", ArtifactID: file.ArtifactID,
		Source: extractor.source(file, spec), EvidenceIDs: []string{evidenceID}, Attributes: attributes,
	}
	extractor.addNode(node)
	extractor.addEdge("declares", packageID, id, "declared", []string{evidenceID}, nil)

	switch underlying := spec.Type.(type) {
	case *ast.StructType:
		extractor.extractFields(file, id, qualified, underlying.Fields, "field")
	case *ast.InterfaceType:
		extractor.extractFields(file, id, qualified, underlying.Methods, "method")
	}
}

func (extractor *extractor) extractFields(file pluginapi.FileRef, parentID, parentQualified string, fields *ast.FieldList, defaultKind string) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		names := field.Names
		if len(names) == 0 {
			name := extractor.render(field.Type)
			names = []*ast.Ident{{Name: name, NamePos: field.Pos()}}
		}
		for _, name := range names {
			kind := defaultKind
			if _, ok := field.Type.(*ast.FuncType); ok {
				kind = "method"
			}
			qualified := parentQualified + "." + name.Name
			id := rkcmodel.StableID("node", kind, qualified)
			evidenceID := extractor.addEvidence("declared", "go.ast.field", file, field, qualified, 1)
			visibility := goVisibility(name.Name)
			extractor.addNode(rkcmodel.Node{
				ID: id, LogicalID: rkcmodel.StableID("logical", "go", kind, qualified), Kind: kind, Name: name.Name,
				QualifiedName: qualified, Signature: name.Name + " " + extractor.render(field.Type), Language: "go",
				Visibility: visibility, PublicSurface: visibility == "public", ArtifactID: file.ArtifactID,
				Source: extractor.source(file, field), EvidenceIDs: []string{evidenceID},
				Attributes: map[string]any{"type": extractor.render(field.Type), "tag": fieldTag(field), "docstring": commentText(field.Doc)},
			})
			extractor.addEdge("declares", parentID, id, "declared", []string{evidenceID}, nil)
		}
	}
}

func (extractor *extractor) extractValues(file pluginapi.FileRef, packageID, packageQualified string, spec *ast.ValueSpec, tokenKind token.Token, inheritedDoc *ast.CommentGroup) {
	kind := "variable"
	if tokenKind == token.CONST {
		kind = "constant"
	}
	for index, name := range spec.Names {
		qualified := packageQualified + "." + name.Name
		id := rkcmodel.StableID("node", kind, qualified)
		evidenceID := extractor.addEvidence("declared", "go.ast.value", file, spec, qualified, 1)
		value := ""
		if index < len(spec.Values) {
			value = extractor.render(spec.Values[index])
		}
		visibility := goVisibility(name.Name)
		extractor.addNode(rkcmodel.Node{
			ID: id, LogicalID: rkcmodel.StableID("logical", "go", kind, qualified), Kind: kind, Name: name.Name,
			QualifiedName: qualified, Signature: strings.TrimSpace(name.Name + " " + extractor.render(spec.Type)), Language: "go",
			Visibility: visibility, PublicSurface: visibility == "public", ArtifactID: file.ArtifactID,
			Source: extractor.source(file, spec), EvidenceIDs: []string{evidenceID},
			Attributes: map[string]any{"type": extractor.render(spec.Type), "value": value, "docstring": commentText(firstComment(spec.Doc, inheritedDoc))},
		})
		extractor.addEdge("declares", packageID, id, "declared", []string{evidenceID}, nil)
	}
}

func (extractor *extractor) extractFuncDecl(file pluginapi.FileRef, packageID, packageQualified string, declaration *ast.FuncDecl) {
	kind := "function"
	qualified := packageQualified + "." + declaration.Name.Name
	receiver := ""
	if declaration.Recv != nil && len(declaration.Recv.List) > 0 {
		kind = "method"
		receiver = normalizeReceiver(extractor.render(declaration.Recv.List[0].Type))
		qualified = packageQualified + "." + receiver + "." + declaration.Name.Name
	}
	if strings.HasSuffix(file.Path, "_test.go") && strings.HasPrefix(declaration.Name.Name, "Test") && declaration.Recv == nil {
		kind = "test"
	}
	id := rkcmodel.StableID("node", kind, qualified)
	evidenceID := extractor.addEvidence("declared", "go.ast.function", file, declaration, qualified, 1)
	arguments := extractor.arguments(declaration.Type.Params)
	returns := extractor.arguments(declaration.Type.Results)
	visibility := goVisibility(declaration.Name.Name)
	attributes := map[string]any{
		"arguments": arguments, "returns": returns, "receiver": receiver,
		"docstring": commentText(declaration.Doc), "variadic": isVariadic(declaration.Type.Params),
	}
	node := rkcmodel.Node{
		ID: id, LogicalID: rkcmodel.StableID("logical", "go", kind, qualified), Kind: kind, Name: declaration.Name.Name,
		QualifiedName: qualified, Signature: extractor.render(declaration.Type), Language: "go",
		Visibility: visibility, PublicSurface: visibility == "public", ArtifactID: file.ArtifactID,
		Source: extractor.source(file, declaration), EvidenceIDs: []string{evidenceID}, Attributes: attributes,
	}
	node.Signature = "func " + declaration.Name.Name + strings.TrimPrefix(node.Signature, "func")
	if receiver != "" {
		node.Signature = "func (" + receiver + ") " + declaration.Name.Name + strings.TrimPrefix(extractor.render(declaration.Type), "func")
	}
	extractor.addNode(node)
	extractor.addEdge("declares", packageID, id, "declared", []string{evidenceID}, nil)
	if receiver != "" {
		typeID := rkcmodel.StableID("node", "class", packageQualified+"."+receiver)
		if _, ok := extractor.seenNodes[typeID]; ok {
			extractor.addEdge("declares", typeID, id, "declared", []string{evidenceID}, nil)
		}
	}
	if declaration.Body != nil {
		ast.Inspect(declaration.Body, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			spelling := callName(call.Fun)
			if spelling == "" {
				return true
			}
			target := extractor.placeholder(spelling, "call")
			callEvidence := extractor.addEvidence("syntax_inferred", "go.ast.call", file, call, spelling, 0.7)
			extractor.addEdge("calls", id, target, "unresolved", []string{callEvidence}, map[string]any{"spelling": spelling})
			return true
		})
	}
}

func (extractor *extractor) placeholder(name, namespace string) string {
	id := rkcmodel.StableID("node", "unresolved_symbol", "go", namespace, name)
	extractor.addNode(rkcmodel.Node{ID: id, Kind: "unresolved_symbol", Name: name, QualifiedName: name, Language: "go", Visibility: "unknown", Attributes: map[string]any{"placeholder": true, "namespace": namespace}})
	return id
}

func (extractor *extractor) addNode(node rkcmodel.Node) {
	if _, exists := extractor.seenNodes[node.ID]; exists {
		return
	}
	extractor.seenNodes[node.ID] = struct{}{}
	extractor.fragment.Nodes = append(extractor.fragment.Nodes, node)
}

func (extractor *extractor) addEdge(kind, from, to, resolution string, evidenceIDs []string, attributes map[string]any) {
	extractor.fragment.Edges = append(extractor.fragment.Edges, rkcmodel.Edge{
		ID: rkcmodel.StableID("edge", kind, from, to), Kind: kind, From: from, To: to,
		Resolution: resolution, Confidence: edgeConfidence(resolution), Producer: PluginID,
		EvidenceIDs: append([]string(nil), evidenceIDs...), Attributes: attributes,
	})
}

func (extractor *extractor) addEvidence(kind, method string, file pluginapi.FileRef, node ast.Node, detail string, confidence float64) string {
	source := extractor.source(file, node)
	id := rkcmodel.StableID("evidence", PluginID, method, file.Path, strconv.Itoa(source.StartLine), strconv.Itoa(source.EndLine), detail)
	extractor.fragment.Evidence = append(extractor.fragment.Evidence, rkcmodel.Evidence{
		ID: id, Kind: kind, Method: method, Confidence: confidence, Source: source,
		Tool: PluginID, ToolVersion: PluginVersion, InputDigest: file.SHA256, Detail: detail,
	})
	return id
}

func (extractor *extractor) source(file pluginapi.FileRef, node ast.Node) *rkcmodel.SourceRange {
	start := extractor.fileset.PositionFor(node.Pos(), false)
	end := extractor.fileset.PositionFor(node.End(), false)
	return &rkcmodel.SourceRange{
		ArtifactID: file.ArtifactID, Path: file.Path, StartByte: int64(start.Offset), EndByte: int64(end.Offset),
		StartLine: start.Line, StartColumn: max(0, start.Column-1), EndLine: end.Line, EndColumn: max(0, end.Column-1),
	}
}

func (extractor *extractor) packageQualified(path, packageName string) string {
	directory := filepath.ToSlash(filepath.Dir(path))
	if directory == "." {
		directory = ""
	}
	if extractor.modulePath != "" {
		if directory == "" {
			return extractor.modulePath
		}
		return strings.TrimSuffix(extractor.modulePath, "/") + "/" + directory
	}
	if directory == "" {
		return packageName
	}
	return directory
}

func (extractor *extractor) arguments(fields *ast.FieldList) []any {
	if fields == nil {
		return nil
	}
	var output []any
	for _, field := range fields.List {
		typeName := extractor.render(field.Type)
		if len(field.Names) == 0 {
			output = append(output, map[string]any{"name": "", "kind": "unnamed", "type": typeName, "required": true})
			continue
		}
		for _, name := range field.Names {
			output = append(output, map[string]any{"name": name.Name, "kind": "positional", "type": typeName, "required": true})
		}
	}
	return output
}

func (extractor *extractor) render(node ast.Node) string {
	if node == nil {
		return ""
	}
	var buffer bytes.Buffer
	if err := printer.Fprint(&buffer, extractor.fileset, node); err != nil {
		return ""
	}
	return buffer.String()
}

func readModulePath(root string) string {
	data, err := sourcepath.ReadFile(root, "go.mod")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	return ""
}

func callName(expression ast.Expr) string {
	switch value := expression.(type) {
	case *ast.Ident:
		return value.Name
	case *ast.SelectorExpr:
		prefix := callName(value.X)
		if prefix == "" {
			return value.Sel.Name
		}
		return prefix + "." + value.Sel.Name
	case *ast.IndexExpr:
		return callName(value.X)
	case *ast.IndexListExpr:
		return callName(value.X)
	case *ast.ParenExpr:
		return callName(value.X)
	default:
		return ""
	}
}

func normalizeReceiver(value string) string {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "*")
	if index := strings.Index(value, "["); index >= 0 {
		value = value[:index]
	}
	return value
}
func goVisibility(name string) string {
	if ast.IsExported(name) {
		return "public"
	}
	return "private"
}
func commentText(group *ast.CommentGroup) string {
	if group == nil {
		return ""
	}
	return strings.TrimSpace(group.Text())
}
func firstComment(primary, fallback *ast.CommentGroup) *ast.CommentGroup {
	if primary != nil {
		return primary
	}
	return fallback
}
func fieldTag(field *ast.Field) string {
	if field.Tag == nil {
		return ""
	}
	value, err := strconv.Unquote(field.Tag.Value)
	if err != nil {
		return field.Tag.Value
	}
	return value
}
func isVariadic(fields *ast.FieldList) bool {
	if fields == nil || len(fields.List) == 0 {
		return false
	}
	_, ok := fields.List[len(fields.List)-1].Type.(*ast.Ellipsis)
	return ok
}
func edgeConfidence(resolution string) float64 {
	if resolution == "unresolved" {
		return 0.7
	}
	return 1
}
func max(left, right int) int {
	if left > right {
		return left
	}
	return right
}

var _ = fmt.Sprintf
