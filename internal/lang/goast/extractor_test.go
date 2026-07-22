package goast

import (
	"go/parser"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

func TestExtractGoDeclarationsCallsAndDeterminism(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGoASTTestFile(t, root, "go.mod", "module example.com/project\n\ngo 1.23\n")
	writeGoASTTestFile(t, root, "sample/types.go", `package sample

import format "fmt"

// Public is documented.
type Public[T any] struct {
	Field string `+"`json:\"field\"`"+`
	hidden int
	*Embedded
}

type Alias = string
type Contract interface { Do(value int) error }
const Exported = 1
var hiddenValue string

func (p *Public[T]) Method(values ...string) (int, error) {
	format.Println(values)
	helper[int]()
	return len(values), nil
}
`)
	writeGoASTTestFile(t, root, "sample/types_test.go", `package sample
func TestPublic() { MethodCall() }
`)
	files := []pluginapi.FileRef{
		{ArtifactID: "ignored", Path: "README.md", Language: "markdown", SHA256: "ignored"},
		{ArtifactID: "test", Path: "sample/types_test.go", Language: "go", SHA256: "sha-test"},
		{ArtifactID: "source", Path: "sample/types.go", Language: "go", SHA256: "sha-source"},
	}
	got, err := Extract(Options{Root: root, SnapshotID: "snapshot", Files: files})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	reversed, err := Extract(Options{Root: root, SnapshotID: "snapshot", Files: []pluginapi.FileRef{files[2], files[1], files[0]}})
	if err != nil {
		t.Fatalf("Extract(reversed) error = %v", err)
	}
	if !reflect.DeepEqual(got, reversed) {
		t.Fatal("Extract() output depends on input file order")
	}
	if len(got.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", got.Diagnostics)
	}

	wantedKinds := map[string]string{
		"sample": "package", "Public": "class", "Field": "field", "hidden": "field", "*Embedded": "field",
		"Alias": "type", "Contract": "interface", "Do": "method", "Exported": "constant",
		"hiddenValue": "variable", "Method": "method", "TestPublic": "test",
		"fmt": "external_dependency", "format.Println": "unresolved_symbol", "helper": "unresolved_symbol",
		"len": "unresolved_symbol", "MethodCall": "unresolved_symbol",
	}
	found := map[string]bool{}
	for _, node := range got.Nodes {
		if kind, ok := wantedKinds[node.Name]; ok {
			found[node.Name] = true
			if node.Kind != kind {
				t.Errorf("node %q kind = %q, want %q", node.Name, node.Kind, kind)
			}
		}
		if node.Name == "Public" {
			if node.QualifiedName != "example.com/project/sample.Public" || node.Attributes["docstring"] != "Public is documented." {
				t.Errorf("Public node = %#v", node)
			}
		}
		if node.Name == "Field" && node.Attributes["tag"] != `json:"field"` {
			t.Errorf("Field tag = %#v", node.Attributes["tag"])
		}
		if node.Name == "Method" && node.Attributes["variadic"] != true {
			t.Errorf("Method variadic = %#v", node.Attributes["variadic"])
		}
	}
	for name := range wantedKinds {
		if !found[name] {
			t.Errorf("missing node %q", name)
		}
	}
	var importAlias, methodOwned bool
	for _, edge := range got.Edges {
		if edge.Kind == "imports" && edge.Attributes["alias"] == "format" {
			importAlias = true
		}
		if edge.Kind == "declares" {
			var fromName, toName string
			for _, node := range got.Nodes {
				if node.ID == edge.From {
					fromName = node.Name
				}
				if node.ID == edge.To {
					toName = node.Name
				}
			}
			if fromName == "Public" && toName == "Method" {
				methodOwned = true
			}
		}
	}
	if !importAlias || !methodOwned {
		t.Errorf("importAlias=%v methodOwned=%v", importAlias, methodOwned)
	}
}

func TestExtractGoMalformedFileReportsDiagnostic(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeGoASTTestFile(t, root, "broken.go", "package broken\nfunc Broken( {\n")
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "broken", Path: "broken.go", Language: "go", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-GO-1001" {
		t.Fatalf("diagnostics = %#v, want one RKC-GO-1001", fragment.Diagnostics)
	}
}

func TestExtractGoRejectsPathsOutsideRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "repository")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeGoASTTestFile(t, parent, "outside.go", "package outside\nfunc Exported() {}\n")
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "outside", Path: "../outside.go", Language: "go", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Nodes) != 0 || len(fragment.Edges) != 0 {
		t.Fatalf("outside-root path was parsed: nodes=%d edges=%d", len(fragment.Nodes), len(fragment.Edges))
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-GO-1001" {
		t.Fatalf("diagnostics = %#v, want one RKC-GO-1001", fragment.Diagnostics)
	}
}

func TestGoASTHelpersBoundaries(t *testing.T) {
	t.Parallel()
	for expression, want := range map[string]string{
		"fn": "fn", "pkg.Call": "pkg.Call", "generic[int]": "generic", "multi[A, B]": "multi", "(wrapped)": "wrapped", "factory().Call": "Call",
	} {
		parsed, err := parser.ParseExpr(expression)
		if err != nil {
			t.Fatalf("ParseExpr(%q): %v", expression, err)
		}
		if got := callName(parsed); got != want {
			t.Errorf("callName(%q) = %q, want %q", expression, got, want)
		}
	}
	if got := normalizeReceiver(" *Public[T] "); got != "Public" {
		t.Errorf("normalizeReceiver() = %q", got)
	}
	if goVisibility("Exported") != "public" || goVisibility("private") != "private" {
		t.Error("goVisibility classification mismatch")
	}
	if edgeConfidence("unresolved") != 0.7 || edgeConfidence("declared") != 1 {
		t.Error("edgeConfidence boundaries mismatch")
	}
	if max(1, 2) != 2 || max(2, 1) != 2 {
		t.Error("max helper mismatch")
	}
}

func writeGoASTTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
