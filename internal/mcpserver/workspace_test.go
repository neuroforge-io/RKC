package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/neuroforge-io/RKC/internal/privatepath"
	"github.com/neuroforge-io/RKC/internal/search"
	"github.com/neuroforge-io/RKC/internal/server"
	"github.com/neuroforge-io/RKC/internal/workspace"
)

// All sources are synthetic. Deliberately overlapping canonical IDs exercise
// routing without relying on repository names, paths or private content.
func workspaceFixture(t *testing.T, ids ...string) (*Server, *workspace.Registry, map[string]*server.Dataset, *int) {
	t.Helper()
	registry := &workspace.Registry{SchemaVersion: workspace.SchemaVersion, Generation: 1, Sources: []workspace.Source{}}
	datasets := map[string]*server.Dataset{}
	for _, id := range ids {
		parent, err := privatepath.MkdirTemp(canonicalWorkspaceTempDir(t), "generations-")
		if err != nil {
			t.Fatal(err)
		}
		generation, err := workspace.CreateGeneration(parent, id)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(generation, "atlas")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		data := []byte("synthetic manifest " + id)
		if err := os.WriteFile(filepath.Join(dir, "rkc-export-manifest.json"), data, 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		source := workspace.Source{ID: id, Label: "Synthetic " + id, Kind: "local", LocalPath: filepath.Join(dir, "never-read-source"), Excludes: []string{}, Limits: workspace.DefaultLimits(), Freshness: workspace.Freshness{Status: "current"}, Active: &workspace.Active{AtlasPath: dir, Generation: filepath.Base(generation), SnapshotID: "snapshot-" + id, ManifestSHA256: hex.EncodeToString(digest[:])}}
		dataset := mcpDataset()
		dataset.Manifest.ID = source.Active.SnapshotID
		dataset.Integrity = server.IntegrityVerified
		datasets[dir] = dataset
		registry.Sources = append(registry.Sources, source)
	}
	loads := new(int)
	w := &workspaceServer{read: func() (workspace.Registry, error) { return *registry, nil }, load: func(path string) (*server.Dataset, error) {
		*loads++
		dataset := datasets[path]
		if dataset == nil {
			return nil, errors.New("synthetic private load path: " + path)
		}
		return dataset, nil
	}}
	return &Server{workspace: w, version: "test"}, registry, datasets, loads
}

func workspaceCall(t *testing.T, s *Server, name string, args map[string]any) (map[string]any, *rpcError) {
	t.Helper()
	request, err := json.Marshal(map[string]any{"name": name, "arguments": args})
	if err != nil {
		t.Fatal(err)
	}
	result, rpcErr := s.callTool(context.Background(), request)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return result.(map[string]any), nil
}
func workspaceQuery(t *testing.T, s *Server, name string, args map[string]any) workspaceQueryResult {
	t.Helper()
	result, rpcErr := workspaceCall(t, s, name, args)
	if rpcErr != nil || result["isError"] != false {
		t.Fatalf("query failed: %#v %#v", result, rpcErr)
	}
	return result["structuredContent"].(workspaceQueryResult)
}
func expectWorkspaceError(t *testing.T, s *Server, name string, args map[string]any) {
	t.Helper()
	result, rpcErr := workspaceCall(t, s, name, args)
	if rpcErr == nil && result["isError"] != true {
		t.Fatalf("expected failure: %#v", result)
	}
}

func TestWorkspaceDiscoveryIsRedactedLazyAndReadOnly(t *testing.T) {
	s, registry, _, loads := workspaceFixture(t, "zeta", "alpha")
	registry.Sources[0].RemoteURL = "https://synthetic.example/never-disclose.git"
	for _, method := range []string{"initialize", "tools/list", "resources/list", "ping"} {
		result, rpcErr := s.handle(context.Background(), method, json.RawMessage(`{}`))
		if rpcErr != nil {
			t.Fatal(rpcErr)
		}
		data, _ := json.Marshal(result)
		if method == "initialize" && (!bytes.Contains(data, []byte("rkc.repositories")) || len(workspaceInstructions) > 512) {
			t.Fatalf("missing bounded initialization guidance: %s", data)
		}
		if method == "tools/list" {
			definitions := result.(map[string]any)["tools"].([]toolDefinition)
			if len(definitions) != len(tools())+1 {
				t.Fatal("missing workspace catalog")
			}
			for _, tool := range definitions {
				if !tool.Annotations["readOnlyHint"] || tool.Annotations["openWorldHint"] || tool.Annotations["destructiveHint"] || !tool.Annotations["idempotentHint"] {
					t.Fatalf("unsafe hints: %+v", tool)
				}
			}
		}
	}
	result, rpcErr := workspaceCall(t, s, "rkc.repositories", nil)
	if rpcErr != nil {
		t.Fatal(rpcErr)
	}
	data, _ := json.Marshal(result)
	for _, forbidden := range []string{"never-disclose", registry.Sources[0].LocalPath, "atlas_path", "remote_url", "local_path"} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("private metadata exposed: %s", data)
		}
	}
	list := result["structuredContent"].(map[string]any)["repositories"].([]repositoryDescriptor)
	if len(list) != 2 || list[0].ID != "alpha" || registry.Sources[0].ID != "zeta" || *loads != 0 {
		t.Fatalf("nonlazy or nondeterministic roster: %+v loads=%d", list, *loads)
	}
	resource, rpcErr := s.handle(context.Background(), "resources/read", json.RawMessage(`{"uri":"rkc://workspace/repositories"}`))
	if rpcErr != nil || resource == nil {
		t.Fatal(rpcErr)
	}
	for _, raw := range []string{`{"uri":"rkc://snapshot/manifest"}`, `{"uri":1}`, `{"uri":"rkc://workspace/repositories","extra":true}`} {
		if _, rpcErr := s.handle(context.Background(), "resources/read", json.RawMessage(raw)); rpcErr == nil {
			t.Fatalf("accepted resource: %s", raw)
		}
	}
	// Constructing workspace schemas must not add selectors to legacy schemas.
	for _, tool := range tools() {
		if _, ok := tool.InputSchema["properties"].(map[string]any)["repository"]; ok {
			t.Fatal("mutated legacy tool schema")
		}
	}
}

func TestWorkspaceSelectionCacheReloadAndRevocation(t *testing.T) {
	s, registry, datasets, loads := workspaceFixture(t, "beta", "alpha")
	expectWorkspaceError(t, s, "rkc.get_symbol", map[string]any{"node": "Alpha"})
	for _, id := range []string{"alpha", "alpha", "beta", "alpha"} {
		result, rpcErr := workspaceCall(t, s, "rkc.get_symbol", map[string]any{"repository": id, "node": "Alpha"})
		if rpcErr != nil || result["isError"] != false {
			t.Fatalf("selection: %#v %#v", result, rpcErr)
		}
		data := result["structuredContent"].(map[string]any)
		if data["repository"].(repositoryDescriptor).ID != id || data["snapshot_id"] != "snapshot-"+id {
			t.Fatalf("mixed source: %#v", data)
		}
	}
	if *loads != 3 {
		t.Fatalf("expected one-entry cache: %d loads", *loads)
	}
	// The same alias now points to a different verified snapshot.
	registry.Generation++
	source := &registry.Sources[1]
	source.Active.SnapshotID = "snapshot-alpha-next"
	source.Active.Fingerprint = "changed-fingerprint"
	datasets[source.Active.AtlasPath].Manifest.ID = source.Active.SnapshotID
	result, rpcErr := workspaceCall(t, s, "rkc.coverage", map[string]any{"repository": "alpha"})
	if rpcErr != nil || result["isError"] != false || *loads != 4 {
		t.Fatalf("failed reload: %#v %#v loads=%d", result, rpcErr, *loads)
	}
	registry.Sources = registry.Sources[:1]
	expectWorkspaceError(t, s, "rkc.coverage", map[string]any{"repository": "alpha"})
	if *loads != 4 {
		t.Fatal("revoked source loaded")
	}
	// Invalid current authority must fail even while a verified atlas is cached.
	s.workspace.read = func() (workspace.Registry, error) {
		return workspace.Registry{}, errors.New("synthetic registry private path")
	}
	expectWorkspaceError(t, s, "rkc.coverage", map[string]any{"repository": "alpha"})
	_, rpcErr = s.handle(context.Background(), "resources/read", json.RawMessage(`{"uri":"rkc://workspace/repositories"}`))
	if rpcErr == nil || strings.Contains(rpcErr.Message, "private path") {
		t.Fatalf("invalid registry leaked or served: %#v", rpcErr)
	}
}

func TestWorkspaceLoadIntegrityNeverFallsBack(t *testing.T) {
	for _, mode := range []string{"pending", "digest", "load", "nil", "snapshot", "integrity", "changed", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s, registry, datasets, loads := workspaceFixture(t, "alpha")
			source := registry.Sources[0]
			if _, err := s.workspace.dataset(context.Background(), source); err != nil {
				t.Fatal(err)
			}
			source.Active = &workspace.Active{AtlasPath: source.Active.AtlasPath, SnapshotID: source.Active.SnapshotID, Generation: source.Active.Generation, ManifestSHA256: source.Active.ManifestSHA256, Fingerprint: "changed-fingerprint"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "pending":
				source.Active = nil
			case "digest":
				source.Active.ManifestSHA256 = strings.Repeat("f", 64)
			case "load":
				s.workspace.load = func(string) (*server.Dataset, error) { return nil, errors.New("synthetic-private-path") }
			case "nil":
				s.workspace.load = func(string) (*server.Dataset, error) { return nil, nil }
			case "snapshot":
				datasets[source.Active.AtlasPath].Manifest.ID = "wrong"
			case "integrity":
				datasets[source.Active.AtlasPath].Integrity = server.IntegrityVerifiedLegacyUnmarked
			case "changed":
				s.workspace.load = func(path string) (*server.Dataset, error) {
					if err := os.WriteFile(filepath.Join(path, "rkc-export-manifest.json"), []byte("changed"), 0600); err != nil {
						t.Fatal(err)
					}
					return datasets[path], nil
				}
			case "cancel":
				s.workspace.load = func(path string) (*server.Dataset, error) { cancel(); return datasets[path], nil }
			}
			if dataset, err := s.workspace.dataset(ctx, source); err == nil || dataset != nil || strings.Contains(err.Error(), "synthetic-private-path") {
				t.Fatalf("accepted invalid replacement: %+v %v", dataset, err)
			}
			if mode != "pending" && s.workspace.cached != nil {
				t.Fatal("obsolete cache retained after invalid replacement")
			}
			if mode == "digest" && *loads != 1 {
				t.Fatal("loaded unbound manifest")
			}
		})
	}
}

func TestWorkspaceQueriesInterleaveAndBoundAcrossSources(t *testing.T) {
	s, registry, _, _ := workspaceFixture(t, "beta", "alpha")
	for _, name := range []string{"rkc.search", "rkc.context"} {
		t.Run(name, func(t *testing.T) {
			args := map[string]any{"query": "Same", "limit": 2, "max_bytes": 32768}
			result := workspaceQuery(t, s, name, args)
			if result.Total != 4 || result.MatchedSources != 2 || len(result.Items) != 2 || !result.Truncated || result.Partial {
				t.Fatalf("wrong query accounting: %+v", result)
			}
			for i, raw := range result.Items {
				var item workspaceQueryItem
				if err := json.Unmarshal(raw, &item); err != nil {
					t.Fatal(err)
				}
				expected := []string{"alpha", "beta"}[i]
				if item.Repository != expected || item.SnapshotID != "snapshot-"+expected || item.Rank != 1 {
					t.Fatalf("mixed/rank result: %+v", item)
				}
				if name == "rkc.context" && !bytes.Contains(item.Value, []byte("citation_id")) {
					t.Fatal("lost citation")
				}
			}
			encoded, _ := json.Marshal(result.Items)
			if len(encoded) != result.Bytes || result.Bytes > result.MaxBytes {
				t.Fatal("wrong encoded byte count")
			}
			repeat := workspaceQuery(t, s, name, args)
			if repeat.Digest != result.Digest {
				t.Fatal("nondeterministic workspace query")
			}
			digest := result.Digest
			result.Digest = ""
			encoded, _ = json.Marshal(result)
			hash := sha256.Sum256(encoded)
			if digest != hex.EncodeToString(hash[:]) {
				t.Fatal("invalid result digest")
			}
			subset := workspaceQuery(t, s, name, map[string]any{"query": "Same", "repositories": []string{"beta"}, "limit": 1})
			if subset.Total != 2 || len(subset.Sources) != 1 || subset.Sources[0].Repository.ID != "beta" {
				t.Fatalf("ignored selector: %+v", subset)
			}
		})
	}
	empty := workspaceQuery(t, s, "rkc.search", map[string]any{"query": "nevermatch"})
	if empty.Total != 0 || len(empty.Items) != 0 || empty.Truncated || empty.Bytes != 2 {
		t.Fatalf("bad empty response: %+v", empty)
	}
	filtered := workspaceQuery(t, s, "rkc.search", map[string]any{"query": "Same", "languages": []string{"python"}})
	if filtered.Total != 0 {
		t.Fatal("lost search filters")
	}
	registry.Sources[0].Active = nil
	registry.Sources[0].Freshness.Status = "pending"
	partial := workspaceQuery(t, s, "rkc.search", map[string]any{"query": "Same"})
	if !partial.Partial || !partial.Truncated || partial.Total != 2 || partial.Sources[1].ErrorCode != "atlas_unavailable" {
		t.Fatalf("failed source looks complete: %+v", partial)
	}
}

func TestWorkspaceOversizedSearchHitsDoNotCrowdOutSmallSources(t *testing.T) {
	s, _, datasets, _ := workspaceFixture(t, "alpha", "beta")
	for path, dataset := range datasets {
		if !strings.Contains(dataset.Manifest.ID, "alpha") {
			continue
		}
		dataset.Bundle.Nodes[0].Signature = strings.Repeat("huge ", 10000)
		dataset.Search = search.BuildFromBundle(dataset.Bundle)
		datasets[path] = dataset
	}
	for _, name := range []string{"rkc.search", "rkc.context"} {
		result := workspaceQuery(t, s, name, map[string]any{"query": "Alpha", "max_bytes": 1024})
		if len(result.Items) != 1 || !result.Truncated || result.Bytes > 1024 || !bytes.Contains(result.Items[0], []byte(`"repository":"beta"`)) {
			t.Fatalf("oversized hit defeated budget/fairness: %+v", result)
		}
	}
}

func TestWorkspaceRejectsAmbiguousUnboundedAndMalformedArguments(t *testing.T) {
	s, registry, _, _ := workspaceFixture(t, "alpha")
	for _, args := range []map[string]any{
		{"query": "Alpha", "repository": "missing"}, {"query": "Alpha", "repository": " alpha"}, {"query": "Alpha", "repository": 1},
		{"query": "Alpha", "repository": "alpha", "repositories": []string{"alpha"}},
		{"query": "Alpha", "repositories": []string{}}, {"query": "Alpha", "repositories": "alpha"}, {"query": "Alpha", "repositories": []any{1}},
		{"query": "Alpha", "repositories": []string{"alpha", "alpha"}}, {"query": "Alpha", "repositories": make([]string, 17)},
		{"query": "Alpha", "limit": 101}, {"query": "Alpha", "limit": 1.5}, {"query": "Alpha", "max_bytes": 1023}, {"query": "Alpha", "max_bytes": 262145},
		{"query": "Alpha", "max_bytes": "1024"}, {"query": "Alpha", "unknown": true}, {"query": " "},
	} {
		expectWorkspaceError(t, s, "rkc.search", args)
	}
	expectWorkspaceError(t, s, "rkc.repositories", map[string]any{"repository": "alpha"})
	expectWorkspaceError(t, s, "rkc.coverage", map[string]any{"repositories": []string{"alpha"}})
	expectWorkspaceError(t, s, "not-a-tool", nil)
	// A source selector remains optional when only one source is registered.
	result, rpcErr := workspaceCall(t, s, "rkc.coverage", nil)
	if rpcErr != nil || result["isError"] != false {
		t.Fatal("single-source compatibility")
	}
	registry.Sources = []workspace.Source{}
	expectWorkspaceError(t, s, "rkc.search", map[string]any{"query": "Alpha"})
	registry.Sources = make([]workspace.Source, 17)
	expectWorkspaceError(t, s, "rkc.search", map[string]any{"query": "Alpha"})
	registry.Sources = make([]workspace.Source, workspace.MaximumSources+1)
	expectWorkspaceError(t, s, "rkc.repositories", nil)
}

func TestWorkspaceConcurrentRequestsAndCancellation(t *testing.T) {
	s, _, _, _ := workspaceFixture(t, "alpha", "beta")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := workspaceQuery(t, s, "rkc.search", map[string]any{"query": "Same", "limit": 2})
			if len(result.Items) != 2 {
				t.Error("concurrent result lost")
			}
		}()
	}
	wg.Wait()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, rpcErr := s.callTool(ctx, json.RawMessage(`{"name":"rkc.repositories"}`)); rpcErr == nil || rpcErr.Code != -32800 {
		t.Fatalf("ignored cancellation: %#v", rpcErr)
	}
	s.workspace.load = func(string) (*server.Dataset, error) { cancel(); return nil, ctx.Err() }
	s.workspace.cached = nil
	registry, _ := s.workspace.registry()
	if _, err := s.workspace.query(ctx, registry, "rkc.search", map[string]any{"query": "Alpha"}, registry.Sources); !errors.Is(err, context.Canceled) {
		t.Fatalf("ignored query cancellation: %v", err)
	}
}

func TestNewWorkspaceReadsPrivateRegistryAndStdio(t *testing.T) {
	root, err := privatepath.MkdirTemp(canonicalWorkspaceTempDir(t), "workspace-")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "registry.json")
	registry := workspace.Registry{SchemaVersion: workspace.SchemaVersion, Generation: 1, Sources: []workspace.Source{}}
	data, _ := json.Marshal(registry)
	file, err := privatepath.CreateTemp(root, "registry-")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(file.Name(), path); err != nil {
		t.Fatal(err)
	}
	s, err := NewWorkspace(path, "test")
	if err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/call\",\"params\":{\"name\":\"rkc.repositories\"}}\n")
	var out bytes.Buffer
	if err := s.Serve(context.Background(), input, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "rkc-workspace-repositories/v1") || !strings.Contains(out.String(), "instructions") {
		t.Fatalf("stdio missing discovery: %s", out.String())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := NewWorkspace(path, "test"); err == nil {
		t.Fatal("accepted missing registry")
	}
	expectWorkspaceError(t, s, "rkc.repositories", nil)
}

// Darwin temporary roots can contain the system /var symlink. Resolve only
// synthetic fixture paths before exercising the production no-symlink policy.
func canonicalWorkspaceTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}
