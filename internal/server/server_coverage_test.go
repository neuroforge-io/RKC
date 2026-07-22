package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	rkcexport "github.com/neuroforge-io/RKC/internal/export"
	graphindex "github.com/neuroforge-io/RKC/internal/graph"
	"github.com/neuroforge-io/RKC/internal/model"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/internal/search"
)

func TestHandlerAllAPIRoutesAndSecurityHeaders(t *testing.T) {
	dataset := richDataset()
	handler := dataset.Handler()
	tests := []struct {
		name       string
		url        string
		wantStatus int
		contains   []string
	}{
		{"health", "/api/v1/health", 200, []string{"snapshot-rich", "search_index_version"}},
		{"manifest", "/api/v1/manifest", 200, []string{"snapshot-rich"}},
		{"coverage", "/api/v1/coverage", 200, []string{"snapshot-rich"}},
		{"artifacts filtered", "/api/v1/artifacts?language=go&status=syntax_parsed&path_prefix=src/&limit=9", 200, []string{"src/a.go"}},
		{"artifact", "/api/v1/artifacts/artifact-a", 200, []string{"src/a.go", "Alpha"}},
		{"artifact missing", "/api/v1/artifacts/missing", 404, []string{"Artifact not found", "application/problem"}},
		{"nodes listed", "/api/v1/nodes?kind=function&language=go&limit=10", 200, []string{"Alpha", "Beta"}},
		{"nodes searched", "/api/v1/nodes?q=Alpha&kind=function&language=go", 200, []string{"retrieval", "pkg.Alpha"}},
		{"node", "/api/v1/nodes/a", 200, []string{"incoming_edges", "evidence-a"}},
		{"node missing", "/api/v1/nodes/missing", 404, []string{"Node not found"}},
		{"edges", "/api/v1/edges?kind=calls&from=a&to=b&resolution=DECLARED", 200, []string{"edge-ab"}},
		{"evidence", "/api/v1/evidence/evidence-a", 200, []string{"syntax_inferred"}},
		{"evidence missing", "/api/v1/evidence/missing", 404, []string{"Evidence not found"}},
		{"diagnostics", "/api/v1/diagnostics?severity=warning&code=RKC-TEST", 200, []string{"diagnostic-a"}},
		{"search", "/api/v1/search?q=Alpha&kinds=function&languages=go&object_types=node&path_prefix=src&limit=1", 200, []string{"pkg.Alpha", "lexical"}},
		{"search missing query", "/api/v1/search", 400, []string{"Missing query"}},
		{"neighborhood", "/api/v1/graph/neighborhood?node_id=a&direction=outgoing&max_depth=2&max_nodes=10&edge_kinds=calls", 200, []string{"edge-ab", "Beta"}},
		{"neighborhood missing seed", "/api/v1/graph/neighborhood", 400, []string{"Missing node"}},
		{"neighborhood unknown", "/api/v1/graph/neighborhood?node_id=missing", 404, []string{"Seed node not found"}},
		{"path", "/api/v1/graph/path?from=a&to=c&max_depth=3&direction=outgoing", 200, []string{"edge-ab", "edge-bc"}},
		{"path missing", "/api/v1/graph/path?from=a", 400, []string{"Missing endpoints"}},
		{"path unknown", "/api/v1/graph/path?from=a&to=missing", 404, []string{"Path endpoint not found"}},
		{"impact", "/api/v1/impact?node_id=c&max_depth=3", 200, []string{"Alpha", "Beta"}},
		{"impact missing", "/api/v1/impact", 400, []string{"Missing node"}},
		{"impact unknown", "/api/v1/impact?node_id=missing", 404, []string{"Seed node not found"}},
		{"components", "/api/v1/graph/components?cyclic=true&edge_kinds=calls", 200, []string{"cyclic", "a", "b", "c"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.url, nil))
			if response.Code != tc.wantStatus {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.wantStatus, response.Body.String())
			}
			for _, value := range tc.contains {
				if !strings.Contains(response.Body.String(), value) && !(value == "application/problem" && strings.Contains(response.Header().Get("Content-Type"), value)) {
					t.Errorf("body/header does not contain %q: headers=%v body=%s", value, response.Header(), response.Body.String())
				}
			}
			for header, want := range map[string]string{"X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "X-Frame-Options": "DENY"} {
				if got := response.Header().Get(header); got != want {
					t.Errorf("%s=%q want %q", header, got, want)
				}
			}
			if response.Header().Get("Content-Security-Policy") == "" || response.Header().Get("Permissions-Policy") == "" {
				t.Error("missing security policy headers")
			}
		})
	}
}

func TestHandlerIsRaceSafeAcrossConcurrentRequests(t *testing.T) {
	handler := richDataset().Handler()
	urls := []string{
		"/api/v1/health", "/api/v1/nodes?q=Alpha", "/api/v1/search?q=Beta",
		"/api/v1/graph/neighborhood?node_id=a", "/api/v1/graph/path?from=a&to=c", "/api/v1/impact?node_id=c",
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 12; worker++ {
		for _, url := range urls {
			wg.Add(1)
			go func(url string) {
				defer wg.Done()
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, url, nil))
				if response.Code != http.StatusOK {
					t.Errorf("%s: status=%d body=%s", url, response.Code, response.Body.String())
				}
			}(url)
		}
	}
	wg.Wait()
}

func TestPaginationReportsExactFilteredTotals(t *testing.T) {
	t.Parallel()
	handler := richDataset().Handler()
	cases := []struct {
		url           string
		wantTotal     int
		wantItems     int
		wantTruncated bool
	}{
		{"/api/v1/artifacts?language=go&limit=1", 1, 1, false},
		{"/api/v1/nodes?kind=function&limit=3", 3, 3, false},
		{"/api/v1/nodes?kind=function&limit=2", 3, 2, true},
		{"/api/v1/edges?kind=calls&limit=3", 3, 3, false},
		{"/api/v1/diagnostics?severity=warning&limit=1", 1, 1, false},
		{"/api/v1/graph/components?cyclic=true&limit=1", 1, 1, false},
	}
	for _, tc := range cases {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, tc.url, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", tc.url, response.Code, response.Body.String())
		}
		var payload struct {
			Items     []json.RawMessage `json:"items"`
			Total     int               `json:"total"`
			Truncated bool              `json:"truncated"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Total != tc.wantTotal || len(payload.Items) != tc.wantItems || payload.Truncated != tc.wantTruncated {
			t.Errorf("%s => total=%d items=%d truncated=%v, want %d/%d/%v", tc.url, payload.Total, len(payload.Items), payload.Truncated, tc.wantTotal, tc.wantItems, tc.wantTruncated)
		}
	}
}

func TestLoadCanonicalSiteFallbackAndErrors(t *testing.T) {
	t.Parallel()
	bundle := richDataset().Bundle
	root := writeVerifiedServerAtlas(t, bundle)
	loaded, err := Load(filepath.Join(root, "site"))
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Root != root || loaded.Manifest.ID != bundle.Snapshot.ID || len(loaded.NodeByID) != len(bundle.Nodes) || loaded.Search == nil || loaded.Graph == nil || loaded.Integrity != IntegrityVerified {
		t.Fatalf("loaded dataset = %+v", loaded)
	}

	legacyRelease := writeVerifiedServerAtlas(t, bundle)
	if err := os.Remove(filepath.Join(legacyRelease, safeoutput.MarkerName)); err != nil {
		t.Fatal(err)
	}
	loaded, err = Load(legacyRelease)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Integrity != IntegrityVerifiedLegacyUnmarked {
		t.Fatalf("legacy release integrity = %q", loaded.Integrity)
	}

	incomplete := t.TempDir()
	writeServerJSON(t, filepath.Join(incomplete, "bundle.json"), bundle)
	writeServerJSON(t, filepath.Join(incomplete, "rkc.manifest.json"), bundle.Snapshot)
	writeServerJSON(t, filepath.Join(incomplete, "coverage.json"), model.BuildCoverage(bundle))
	if _, err := Load(incomplete); err == nil || !strings.Contains(err.Error(), "missing its export manifest") {
		t.Fatalf("expected incomplete canonical dataset error, got %v", err)
	}

	invalid := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalid, "bundle.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(invalid); err == nil || !strings.Contains(err.Error(), "load bundle") {
		t.Fatalf("expected invalid bundle error, got %v", err)
	}
	missing := t.TempDir()
	if _, err := Load(missing); err == nil || !strings.Contains(err.Error(), "load manifest") {
		t.Fatalf("expected missing dataset error, got %v", err)
	}
}

func TestVerifiedStaticSiteIsCapturedAndCannotBeReplacedAfterLoad(t *testing.T) {
	t.Parallel()
	root := writeVerifiedServerAtlas(t, richDataset().Bundle)
	indexPath := filepath.Join(root, "site", "index.html")
	original, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	dataset, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(dataset.staticSite) == 0 {
		t.Fatal("verified static site was not captured")
	}
	assertIndex := func(t *testing.T, url string) {
		t.Helper()
		response := httptest.NewRecorder()
		dataset.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, url, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status=%d body=%s", url, response.Code, response.Body.String())
		}
		if !bytes.Equal(response.Body.Bytes(), original) {
			t.Fatalf("%s served bytes outside the verified capture", url)
		}
		if response.Header().Get("ETag") == "" || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("%s missing immutable identity or security headers: %v", url, response.Header())
		}
	}
	assertIndex(t, "/")
	assertIndex(t, "/client/side/route")

	if err := os.WriteFile(indexPath, []byte("replaced after verification"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertIndex(t, "/")

	t.Run("symlink replacement", func(t *testing.T) {
		if err := os.Remove(indexPath); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(t.TempDir(), "untrusted.html")
		if err := os.WriteFile(outside, []byte("symlink target after verification"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, indexPath); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		assertIndex(t, "/")
	})

	response := httptest.NewRecorder()
	dataset.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/not-a-route", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown API route fell through to SPA: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLoadFailsClosedOnTampering(t *testing.T) {
	t.Parallel()
	bundle := richDataset().Bundle
	t.Run("file digest", func(t *testing.T) {
		root := writeVerifiedServerAtlas(t, bundle)
		path := filepath.Join(root, "bundle.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, ' '), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "does not match its size or sha256") {
			t.Fatalf("tampered bundle error = %v", err)
		}
	})
	t.Run("unmanifested file", func(t *testing.T) {
		root := writeVerifiedServerAtlas(t, bundle)
		if err := os.WriteFile(filepath.Join(root, "injected.txt"), []byte("untrusted"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "absent from rkc-export-manifest.json") {
			t.Fatalf("unmanifested file error = %v", err)
		}
	})
	t.Run("marker snapshot", func(t *testing.T) {
		root := writeVerifiedServerAtlas(t, bundle)
		writeServerJSON(t, filepath.Join(root, safeoutput.MarkerName), safeoutput.Marker{
			SchemaVersion: "1.0", Producer: "rkc", Kind: "atlas", SnapshotID: "different-snapshot",
		})
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "ownership marker snapshot") {
			t.Fatalf("marker mismatch error = %v", err)
		}
	})
	t.Run("bundle snapshot", func(t *testing.T) {
		root := writeVerifiedServerAtlas(t, bundle)
		changed := bundle
		changed.Snapshot.Status = "failed"
		writeServerJSON(t, filepath.Join(root, "bundle.json"), changed)
		rewriteServerExportManifest(t, root, bundle.Snapshot.ID)
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "bundle snapshot does not match") {
			t.Fatalf("snapshot mismatch error = %v", err)
		}
	})
	t.Run("invalid graph", func(t *testing.T) {
		root := writeVerifiedServerAtlas(t, bundle)
		changed := bundle
		changed.Edges = append([]model.Edge(nil), bundle.Edges...)
		changed.Edges[0].To = "missing-node"
		writeServerJSON(t, filepath.Join(root, "bundle.json"), changed)
		rewriteServerExportManifest(t, root, bundle.Snapshot.ID)
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "validate bundle") || !strings.Contains(err.Error(), "edge target missing") {
			t.Fatalf("invalid graph error = %v", err)
		}
	})
	t.Run("coverage mismatch", func(t *testing.T) {
		root := writeVerifiedServerAtlas(t, bundle)
		coverage := model.BuildCoverage(bundle)
		coverage.SnapshotID = "different-snapshot"
		writeServerJSON(t, filepath.Join(root, "coverage.json"), coverage)
		rewriteServerExportManifest(t, root, bundle.Snapshot.ID)
		if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "coverage.json does not match") {
			t.Fatalf("coverage mismatch error = %v", err)
		}
	})
}

func TestLoadRebuildsPersistedSearchIndexFromValidatedBundle(t *testing.T) {
	t.Parallel()
	bundle := richDataset().Bundle
	root := writeVerifiedServerAtlas(t, bundle)
	stale := search.Build([]search.Document{{ID: "injected", ObjectType: "node", Title: "Injected"}})
	if err := stale.Save(filepath.Join(root, "search", "index.json")); err != nil {
		t.Fatal(err)
	}
	rewriteServerExportManifest(t, root, bundle.Snapshot.ID)
	loaded, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	response := loaded.Search.Search(search.Query{Text: "Alpha", Limit: 10})
	if len(response.Hits) == 0 || response.Hits[0].Document.ID != "a" {
		t.Fatalf("rebuilt search results = %+v", response.Hits)
	}
	if injected := loaded.Search.Search(search.Query{Text: "Injected", Limit: 10}); len(injected.Hits) != 0 {
		t.Fatalf("persisted stale index was trusted: %+v", injected.Hits)
	}
}

func TestLoadLegacyBundle(t *testing.T) {
	t.Parallel()
	bundle := richDataset().Bundle
	root := t.TempDir()
	writeServerJSON(t, filepath.Join(root, "rkc.manifest.json"), bundle.Snapshot)
	graph := filepath.Join(root, "graph")
	if err := os.Mkdir(graph, 0o700); err != nil {
		t.Fatal(err)
	}
	writeServerJSONL(t, filepath.Join(graph, "artifacts.jsonl"), bundle.Artifacts)
	writeServerJSONL(t, filepath.Join(graph, "nodes.jsonl"), bundle.Nodes)
	writeServerJSONL(t, filepath.Join(graph, "edges.jsonl"), bundle.Edges)
	writeServerJSONL(t, filepath.Join(graph, "evidence.jsonl"), bundle.Evidence)
	writeServerJSONL(t, filepath.Join(graph, "diagnostics.jsonl"), bundle.Diagnostics)
	loaded, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Bundle.Nodes) != 3 || len(loaded.Bundle.Edges) != 3 || loaded.Integrity != IntegrityLegacyUnverified {
		t.Fatalf("legacy load = %+v", loaded.Bundle)
	}
	invalidEdges := append([]model.Edge(nil), bundle.Edges...)
	invalidEdges[0].To = "missing-node"
	writeServerJSONL(t, filepath.Join(graph, "edges.jsonl"), invalidEdges)
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "validate bundle") {
		t.Fatalf("expected strict legacy graph validation error, got %v", err)
	}
	writeServerJSONL(t, filepath.Join(graph, "edges.jsonl"), bundle.Edges)
	if err := os.WriteFile(filepath.Join(graph, "nodes.jsonl"), []byte("not-json\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLegacyBundle(root); err == nil {
		t.Fatal("expected invalid legacy JSONL error")
	}
}

func TestLegacyJSONLReaderFailsClosedAtEveryBudget(t *testing.T) {
	t.Parallel()
	type testCase struct {
		name       string
		content    string
		limits     legacyLoadLimits
		budget     legacyLoadBudget
		want       string
		useSymlink bool
	}
	base := legacyLoadLimits{fileBytes: 1024, totalBytes: 2048, lineBytes: 1024, recordsPerFile: 10, totalRecords: 20}
	cases := []testCase{
		{name: "file bytes", content: "{}\n", limits: legacyLoadLimits{fileBytes: 2, totalBytes: 2048, lineBytes: 1024, recordsPerFile: 10, totalRecords: 20}, want: "file exceeds"},
		{name: "line bytes", content: "{}\n", limits: legacyLoadLimits{fileBytes: 1024, totalBytes: 2048, lineBytes: 1, recordsPerFile: 10, totalRecords: 20}, want: "bounded JSONL"},
		{name: "records per file", content: "{}\n{}\n", limits: legacyLoadLimits{fileBytes: 1024, totalBytes: 2048, lineBytes: 1024, recordsPerFile: 1, totalRecords: 20}, want: "record count exceeds 1"},
		{name: "total bytes", content: "{}\n", limits: base, budget: legacyLoadBudget{bytes: 2048}, want: "legacy dataset exceeds 2048 total bytes"},
		{name: "total records", content: "{}\n", limits: legacyLoadLimits{fileBytes: 1024, totalBytes: 2048, lineBytes: 1024, recordsPerFile: 10, totalRecords: 1}, budget: legacyLoadBudget{records: 1}, want: "dataset record count exceeds 1"},
		{name: "strict fields", content: "{\"unexpected\":true}\n", limits: base, want: "unknown field"},
		{name: "symlink", content: "{}\n", limits: base, want: "is a symlink", useSymlink: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			graph := filepath.Join(root, "graph")
			if err := os.Mkdir(graph, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(graph, "nodes.jsonl")
			if tc.useSymlink {
				outside := filepath.Join(t.TempDir(), "nodes.jsonl")
				if err := os.WriteFile(outside, []byte(tc.content), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			} else if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			var nodes []model.Node
			budget := tc.budget
			err := readLegacyJSONL(root, "graph/nodes.jsonl", &nodes, tc.limits, &budget)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error=%v, want substring %q", err, tc.want)
			}
		})
	}
}

func TestServerParsingHelpers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		input                    string
		fallback, min, max, want int
	}{
		{"", 5, 1, 10, 5}, {"bad", 5, 1, 10, 5}, {"0", 5, 1, 10, 5}, {"20", 5, 1, 10, 10}, {"7", 5, 1, 10, 7},
	} {
		if got := parseBoundedInt(tc.input, tc.fallback, tc.min, tc.max); got != tc.want {
			t.Errorf("parseBoundedInt(%q)=%d want %d", tc.input, got, tc.want)
		}
	}
	set := setFromCSV(" go,python,go, ")
	if len(set) != 2 {
		t.Fatalf("setFromCSV=%v", set)
	}
	if optionalSet("") != nil || len(optionalSet("go")) != 1 {
		t.Fatal("optionalSet mismatch")
	}
	req := httptest.NewRequest(http.MethodGet, "/?second=value&limit=3", nil)
	if firstQuery(req, "first", "second") != "value" || parseLimit(req, 10, 5) != 3 {
		t.Fatal("request helper mismatch")
	}
	options := traversalOptions(httptest.NewRequest(http.MethodGet, "/?direction=invalid&hops=999&max_nodes=0&include_unresolved=true", nil), 2, 3)
	if options.Direction != graphindex.DirectionBoth || options.MaxDepth != 64 || options.MaxNodes != 3 || !options.IncludeUnresolved {
		t.Fatalf("traversalOptions=%+v", options)
	}
}

func richDataset() *Dataset {
	artifact := model.Artifact{ID: "artifact-a", Path: "src/a.go", Kind: "file", Language: "go", Status: "syntax_parsed", Text: true}
	evidence := model.Evidence{ID: "evidence-a", Kind: "syntax_inferred", Method: "test", Confidence: 1, Source: &model.SourceRange{ArtifactID: artifact.ID, Path: artifact.Path, StartLine: 1, EndLine: 1}}
	a := model.Node{ID: "a", LogicalID: "logical-a", Kind: "function", Name: "Alpha", QualifiedName: "pkg.Alpha", Language: "go", ArtifactID: artifact.ID, EvidenceIDs: []string{evidence.ID}}
	b := model.Node{ID: "b", LogicalID: "logical-b", Kind: "function", Name: "Beta", QualifiedName: "pkg.Beta", Language: "go", ArtifactID: artifact.ID}
	c := model.Node{ID: "c", LogicalID: "logical-c", Kind: "function", Name: "Gamma", QualifiedName: "pkg.Gamma", Language: "go", ArtifactID: artifact.ID}
	edges := []model.Edge{
		{ID: "edge-ab", Kind: "calls", From: "a", To: "b", Resolution: "declared", Confidence: 1},
		{ID: "edge-bc", Kind: "calls", From: "b", To: "c", Resolution: "declared", Confidence: 1},
		{ID: "edge-ca", Kind: "calls", From: "c", To: "a", Resolution: "declared", Confidence: 1},
	}
	diagnostic := model.Diagnostic{ID: "diagnostic-a", Severity: "warning", Code: "RKC-TEST", Message: "test warning"}
	bundle := model.Bundle{Snapshot: model.Snapshot{
		ID: "snapshot-rich", SchemaVersion: model.SchemaVersion, RootName: "fixture", Status: "committed",
		ContentDigest: strings.Repeat("a", sha256.Size*2), Tool: model.ToolInfo{Name: "rkc", Version: "test"},
	}, Artifacts: []model.Artifact{artifact}, Nodes: []model.Node{a, b, c}, Edges: edges, Evidence: []model.Evidence{evidence}, Diagnostics: []model.Diagnostic{diagnostic}}
	return &Dataset{
		Root: tFixtureRoot(), Manifest: bundle.Snapshot, Coverage: model.BuildCoverage(bundle), Bundle: bundle,
		NodeByID: map[string]model.Node{"a": a, "b": b, "c": c}, ArtifactByID: map[string]model.Artifact{artifact.ID: artifact}, EvidenceByID: map[string]model.Evidence{evidence.ID: evidence},
		Graph: graphindex.Build(bundle.Nodes, bundle.Edges), Search: search.BuildFromBundle(bundle), LoadedAt: time.Unix(1, 0).UTC(),
	}
}

func writeVerifiedServerAtlas(t *testing.T, bundle model.Bundle) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "atlas")
	if err := rkcexport.WriteAll(bundle, model.BuildCoverage(bundle), rkcexport.Options{Root: t.TempDir(), Output: root}); err != nil {
		t.Fatal(err)
	}
	writeServerJSON(t, filepath.Join(root, safeoutput.MarkerName), safeoutput.Marker{
		SchemaVersion: "1.0", Producer: "rkc", Kind: "atlas", SnapshotID: bundle.Snapshot.ID,
	})
	return root
}

func rewriteServerExportManifest(t *testing.T, root, snapshotID string) {
	t.Helper()
	manifestPath := filepath.Join(root, exportManifestName)
	var files []datasetExportFile
	var totalBytes, canonicalBytes int64
	if err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || path == manifestPath || entry.Name() == safeoutput.MarkerName {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		sum := sha256.Sum256(data)
		canonical := relative != "rkc.execution.json"
		files = append(files, datasetExportFile{Path: relative, Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:]), Canonical: canonical})
		totalBytes += int64(len(data))
		if canonical {
			canonicalBytes += int64(len(data))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	canonicalRecords := make([]datasetExportFile, 0, len(files))
	for _, file := range files {
		if file.Canonical {
			canonicalRecords = append(canonicalRecords, file)
		}
	}
	canonicalJSON, err := json.Marshal(canonicalRecords)
	if err != nil {
		t.Fatal(err)
	}
	canonicalSum := sha256.Sum256(canonicalJSON)
	writeServerJSON(t, manifestPath, datasetExportManifest{
		SchemaVersion: model.SchemaVersion, SnapshotID: snapshotID, Files: files, TotalBytes: totalBytes,
		CanonicalBytes: canonicalBytes, CanonicalFilesDigest: hex.EncodeToString(canonicalSum[:]),
	})
}

func tFixtureRoot() string {
	return filepath.Join(os.TempDir(), "rkc-server-fixture-does-not-need-to-exist")
}

func writeServerJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeServerJSONL[T any](t *testing.T, path string, values []T) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range values {
		if _, err := fmt.Fprintln(file, mustServerJSON(t, value)); err != nil {
			_ = file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustServerJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
