package scipindex

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestVocabularyRangeAndLanguageHelperCoverage(t *testing.T) {
	t.Parallel()
	kindCases := map[int32]string{
		7: "class", 21: "interface", 53: "trait", 11: "enum", 12: "enum_member",
		17: "function", 9: "constructor", 15: "field", 22: "property",
		8: "constant", 37: "parameter", 13: "event", 28: "message", 29: "module",
		30: "namespace", 35: "package", 16: "file", 54: "type", 61: "variable",
		26: "method", 73: "type", 0: "unresolved_symbol",
	}
	for input, want := range kindCases {
		if got := mapSymbolKind(input); got != want {
			t.Errorf("mapSymbolKind(%d) = %q, want %q", input, got, want)
		}
	}
	syntaxCases := map[int32]string{
		14: "namespace", 15: "function", 17: "function", 19: "type",
		9: "constant", 12: "variable", 0: "unresolved_symbol",
	}
	for input, want := range syntaxCases {
		if got := mapSyntaxKind(input); got != want {
			t.Errorf("mapSyntaxKind(%d) = %q, want %q", input, got, want)
		}
	}
	languageCases := map[string]string{
		"CPP": "cpp", "C++": "cpp", "CSharp": "csharp", "C#": "csharp",
		"JavaScriptReact": "javascript", "TypeScriptReact": "typescript",
		"Objective_C": "objective-c", "Objective_CPP": "objective-cpp",
		"ShellScript": "shell", "Kotlin": "kotlin",
	}
	for input, want := range languageCases {
		if got := normalizeLanguage(input, "fallback"); got != want {
			t.Errorf("normalizeLanguage(%q) = %q, want %q", input, got, want)
		}
	}
	if got := normalizeLanguage("", "Go"); got != "go" {
		t.Errorf("fallback language = %q", got)
	}
	nameCases := map[string]string{
		"local 42": "42", "scip . . . pkg/Foo#": "Foo",
		"scip . . . pkg/`odd``name`.": "odd`name",
		"###":                         "###",
	}
	for input, want := range nameCases {
		if got := symbolDisplayName(input); got != want {
			t.Errorf("symbolDisplayName(%q) = %q, want %q", input, got, want)
		}
	}
	if got := occurrenceEdgeKinds(roleImport | roleWrite | roleRead); strings.Join(got, ",") != "imports,writes,reads" {
		t.Fatalf("role edges = %v", got)
	}
	if got := occurrenceEdgeKinds(0); len(got) != 1 || got[0] != "references" {
		t.Fatalf("default role edges = %v", got)
	}
	if !rangeSmaller(
		sourcePosition{startLine: 1, endLine: 1, startCharacter: 1, endCharacter: 2},
		sourcePosition{startLine: 1, endLine: 2},
	) || !rangeSmaller(
		sourcePosition{startLine: 1, endLine: 1, startCharacter: 1, endCharacter: 2},
		sourcePosition{startLine: 1, endLine: 1, startCharacter: 1, endCharacter: 5},
	) {
		t.Fatal("rangeSmaller rejected smaller ranges")
	}
	if comparePosition(2, 0, 1, 9) != 1 {
		t.Fatal("comparePosition greater-than branch failed")
	}
	if comparePosition(1, 0, 2, 0) != -1 || comparePosition(1, 2, 1, 2) != 0 {
		t.Fatal("comparePosition ordering branches failed")
	}
}

func TestPositionMapperAllEncodingsAndBoundaries(t *testing.T) {
	t.Parallel()
	source := []byte("a🚀z\r\nβx\n")
	utf8Mapper, err := newPositionMapper(source, 1)
	if err != nil {
		t.Fatal(err)
	}
	for character, want := range map[int32]int{0: 0, 1: 1, 5: 5, 6: 6} {
		got, err := utf8Mapper.byteOffset(0, character)
		if err != nil || got != want {
			t.Errorf("UTF-8 offset %d = %d, %v; want %d", character, got, err, want)
		}
	}
	if _, err := utf8Mapper.byteOffset(0, 2); err == nil ||
		!strings.Contains(err.Error(), "code-point boundary") {
		t.Fatalf("UTF-8 split = %v", err)
	}
	utf16Mapper, _ := newPositionMapper(source, 2)
	for character, want := range map[int32]int{0: 0, 1: 1, 3: 5, 4: 6} {
		got, err := utf16Mapper.byteOffset(0, character)
		if err != nil || got != want {
			t.Errorf("UTF-16 offset %d = %d, %v; want %d", character, got, err, want)
		}
	}
	if _, err := utf16Mapper.byteOffset(0, 2); err == nil ||
		!strings.Contains(err.Error(), "surrogate") {
		t.Fatalf("UTF-16 split = %v", err)
	}
	utf32Mapper, _ := newPositionMapper(source, 3)
	if got, err := utf32Mapper.byteOffset(0, 2); err != nil || got != 5 {
		t.Fatalf("UTF-32 offset = %d, %v", got, err)
	}
	if got, err := utf32Mapper.byteOffset(1, 2); err != nil || got != len(source)-1 {
		t.Fatalf("line-two offset = %d, %v", got, err)
	}
	for _, position := range [][2]int32{{-1, 0}, {99, 0}, {0, -1}, {0, 99}} {
		if _, err := utf8Mapper.byteOffset(position[0], position[1]); err == nil {
			t.Errorf("byteOffset(%v) succeeded", position)
		}
	}
	if _, err := newPositionMapper(source, 0); err == nil {
		t.Fatal("ambiguous mapper encoding succeeded")
	}
	valid, err := utf8Mapper.sourceRange("x", "artifact", sourcePosition{
		startLine: 0, startCharacter: 1, endLine: 0, endCharacter: 5,
	})
	if err != nil || valid.StartByte != 1 || valid.EndByte != 5 {
		t.Fatalf("sourceRange = %+v, %v", valid, err)
	}
	if _, err := utf8Mapper.sourceRange("x", "artifact", sourcePosition{
		startLine: 0, startCharacter: 5, endLine: 0, endCharacter: 1,
	}); err == nil || !strings.Contains(err.Error(), "precedes") {
		t.Fatalf("reverse sourceRange = %v", err)
	}
	if _, err := (*positionMapper)(nil).sourceRange("x", "artifact", sourcePosition{}); err == nil {
		t.Fatal("nil mapper succeeded")
	}
	if _, err := utf8Mapper.sourceRange("x", "artifact", sourcePosition{
		startLine: 0, endLine: 99,
	}); err == nil || !strings.Contains(err.Error(), "end position") {
		t.Fatalf("invalid end position = %v", err)
	}
}

func TestWireReaderAndParserCompatibilityCoverage(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		wire int
		data []byte
	}{
		{0, encodeVarint(42)},
		{1, bytes.Repeat([]byte{1}, 8)},
		{2, append(encodeVarint(3), []byte("abc")...)},
		{5, bytes.Repeat([]byte{1}, 4)},
	} {
		reader := newMessageReader(test.data)
		if err := reader.skip(test.wire); err != nil || reader.remaining != 0 {
			t.Errorf("skip(%d) = remaining %d, %v", test.wire, reader.remaining, err)
		}
	}
	for _, wire := range []int{3, 4, 6} {
		if err := newMessageReader(nil).skip(wire); err == nil {
			t.Errorf("skip(%d) succeeded", wire)
		}
	}
	if err := newMessageReader([]byte{1}).discard(2); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("discard overflow = %v", err)
	}
	if _, err := newMessageReader(append(encodeVarint(10), []byte("x")...)).bytes(5); err == nil {
		t.Fatal("oversized bytes succeeded")
	}
	if _, err := newMessageReader(append(encodeVarint(3), []byte("x")...)).bytes(5); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated bytes = %v", err)
	}
	if _, err := newMessageReader(append(encodeVarint(1), 0xff)).string(); err == nil {
		t.Fatal("invalid UTF-8 string succeeded")
	}
	if _, _, _, err := newMessageReader([]byte{0}).next(); err == nil {
		t.Fatal("zero field key succeeded")
	}
	if _, _, _, err := newMessageReader([]byte{1}).next(); err == nil {
		t.Fatal("field zero key succeeded")
	}
	if _, err := newMessageReader([]byte{0x80}).varint(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated varint = %v", err)
	}
	if _, err := newMessageReader([]byte{
		0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x02,
	}).varint(); err == nil || !strings.Contains(err.Error(), "overflows") {
		t.Fatalf("overflowing varint = %v", err)
	}
	if err := newMessageReader(nil).discard(-1); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("negative discard = %v", err)
	}

	tool := message(fieldString(1, "tool"), fieldString(2, "v1"), fieldString(3, "--arg"), fieldVarint(9, 1))
	meta, err := parseMetadata(message(
		fieldVarint(1, 0), fieldMessage(2, tool), fieldString(3, "file:///root"),
		fieldVarint(4, 1), fieldVarint(9, 1),
	))
	if err != nil || meta.toolName != "tool" || meta.toolVersion != "v1" ||
		meta.projectRoot != "file:///root" || meta.textEncoding != 1 {
		t.Fatalf("metadata = %+v, %v", meta, err)
	}
	relation := relationshipMessage("target", true, true, true, true)
	symbol := symbolMessage("symbol", "display", 17, "fn()", "parent", relation)
	diagnostic := diagnosticMessage(1, "E1", "failure", "compiler")
	occurrenceData := message(
		fieldVarint(1, 0), fieldVarint(1, 1), fieldVarint(1, 2),
		fieldString(2, "symbol"), fieldVarint(3, uint64(roleWrite)),
		fieldString(4, "override"), fieldVarint(5, 15),
		fieldMessage(6, diagnostic),
		fieldVarint(7, 0), fieldVarint(7, 0), fieldVarint(7, 3),
		typedRange(9, 0, 0, 0, 2),
		typedEnclosingRange(10, 0, 0, 3),
		fieldVarint(20, 1),
	)
	documentData := message(
		fieldString(1, "x.go"), fieldMessage(2, occurrenceData),
		fieldMessage(3, symbol), fieldString(4, "Go"), fieldString(5, "package x"),
		fieldVarint(6, 1), fieldVarint(20, 1),
	)
	document, err := parseDocument(documentData)
	if err != nil || document.text != "package x" || len(document.occurrences) != 1 ||
		len(document.symbols) != 1 {
		t.Fatalf("document = %+v, %v", document, err)
	}
	parsedSymbol := document.symbols[0]
	if len(parsedSymbol.documentation) != 1 || len(parsedSymbol.relationships) != 1 ||
		parsedSymbol.signature != "fn()" || parsedSymbol.enclosingSymbol != "parent" {
		t.Fatalf("symbol = %+v", parsedSymbol)
	}
	parsedOccurrence := document.occurrences[0]
	if len(parsedOccurrence.overrideDocumentation) != 1 ||
		len(parsedOccurrence.diagnostics) != 1 || parsedOccurrence.typedRange == nil ||
		parsedOccurrence.typedEnclosingRange == nil {
		t.Fatalf("occurrence = %+v", parsedOccurrence)
	}
	if position, ok, err := occurrenceRange(parsedOccurrence); err != nil || !ok ||
		position.endCharacter != 2 {
		t.Fatalf("typed occurrence range = %+v, %v, %v", position, ok, err)
	}
	if position, ok, err := occurrenceEnclosingRange(parsedOccurrence); err != nil || !ok ||
		position.endCharacter != 3 {
		t.Fatalf("typed enclosing range = %+v, %v, %v", position, ok, err)
	}
	if _, ok, err := legacyRange(nil); err != nil || ok {
		t.Fatalf("empty legacy range = %v, %v", ok, err)
	}
	if _, _, err := legacyRange([]int32{1}); err == nil {
		t.Fatal("short legacy range succeeded")
	}
	if _, _, err := legacyRange([]int32{-1, 0, 1}); err == nil {
		t.Fatal("negative legacy range succeeded")
	}
	if err := validatePosition(sourcePosition{
		startLine: 2, endLine: 1,
	}); err == nil || !strings.Contains(err.Error(), "precedes") {
		t.Fatalf("reversed line range = %v", err)
	}
	if _, err := parsePackedInt32(append(
		append(append(append(encodeVarint(1), encodeVarint(2)...), encodeVarint(3)...), encodeVarint(4)...),
		encodeVarint(5)...,
	)); err == nil {
		t.Fatal("five-value packed range succeeded")
	}
}

func TestExtractorMergeAndDiagnosticHelperCoverage(t *testing.T) {
	t.Parallel()
	value := &extractor{
		files: map[string]pluginapi.FileRef{"x.go": {ArtifactID: "artifact", Path: "x.go"}},
		nodes: map[string]rkcmodel.Node{}, edges: map[string]rkcmodel.Edge{},
		evidence: map[string]rkcmodel.Evidence{}, diagnostics: map[string]rkcmodel.Diagnostic{},
		input:    Input{SHA256: strings.Repeat("a", 64)},
		metadata: metadata{toolName: "compiler", toolVersion: "1"},
	}
	if id := value.addSymbol("x.go", "go", nil, symbolInformation{}, nil); id != "" {
		t.Fatalf("empty symbol ID = %q", id)
	}
	global := value.ensureSymbol("x.go", "go", "scip . . . pkg/Foo#", 19)
	local := value.ensureSymbol("x.go", "go", "local 1", 12)
	if global == local || value.nodeID("other.go", "local 1") == local {
		t.Fatal("local symbol identity is not document-scoped")
	}
	value.upsertNode(rkcmodel.Node{
		ID: global, Name: "Foo", Kind: "class", Language: "go",
		Signature: "type Foo struct{}", ArtifactID: "artifact",
		Source:      &rkcmodel.SourceRange{Path: "x.go", StartLine: 1},
		EvidenceIDs: []string{"e1"}, PublicSurface: true,
		Attributes: map[string]any{"documentation": "docs"},
	})
	node := value.nodes[global]
	if node.Kind != "type" || node.Signature == "" || node.ArtifactID == "" ||
		node.Source == nil || node.Attributes["documentation"] != "docs" {
		t.Fatalf("upserted node = %+v", node)
	}
	value.addEdge("references", global, local, []string{"e1"}, map[string]any{"a": 1})
	value.addEdge("references", global, local, []string{"e2"}, map[string]any{"b": 2})
	value.addEdge("references", global, global, nil, nil)
	edge := value.edges[rkcmodel.StableID("edge", "references", global, local)]
	if len(edge.EvidenceIDs) != 2 || edge.Attributes["b"] != 2 {
		t.Fatalf("merged edge = %+v", edge)
	}
	for _, severity := range []int32{0, 1, 2, 3, 4} {
		value.addDiagnostic("x.go", nil, compilerDiagnostic{
			severity: severity, message: "message", source: "compiler",
		})
	}
	if len(value.diagnostics) != 1 {
		t.Fatalf("diagnostic severity dedupe = %+v", value.diagnostics)
	}
	value.metadata.toolName = ""
	if value.toolName() != PluginID {
		t.Fatalf("empty tool name = %q", value.toolName())
	}
	values := appendUnique([]string{"b", "a"}, "a", "", "c")
	if strings.Join(values, ",") != "a,b,c" {
		t.Fatalf("appendUnique = %v", values)
	}

	mapper, err := newPositionMapper([]byte("type Foo struct{}\n"), 1)
	if err != nil {
		t.Fatal(err)
	}
	definition := occurrence{
		roles: roleDefinition | roleGenerated | roleTest,
		typedRange: &sourcePosition{
			startLine: 0, startCharacter: 0, endLine: 0, endCharacter: 8,
		},
	}
	full := value.addSymbol("x.go", "go", mapper, symbolInformation{
		symbol: "scip . . . pkg/Bar#", documentation: []string{"docs"},
		signatureLang: "Go", signature: "type Bar struct{}", enclosingSymbol: global,
		relationships: []relationship{{
			symbol: "scip . . . pkg/Target#", isImplementation: true,
			isReference: true, isTypeDefinition: true, isDefinition: true,
		}},
	}, &definition)
	if full == "" || value.nodes[full].Source == nil ||
		value.nodes[full].Attributes["generated"] != true ||
		value.nodes[full].Attributes["test"] != true {
		t.Fatalf("full symbol = %q, %+v", full, value.nodes[full])
	}
}

func TestInputAndIndexFailureCoverage(t *testing.T) {
	t.Parallel()
	if _, _, err := PrepareInputs(nil, nil); err == nil {
		t.Fatal("nil context succeeded")
	}
	if inputs, digest, err := PrepareInputs(context.Background(), nil); err != nil ||
		inputs != nil || digest != "" {
		t.Fatalf("empty inputs = %+v, %q, %v", inputs, digest, err)
	}
	tooMany := make([]string, MaximumIndexCount+1)
	if _, _, err := PrepareInputs(context.Background(), tooMany); err == nil ||
		!strings.Contains(err.Error(), "maximum") {
		t.Fatalf("too many inputs = %v", err)
	}
	for _, path := range []string{"", " \t", "bad\x00path"} {
		if _, _, err := PrepareInputs(context.Background(), []string{path}); err == nil {
			t.Errorf("invalid path %q succeeded", path)
		}
	}
	root := t.TempDir()
	if _, _, err := PrepareInputs(context.Background(), []string{root}); err == nil ||
		!strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory input = %v", err)
	}
	missing := filepath.Join(root, "missing.scip")
	if _, _, err := PrepareInputs(context.Background(), []string{missing}); err == nil {
		t.Fatal("missing input succeeded")
	}
	valid := writeNamedIndex(t, root, "valid.scip", fieldMessage(1, message(fieldVarint(1, 0))))
	if _, _, err := PrepareInputs(context.Background(), []string{valid, valid}); err == nil ||
		!strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate input = %v", err)
	}
	link := filepath.Join(root, "link.scip")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PrepareInputs(context.Background(), []string{link}); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink input = %v", err)
	}
	cancelledPrepare, cancelPrepare := context.WithCancel(context.Background())
	cancelPrepare()
	if _, _, err := PrepareInputs(cancelledPrepare, []string{valid}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled PrepareInputs = %v", err)
	}
	oversized := filepath.Join(root, "large.scip")
	file, err := os.OpenFile(oversized, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(MaximumIndexBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if _, _, err := PrepareInputs(context.Background(), []string{oversized}); err == nil ||
		!strings.Contains(err.Error(), "maximum") {
		t.Fatalf("oversized input = %v", err)
	}
	if !sameFileSnapshot(nil, nil) {
		// Expected false; this branch keeps the assertion readable.
	} else {
		t.Fatal("nil snapshots compare equal")
	}
	reader := &contextReader{ctx: context.Background(), reader: strings.NewReader("ok")}
	data := make([]byte, 2)
	if n, err := reader.Read(data); err != nil || n != 2 || string(data) != "ok" {
		t.Fatalf("contextReader = %q, %d, %v", data, n, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	reader = &contextReader{ctx: cancelled, reader: strings.NewReader("x")}
	if _, err := reader.Read(data); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled contextReader = %v", err)
	}
	if _, err := Extract(nil, Options{}); err == nil {
		t.Fatal("Extract(nil) succeeded")
	}
	if value, err := Extract(context.Background(), Options{Root: string([]byte{0})}); err != nil {
		t.Fatalf("Extract(empty inputs) = %+v, %v", value, err)
	}
	if fragment, err := Extract(context.Background(), Options{Root: root}); err != nil ||
		len(fragment.Nodes) != 0 {
		t.Fatalf("Extract(empty) = %+v, %v", fragment, err)
	}

	for name, index := range map[string][]byte{
		"missing metadata": fieldVarint(20, 1),
		"duplicate metadata": message(
			fieldMessage(1, message()),
			fieldMessage(1, message()),
		),
		"bad metadata wire": fieldVarint(1, 1),
		"bad document wire": message(fieldMessage(1, message()), fieldVarint(2, 1)),
		"bad external wire": message(fieldMessage(1, message()), fieldVarint(3, 1)),
		"unsupported wire":  message(fieldMessage(1, message()), []byte{byte(20<<3 | 3)}),
	} {
		t.Run(name, func(t *testing.T) {
			path := writeNamedIndex(t, root, strings.ReplaceAll(name, " ", "-")+".scip", index)
			inputs, _, err := PrepareInputs(context.Background(), []string{path})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Extract(context.Background(), Options{Root: root, Inputs: inputs}); err == nil {
				t.Fatalf("Extract(%s) succeeded", name)
			}
		})
	}
}

func TestNegativeAndMultiLineRangeCompatibility(t *testing.T) {
	t.Parallel()
	multi, err := parseMultiLineRange(message(
		fieldVarint(1, 1), fieldVarint(2, 2), fieldVarint(3, 3),
		fieldVarint(4, 4), fieldVarint(9, 9),
	))
	if err != nil || multi != (sourcePosition{
		startLine: 1, startCharacter: 2, endLine: 3, endCharacter: 4,
	}) {
		t.Fatalf("multi range = %+v, %v", multi, err)
	}
	negative := message(fieldVarint(1, math.MaxUint64), fieldVarint(2, 0), fieldVarint(3, 1))
	position, err := parseSingleLineRange(negative)
	if err != nil || position.startLine != -1 {
		t.Fatalf("negative wire range = %+v, %v", position, err)
	}
	if err := validatePosition(position); err == nil {
		t.Fatal("negative typed position validated")
	}
}

func TestParserRejectsMalformedFields(t *testing.T) {
	t.Parallel()
	parsers := []struct {
		name  string
		parse func([]byte) error
		data  []byte
	}{
		{"metadata tool wire", func(data []byte) error { _, err := parseMetadata(data); return err }, fieldVarint(2, 1)},
		{"metadata tool payload", func(data []byte) error { _, err := parseMetadata(data); return err }, fieldMessage(2, []byte{0x80})},
		{"metadata root wire", func(data []byte) error { _, err := parseMetadata(data); return err }, fieldVarint(3, 1)},
		{"metadata root payload", func(data []byte) error { _, err := parseMetadata(data); return err }, []byte{0x1a, 0x02, 'x'}},
		{"metadata encoding wire", func(data []byte) error { _, err := parseMetadata(data); return err }, fieldString(4, "x")},
		{"metadata encoding payload", func(data []byte) error { _, err := parseMetadata(data); return err }, []byte{0x20, 0x80}},
		{"tool name wire", func(data []byte) error { _, _, err := parseToolInfo(data); return err }, fieldVarint(1, 1)},
		{"tool version wire", func(data []byte) error { _, _, err := parseToolInfo(data); return err }, fieldVarint(2, 1)},
		{"document string wire", func(data []byte) error { _, err := parseDocument(data); return err }, fieldVarint(1, 1)},
		{"document occurrence wire", func(data []byte) error { _, err := parseDocument(data); return err }, fieldVarint(2, 1)},
		{"document occurrence payload", func(data []byte) error { _, err := parseDocument(data); return err }, fieldMessage(2, []byte{0x80})},
		{"document symbol wire", func(data []byte) error { _, err := parseDocument(data); return err }, fieldVarint(3, 1)},
		{"document symbol payload", func(data []byte) error { _, err := parseDocument(data); return err }, fieldMessage(3, []byte{0x80})},
		{"document encoding wire", func(data []byte) error { _, err := parseDocument(data); return err }, fieldString(6, "x")},
		{"occurrence range wire", func(data []byte) error { _, err := parseOccurrence(data); return err }, fieldFixed32(1, 1)},
		{"occurrence range overflow", func(data []byte) error { _, err := parseOccurrence(data); return err }, message(fieldVarint(1, 0), fieldVarint(1, 0), fieldVarint(1, 0), fieldVarint(1, 0), fieldVarint(1, 0))},
		{"occurrence enclosing overflow", func(data []byte) error { _, err := parseOccurrence(data); return err }, message(fieldVarint(7, 0), fieldVarint(7, 0), fieldVarint(7, 0), fieldVarint(7, 0), fieldVarint(7, 0))},
		{"occurrence symbol wire", func(data []byte) error { _, err := parseOccurrence(data); return err }, fieldVarint(2, 1)},
		{"occurrence roles wire", func(data []byte) error { _, err := parseOccurrence(data); return err }, fieldString(3, "x")},
		{"occurrence docs wire", func(data []byte) error { _, err := parseOccurrence(data); return err }, fieldVarint(4, 1)},
		{"occurrence diagnostic wire", func(data []byte) error { _, err := parseOccurrence(data); return err }, fieldVarint(6, 1)},
		{"occurrence diagnostic payload", func(data []byte) error { _, err := parseOccurrence(data); return err }, fieldMessage(6, []byte{0x80})},
		{"occurrence typed wire", func(data []byte) error { _, err := parseOccurrence(data); return err }, fieldVarint(8, 1)},
		{"occurrence typed payload", func(data []byte) error { _, err := parseOccurrence(data); return err }, fieldMessage(8, fieldString(1, "x"))},
		{"numeric wire", func(data []byte) error { _, err := parseNumericMessage(data, 3); return err }, fieldString(1, "x")},
		{"symbol string wire", func(data []byte) error { _, err := parseSymbolInformation(data); return err }, fieldVarint(1, 1)},
		{"symbol relation wire", func(data []byte) error { _, err := parseSymbolInformation(data); return err }, fieldVarint(4, 1)},
		{"symbol relation payload", func(data []byte) error { _, err := parseSymbolInformation(data); return err }, fieldMessage(4, []byte{0x80})},
		{"symbol kind wire", func(data []byte) error { _, err := parseSymbolInformation(data); return err }, fieldString(5, "x")},
		{"symbol signature wire", func(data []byte) error { _, err := parseSymbolInformation(data); return err }, fieldVarint(7, 1)},
		{"symbol signature payload", func(data []byte) error { _, err := parseSymbolInformation(data); return err }, fieldMessage(7, []byte{0x80})},
		{"relationship symbol wire", func(data []byte) error { _, err := parseRelationship(data); return err }, fieldVarint(1, 1)},
		{"relationship flag wire", func(data []byte) error { _, err := parseRelationship(data); return err }, fieldString(2, "x")},
		{"signature language wire", func(data []byte) error { _, _, err := parseSignature(data); return err }, fieldVarint(4, 1)},
		{"signature text wire", func(data []byte) error { _, _, err := parseSignature(data); return err }, fieldVarint(5, 1)},
		{"diagnostic severity wire", func(data []byte) error { _, err := parseCompilerDiagnostic(data); return err }, fieldString(1, "x")},
		{"diagnostic string wire", func(data []byte) error { _, err := parseCompilerDiagnostic(data); return err }, fieldVarint(2, 1)},
	}
	for _, test := range parsers {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.parse(test.data); err == nil {
				t.Fatal("malformed protobuf succeeded")
			}
		})
	}

	for _, value := range []occurrence{
		{typedRange: &sourcePosition{startLine: -1}},
		{typedRange: &sourcePosition{startLine: 2, endLine: 1}},
	} {
		if _, _, err := occurrenceRange(value); err == nil {
			t.Fatal("invalid typed occurrence range succeeded")
		}
	}
	for _, value := range []occurrence{
		{typedEnclosingRange: &sourcePosition{startCharacter: -1}},
		{typedEnclosingRange: &sourcePosition{startLine: 2, endLine: 1}},
	} {
		if _, _, err := occurrenceEnclosingRange(value); err == nil {
			t.Fatal("invalid typed enclosing range succeeded")
		}
	}
}

func TestExtractorDocumentAndOccurrenceFailureCoverage(t *testing.T) {
	t.Parallel()
	newExtractor := func(root string) *extractor {
		return &extractor{
			ctx: context.Background(), root: root,
			files: map[string]pluginapi.FileRef{}, artifacts: map[string]rkcmodel.Artifact{},
			nodes: map[string]rkcmodel.Node{}, edges: map[string]rkcmodel.Edge{},
			evidence: map[string]rkcmodel.Evidence{}, diagnostics: map[string]rkcmodel.Diagnostic{},
			parsed: map[string]map[string]struct{}{},
			input:  Input{SHA256: strings.Repeat("b", 64)},
		}
	}
	for name, setup := range map[string]func(*extractor) document{
		"document limit": func(value *extractor) document {
			value.counts.documents = maximumDocuments
			return document{}
		},
		"symbol limit": func(value *extractor) document {
			value.counts.symbols = maximumSymbols
			return document{symbols: []symbolInformation{{}}}
		},
		"occurrence limit": func(value *extractor) document {
			value.counts.occurrences = maximumOccurrences
			return document{occurrences: []occurrence{{}}}
		},
		"non canonical": func(*extractor) document {
			return document{path: "../escape.go"}
		},
		"missing inventory": func(*extractor) document {
			return document{path: "missing.go"}
		},
		"missing artifact": func(value *extractor) document {
			value.files["x.go"] = pluginapi.FileRef{Path: "x.go", ArtifactID: "artifact"}
			return document{path: "x.go"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := newExtractor(t.TempDir())
			if err := value.extractDocument(setup(value)); err == nil {
				t.Fatal("invalid document succeeded")
			}
		})
	}

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "x.go"), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	value := newExtractor(root)
	value.files["x.go"] = pluginapi.FileRef{
		Path: "x.go", ArtifactID: "artifact", SizeBytes: 99, Language: "go",
	}
	value.artifacts["x.go"] = rkcmodel.Artifact{ID: "artifact", Path: "x.go"}
	if err := value.extractDocument(document{path: "x.go"}); err == nil ||
		!strings.Contains(err.Error(), "size changed") {
		t.Fatalf("size mismatch = %v", err)
	}
	value.files["x.go"] = pluginapi.FileRef{
		Path: "x.go", ArtifactID: "artifact", SizeBytes: 2, Language: "go",
	}
	if err := value.extractDocument(document{
		path: "x.go", positionEncoding: 0,
		occurrences: []occurrence{{symbol: "x"}},
	}); err == nil || !strings.Contains(err.Error(), "encoding") {
		t.Fatalf("ambiguous encoding = %v", err)
	}

	mapper, err := newPositionMapper([]byte("x\n"), 1)
	if err != nil {
		t.Fatal(err)
	}
	value = newExtractor(root)
	value.files["x.go"] = pluginapi.FileRef{
		Path: "x.go", ArtifactID: "artifact", SizeBytes: 2, Language: "go",
	}
	value.nodes = map[string]rkcmodel.Node{}
	if err := value.addOccurrence("x.go", "go", mapper, nil, occurrence{
		typedRange: &sourcePosition{startCharacter: 2}, symbol: "local 1",
	}); err == nil {
		t.Fatal("out-of-bounds occurrence succeeded")
	}
	if err := value.addOccurrence("x.go", "go", mapper, nil, occurrence{
		typedRange:  &sourcePosition{endCharacter: 1},
		diagnostics: []compilerDiagnostic{{message: "warning"}},
	}); err != nil || len(value.diagnostics) != 1 {
		t.Fatalf("diagnostic-only occurrence = %v, %+v", err, value.diagnostics)
	}
	if err := value.addOccurrence("x.go", "go", mapper, nil, occurrence{
		typedRange: &sourcePosition{endCharacter: 1}, symbol: "local 1",
		roles: roleDefinition, overrideDocumentation: []string{"docs"},
	}); err != nil {
		t.Fatal(err)
	}
	node := value.nodes[value.nodeID("x.go", "local 1")]
	if node.Source == nil || node.Attributes["occurrence_documentation"] != "docs" {
		t.Fatalf("definition occurrence node = %+v", node)
	}
}

func fieldFixed32(number int, value uint32) []byte {
	return []byte{
		byte(number<<3 | 5),
		byte(value), byte(value >> 8), byte(value >> 16), byte(value >> 24),
	}
}
