package manifest

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
)

func TestExtractManifestsRichInputsAndDeterminism(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sensitiveValue := "docker" + "-value-123456789"
	writeManifestTestFile(t, root, "frontend/package.json", `{
  "name": "@example/app",
  "version": "1.2.3",
  "dependencies": {"alpha": "^1.0.0"},
  "devDependencies": {"beta": "~2.0.0"},
  "optionalDependencies": {"gamma": "3.x"},
  "peerDependencies": {"delta": ">=4"},
  "scripts": {"build": "node build.js", "test": "node test.js"},
  "bin": {"example": "bin/example.js"}
}`)
	writeManifestTestFile(t, root, "go.mod", strings.Join([]string{
		"module example.com/root",
		"go 1.23",
		"require example.com/inline v1.2.3",
		"require (",
		"  example.com/direct v2.0.0",
		"  example.com/indirect v3.0.0 // indirect",
		")",
		"replace example.com/old => ../local",
		"replace (",
		"  example.com/source v1.0.0 => example.com/fork v1.1.0",
		")",
		"",
	}, "\n"))
	writeManifestTestFile(t, root, "requirements-prod.txt", strings.Join([]string{
		"requests==2.32.0",
		"typing-extensions>=4; python_version < \"3.11\" # compatibility",
		"-r requirements-base.txt",
		"# comment",
		"",
	}, "\n"))
	writeManifestTestFile(t, root, "deploy/Dockerfile", strings.Join([]string{
		"FROM golang:1.24 AS build",
		"ARG BUILD_MODE=release",
		"ENV DB_PASSWORD=" + sensitiveValue + " PUBLIC_MODE=enabled",
		"EXPOSE 8080/tcp 8443",
		"FROM scratch",
		"EXPOSE 9090",
		"",
	}, "\n"))

	files := []pluginapi.FileRef{
		{ArtifactID: "requirements", Path: "requirements-prod.txt", SHA256: "sha-r"},
		{ArtifactID: "docker", Path: "deploy/Dockerfile", SHA256: "sha-d"},
		{ArtifactID: "npm", Path: "frontend/package.json", SHA256: "sha-n"},
		{ArtifactID: "go", Path: "go.mod", SHA256: "sha-g"},
	}
	got, err := Extract(Options{Root: root, Files: files})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	reversedFiles := append([]pluginapi.FileRef(nil), files...)
	for left, right := 0, len(reversedFiles)-1; left < right; left, right = left+1, right-1 {
		reversedFiles[left], reversedFiles[right] = reversedFiles[right], reversedFiles[left]
	}
	reversed, err := Extract(Options{Root: root, Files: reversedFiles})
	if err != nil {
		t.Fatalf("Extract(reversed) error = %v", err)
	}
	if !reflect.DeepEqual(got, reversed) {
		t.Fatal("Extract() output depends on input file order")
	}
	if len(got.Diagnostics) != 0 {
		t.Fatalf("unexpected diagnostics: %#v", got.Diagnostics)
	}

	wantedNames := map[string]bool{
		"@example/app": false, "alpha": false, "beta": false, "gamma": false, "delta": false,
		"build": false, "test": false, "example": false, "example.com/root": false,
		"example.com/inline": false, "example.com/direct": false, "example.com/indirect": false,
		"requests": false, "typing-extensions": false, "DB_PASSWORD": false, "PUBLIC_MODE": false,
		"8080/tcp": false, "8443": false, "9090": false,
	}
	for _, node := range got.Nodes {
		if _, ok := wantedNames[node.Name]; ok {
			wantedNames[node.Name] = true
		}
		if node.Name == "DB_PASSWORD" {
			if node.Attributes["default"] != "<redacted>" || node.Attributes["secret_like"] != true {
				t.Errorf("secret Docker attribute = %#v", node.Attributes)
			}
		}
	}
	for name, found := range wantedNames {
		if !found {
			t.Errorf("missing extracted node %q", name)
		}
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), sensitiveValue) {
		t.Fatal("Docker secret-like default leaked into fragment")
	}

	var foundIndirect bool
	for _, edge := range got.Edges {
		if edge.Kind == "depends_on" && edge.Attributes["indirect"] == true {
			foundIndirect = true
		}
	}
	if !foundIndirect {
		t.Error("go.mod indirect dependency metadata was not retained")
	}
}

func TestExtractManifestDiagnostics(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeManifestTestFile(t, root, "package.json", `{broken`)
	writeManifestTestFile(t, root, "requirements.txt", strings.Repeat("a", 2*1024*1024+1))
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{
		{ArtifactID: "package", Path: "package.json", SHA256: "one"},
		{ArtifactID: "requirements", Path: "requirements.txt", SHA256: "two"},
		{ArtifactID: "missing-go", Path: "missing/go.mod", SHA256: "three"},
		{ArtifactID: "missing-docker", Path: "missing/Dockerfile", SHA256: "four"},
	}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	codes := map[string]int{}
	for _, diagnostic := range fragment.Diagnostics {
		codes[diagnostic.Code]++
	}
	for _, code := range []string{"RKC-MAN-1002", "RKC-MAN-1101", "RKC-MAN-1202", "RKC-MAN-1301"} {
		if codes[code] != 1 {
			t.Errorf("diagnostic %s count = %d, want 1 (all=%#v)", code, codes[code], codes)
		}
	}
}

func TestPackageJSONRejectsTrailingJSON(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeManifestTestFile(t, root, "package.json", `{"name":"first"} {"name":"second"}`)
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "package", Path: "package.json", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("package.json with trailing document produced %d nodes", len(fragment.Nodes))
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-MAN-1002" {
		t.Fatalf("diagnostics = %#v, want one RKC-MAN-1002", fragment.Diagnostics)
	}
}

func TestExtractManifestRejectsPathsOutsideRoot(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	root := filepath.Join(parent, "repository")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	writeManifestTestFile(t, parent, "package.json", `{"name":"outside"}`)
	fragment, err := Extract(Options{Root: root, Files: []pluginapi.FileRef{{ArtifactID: "outside", Path: "../package.json", SHA256: "sha"}}})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(fragment.Nodes) != 0 {
		t.Fatalf("outside-root path produced %d nodes", len(fragment.Nodes))
	}
	if len(fragment.Diagnostics) != 1 || fragment.Diagnostics[0].Code != "RKC-MAN-1001" {
		t.Fatalf("diagnostics = %#v, want one RKC-MAN-1001", fragment.Diagnostics)
	}
}

func TestManifestHelpersBoundaries(t *testing.T) {
	t.Parallel()
	if got := dockerAssignments(nil); got != nil {
		t.Errorf("dockerAssignments(nil) = %#v", got)
	}
	if got := dockerAssignments([]string{"NAME", "value", "with", "spaces"}); !reflect.DeepEqual(got, []dockerAssignment{{name: "NAME", value: "value with spaces"}}) {
		t.Errorf("legacy ENV assignment = %#v", got)
	}
	if got := dockerAssignments([]string{"A=1", "B=", "C"}); !reflect.DeepEqual(got, []dockerAssignment{{name: "A", value: "1"}, {name: "B", value: ""}, {name: "C", value: ""}}) {
		t.Errorf("key=value assignments = %#v", got)
	}
	if got := escapeJSONPointer("a~/b"); got != "a~0~1b" {
		t.Errorf("escapeJSONPointer() = %q", got)
	}
	if got := stripLineComment("value // indirect"); got != "value " {
		t.Errorf("stripLineComment() = %q", got)
	}
	if got := indexOf([]string{"a", "=>", "b"}, "=>"); got != 1 {
		t.Errorf("indexOf() = %d", got)
	}
	if got := indexOf(nil, "missing"); got != -1 {
		t.Errorf("indexOf(nil) = %d", got)
	}
	if got := joinAfter([]string{"a", "b", "c"}, 1); got != "b c" {
		t.Errorf("joinAfter() = %q", got)
	}
	if got := joinAfter([]string{"a"}, 1); got != "" {
		t.Errorf("joinAfter(boundary) = %q", got)
	}
}

func writeManifestTestFile(t *testing.T, root, relative, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
