package export

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
)

func TestWriteAllProducesCompleteDeterministicRedactedExport(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	secret := "sk_live_" + "0123456789abcdef0123456789"
	source := []byte("package fixture\n// api_key=" + secret + "\nfunc Login() {}\n")
	path := filepath.Join(root, "src", "login.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, source, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := exportFixture(root, "src/login.go", source)
	coverage := model.BuildCoverage(bundle)

	first := filepath.Join(t.TempDir(), "atlas-one")
	second := filepath.Join(t.TempDir(), "atlas-two")
	for _, output := range []string{first, second} {
		if err := WriteAll(bundle, coverage, Options{Root: root, Output: output, IncludeSources: true, NotebookMaxSize: 256}); err != nil {
			t.Fatal(err)
		}
		for _, relative := range []string{
			"rkc.manifest.json", "rkc.execution.json", "rkc.export-policy.json", "coverage.json", "bundle.json",
			"graph/nodes.jsonl", "search/index.json", "docs/README.md", "docs/symbols/function-1.md",
			"normalized/src/login.go.md", "normalized/redactions.json", "notebooklm/manifest.json",
			"notebooklm/UPLOAD.md",
			"site/index.html", "site/styles.css", "site/app.js", "site/data/atlas.json",
			"site/data/bootstrap.json",
			"integrations/diagnostics.sarif.json", "integrations/graph.graphml", "integrations/architecture.mmd",
			"integrations/symbols.csv", "integrations/edges.csv", "rkc-export-manifest.json",
		} {
			if _, err := os.Stat(filepath.Join(output, relative)); err != nil {
				t.Errorf("missing %s: %v", relative, err)
			}
		}
		app, err := os.ReadFile(filepath.Join(output, "site", "app.js"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(app, []byte(`"default_args":[]`)) ||
			!bytes.Contains(app, []byte("command?.default_args||['--help']")) {
			t.Fatal("command defaults must preserve intentionally empty argument lists")
		}
		normalized, err := os.ReadFile(filepath.Join(output, "normalized", "src", "login.go.md"))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(normalized, []byte(secret)) || !bytes.Contains(normalized, []byte("***")) {
			t.Fatalf("normalized source was not redacted: %s", normalized)
		}
		manifestData, err := os.ReadFile(filepath.Join(output, "rkc-export-manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest exportManifest
		if err := json.Unmarshal(manifestData, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest.SnapshotID != bundle.Snapshot.ID || manifest.CanonicalFilesDigest == "" || manifest.CanonicalBytes <= 0 || manifest.TotalBytes < manifest.CanonicalBytes {
			t.Fatalf("invalid export manifest: %+v", manifest)
		}
		for _, file := range manifest.Files {
			if file.Path == safeoutput.MarkerName || (file.Path == "rkc.execution.json" && file.Canonical) {
				t.Fatalf("incorrect manifest classification: %+v", file)
			}
		}
		notebookManifestData, err := os.ReadFile(filepath.Join(output, "notebooklm", "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var notebookManifest struct {
			SourceCount    int   `json:"source_count"`
			SourceBytes    int64 `json:"source_bytes"`
			MaxSourceBytes int64 `json:"max_source_bytes"`
			SourceFiles    []struct {
				Path  string `json:"path"`
				Bytes int64  `json:"bytes"`
			} `json:"source_files"`
			UploadGuide string `json:"upload_guide"`
		}
		if err := json.Unmarshal(notebookManifestData, &notebookManifest); err != nil {
			t.Fatal(err)
		}
		if notebookManifest.SourceCount != len(notebookManifest.SourceFiles) || notebookManifest.SourceCount < 4 || notebookManifest.SourceBytes <= 0 || notebookManifest.MaxSourceBytes <= 0 || notebookManifest.UploadGuide != "UPLOAD.md" {
			t.Fatalf("invalid NotebookLM manifest: %+v", notebookManifest)
		}
		guide, err := os.ReadFile(filepath.Join(output, "notebooklm", "UPLOAD.md"))
		if err != nil || !bytes.Contains(guide, []byte("Recommended upload order")) || !bytes.Contains(guide, []byte(markdownText(bundle.Snapshot.ID))) || !bytes.Contains(guide, []byte("NeuroForgeIO")) || !bytes.Contains(guide, []byte("MIT License")) {
			t.Fatalf("invalid NotebookLM upload guide: %q (error %v)", guide, err)
		}
		overview, err := os.ReadFile(filepath.Join(output, "notebooklm", "00_repository_overview.md"))
		if err != nil || !bytes.Contains(overview, []byte("01_coverage_and_diagnostics.md")) ||
			bytes.Contains(overview, []byte("(coverage.md)")) || bytes.Contains(overview, []byte("(symbols/)")) ||
			bytes.Contains(overview, []byte("commit: \"\"")) {
			t.Fatalf("invalid pack-local NotebookLM overview: %q (error %v)", overview, err)
		}
		symbolPack, err := os.ReadFile(filepath.Join(output, "notebooklm", "02_symbols_001.md"))
		if err != nil || bytes.Count(symbolPack, []byte(untrustedRepositoryDataNotice)) != 1 {
			t.Fatalf("NotebookLM symbol pack trust notice count = %d (error %v)", bytes.Count(symbolPack, []byte(untrustedRepositoryDataNotice)), err)
		}
	}
	var firstManifest, secondManifest exportManifest
	readExportJSON(t, filepath.Join(first, "rkc-export-manifest.json"), &firstManifest)
	readExportJSON(t, filepath.Join(second, "rkc-export-manifest.json"), &secondManifest)
	if firstManifest.CanonicalFilesDigest != secondManifest.CanonicalFilesDigest || firstManifest.CanonicalBytes != secondManifest.CanonicalBytes {
		t.Fatalf("identical exports differ: %+v vs %+v", firstManifest, secondManifest)
	}
}

func TestWriteAllHonorsFeatureDisables(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := []byte("plain\n")
	if err := os.WriteFile(filepath.Join(root, "plain.txt"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := exportFixture(root, "plain.txt", source)
	output := filepath.Join(t.TempDir(), "minimal")
	options := Options{Root: root, Output: output, DisableStaticSite: true, DisableJSONLGraph: true, DisableSearchIndex: true, DisableIntegrations: true}
	if err := WriteAll(bundle, model.BuildCoverage(bundle), options); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"site", "graph", "search", "integrations", "normalized"} {
		if _, err := os.Stat(filepath.Join(output, name)); !os.IsNotExist(err) {
			t.Errorf("disabled output %s exists: %v", name, err)
		}
	}
	var policy exportPolicy
	readExportJSON(t, filepath.Join(output, "rkc.export-policy.json"), &policy)
	if policy.StaticSite || policy.JSONLGraph || policy.SearchIndex || policy.IntegrationExports || policy.NormalizedSources || !policy.SecretRedaction || policy.NotebookMaximumBytes != 4_000_000 {
		t.Fatalf("bad policy: %+v", policy)
	}
}

func TestNormalizedSourcesRejectTraversalSymlinksAndTOCTOU(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir()
	outsidePath := filepath.Join(outside, "secret.txt")
	outsideData := []byte("api_" + "key=outside-secret-material\n")
	if err := os.WriteFile(outsidePath, outsideData, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "atlas")

	for _, bad := range []string{"../secret.txt", "/absolute.txt", "a/../secret.txt", `a\\secret.txt`, ".", ""} {
		bundle := exportFixture(root, bad, outsideData)
		err := writeNormalizedSources(bundle, Options{Root: root, Output: output, IncludeSources: true})
		if err == nil {
			t.Errorf("path %q unexpectedly accepted", bad)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(output), "secret.txt.md")); !os.IsNotExist(err) {
		t.Fatalf("traversal created an external file: %v", err)
	}

	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	bundle := exportFixture(root, "linked/secret.txt", outsideData)
	if err := writeNormalizedSources(bundle, Options{Root: root, Output: filepath.Join(t.TempDir(), "source-link")}); err == nil || !strings.Contains(err.Error(), "escapes repository root") {
		t.Fatalf("source symlink escape error = %v", err)
	}

	sourceDir := filepath.Join(root, "safe")
	if err := os.MkdirAll(sourceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	safeData := []byte("safe\n")
	if err := os.WriteFile(filepath.Join(sourceDir, "file.txt"), safeData, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle = exportFixture(root, "safe/file.txt", safeData)
	output = filepath.Join(t.TempDir(), "output-link")
	if err := os.MkdirAll(filepath.Join(output, "normalized"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(output, "normalized", "safe")); err != nil {
		t.Fatal(err)
	}
	if err := writeNormalizedSources(bundle, Options{Root: root, Output: output}); err == nil || !strings.Contains(err.Error(), "escapes normalized-source root") {
		t.Fatalf("output symlink escape error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "file.txt.md")); !os.IsNotExist(err) {
		t.Fatalf("output symlink wrote outside: %v", err)
	}

	tampered := exportFixture(root, "safe/file.txt", []byte("old\n"))
	if err := writeNormalizedSources(tampered, Options{Root: root, Output: filepath.Join(t.TempDir(), "tampered")}); err == nil || !strings.Contains(err.Error(), "content changed after inventory") {
		t.Fatalf("expected source TOCTOU rejection, got %v", err)
	}
}

func TestExportFormattingAndIntegrationHelpers(t *testing.T) {
	t.Parallel()
	if !strings.Contains(siteHTML, "NeuroForgeIO") || !strings.Contains(siteHTML, "MIT") {
		t.Fatal("static atlas is missing NeuroForgeIO/MIT attribution")
	}
	bundle := exportFixture(t.TempDir(), "x.go", []byte("x"))
	coverage := model.BuildCoverage(bundle)
	if overview := repositoryOverview(bundle, coverage); !strings.Contains(overview, "Repository atlas") || !strings.Contains(overview, "Provenance") || !strings.Contains(overview, bundle.Snapshot.Git.Commit) {
		t.Fatalf("overview = %q", overview)
	}
	if overview := notebookRepositoryOverview(bundle, coverage); !strings.Contains(overview, bundle.Snapshot.Git.Commit) {
		t.Fatalf("NotebookLM overview omitted commit provenance: %q", overview)
	}
	if report := coverageMarkdown(coverage); !strings.Contains(report, "Coverage and confidence") {
		t.Fatalf("coverage report = %q", report)
	}
	if got := notebookDiagnostics(model.Bundle{}, coverage); !strings.Contains(got, "No diagnostics") {
		t.Fatalf("no-diagnostics report = %q", got)
	}
	if got := safeFilename("../a:b c"); strings.ContainsAny(got, "/\\:") || got == "" {
		t.Fatalf("unsafe filename = %q", got)
	}
	if fenceLanguage("typescript") != "typescript" || fenceLanguage("unknown") != "unknown" || fenceLanguage("shell") != "bash" {
		t.Fatal("fence language mismatch")
	}
	if markdownCell("a|b\nc") != "a\\|b c" {
		t.Fatalf("markdownCell = %q", markdownCell("a|b\nc"))
	}
	if value, ok := stringAttribute(map[string]any{"x": " y "}, "x"); !ok || value != " y " {
		t.Fatalf("stringAttribute = %q %v", value, ok)
	}
	if _, ok := stringAttribute(map[string]any{"x": 1}, "x"); ok {
		t.Fatal("non-string attribute accepted")
	}
	if firstSentence(" First sentence. Second.") != "First sentence." || firstSentence("  ") != "RKC diagnostic" {
		t.Fatal("firstSentence mismatch")
	}
	for input, want := range map[string]string{"fatal": "error", "error": "error", "warning": "warning", "note": "note"} {
		if got := sarifLevel(input); got != want {
			t.Errorf("sarifLevel(%q)=%q want %q", input, got, want)
		}
	}
	output := t.TempDir()
	if err := writePacks(output, "pack", "Title", []string{strings.Repeat("a", 220), strings.Repeat("b", 220)}, 250); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, "pack_002.md")); err != nil {
		t.Fatalf("pack split missing: %v", err)
	}
	if err := writeCSV(filepath.Join(output, "bad.csv"), []string{"h"}, func(*csv.Writer) error { return errors.New("row failure") }); err == nil {
		t.Fatal("writeCSV swallowed row error")
	}
}

func TestNotebookSourceInventoryIsSortedAndRejectsLinks(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "02_symbols_001.md"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "00_repository_overview.md"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "UPLOAD.md"), []byte("guide"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "notes.txt"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, total, max, err := notebookSourceInventory(directory)
	if err != nil || len(sources) != 2 || sources[0].Path != "00_repository_overview.md" || sources[1].Path != "02_symbols_001.md" || total != 6 || max != 3 {
		t.Fatalf("inventory = %+v total=%d max=%d err=%v", sources, total, max, err)
	}
	if err := os.Symlink(filepath.Join(directory, "00_repository_overview.md"), filepath.Join(directory, "linked.md")); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := notebookSourceInventory(directory); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("symlink inventory error = %v", err)
	}
}

func TestNotebookPacksAreSortedForHumanAndAgentNavigation(t *testing.T) {
	t.Parallel()
	bundle := model.Bundle{
		Nodes: []model.Node{
			{ID: "z-node", Kind: "function", Name: "Zulu"},
			{ID: "a-node", Kind: "function", Name: "Alpha"},
		},
		Edges: []model.Edge{
			{ID: "a-edge", Kind: "calls", From: "z-node", To: "a-node", Resolution: "declared"},
			{ID: "z-edge", Kind: "calls", From: "a-node", To: "z-node", Resolution: "declared"},
		},
	}
	directory := t.TempDir()
	if err := writeNotebookSymbolPacks(directory, bundle, 1_000_000); err != nil {
		t.Fatal(err)
	}
	if err := writeNotebookRelationPacks(directory, bundle, 1_000_000); err != nil {
		t.Fatal(err)
	}
	symbols, err := os.ReadFile(filepath.Join(directory, "02_symbols_001.md"))
	if err != nil {
		t.Fatal(err)
	}
	if alpha, zulu := bytes.Index(symbols, []byte("## Alpha")), bytes.Index(symbols, []byte("## Zulu")); alpha < 0 || zulu <= alpha {
		t.Fatalf("symbol pack is not name-sorted:\n%s", symbols)
	}
	relations, err := os.ReadFile(filepath.Join(directory, "03_relationships_001.md"))
	if err != nil {
		t.Fatal(err)
	}
	if alpha, zulu := bytes.Index(relations, []byte("From: Alpha")), bytes.Index(relations, []byte("From: Zulu")); alpha < 0 || zulu <= alpha {
		t.Fatalf("relationship pack is not source-sorted:\n%s", relations)
	}
}

func TestBrowserAssetsAccessibilityAndSerializationContract(t *testing.T) {
	t.Parallel()
	bundle := exportFixture(t.TempDir(), "x.go", []byte("package x\n"))
	coverage := model.BuildCoverage(bundle)
	assets, err := BrowserAssets(bundle, coverage)
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 5 {
		t.Fatalf("browser asset count = %d, want 5", len(assets))
	}
	for name, markers := range map[string][]string{
		"index.html": {
			`class="skip-link"`, `role="tablist"`, `role="tabpanel"`,
			`aria-live="polite"`, `data-view="commands"`, `<noscript>`,
		},
		"styles.css": {
			":focus-visible", "prefers-reduced-motion", "prefers-contrast",
			"forced-colors", "@media (max-width: 560px)", ".coverage-grid",
			"max-height: min(48vh, 420px)", ".command-layout", ".danger",
			".job-meta",
		},
		"app.js": {
			"handleListKeys", "clearFilters", "aria-selected", "role=\"progressbar\"",
			"role=\"button\"", "atlas data is incomplete", "renderSelectionPrompt",
			"<caption class=\"sr-only\">", "<th scope=\"col\">Category</th>",
			"<th scope=\"row\">", "argument.default??''", "Object.create(null)",
			"event.key==='Enter'||event.key===' '", "Not applicable", "coverage.claims_total?",
			"/api/v1/nodes?limit=120", "runWorkbenchCommand", "parseCommandArguments",
			"cancelWorkbenchJob", "cleanup_failed", "deadline_at",
			`"name":"runs"`, `"default_args":["--coverage",".rkc/coverage.json"]`,
			`"default_args":["--dir",".rkc","resource guard"]`,
			"--scip-index /path/index.scip", "Compiler evidence",
			"Safe CLI workflows", "executes only its explicit allowlist",
			"./data/bootstrap.json", "ensureFullStaticData",
			"Loading complete offline details",
		},
	} {
		content := string(assets[name])
		for _, marker := range markers {
			if !strings.Contains(content, marker) {
				t.Errorf("%s is missing accessibility marker %q", name, marker)
			}
		}
		if strings.Contains(content, "onclick=") {
			t.Errorf("%s contains an inline event handler", name)
		}
	}
	app := string(assets["app.js"])
	if strings.Contains(app, "__RKC_COMMAND_CATALOG__") || strings.Contains(app, "const defaults={") {
		t.Fatal("browser retained an unresolved or independently hard-coded command catalogue")
	}
	renderListStart := strings.Index(app, "function renderList(){")
	renderListEnd := strings.Index(app, "function renderOverview(){")
	if renderListStart < 0 || renderListEnd <= renderListStart {
		t.Fatal("browser renderList implementation is missing")
	}
	renderList := app[renderListStart:renderListEnd]
	apiRanking := strings.Index(renderList, "if(state.api){")
	apiAppend := strings.Index(renderList, "for(const node of state.bundle.nodes)candidates.push({node,score:0})")
	localHaystack := strings.Index(renderList, "const haystack=[node.id,node.name")
	if apiRanking < 0 || apiAppend < apiRanking || localHaystack < apiAppend {
		t.Fatal("browser does not preserve authoritative API search results before local static filtering")
	}
	if !strings.Contains(renderList, "node.source?.path") || !strings.Contains(renderList, "Object.values(node.attributes||{})") {
		t.Fatal("static browser search omits source paths or documentation attributes")
	}
	var data struct {
		Bundle   model.Bundle   `json:"bundle"`
		Coverage model.Coverage `json:"coverage"`
	}
	if err := json.Unmarshal(assets["data/atlas.json"], &data); err != nil {
		t.Fatalf("decode browser data: %v", err)
	}
	if data.Bundle.Snapshot.ID != bundle.Snapshot.ID || data.Coverage.SymbolsTotal != coverage.SymbolsTotal {
		t.Fatalf("browser data binding mismatch: snapshot=%q symbols=%d", data.Bundle.Snapshot.ID, data.Coverage.SymbolsTotal)
	}
	var bootstrap struct {
		Bundle          model.Bundle              `json:"bundle"`
		Coverage        model.Coverage            `json:"coverage"`
		Facets          map[string]map[string]int `json:"facets"`
		StaticBootstrap bool                      `json:"static_bootstrap"`
		ListTruncated   bool                      `json:"list_truncated"`
	}
	if err := json.Unmarshal(assets["data/bootstrap.json"], &bootstrap); err != nil {
		t.Fatalf("decode browser bootstrap: %v", err)
	}
	if !bootstrap.StaticBootstrap || bootstrap.Bundle.Snapshot.ID != bundle.Snapshot.ID || bootstrap.Coverage.SymbolsTotal != coverage.SymbolsTotal || len(bootstrap.Bundle.Nodes) > staticBootstrapNodeLimit || len(bootstrap.Bundle.Edges) != 0 || len(bootstrap.Bundle.Evidence) != 0 {
		t.Fatalf("invalid bounded browser bootstrap: %+v", bootstrap)
	}
	if len(assets["data/bootstrap.json"]) >= len(assets["data/atlas.json"]) {
		t.Fatalf("browser bootstrap is not smaller than full atlas: %d >= %d", len(assets["data/bootstrap.json"]), len(assets["data/atlas.json"]))
	}

	invalid := bundle
	invalid.Nodes[0].Attributes = map[string]any{"channel": make(chan int)}
	if _, err := BrowserAssets(invalid, coverage); err == nil || !strings.Contains(err.Error(), "canonicalize export bundle") {
		t.Fatalf("BrowserAssets invalid bundle error = %v", err)
	}
	output := filepath.Join(t.TempDir(), "invalid-output")
	if err := WriteAll(invalid, coverage, Options{Root: t.TempDir(), Output: output}); err == nil || !strings.Contains(err.Error(), "canonicalize export bundle") {
		t.Fatalf("WriteAll invalid bundle error = %v", err)
	}
	if _, err := os.Stat(output); !os.IsNotExist(err) {
		t.Fatalf("invalid export created output before canonical validation: %v", err)
	}
}

func TestSiteAssetsOmitStaticAtlasData(t *testing.T) {
	t.Parallel()
	assets, err := SiteAssets()
	if err != nil {
		t.Fatal(err)
	}
	if len(assets) != 3 {
		t.Fatalf("live site asset count = %d, want 3", len(assets))
	}
	if _, ok := assets["data/atlas.json"]; ok {
		t.Fatal("live site assets retained the full static atlas payload")
	}
	if _, ok := assets["data/bootstrap.json"]; ok {
		t.Fatal("live site assets retained the static bootstrap payload")
	}
	for _, name := range []string{"index.html", "styles.css", "app.js"} {
		if len(assets[name]) == 0 {
			t.Errorf("live site asset %q is missing or empty", name)
		}
	}
}

func TestExportRejectsSensitiveRepositoryProvenanceBeforeWriting(t *testing.T) {
	const sentinel = "EXPORT_ORIGIN_SECRET_SENTINEL"
	bundle := exportFixture(t.TempDir(), "source.go", []byte("package fixture\n"))
	bundle.Snapshot.Git.Origin = "https://alice:" + sentinel + "@example.test/Owner/Repo.git?token=" + sentinel
	bundle.Snapshot.Metadata = map[string]string{"source_reference": bundle.Snapshot.Git.Origin}
	output := filepath.Join(t.TempDir(), "atlas")
	err := WriteAll(bundle, model.BuildCoverage(bundle), Options{Output: output})
	if err == nil {
		t.Fatal("sensitive repository provenance was exported")
	}
	if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), "alice") {
		t.Fatalf("export error disclosed repository provenance: %q", err)
	}
	if _, statErr := os.Stat(output); !os.IsNotExist(statErr) {
		t.Fatalf("invalid provenance created output before rejection: %v", statErr)
	}
}

func TestMarkdownGenerationContainsAdversarialRepositoryText(t *testing.T) {
	t.Parallel()
	payload := "declared text\n``````\n<script>alert('not inert')</script>\n# injected heading"
	block := markdownFencedBlock(payload, "go` onmouseover=<script>")
	openingEnd := strings.IndexByte(block, '\n')
	if openingEnd < 0 {
		t.Fatalf("fenced block has no opening line: %q", block)
	}
	opening := block[:openingEnd]
	fenceLength := 0
	for fenceLength < len(opening) && opening[fenceLength] == '`' {
		fenceLength++
	}
	fence := strings.Repeat("`", fenceLength)
	if fenceLength != 7 {
		t.Fatalf("fence length = %d, want 7: %q", fenceLength, opening)
	}
	if strings.Contains(opening[fenceLength:], "`") || strings.ContainsAny(opening[fenceLength:], " <>='") {
		t.Fatalf("unsafe fence info string: %q", opening)
	}
	if !strings.Contains(block, payload) || !strings.HasSuffix(block, "\n"+fence+"\n") {
		t.Fatalf("payload was not enclosed by the dynamic fence: %q", block)
	}

	malicious := "name|cell\n## forged <script>& [link](javascript:alert(1)) `code`"
	wantEscaped := "name\\|cell \\#\\# forged &lt;script&gt;&amp; \\[link\\]\\(javascript:alert\\(1\\)\\) \\`code\\`"
	if got := markdownText(malicious); got != wantEscaped {
		t.Fatalf("markdownText = %q, want %q", got, wantEscaped)
	}

	node := model.Node{
		ID:            "node|`id`",
		Kind:          "function|forged",
		Language:      "go`unsafe",
		Visibility:    "public\n| forged | row |",
		QualifiedName: malicious,
		Signature:     payload,
		Source:        &model.SourceRange{Path: "source|name.go", StartLine: 1, EndLine: 2},
		EvidenceIDs:   []string{"evidence\n- forged", "`second`"},
		Attributes: map[string]any{
			"docstring": payload,
			"arguments": []any{map[string]any{
				"name": "x|y", "kind": "parameter\n| forged", "type": "<img src=x>", "required": "yes|no", "default": "[bad](javascript:x)",
			}},
		},
	}
	first := symbolMarkdown(node, nil, nil, nil)
	second := symbolMarkdown(node, nil, nil, nil)
	if first != second {
		t.Fatal("Markdown rendering is not deterministic")
	}
	for _, expected := range []string{
		"# " + markdownText(malicious),
		untrustedRepositoryDataNotice,
		"| Kind | function\\|forged |",
		"| Visibility | public \\| forged \\| row \\| |",
		"## Signature (repository-provided)",
		markdownFencedBlock(payload, node.Language),
		"## Declared documentation (repository-provided)",
		markdownFencedBlock(payload, "text"),
		"| x\\|y | parameter \\| forged | &lt;img src=x&gt; | yes\\|no | \\[bad\\]\\(javascript:x\\) |",
		"- evidence \\- forged",
	} {
		if !strings.Contains(first, expected) {
			t.Errorf("generated symbol Markdown lacks %q:\n%s", expected, first)
		}
	}
	if strings.Contains(first, "\n## forged <script>") || strings.Contains(first, "\n| forged | row |\n") {
		t.Fatalf("repository text escaped its intended Markdown context:\n%s", first)
	}
}

func TestNormalizedSourceUsesInertDynamicFence(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	source := []byte("before\n`````\n</code><script>alert(1)</script>\nafter\n")
	if err := os.WriteFile(filepath.Join(root, "hostile.go"), source, 0o600); err != nil {
		t.Fatal(err)
	}
	bundle := exportFixture(root, "hostile.go", source)
	output := filepath.Join(t.TempDir(), "atlas")
	if err := writeNormalizedSources(bundle, Options{Root: root, Output: output}); err != nil {
		t.Fatal(err)
	}
	generated, err := os.ReadFile(filepath.Join(output, "normalized", "hostile.go.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(generated)
	for _, expected := range []string{
		"# Normalized repository source",
		untrustedRepositoryDataNotice,
		"Repository path: hostile\\.go",
		"## Repository-provided source",
		markdownFencedBlock(string(source), "go"),
	} {
		if !strings.Contains(text, expected) {
			t.Errorf("normalized source lacks %q:\n%s", expected, text)
		}
	}
}

func TestExportLowLevelErrors(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAll(model.Bundle{}, model.Coverage{}, Options{Output: file}); err == nil {
		t.Fatal("expected output-directory error")
	}
	if err := writeJSON(filepath.Join(root, "bad.json"), make(chan int)); err == nil || !strings.Contains(err.Error(), "marshal") {
		t.Fatalf("expected marshal error, got %v", err)
	}
	if err := writeJSONL(filepath.Join(root, "missing", "values.jsonl"), []int{1}); err == nil {
		t.Fatal("expected create error")
	}
	if err := Copy(io.Discard, strings.NewReader("content")); err != nil {
		t.Fatal(err)
	}
	if err := Copy(errorWriter{}, strings.NewReader("content")); err == nil {
		t.Fatal("Copy swallowed writer error")
	}
	if err := Copy(io.Discard, errorReader{}); err == nil {
		t.Fatal("Copy swallowed reader error")
	}
}

func exportFixture(root, artifactPath string, source []byte) model.Bundle {
	digest := sha256.Sum256(source)
	artifact := model.Artifact{ID: "artifact-1", ContentID: "content-1", LogicalID: "logical-artifact", Path: artifactPath, Kind: "file", Language: "go", MediaType: "text/plain", SHA256: hex.EncodeToString(digest[:]), SizeBytes: int64(len(source)), LineCount: 3, Text: true, Status: "syntax_parsed"}
	evidence := model.Evidence{ID: "evidence-1", Kind: "syntax_derived", Method: "test", Confidence: 1, Source: &model.SourceRange{ArtifactID: artifact.ID, Path: artifactPath, StartLine: 1, EndLine: 3}, Tool: "test", ToolVersion: "1"}
	node := model.Node{ID: "function-1", LogicalID: "logical-function", Kind: "function", Name: "Login", QualifiedName: "fixture.Login", Signature: "func Login()", Language: "go", Visibility: "public", PublicSurface: true, ArtifactID: artifact.ID, Source: evidence.Source, EvidenceIDs: []string{evidence.ID}, Attributes: map[string]any{"docstring": "Logs a user in.", "arguments": []any{map[string]any{"name": "user", "kind": "parameter", "type": "string", "required": true}}}}
	repository := model.Node{ID: "repository-1", LogicalID: "logical-repository", Kind: "repository", Name: "fixture", QualifiedName: "fixture", Visibility: "repository"}
	edge := model.Edge{ID: "edge-1", Kind: "contains", From: repository.ID, To: node.ID, Resolution: "declared", Confidence: 1, Producer: "test", EvidenceIDs: []string{evidence.ID}}
	diagnostic := model.Diagnostic{ID: "diagnostic-1", Severity: "warning", Code: "RKC-TEST", Message: "First sentence. More detail.", Stage: "test", Plugin: "test", Source: evidence.Source, HelpURI: "https://example.invalid/help"}
	return model.Bundle{
		Snapshot:  model.Snapshot{SchemaVersion: model.SchemaVersion, ID: "snapshot-1", RepositoryID: repository.ID, CreatedAt: time.Unix(1, 0).UTC(), Status: "committed", RootName: "fixture", RootPath: root, ContentDigest: "digest", Git: model.GitInfo{Commit: strings.Repeat("a", 40)}, Tool: model.ToolInfo{Name: "rkc", Version: "test"}},
		Artifacts: []model.Artifact{artifact}, Nodes: []model.Node{repository, node}, Edges: []model.Edge{edge}, Evidence: []model.Evidence{evidence}, Diagnostics: []model.Diagnostic{diagnostic},
	}
}

func readExportJSON(t *testing.T, path string, target any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, target); err != nil {
		t.Fatal(err)
	}
}

type errorWriter struct{}

func (errorWriter) Write([]byte) (int, error) { return 0, errors.New("write failure") }

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) { return 0, errors.New("read failure") }
