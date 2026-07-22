package tssyntax

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestExtractIsDeterministicFiltersInputsAndReusesProject(t *testing.T) {
	root := t.TempDir()
	writeSyntaxTestFile(t, root, "package.json", `{"name":"coverage-package"}`)
	writeSyntaxTestFile(t, root, "a.ts", "const alpha = 1;\n")
	writeSyntaxTestFile(t, root, "b.js", "const dependency = require(\"dependency\");\n")
	writeSyntaxTestFile(t, root, "empty.ts", " \n\t")
	if err := os.WriteFile(filepath.Join(root, "invalid.ts"), []byte{0xff}, 0o600); err != nil {
		t.Fatal(err)
	}
	files := []pluginapi.FileRef{
		{ArtifactID: "missing-z", Path: "z-missing.ts", Language: "typescript", SHA256: "z"},
		{ArtifactID: "ignored", Path: "missing.md", Language: "markdown", SHA256: "ignored"},
		{ArtifactID: "b", Path: "b.js", Language: "javascript", SHA256: "b"},
		{ArtifactID: "empty", Path: "empty.ts", Language: "typescript", SHA256: "empty"},
		{ArtifactID: "invalid", Path: "invalid.ts", Language: "typescript", SHA256: "invalid"},
		{ArtifactID: "a", Path: "a.ts", Language: "typescript", SHA256: "a"},
		{ArtifactID: "missing-a", Path: "c-missing.ts", Language: "typescript", SHA256: "c"},
	}

	got, err := Extract(Options{Root: root, SnapshotID: "snapshot", Files: files})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	reversed := append([]pluginapi.FileRef(nil), files...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	want, err := Extract(Options{Root: root, SnapshotID: "snapshot", Files: reversed})
	if err != nil {
		t.Fatalf("Extract(reversed) error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("extractor output depends on file input order")
	}

	projects := 0
	modules := 0
	commonJS := false
	for _, node := range got.Nodes {
		switch node.Kind {
		case "project":
			projects++
		case "module":
			modules++
			if node.Name == "b" && node.Attributes["module_system"] == "commonjs" {
				commonJS = true
			}
		}
	}
	if projects != 1 || modules != 2 || !commonJS {
		t.Fatalf("projects=%d modules=%d commonJS=%v; nodes=%#v", projects, modules, commonJS, got.Nodes)
	}
	if len(got.Diagnostics) != 3 {
		t.Fatalf("diagnostics=%#v, want two read errors and one UTF-8 warning", got.Diagnostics)
	}
	if got.Diagnostics[0].ID > got.Diagnostics[1].ID || got.Diagnostics[1].ID > got.Diagnostics[2].ID {
		t.Fatalf("diagnostics are not sorted: %#v", got.Diagnostics)
	}
}

func TestExtractWithoutPackageUsesRepositoryModuleName(t *testing.T) {
	root := t.TempDir()
	writeSyntaxTestFile(t, root, "src/value.ts", "let value = 1;\n")
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{
		ArtifactID: "value", Path: "src/value.ts", Language: "typescript", SHA256: "value",
	}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	for _, node := range fragment.Nodes {
		if node.Kind == "project" {
			t.Fatalf("unexpected project node without package.json: %#v", node)
		}
		if node.Kind == "module" {
			if node.QualifiedName != "src.value" || node.Attributes["module_system"] != "script" {
				t.Fatalf("module = %#v", node)
			}
			return
		}
	}
	t.Fatal("module node not found")
}

func TestExtractCoversRichJavaScriptAndTypeScriptSyntax(t *testing.T) {
	root := t.TempDir()
	writeSyntaxTestFile(t, root, "package.json", `{"name":"syntax-fixture"}`)
	source := `import "side-effect"
import alias, { Thing } from "external";
import type { Local } from "./local";
const local = require("./required");
let dependency = require("@scope/pkg/subpath");
var absolute = require("/absolute");

export default function () {}
export function *generate<T>(head: T, ...rest: Array<T>): Iterator<T> {
  helper.call(head);
  broken(
}
declare function declared(value?: string): void;
function hidden() {}

export default class Child extends ns.Base<T> implements First<Map<string, number>>, Second {
  constructor() {}
  private static async method?<T>(value: T = fallback): Promise<void> { service.call(value); }
  protected abstract declaration(value: string): void;
  get() { return read(); }
  set(value: string) { write(value); }
  const nested = (x: string) => { router.get("/nested", handler); };
  enum Inner { One, Two = build([1, 2]) }
  type InnerAlias = string;
  class Nested {}
}

export interface Service {
  lookup<T>(id?: string): Promise<T>;
}
type Alias = { value: string };
export enum Status { "ready" = "r", Retry = build(1, [2]), , Done }
const simple = 1;
let expression = value => value.trim();
var block = (value: string = "", nested: Map<string, Array<number>>) => {
  fastify.head("/health", handler);
};
`
	writeSyntaxTestFile(t, root, "src/index.ts", source)
	fragment, err := Extract(Options{Root: root, SnapshotID: "snapshot", Files: []pluginapi.FileRef{{
		ArtifactID: "rich", Path: "src/index.ts", Language: "typescript", SHA256: "rich",
	}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", fragment.Diagnostics)
	}

	wantedNodes := map[string]bool{
		"default": false, "generate": false, "declared": false, "Child": false,
		"method": false, "Service": false, "Alias": false, "Status": false,
		"ready": false, "Done": false, "nested": false, "HEAD /health": false,
	}
	for _, node := range fragment.Nodes {
		if _, ok := wantedNodes[node.Name]; ok {
			wantedNodes[node.Name] = true
		}
	}
	for name, found := range wantedNodes {
		if !found {
			t.Errorf("missing node %q", name)
		}
	}
	wantedEdges := map[string]bool{"imports": false, "inherits": false, "implements": false, "calls": false, "exposes": false, "handles": false}
	for _, edge := range fragment.Edges {
		if _, ok := wantedEdges[edge.Kind]; ok {
			wantedEdges[edge.Kind] = true
		}
	}
	for kind, found := range wantedEdges {
		if !found {
			t.Errorf("missing edge kind %q", kind)
		}
	}
}

func TestFunctionsInTestFilesAreClassifiedAsTests(t *testing.T) {
	root := t.TempDir()
	writeSyntaxTestFile(t, root, "fixture.spec.js", "function itWorks() {}\nfunction helper() {}\n")
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{
		ArtifactID: "test", Path: "fixture.spec.js", Language: "javascript", SHA256: "test",
	}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	kinds := map[string]string{}
	for _, node := range fragment.Nodes {
		kinds[node.Name] = node.Kind
	}
	if kinds["itWorks"] != "test" || kinds["helper"] != "function" {
		t.Fatalf("function kinds = %#v", kinds)
	}
}

func TestParserDefensiveBranches(t *testing.T) {
	tests := []struct {
		name string
		src  string
		call func(*parser) int
		want int
	}{
		{name: "empty import", src: "import", call: func(p *parser) int { return p.parseImport(0, len(p.tokens)) }, want: 1},
		{name: "missing require", src: "const value", call: func(p *parser) int { return p.parseRequire(0, len(p.tokens)) }, want: 1},
		{name: "function without name", src: "function ()", call: func(p *parser) int { return p.parseFunction(0, len(p.tokens), "", "", false, false, "function", false) }, want: 1},
		{name: "function without params", src: "function named", call: func(p *parser) int { return p.parseFunction(0, len(p.tokens), "", "", false, false, "function", false) }, want: 1},
		{name: "function unclosed params", src: "function named(", call: func(p *parser) int { return p.parseFunction(0, len(p.tokens), "", "", false, false, "function", false) }, want: 1},
		{name: "class without name", src: "class", call: func(p *parser) int { return p.parseClass(0, len(p.tokens), "", "", false, false) }, want: 1},
		{name: "class without body", src: "class Named;", call: func(p *parser) int { return p.parseClass(0, len(p.tokens), "", "", false, false) }, want: 2},
		{name: "named type without name", src: "type", call: func(p *parser) int { return p.parseNamedType(0, len(p.tokens), "", "", false, "type") }, want: 1},
		{name: "enum without name", src: "enum", call: func(p *parser) int { return p.parseEnum(0, len(p.tokens), "", "", false) }, want: 1},
		{name: "enum without body", src: "enum Named;", call: func(p *parser) int { return p.parseEnum(0, len(p.tokens), "", "", false) }, want: 1},
		{name: "variable without name", src: "const", call: func(p *parser) int { return p.parseVariable(0, len(p.tokens), "", "", false) }, want: 1},
		{name: "method only modifiers", src: "public static", call: func(p *parser) int { return p.parseMethod(0, len(p.tokens), "owner", "Owner") }, want: 1},
		{name: "method without params", src: "method", call: func(p *parser) int { return p.parseMethod(0, len(p.tokens), "owner", "Owner") }, want: 1},
		{name: "method unclosed params", src: "method(", call: func(p *parser) int { return p.parseMethod(0, len(p.tokens), "owner", "Owner") }, want: 1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			p := syntaxTestParser(t, test.src, "fixture.ts")
			if got := test.call(p); got != test.want {
				t.Fatalf("next token = %d, want %d", got, test.want)
			}
		})
	}

	for _, source := range []string{"function open() {", "class Open {", "interface Open {", "enum Open { One", "method() {"} {
		p := syntaxTestParser(t, source, "broken.ts")
		switch p.tokens[0].text {
		case "function":
			p.parseFunction(0, len(p.tokens), "", "", false, false, "function", false)
		case "class":
			p.parseClass(0, len(p.tokens), "", "", false, false)
		case "interface":
			p.parseNamedType(0, len(p.tokens), "", "", false, "interface")
		case "enum":
			p.parseEnum(0, len(p.tokens), "", "", false)
		default:
			p.parseMethod(0, len(p.tokens), "owner", "Owner")
		}
	}

	p := syntaxTestParser(t, "async", "fixture.ts")
	p.parseRange(0, len(p.tokens), "", "")
	if p.isRequireDeclaration(0, len(p.tokens)) {
		t.Fatal("short declaration was classified as require")
	}
	p = syntaxTestParser(t, "const value; require(\"late\")", "fixture.ts")
	if p.isRequireDeclaration(0, len(p.tokens)) {
		t.Fatal("require after statement terminator was classified as declaration initializer")
	}
	p.parseRequire(0, len(p.tokens))
}

func TestParserMethodRecognitionBranches(t *testing.T) {
	tests := []struct {
		source string
		want   bool
	}{
		{source: "get()", want: true},
		{source: "public static", want: false},
		{source: "constructor()", want: true},
		{source: "constructor", want: false},
		{source: "1", want: false},
		{source: "method?()", want: true},
		{source: "method<T>()", want: true},
		{source: "method<T", want: false},
		{source: "method", want: false},
	}
	for _, test := range tests {
		p := syntaxTestParser(t, test.source, "fixture.ts")
		if got := p.looksLikeMethod(0, len(p.tokens)); got != test.want {
			t.Errorf("looksLikeMethod(%q) = %v, want %v", test.source, got, test.want)
		}
	}
}

func TestParserStructuralHelpers(t *testing.T) {
	p := syntaxTestParser(t, "((value));", "fixture.ts")
	if got := p.matching(0, len(p.tokens), "(", ")"); got != 4 {
		t.Fatalf("matching nested close = %d, want 4", got)
	}
	if p.matching(len(p.tokens), len(p.tokens), "(", ")") != -1 || p.matching(2, len(p.tokens), "(", ")") != -1 {
		t.Fatal("matching accepted an invalid start")
	}
	if p.skipBalanced(0, len(p.tokens), "(", ")") != 5 || p.skipBalanced(2, len(p.tokens), "(", ")") != 2 {
		t.Fatal("skipBalanced boundary mismatch")
	}

	p = syntaxTestParser(t, "(value);", "fixture.ts")
	if got := p.statementEnd(0, len(p.tokens)); got != len(p.tokens) {
		t.Fatalf("statementEnd nested expression = %d, want %d", got, len(p.tokens))
	}
	p = syntaxTestParser(t, "value\nconst next = 1", "fixture.ts")
	if got := p.statementEnd(0, len(p.tokens)); got != 1 {
		t.Fatalf("statementEnd newline declaration = %d, want 1", got)
	}
	if got := p.statementEnd(len(p.tokens), len(p.tokens)); got != len(p.tokens) {
		t.Fatalf("statementEnd empty = %d", got)
	}

	p = syntaxTestParser(t, "Map<string, Array<number>>;", "fixture.ts")
	stop := p.untilBodyOrTerminator(0, len(p.tokens))
	if stop >= len(p.tokens) || p.tokens[stop].text != ";" {
		t.Fatalf("untilBodyOrTerminator stop = %d tokens=%#v", stop, p.tokens)
	}
	p = syntaxTestParser(t, "Plain", "fixture.ts")
	if p.untilBodyOrTerminator(0, len(p.tokens)) != len(p.tokens) {
		t.Fatal("untilBodyOrTerminator did not return end")
	}

	if p.render(-1, 1) != "" || p.render(0, 0) != "" || p.render(len(p.tokens), len(p.tokens)+1) != "" {
		t.Fatal("render accepted invalid bounds")
	}
	if got := p.render(0, len(p.tokens)+10); got != "Plain" {
		t.Fatalf("render clamped result = %q", got)
	}
	if got := p.renderCompact(0, len(p.tokens)); got != "Plain" {
		t.Fatalf("renderCompact = %q", got)
	}

	p = syntaxTestParser(t, "ns.Base<T>, Other", "fixture.ts")
	if got := p.readTypeName(0, len(p.tokens)); got != "ns.Base<T>" {
		t.Fatalf("readTypeName = %q", got)
	}
	p = syntaxTestParser(t, "First<Map<A, B>>, , Second {", "fixture.ts")
	types, next := p.readTypeList(0, len(p.tokens))
	if !reflect.DeepEqual(types, []string{"First<Map<A,B>>", "Second"}) || next >= len(p.tokens) || p.tokens[next].text != "{" {
		t.Fatalf("readTypeList = %#v next=%d", types, next)
	}
	p = syntaxTestParser(t, "Only", "fixture.ts")
	types, next = p.readTypeList(0, len(p.tokens))
	if !reflect.DeepEqual(types, []string{"Only"}) || next != len(p.tokens) {
		t.Fatalf("readTypeList end = %#v next=%d", types, next)
	}
}

func TestParserParametersAndUnresolvedEdges(t *testing.T) {
	p := syntaxTestParser(t, "(...rest: Array<string>, optional?: number, defaulted: string = \"x\", nested: Map<string, Array<number>>)", "fixture.ts")
	close := p.matching(0, len(p.tokens), "(", ")")
	parameters := p.parameters(1, close)
	if len(parameters) != 4 {
		t.Fatalf("parameters = %#v", parameters)
	}
	first := parameters[0].(map[string]any)
	second := parameters[1].(map[string]any)
	third := parameters[2].(map[string]any)
	if first["rest"] != true || first["required"] != true || second["optional"] != true || third["default"] != `"x"` || third["required"] != false {
		t.Fatalf("parameter metadata = %#v", parameters)
	}
	before := len(p.extractor.fragment.Edges)
	p.unresolvedEdge("implements", "from", "   ", "evidence")
	if len(p.extractor.fragment.Edges) != before {
		t.Fatal("blank unresolved symbol created an edge")
	}
	p.unresolvedEdge("implements", "from", "Contract", "evidence")
	if len(p.extractor.fragment.Edges) != before+1 || len(p.extractor.fragment.Nodes) != 1 {
		t.Fatalf("unresolved edge result: nodes=%#v edges=%#v", p.extractor.fragment.Nodes, p.extractor.fragment.Edges)
	}
}

func TestLexerDiagnosticsStringsNumbersAndPunctuation(t *testing.T) {
	file := pluginapi.FileRef{ArtifactID: "artifact", Path: "fixture.ts", Language: "typescript"}
	tokens, diagnostics := lex([]byte{0xff}, file)
	if len(tokens) != 0 || len(diagnostics) != 1 || diagnostics[0].Code != "RKC-TS-1002" {
		t.Fatalf("invalid UTF-8 result tokens=%#v diagnostics=%#v", tokens, diagnostics)
	}
	tokens, diagnostics = lex([]byte("/* first\nsecond */ /* unterminated"), file)
	if len(tokens) != 0 || len(diagnostics) != 1 || diagnostics[0].Code != "RKC-TS-1003" || diagnostics[0].Source.StartLine != 2 {
		t.Fatalf("block comment result tokens=%#v diagnostics=%#v", tokens, diagnostics)
	}

	source := "'it\\'s' \"quoted\" `multi\nline` 'broken\nnext 123abc._ $value Ω9 ... => ?. ?? == != <= >= ++ -- && || ** += -= *= /= %= << >> + "
	tokens, diagnostics = lex([]byte(source), file)
	if len(diagnostics) != 0 {
		t.Fatalf("unexpected lexer diagnostics: %#v", diagnostics)
	}
	wanted := map[string]bool{"`multi\nline`": false, "123abc._": false, "$value": false, "Ω9": false, "...": false, "=>": false, "+": false}
	for _, tok := range tokens {
		if _, ok := wanted[tok.text]; ok {
			wanted[tok.text] = true
		}
	}
	for spelling, found := range wanted {
		if !found {
			t.Errorf("lexer did not emit %q; tokens=%#v", spelling, tokens)
		}
	}
	last, _ := lex([]byte("+"), file)
	if len(last) != 1 || last[0].text != "+" {
		t.Fatalf("single punctuation token = %#v", last)
	}
}

func TestExtractorDeduplicationEvidenceAndConfidence(t *testing.T) {
	e := newSyntaxTestExtractor()
	e.addNode(rkcmodel.Node{ID: "node", Name: "first"})
	e.addNode(rkcmodel.Node{ID: "node", Name: "duplicate"})
	if len(e.fragment.Nodes) != 1 || e.fragment.Nodes[0].Name != "first" {
		t.Fatalf("node deduplication = %#v", e.fragment.Nodes)
	}
	e.addEdge("declares", "from", "to", "declared", []string{"one"}, nil)
	e.addEdge("declares", "from", "to", "declared", []string{"one"}, nil)
	e.addEdge("calls", "from", "unresolved", "unresolved", []string{"two"}, nil)
	e.addEdge("exposes", "from", "inferred", "syntax_inferred", []string{"three"}, nil)
	if len(e.fragment.Edges) != 3 {
		t.Fatalf("edge deduplication = %#v", e.fragment.Edges)
	}
	confidence := map[string]float64{}
	for _, edge := range e.fragment.Edges {
		confidence[edge.Resolution] = edge.Confidence
	}
	if confidence["declared"] != 1 || confidence["unresolved"] != 0.7 || confidence["syntax_inferred"] != 0.75 {
		t.Fatalf("edge confidence = %#v", confidence)
	}

	file := pluginapi.FileRef{ArtifactID: "artifact", Path: "fixture.ts", SHA256: "digest"}
	start := token{start: 2, end: 3, line: 1, column: 2, endLine: 1, endColumn: 3}
	end := token{start: 4, end: 8, line: 1, column: 4, endLine: 1, endColumn: 8}
	evidenceID := e.evidence(file, "declared", "method", start, end, "detail", 0.9)
	if evidenceID == "" || len(e.fragment.Evidence) != 1 || e.fragment.Evidence[0].InputDigest != "digest" || e.fragment.Evidence[0].Source.EndByte != 8 {
		t.Fatalf("evidence = %#v", e.fragment.Evidence)
	}
	first := e.placeholder("typescript", "call", "target")
	second := e.placeholder("typescript", "call", "target")
	if first != second {
		t.Fatal("placeholder identity is not stable")
	}
	e.error(file, "CODE", "message", 3, 4)
	if len(e.fragment.Diagnostics) != 1 || e.fragment.Diagnostics[0].Source.StartLine != 3 {
		t.Fatalf("diagnostic = %#v", e.fragment.Diagnostics)
	}
}

func TestPackageAndNamingHelpers(t *testing.T) {
	root := t.TempDir()
	if readPackageName(root) != "" {
		t.Fatal("missing package.json produced a package name")
	}
	cases := []struct {
		content string
		want    string
	}{
		{content: `{}`, want: ""},
		{content: `{"name" "missing-colon"}`, want: ""},
		{content: `{"name": 123}`, want: ""},
		{content: `{"name":"unterminated}`, want: ""},
		{content: `{"name":'single-quoted'}`, want: "single-quoted"},
		{content: `{"name":"double-quoted"}`, want: "double-quoted"},
	}
	for _, test := range cases {
		writeSyntaxTestFile(t, root, "package.json", test.content)
		if got := readPackageName(root); got != test.want {
			t.Errorf("readPackageName(%q) = %q, want %q", test.content, got, test.want)
		}
	}

	qualifiedCases := []struct{ packageName, path, want string }{
		{"", "./src/item.ts", "src.item"},
		{"pkg", "index.ts", "pkg"},
		{"pkg", "src/index.ts", "pkg"},
		{"pkg", "src/item.ts", "pkg/src/item"},
	}
	for _, test := range qualifiedCases {
		if got := moduleQualifiedName(test.packageName, test.path); got != test.want {
			t.Errorf("moduleQualifiedName(%q, %q) = %q, want %q", test.packageName, test.path, got, test.want)
		}
	}
	if detectModuleSystem([]token{{text: "export"}}) != "esm" || detectModuleSystem([]token{{text: "module"}}) != "commonjs" || detectModuleSystem(nil) != "script" {
		t.Fatal("module-system detection mismatch")
	}

	dependencyCases := map[string]string{
		"./": ".", "../local/": "local", "/absolute/path": "path", "@scope/package/subpath": "@scope/package", "@scope": "@scope", "plain/subpath": "plain",
	}
	for value, want := range dependencyCases {
		if got := dependencyName(value); got != want {
			t.Errorf("dependencyName(%q) = %q, want %q", value, got, want)
		}
	}
}

func TestRouteAndSmallHelpers(t *testing.T) {
	file := pluginapi.FileRef{Path: "fixture.ts"}
	routeTokens, _ := lex([]byte(`"/items", handler`), file)
	for _, spelling := range []string{"app.get", "router.post", "server.put", "fastify.patch", "app.delete", "app.options", "app.head"} {
		method, route, ok := routeCall(spelling, routeTokens, 0, len(routeTokens))
		if !ok || route != "/items" || method != strings.ToLower(strings.Split(spelling, ".")[1]) {
			t.Errorf("routeCall(%q) = %q %q %v", spelling, method, route, ok)
		}
	}
	invalidRouteTokens, _ := lex([]byte(`"items"`), file)
	for _, test := range []struct {
		spelling string
		tokens   []token
	}{
		{spelling: "get", tokens: routeTokens},
		{spelling: "other.get", tokens: routeTokens},
		{spelling: "app.trace", tokens: routeTokens},
		{spelling: "app.get", tokens: invalidRouteTokens},
		{spelling: "app.get", tokens: nil},
	} {
		if _, _, ok := routeCall(test.spelling, test.tokens, 0, len(test.tokens)); ok {
			t.Errorf("routeCall accepted invalid input %#v", test)
		}
	}

	for value, want := range map[string]string{`'one'`: "one", `"two"`: "two", "`three`": "three", "plain": "plain", "": ""} {
		if got := unquoteToken(value); got != want {
			t.Errorf("unquoteToken(%q) = %q, want %q", value, got, want)
		}
	}
	if qualify("module", "parent", "name") != "parent.name" || qualify("module", "", "name") != "module.name" || qualify("", "", "name") != "name" {
		t.Fatal("qualify precedence mismatch")
	}
	values := []string{" first ", "", "   ", "second"}
	if got := cleanStrings(values); !reflect.DeepEqual(got, []string{"first", "second"}) {
		t.Fatalf("cleanStrings = %#v", got)
	}
	if !hasToken([]token{{text: "wanted"}}, "wanted") || hasToken(nil, "wanted") {
		t.Fatal("hasToken boundary mismatch")
	}
	for _, value := range []string{"export", "import", "function", "class", "interface", "type", "enum", "const", "let", "var"} {
		if !startsDeclaration(value) {
			t.Errorf("startsDeclaration(%q) = false", value)
		}
	}
	if startsDeclaration("value") {
		t.Fatal("ordinary identifier classified as declaration")
	}
	for _, value := range []string{"async", "abstract", "declare", "public", "private", "protected", "readonly", "static", "override", "get", "set"} {
		if !isModifier(value) {
			t.Errorf("isModifier(%q) = false", value)
		}
	}
	if isModifier("value") {
		t.Fatal("ordinary identifier classified as modifier")
	}
	for _, value := range []string{"if", "for", "while", "switch", "catch", "function", "return", "new", "typeof", "delete", "void", "await", "super", "this"} {
		if !isCallKeyword(value) {
			t.Errorf("isCallKeyword(%q) = false", value)
		}
	}
	if isCallKeyword("call") {
		t.Fatal("ordinary identifier classified as call keyword")
	}
	if !isIdentifierStart('_') || !isIdentifierStart('$') || !isIdentifierStart('λ') || isIdentifierStart('1') || !isIdentifierPart('1') {
		t.Fatal("identifier rune classification mismatch")
	}
	for _, value := range []string{"=>", "?.", "??", "==", "!=", "<=", ">=", "++", "--", "&&", "||", "**", "+=", "-=", "*=", "/=", "%=", "<<", ">>"} {
		if !isTwoCharPunctuation(value) {
			t.Errorf("isTwoCharPunctuation(%q) = false", value)
		}
	}
	if isTwoCharPunctuation("+++") || min(1, 2) != 1 || min(2, 1) != 1 || max(1, 2) != 2 || max(2, 1) != 2 {
		t.Fatal("small helper boundary mismatch")
	}
}

func TestExtractPropagatesAbsolutePathFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows does not permit removing the current working directory")
	}
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	doomed := t.TempDir()
	if err := os.Chdir(doomed); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(doomed); err != nil {
		_ = os.Chdir(original)
		t.Fatal(err)
	}
	_, extractErr := Extract(Options{Root: "."})
	restoreErr := os.Chdir(original)
	if restoreErr != nil {
		t.Fatalf("restore working directory: %v", restoreErr)
	}
	if extractErr == nil {
		t.Fatal("Extract() succeeded when filepath.Abs could not resolve the current directory")
	}
}

func syntaxTestParser(t *testing.T, source, path string) *parser {
	t.Helper()
	file := pluginapi.FileRef{ArtifactID: "artifact", Path: path, Language: "typescript", SHA256: "digest"}
	tokens, diagnostics := lex([]byte(source), file)
	if len(diagnostics) != 0 {
		t.Fatalf("lex(%q) diagnostics = %#v", source, diagnostics)
	}
	e := newSyntaxTestExtractor()
	return &parser{extractor: e, file: file, data: []byte(source), tokens: tokens, moduleID: "module", moduleQualified: "fixture"}
}

func newSyntaxTestExtractor() *extractor {
	return &extractor{
		seenNodes: map[string]struct{}{},
		seenEdges: map[string]struct{}{},
		modules:   map[string]string{},
		projects:  map[string]string{},
	}
}

func writeSyntaxTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
