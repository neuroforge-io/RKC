package export

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/model"
)

func TestCoverageHeadroomWriteAllStageFailures(t *testing.T) {
	bundle := exportFixture(t.TempDir(), "source.go", []byte("package fixture\n"))
	coverage := model.BuildCoverage(bundle)
	tests := []struct {
		name          string
		blockedPath   string
		blockWithFile bool
	}{
		{"manifest", "rkc.manifest.json", false},
		{"execution receipt", "rkc.execution.json", false},
		{"export policy", "rkc.export-policy.json", false},
		{"coverage", "coverage.json", false},
		{"bundle", "bundle.json", false},
		{"graph directory", "graph", true},
		{"artifact JSONL", "graph/artifacts.jsonl", false},
		{"node JSONL", "graph/nodes.jsonl", false},
		{"edge JSONL", "graph/edges.jsonl", false},
		{"evidence JSONL", "graph/evidence.jsonl", false},
		{"diagnostic JSONL", "graph/diagnostics.jsonl", false},
		{"conflict JSONL", "graph/conflicts.jsonl", false},
		{"document JSONL", "graph/documents.jsonl", false},
		{"claim JSONL", "graph/claims.jsonl", false},
		{"execution path JSONL", "graph/execution-paths.jsonl", false},
		{"search directory", "search", true},
		{"search index", "search/index.json", false},
		{"documentation directory", "docs", true},
		{"documentation overview", "docs/README.md", false},
		{"documentation coverage", "docs/coverage.md", false},
		{"symbol documentation", "docs/symbols/function-1.md", false},
		{"notebook directory", "notebooklm", true},
		{"notebook overview", "notebooklm/00_repository_overview.md", false},
		{"notebook diagnostics", "notebooklm/01_coverage_and_diagnostics.md", false},
		{"notebook symbols", "notebooklm/02_symbols_001.md", false},
		{"notebook relations", "notebooklm/03_relationships_001.md", false},
		{"notebook manifest", "notebooklm/manifest.json", false},
		{"site directory", "site", true},
		{"site index", "site/index.html", false},
		{"site stylesheet", "site/styles.css", false},
		{"site script", "site/app.js", false},
		{"site atlas", "site/data/atlas.json", false},
		{"integration directory", "integrations", true},
		{"SARIF integration", "integrations/diagnostics.sarif.json", false},
		{"GraphML integration", "integrations/graph.graphml", false},
		{"Mermaid integration", "integrations/architecture.mmd", false},
		{"symbol CSV integration", "integrations/symbols.csv", false},
		{"edge CSV integration", "integrations/edges.csv", false},
		{"export manifest", "rkc-export-manifest.json", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := filepath.Join(t.TempDir(), "atlas")
			if err := os.MkdirAll(output, 0o700); err != nil {
				t.Fatal(err)
			}
			blocked := filepath.Join(output, filepath.FromSlash(test.blockedPath))
			if test.blockWithFile {
				if err := os.MkdirAll(filepath.Dir(blocked), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.MkdirAll(blocked, 0o700); err != nil {
				t.Fatal(err)
			}
			err := WriteAll(bundle, coverage, Options{Output: output})
			if err == nil {
				t.Fatalf("WriteAll unexpectedly accepted blocked %s", test.blockedPath)
			}
		})
	}
}

func TestCoverageHeadroomLowLevelFailures(t *testing.T) {
	if err := writeJSONL(filepath.Join(t.TempDir(), "invalid.jsonl"), []any{make(chan int)}); err == nil || !strings.Contains(err.Error(), "encode") {
		t.Fatalf("writeJSONL encode error = %v", err)
	}

	packs := t.TempDir()
	if err := writePacks(packs, "empty", "Empty", nil, 1); err != nil {
		t.Fatalf("empty packs: %v", err)
	}
	blockedPack := filepath.Join(packs, "blocked_001.md")
	if err := os.Mkdir(blockedPack, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writePacks(
		packs,
		"blocked",
		"Blocked",
		[]string{strings.Repeat("a", 500), strings.Repeat("b", 500)},
		1,
	); err == nil {
		t.Fatal("writePacks swallowed a split-pack write failure")
	}

	if err := writeExportManifest(filepath.Join(t.TempDir(), "missing"), "snapshot"); err == nil || !strings.Contains(err.Error(), "build export manifest") {
		t.Fatalf("missing export root error = %v", err)
	}
	normalizedOutput := t.TempDir()
	if err := os.WriteFile(filepath.Join(normalizedOutput, "normalized"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeNormalizedSources(model.Bundle{}, Options{Output: normalizedOutput}); err == nil {
		t.Fatal("writeNormalizedSources accepted a regular-file output root")
	}
}

func TestCoverageHeadroomArtifactAndFormattingBoundaries(t *testing.T) {
	artifact := model.Artifact{Path: "artifact.txt"}
	if _, err := readVerifiedArtifact(filepath.Join(t.TempDir(), "missing"), artifact); err == nil {
		t.Fatal("readVerifiedArtifact accepted a missing root")
	}
	root := t.TempDir()
	if _, err := readVerifiedArtifact(root, artifact); err == nil {
		t.Fatal("readVerifiedArtifact accepted a missing artifact")
	}
	if err := os.Mkdir(filepath.Join(root, artifact.Path), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := readVerifiedArtifact(root, artifact); err == nil || !strings.Contains(err.Error(), "not a regular") {
		t.Fatalf("directory artifact error = %v", err)
	}
	if err := os.Remove(filepath.Join(root, artifact.Path)); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real.txt")
	if err := os.WriteFile(real, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, artifact.Path)); err != nil {
		t.Fatal(err)
	}
	if _, err := readVerifiedArtifact(root, artifact); err == nil || !strings.Contains(err.Error(), "not a regular") {
		t.Fatalf("symlink artifact error = %v", err)
	}

	if err := verifyResolvedParent(filepath.Join(root, "missing-root"), root); err == nil {
		t.Fatal("verifyResolvedParent accepted a missing root")
	}
	if err := verifyResolvedParent(root, filepath.Join(root, "missing-parent")); err == nil {
		t.Fatal("verifyResolvedParent accepted a missing parent")
	}
	if got := fenceLanguage("csharp"); got != "csharp" {
		t.Fatalf("csharp fence = %q", got)
	}
	for input, want := range map[string]string{
		"cpp":        "cpp",
		"javascript": "javascript",
		"python":     "python",
		"<>`":        "text",
	} {
		if got := fenceLanguage(input); got != want {
			t.Errorf("fenceLanguage(%q) = %q, want %q", input, got, want)
		}
	}
	if got := markdownText("tab\tcarriage\rnull\x00"); !strings.Contains(got, "\ufffd") || strings.ContainsAny(got, "\t\r") {
		t.Fatalf("control-character Markdown = %q", got)
	}
	if value, ok := stringAttribute(map[string]any{}, "missing"); ok || value != "" {
		t.Fatalf("missing string attribute = %q, %t", value, ok)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Fatalf("firstNonEmpty blanks = %q", got)
	}

	var arguments strings.Builder
	writeArguments(&arguments, []any{"not a record", map[string]any{"name": "accepted"}})
	if !strings.Contains(arguments.String(), "accepted") {
		t.Fatalf("argument rendering = %q", arguments.String())
	}
	writeRelations(&arguments, "empty", nil, true, nil)

	diagnosticBundle := model.Bundle{
		Snapshot: model.Snapshot{Tool: model.ToolInfo{Name: "rkc", Version: "test"}},
		Diagnostics: []model.Diagnostic{{
			ID:       "diagnostic",
			Severity: "warning",
			Message:  "Message without code.",
			Source: &model.SourceRange{
				Path:        "source.go",
				StartLine:   1,
				EndLine:     2,
				StartColumn: 2,
				EndColumn:   4,
			},
		}},
	}
	sarifPath := filepath.Join(t.TempDir(), "diagnostics.sarif.json")
	if err := writeSARIF(sarifPath, diagnosticBundle); err != nil {
		t.Fatal(err)
	}
	mermaidBundle := model.Bundle{
		Nodes: []model.Node{{ID: "selected", Kind: "repository", Name: "selected"}, {ID: "ignored", Kind: "function"}},
		Edges: []model.Edge{{From: "selected", To: "missing", Kind: "contains"}},
	}
	if err := writeMermaid(filepath.Join(t.TempDir(), "architecture.mmd"), mermaidBundle); err != nil {
		t.Fatal(err)
	}
}

func TestCoverageHeadroomNormalizedOutputFaults(t *testing.T) {
	root := t.TempDir()
	source := []byte("package fixture\n")
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "nested", "source.go"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := exportFixture(root, "nested/source.go", source)

	mkdirOutput := t.TempDir()
	if err := os.Mkdir(filepath.Join(mkdirOutput, "normalized"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mkdirOutput, "normalized", "nested"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeNormalizedSources(bundle, Options{Root: root, Output: mkdirOutput}); err == nil {
		t.Fatal("normalized source accepted a regular-file parent")
	}

	writeOutput := t.TempDir()
	blockedTarget := filepath.Join(writeOutput, "normalized", "nested", "source.go.md")
	if err := os.MkdirAll(blockedTarget, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := writeNormalizedSources(bundle, Options{Root: root, Output: writeOutput}); err == nil {
		t.Fatal("normalized source accepted a directory as its file target")
	}

	secretA := []byte(strings.Join([]string{"sk", "live", "0123456789abcdef0123456789"}, "_") + "\n")
	secretB := []byte(strings.Join([]string{"sk", "live", "abcdef0123456789abcdef0123"}, "_") + "\n")
	if err := os.WriteFile(filepath.Join(root, "a.go"), secretA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.go"), secretB, 0o600); err != nil {
		t.Fatal(err)
	}
	redacted := exportFixture(root, "b.go", secretB)
	second := exportFixture(root, "a.go", secretA)
	redacted.Artifacts = append(redacted.Artifacts, second.Artifacts[0])
	if err := writeNormalizedSources(redacted, Options{Root: root, Output: t.TempDir()}); err != nil {
		t.Fatalf("sorted redaction export: %v", err)
	}

	gitBundle := bundle
	gitBundle.Snapshot.Git.Commit = "0123456789abcdef"
	gitBundle.Snapshot.Git.Dirty = true
	overview := repositoryOverview(gitBundle, model.BuildCoverage(gitBundle))
	if !strings.Contains(overview, "Git commit") || !strings.Contains(overview, "dirty") {
		t.Fatalf("Git provenance overview = %q", overview)
	}

	node := model.Node{ID: "source", Kind: "function", Name: "FallbackName"}
	target := model.Node{ID: "target", Kind: "function", Name: "TargetName"}
	markdown := symbolMarkdown(
		node,
		[]model.Edge{{ID: "edge", Kind: "calls", From: node.ID, To: target.ID}},
		nil,
		map[string]model.Node{target.ID: target},
	)
	if !strings.Contains(markdown, "FallbackName") || !strings.Contains(markdown, "TargetName") {
		t.Fatalf("fallback symbol Markdown = %q", markdown)
	}

	manifestRoot := t.TempDir()
	if err := os.Symlink(filepath.Join(manifestRoot, "vanished"), filepath.Join(manifestRoot, "broken")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := writeExportManifest(manifestRoot, "snapshot"); err == nil || !strings.Contains(err.Error(), "build export manifest") {
		t.Fatalf("broken manifest input error = %v", err)
	}
}
