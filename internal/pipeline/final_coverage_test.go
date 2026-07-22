package pipeline

import (
	"context"
	"crypto/sha256"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestReadInventoriedSourceRejectsUntrustedMetadata(t *testing.T) {
	root := t.TempDir()
	data := []byte("package fixture\n")
	file := inventoriedPipelineFile(t, root, "nested/source.go", data)

	got, info, err := readInventoriedSource(root, file)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) || info == nil || !info.Mode().IsRegular() {
		t.Fatalf("valid inventoried source = %q, %#v", got, info)
	}

	tests := []struct {
		name string
		edit func(*pluginapi.FileRef)
		want string
	}{
		{
			name: "negative size",
			edit: func(candidate *pluginapi.FileRef) { candidate.SizeBytes = -1 },
			want: "invalid size",
		},
		{
			name: "overflow sentinel size",
			edit: func(candidate *pluginapi.FileRef) {
				candidate.SizeBytes = int64(^uint64(0) >> 1)
			},
			want: "invalid size",
		},
		{
			name: "malformed digest",
			edit: func(candidate *pluginapi.FileRef) { candidate.SHA256 = "not-hex" },
			want: "valid SHA-256",
		},
		{
			name: "short digest",
			edit: func(candidate *pluginapi.FileRef) { candidate.SHA256 = strings.Repeat("0", 62) },
			want: "valid SHA-256",
		},
		{
			name: "materialized path mismatch",
			edit: func(candidate *pluginapi.FileRef) {
				candidate.Materialized = filepath.Join(root, "different.go")
			},
			want: "materialized source path",
		},
		{
			name: "inventory size mismatch",
			edit: func(candidate *pluginapi.FileRef) { candidate.SizeBytes++ },
			want: "size does not match inventory",
		},
		{
			name: "inventory digest mismatch",
			edit: func(candidate *pluginapi.FileRef) { candidate.SHA256 = strings.Repeat("0", 64) },
			want: "content does not match inventory",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := file
			test.edit(&candidate)
			if _, _, err := readInventoriedSource(root, candidate); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("readInventoriedSource() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestResolveInventoriedSourceRejectsUnsafeShapes(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, "nested", "source.go"), "package fixture\n")
	mustWritePipelineFile(t, filepath.Join(root, "plain"), "not a directory\n")

	absolute, info, err := resolveInventoriedSource(root, "nested/source.go")
	if err != nil {
		t.Fatal(err)
	}
	if absolute != filepath.Join(root, "nested", "source.go") || info == nil || !info.Mode().IsRegular() {
		t.Fatalf("resolved source = %q, %#v", absolute, info)
	}

	for _, path := range []string{"", ".", "/absolute.go", "nested//source.go", "nested/../nested/source.go"} {
		if _, _, err := resolveInventoriedSource(root, path); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Errorf("resolveInventoriedSource(%q) error = %v, want canonical-path rejection", path, err)
		}
	}
	if _, _, err := resolveInventoriedSource(root, "../outside.go"); err == nil || !strings.Contains(err.Error(), "escapes") {
		t.Fatalf("escaping path error = %v", err)
	}
	if _, _, err := resolveInventoriedSource(root, "missing/source.go"); err == nil {
		t.Fatal("missing path was accepted")
	}
	if _, _, err := resolveInventoriedSource(root, "plain/child.go"); err == nil || !strings.Contains(err.Error(), "non-directory") {
		t.Fatalf("non-directory component error = %v", err)
	}
	if _, _, err := resolveInventoriedSource(root, "nested"); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory source error = %v", err)
	}

	rootFile := filepath.Join(t.TempDir(), "root-file")
	mustWritePipelineFile(t, rootFile, "not a repository\n")
	if _, _, err := resolveInventoriedSource(rootFile, "source.go"); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("file root error = %v", err)
	}
	if _, _, err := resolveInventoriedSource(filepath.Join(t.TempDir(), "missing"), "source.go"); err == nil || !strings.Contains(err.Error(), "inspect repository root") {
		t.Fatalf("missing root error = %v", err)
	}

	rootAlias := filepath.Join(t.TempDir(), "root-link")
	if err := os.Symlink(root, rootAlias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := resolveInventoriedSource(rootAlias, "nested/source.go"); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlink root error = %v", err)
	}
	fileAlias := filepath.Join(root, "source-link.go")
	if err := os.Symlink(filepath.Join(root, "nested", "source.go"), fileAlias); err != nil {
		t.Fatal(err)
	}
	if _, _, err := resolveInventoriedSource(root, "source-link.go"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink source error = %v", err)
	}
}

func TestReverifyInventoriedSourcesRequiresAndPreservesIdentity(t *testing.T) {
	root := t.TempDir()
	file := inventoriedPipelineFile(t, root, "source.go", []byte("package fixture\n"))
	key := sourceIdentityKey(file)

	if err := reverifyInventoriedSources(root, []pluginapi.FileRef{file}, nil); err == nil || !strings.Contains(err.Error(), "missing baseline identity") {
		t.Fatalf("missing identity error = %v", err)
	}
	if err := reverifyInventoriedSources(root, []pluginapi.FileRef{file}, map[string]sourceFileIdentity{key: {}}); err == nil || !strings.Contains(err.Error(), "missing baseline identity") {
		t.Fatalf("nil identity error = %v", err)
	}
	_, identities, err := collectSensitiveLiteralsAndIdentity(root, []pluginapi.FileRef{file})
	if err != nil {
		t.Fatal(err)
	}
	if err := reverifyInventoriedSources(root, []pluginapi.FileRef{file}, identities); err != nil {
		t.Fatalf("unchanged source rejected: %v", err)
	}
	if sourceIdentityKey(pluginapi.FileRef{ArtifactID: "a", Path: "b"}) != "a\x00b" {
		t.Fatal("source identity key does not delimit artifact and path")
	}
}

func TestDedupeBundleCompletesMetadataAndKeepsStrongestEdge(t *testing.T) {
	source := &rkcmodel.SourceRange{ArtifactID: "artifact", Path: "source.go", StartLine: 1, EndLine: 1}
	edgeID := rkcmodel.StableID("edge", "calls", "node", "target")
	bundle := rkcmodel.Bundle{
		Artifacts: []rkcmodel.Artifact{
			{ID: "artifact", Path: "source.go", Status: "semantic_parsed"},
			{ID: "artifact", Path: "source.go", Status: "text"},
			{ID: "other", Path: "other.go", Status: "included"},
		},
		Evidence: []rkcmodel.Evidence{{ID: "evidence"}, {ID: "evidence"}, {ID: "other-evidence"}},
		Nodes: []rkcmodel.Node{
			{ID: "node", Kind: "function", EvidenceIDs: []string{"z", ""}},
			{ID: "node", LogicalID: "logical", Signature: "func Node()", Source: source, ArtifactID: "artifact", EvidenceIDs: []string{"a"}, Attributes: map[string]any{"primary": true}, PublicSurface: true},
			{ID: "node", LogicalID: "ignored", Signature: "ignored", Source: &rkcmodel.SourceRange{Path: "ignored.go"}, ArtifactID: "ignored", Attributes: map[string]any{"primary": false, "secondary": "kept"}},
			{ID: "target", LogicalID: "target", Kind: "function", EvidenceIDs: []string{"b", "b", ""}},
		},
		Edges: []rkcmodel.Edge{
			{ID: edgeID, Kind: "calls", From: "node", To: "target", Resolution: rkcmodel.ResolutionUnresolved, Confidence: 0.1, EvidenceIDs: []string{"z", ""}},
			{ID: edgeID, Kind: "calls", From: "node", To: "target", Resolution: rkcmodel.ResolutionCompilerResolved, Confidence: 0.8, Producer: "compiler", EvidenceIDs: []string{"a"}, Attributes: map[string]any{"primary": true}},
			{ID: edgeID, Kind: "calls", From: "node", To: "target", Resolution: rkcmodel.ResolutionDeclared, Confidence: 1, Producer: "lower-rank", Attributes: map[string]any{"primary": false, "secondary": "kept"}},
			{Kind: "imports", From: "node", To: "target", Resolution: rkcmodel.ResolutionDeclared},
		},
		Diagnostics: []rkcmodel.Diagnostic{{ID: "diagnostic"}, {ID: "diagnostic"}},
		Documents:   []rkcmodel.Document{{ID: "document", Title: "first"}, {ID: "document", Title: "last"}},
		Claims:      []rkcmodel.Claim{{ID: "claim", Text: "first"}, {ID: "claim", Text: "last"}},
		Conflicts:   []rkcmodel.Conflict{{ID: "conflict"}, {ID: "conflict", SubjectID: "last"}},
		Paths:       []rkcmodel.ExecutionPath{{ID: "path"}, {ID: "path", Name: "last"}},
	}

	dedupeBundle(&bundle)
	if len(bundle.Artifacts) != 2 || len(bundle.Evidence) != 2 || len(bundle.Nodes) != 2 || len(bundle.Edges) != 2 || len(bundle.Diagnostics) != 1 {
		t.Fatalf("unexpected dedupe cardinalities: %+v", bundle)
	}
	node := pipelineNodeByID(t, bundle.Nodes, "node")
	if node.LogicalID != "logical" || node.Signature != "func Node()" || node.Source != source || node.ArtifactID != "artifact" || !node.PublicSurface {
		t.Fatalf("node metadata was not completed: %+v", node)
	}
	if node.Attributes["primary"] != true || node.Attributes["secondary"] != "kept" || strings.Join(node.EvidenceIDs, ",") != "a,z" {
		t.Fatalf("node metadata was overwritten or lost: %+v", node)
	}
	edge := pipelineEdgeByID(t, bundle.Edges, edgeID)
	if edge.Resolution != rkcmodel.ResolutionCompilerResolved || edge.Confidence != 0.8 || edge.Producer != "compiler" {
		t.Fatalf("strongest edge was not retained: %+v", edge)
	}
	if edge.Attributes["primary"] != true || edge.Attributes["secondary"] != "kept" || strings.Join(edge.EvidenceIDs, ",") != "a,z" {
		t.Fatalf("edge metadata was overwritten or lost: %+v", edge)
	}
	for _, edge := range bundle.Edges {
		if edge.ID == "" {
			t.Fatalf("edge ID was not synthesized: %+v", edge)
		}
	}

	artifactBundle := rkcmodel.Bundle{
		Artifacts: []rkcmodel.Artifact{{ID: "artifact", Status: "parsed", SizeBytes: 17, SHA256: "digest"}},
		Nodes:     []rkcmodel.Node{{ID: "artifact"}, {ID: "unmatched"}},
	}
	updateArtifactNodes(&artifactBundle)
	if got := artifactBundle.Nodes[0].Attributes; got["status"] != "parsed" || got["size_bytes"] != int64(17) || got["sha256"] != "digest" {
		t.Fatalf("artifact node attributes = %#v", got)
	}
}

func TestResolveHeuristicEdgesCoversFallbackAndBoundaryCases(t *testing.T) {
	bundle := rkcmodel.Bundle{
		Nodes: []rkcmodel.Node{
			{ID: "caller", Kind: "function", Name: "Caller"},
			{ID: "unique", Kind: "function", Name: "Unique", QualifiedName: "pkg.Unique"},
			{ID: "other", Kind: "function", Name: "Other", QualifiedName: "pkg.Other"},
			{ID: "thing", Kind: "function", Name: "Thing", QualifiedName: "pkg.Thing"},
			{ID: "dup-a", Kind: "function", Name: "Dup", QualifiedName: "a.Dup"},
			{ID: "dup-b", Kind: "function", Name: "Dup", QualifiedName: "b.Dup"},
			{ID: "self", Kind: "function", Name: "Self", QualifiedName: "pkg.Self"},
			{ID: "u-unique", Kind: "unresolved_symbol", Name: "scope/Unique"},
			{ID: "u-other", Kind: "unresolved_symbol", Name: "pkg.Other"},
			{ID: "u-thing", Kind: "unresolved_symbol", Name: "Thing"},
			{ID: "u-dup", Kind: "unresolved_symbol", Name: "Dup"},
			{ID: "u-self", Kind: "unresolved_symbol", Name: "Self"},
			{ID: "orphan", Kind: "unresolved_symbol", Name: "Orphan"},
		},
		Edges: []rkcmodel.Edge{
			{ID: "already-resolved", Kind: "calls", From: "caller", To: "unique", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "missing-target", Kind: "calls", From: "caller", To: "missing", Resolution: rkcmodel.ResolutionUnresolved},
			{ID: "concrete-target", Kind: "calls", From: "caller", To: "unique", Resolution: rkcmodel.ResolutionUnresolved},
			{ID: "fallback-separator", Kind: "calls", From: "caller", To: "u-unique", Resolution: rkcmodel.ResolutionUnresolved, Confidence: 0.9},
			{ID: "blank-spelling", Kind: "calls", From: "caller", To: "u-other", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"spelling": " "}},
			{ID: "non-string-spelling", Kind: "calls", From: "caller", To: "u-thing", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"spelling": 17}},
			{ID: "ambiguous", Kind: "calls", From: "caller", To: "u-dup", Resolution: rkcmodel.ResolutionUnresolved},
			{ID: "self-reference", Kind: "calls", From: "self", To: "u-self", Resolution: rkcmodel.ResolutionUnresolved},
		},
	}

	resolveHeuristicEdges(&bundle)
	for _, id := range []string{"fallback-separator", "blank-spelling", "non-string-spelling"} {
		edge := pipelineEdgeByOriginalID(t, bundle.Edges, id)
		if edge.Resolution != rkcmodel.ResolutionSyntaxInferred || edge.Attributes["resolver"] != "unique_name_match" {
			t.Errorf("edge %q was not resolved: %+v", id, edge)
		}
	}
	if edge := pipelineEdgeByOriginalID(t, bundle.Edges, "fallback-separator"); edge.Confidence != 0.9 {
		t.Fatalf("existing higher confidence was lowered: %+v", edge)
	}
	for _, id := range []string{"ambiguous", "self-reference", "missing-target", "concrete-target"} {
		if edge := pipelineEdgeByOriginalID(t, bundle.Edges, id); edge.Resolution != rkcmodel.ResolutionUnresolved {
			t.Errorf("edge %q unexpectedly resolved: %+v", id, edge)
		}
	}
	for _, node := range bundle.Nodes {
		if node.ID == "orphan" {
			t.Fatal("unreferenced placeholder was retained")
		}
	}
}

func TestScanRunsFrameworkPipelineAndRecordsDirtyGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	fixtures := map[string]string{
		"README.md":        "# Fixture\n\nA small service fixture.\n",
		"main.go":          "package fixture\n\nfunc Ready() bool { return true }\n",
		"app.ts":           "export function greet(name: string): string { return name; }\n",
		"package.json":     `{"name":"pipeline-fixture","version":"1.0.0","scripts":{"test":"node test.js"},"dependencies":{"left-pad":"1.3.0"}}`,
		"go.mod":           "module example.com/pipeline-fixture\n\ngo 1.22\n",
		"requirements.txt": "example-package==1.2.3\n",
		"Dockerfile":       "FROM scratch AS runtime\n",
		".env.example":     "SERVICE_ENDPOINT=https://example.invalid\n",
		"openapi.json":     `{"openapi":"3.0.3","info":{"title":"Fixture API","version":"1.0.0"},"paths":{"/health":{"get":{"operationId":"health","responses":{"200":{"description":"ok"}}}}}}`,
		"schema.json":      `{"$schema":"https://json-schema.org/draft/2020-12/schema","title":"Fixture","type":"object","required":["name"],"properties":{"name":{"type":"string"}}}`,
	}
	for path, contents := range fixtures {
		mustWritePipelineFile(t, filepath.Join(root, path), contents)
	}
	pipelineGit(t, "init", "--quiet", root)
	pipelineGit(t, "-C", root, "config", "user.name", "RKC Test")
	pipelineGit(t, "-C", root, "config", "user.email", "rkc@example.invalid")
	pipelineGit(t, "-C", root, "add", "-A", "-f")
	pipelineGit(t, "-C", root, "commit", "--quiet", "-m", "fixture")
	pipelineGit(t, "-C", root, "remote", "add", "origin", "https://example.invalid/pipeline-fixture.git")
	mustWritePipelineFile(t, filepath.Join(root, "README.md"), fixtures["README.md"]+"Dirty working tree.\n")

	bundle, coverage, err := Scan(context.Background(), Options{
		Root: root, ToolVersion: "coverage-test", SourceReference: "fixture-source",
		Excludes: []string{".git"}, DisablePythonAST: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bundle.Snapshot.Git.Dirty || bundle.Snapshot.Git.WorkingTreeDigest != bundle.Snapshot.ContentDigest {
		t.Fatalf("dirty Git provenance = %+v", bundle.Snapshot.Git)
	}
	if bundle.Snapshot.Git.Origin != "https://example.invalid/pipeline-fixture.git" || bundle.Snapshot.Tool.Version != "coverage-test" {
		t.Fatalf("scan provenance = %+v", bundle.Snapshot)
	}
	if coverage.SnapshotID != bundle.Snapshot.ID || coverage.ArtifactsInventoried < len(fixtures) {
		t.Fatalf("coverage = %+v; artifacts = %d", coverage, len(bundle.Artifacts))
	}
	wantedKinds := map[string]bool{
		"api_service": false, "api_endpoint": false, "schema": false,
		"project": false, "environment_variable": false, "function": false,
	}
	for _, node := range bundle.Nodes {
		if _, wanted := wantedKinds[node.Kind]; wanted {
			wantedKinds[node.Kind] = true
		}
	}
	for kind, found := range wantedKinds {
		if !found {
			t.Errorf("framework pipeline did not produce %q node", kind)
		}
	}
}

func TestScanPropagatesInventoryLimitsAndFailsClosed(t *testing.T) {
	limited := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(limited, "a.txt"), "a\n")
	mustWritePipelineFile(t, filepath.Join(limited, "b.txt"), "b\n")
	if _, _, err := Scan(context.Background(), Options{Root: limited, MaxFiles: 1}); err == nil || !strings.Contains(err.Error(), "path limit") {
		t.Fatalf("inventory limit error = %v", err)
	}

	pythonRoot := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(pythonRoot, "main.py"), "def ready():\n    return True\n")
	bundle, coverage, err := Scan(context.Background(), Options{
		Root: pythonRoot, PythonInterpreter: filepath.Join(pythonRoot, "missing-python"), PythonPlugin: "missing.py",
		FailClosedOnPluginError: true,
		DisableGoAST:            true, DisableTypeScript: true, DisableFrameworks: true, DisableSecretScan: true,
	})
	if err == nil || !strings.Contains(err.Error(), "Python adapter failed closed") {
		t.Fatalf("fail-closed plugin error = %v", err)
	}
	if bundle.Snapshot.ID != "" || coverage.SnapshotID != "" {
		t.Fatalf("fail-closed scan returned partial output: bundle=%+v coverage=%+v", bundle.Snapshot, coverage)
	}
}

func inventoriedPipelineFile(t *testing.T, root, relative string, data []byte) pluginapi.FileRef {
	t.Helper()
	absolute := filepath.Join(root, filepath.FromSlash(relative))
	mustWritePipelineFile(t, absolute, string(data))
	digest := sha256.Sum256(data)
	return pluginapi.FileRef{
		ArtifactID:   rkcmodel.StableID("artifact", relative),
		Path:         relative,
		Language:     "go",
		SHA256:       stringHex(digest[:]),
		SizeBytes:    int64(len(data)),
		Materialized: absolute,
	}
}

func pipelineNodeByID(t *testing.T, nodes []rkcmodel.Node, id string) rkcmodel.Node {
	t.Helper()
	for _, node := range nodes {
		if node.ID == id {
			return node
		}
	}
	t.Fatalf("node %q not found in %+v", id, nodes)
	return rkcmodel.Node{}
}

func pipelineEdgeByID(t *testing.T, edges []rkcmodel.Edge, id string) rkcmodel.Edge {
	t.Helper()
	for _, edge := range edges {
		if edge.ID == id {
			return edge
		}
	}
	t.Fatalf("edge %q not found in %+v", id, edges)
	return rkcmodel.Edge{}
}

func pipelineEdgeByOriginalID(t *testing.T, edges []rkcmodel.Edge, id string) rkcmodel.Edge {
	t.Helper()
	for _, edge := range edges {
		if edge.ID == id || edge.Attributes["original_test_id"] == id {
			return edge
		}
		if id == "fallback-separator" && edge.To == "unique" && edge.Resolution == rkcmodel.ResolutionSyntaxInferred {
			return edge
		}
		if id == "blank-spelling" && edge.To == "other" && edge.Resolution == rkcmodel.ResolutionSyntaxInferred {
			return edge
		}
		if id == "non-string-spelling" && edge.To == "thing" && edge.Resolution == rkcmodel.ResolutionSyntaxInferred {
			return edge
		}
	}
	t.Fatalf("edge originating as %q not found in %+v", id, edges)
	return rkcmodel.Edge{}
}

func pipelineGit(t *testing.T, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
}
