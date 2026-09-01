package flow

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestResolveTargetRequiresExactSpanForAmbiguousSpelling(t *testing.T) {
	const source = `package fixture

func caller() {
	target(); target()
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var declaration *ast.FuncDecl
	var calls []*ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.FuncDecl:
			declaration = value
		case *ast.CallExpr:
			calls = append(calls, value)
		}
		return true
	})
	if declaration == nil || len(calls) != 2 {
		t.Fatalf("fixture parse = declaration:%v calls:%d", declaration != nil, len(calls))
	}

	const callerID = "caller"
	const targetA = "target-a"
	const targetB = "target-b"
	graph := &callGraph{
		byFunction: map[string][]callSite{
			callerID: {
				{CallerID: callerID, TargetID: targetA, Spelling: "target", Span: exactTestCallSpan(fset, calls[0])},
				// An exact but unresolved site must never participate in either
				// the exact-match or residual-candidate pass.
				{CallerID: callerID, TargetID: "missing", Spelling: "target", Span: exactTestCallSpan(fset, calls[0])},
				{CallerID: callerID, TargetID: targetB, Spelling: "target", Span: exactTestCallSpan(fset, calls[1])},
			},
		},
		functionByID: map[string]rkcmodel.Node{
			targetA: {ID: targetA, Kind: "function", QualifiedName: "fixture.targetA"},
			targetB: {ID: targetB, Kind: "function", QualifiedName: "fixture.targetB"},
		},
	}
	analyzer := newGoAnalyzer(context.Background(), Options{}, graph, defaultAnalysisLimits)
	builder := &valueFlowBuilder{
		analyzer:    analyzer,
		function:    rkcmodel.Node{ID: callerID, Kind: "function", QualifiedName: "fixture.caller"},
		fset:        fset,
		declaration: declaration,
	}
	if target, resolved := builder.resolveTarget(calls[0], "target"); !resolved || target != targetA {
		t.Fatalf("first exact call resolved to %q, %v; want %q", target, resolved, targetA)
	}
	if target, resolved := builder.resolveTarget(calls[1], "target"); !resolved || target != targetB {
		t.Fatalf("second exact call resolved to %q, %v; want %q", target, resolved, targetB)
	}
	delete(graph.functionByID, targetB)
	if target, resolved := builder.resolveTarget(calls[1], "target"); resolved || target != "" {
		t.Fatalf("unresolved exact site borrowed another call target %q, %v", target, resolved)
	}
	graph.functionByID[targetB] = rkcmodel.Node{ID: targetB, Kind: "function", QualifiedName: "fixture.targetB"}

	line := fset.PositionFor(calls[0].Pos(), false).Line
	graph.byFunction[callerID] = []callSite{
		{CallerID: callerID, TargetID: targetA, Spelling: "target", Span: &rkcmodel.SourceRange{Path: "fixture.go", StartLine: line, EndLine: line}},
		{CallerID: callerID, TargetID: targetB, Spelling: "target", Span: &rkcmodel.SourceRange{Path: "fixture.go", StartLine: line, EndLine: line}},
	}
	if target, resolved := builder.resolveTarget(calls[0], "target"); resolved || target != "" {
		t.Fatalf("line-only ambiguous call resolved to %q, %v", target, resolved)
	}
	requireFlowDiagnostic(t, analyzer.fragment, "RKC-FLOW-2025")
}

func TestExactCallSpanSupportsByteAndColumnAuthority(t *testing.T) {
	start := token.Position{Offset: 42, Line: 7, Column: 4}
	end := token.Position{Offset: 51, Line: 7, Column: 13}
	if !exactCallSpan(&rkcmodel.SourceRange{StartByte: 42, EndByte: 51}, start, end) {
		t.Fatal("exact byte span did not match")
	}
	if exactCallSpan(&rkcmodel.SourceRange{StartByte: 43, EndByte: 51}, start, end) {
		t.Fatal("wrong byte span matched")
	}
	if !exactCallSpan(&rkcmodel.SourceRange{StartLine: 7, StartColumn: 3, EndLine: 7, EndColumn: 12}, start, end) {
		t.Fatal("exact column span did not match")
	}
	if exactCallSpan(&rkcmodel.SourceRange{StartLine: 7, EndLine: 7}, start, end) {
		t.Fatal("line-only span was treated as exact")
	}
	if exactCallSpan(nil, start, end) {
		t.Fatal("nil span matched")
	}
}

func TestCallSpanPreservesDirectColumnCoordinates(t *testing.T) {
	edge := rkcmodel.Edge{Attributes: map[string]any{"span": map[string]any{
		"path": "fixture.go", "start_line": 3, "start_column": 5,
		"end_line": 3, "end_column": 14, "start_byte": 20, "end_byte": 29,
	}}}
	span := callSpan(edge, nil)
	if span == nil || span.StartColumn != 5 || span.EndColumn != 14 || span.StartByte != 20 || span.EndByte != 29 {
		t.Fatalf("direct call span lost exact coordinates: %+v", span)
	}
}

func TestFlowIntegerAttributeNormalizesSupportedNumbers(t *testing.T) {
	cases := []struct {
		name  string
		value any
		want  int
	}{
		{"int", int(7), 7},
		{"int32", int32(8), 8},
		{"int64", int64(9), 9},
		{"float32", float32(10.9), 10},
		{"float64", float64(11.9), 11},
		{"unsupported", "12", 0},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := flowIntegerAttribute(test.value); got != test.want {
				t.Fatalf("flowIntegerAttribute(%#v) = %d, want %d", test.value, got, test.want)
			}
		})
	}
}

func TestReadArtifactSourceRejectsBinaryArtifacts(t *testing.T) {
	if _, err := readArtifactSource(t.TempDir(), rkcmodel.Artifact{Path: "binary", Text: false}); err == nil {
		t.Fatal("binary artifact source was accepted")
	}
}

func exactTestCallSpan(fset *token.FileSet, call *ast.CallExpr) *rkcmodel.SourceRange {
	start := fset.PositionFor(call.Pos(), false)
	end := fset.PositionFor(call.End(), false)
	return &rkcmodel.SourceRange{
		Path: "fixture.go", StartByte: int64(start.Offset), EndByte: int64(end.Offset),
		StartLine: start.Line, StartColumn: max(0, start.Column-1),
		EndLine: end.Line, EndColumn: max(0, end.Column-1),
	}
}
