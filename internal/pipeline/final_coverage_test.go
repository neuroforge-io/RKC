package pipeline

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/framework/openapi"
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
			{ID: "go-scope", Kind: "package", Name: "pkg", QualifiedName: "pkg", Language: "go"},
			{ID: "python-scope", Kind: "module", Name: "scripts", QualifiedName: "scripts", Language: "python"},
			{ID: "javascript-scope", Kind: "module", Name: "src", QualifiedName: "src", Language: "typescript"},
			{ID: "caller", Kind: "function", Name: "Caller", Language: "go"},
			{ID: "unique", Kind: "function", Name: "Unique", QualifiedName: "pkg.Unique", Language: "go"},
			{ID: "other", Kind: "function", Name: "Other", QualifiedName: "pkg.Other", Language: "go"},
			{ID: "thing", Kind: "function", Name: "Thing", QualifiedName: "pkg.Thing", Language: "go"},
			{ID: "dup-a", Kind: "function", Name: "Dup", QualifiedName: "a.Dup", Language: "go"},
			{ID: "dup-b", Kind: "function", Name: "Dup", QualifiedName: "b.Dup", Language: "go"},
			{ID: "self", Kind: "function", Name: "Self", QualifiedName: "pkg.Self", Language: "go"},
			{ID: "python-caller", Kind: "function", Name: "python_caller", Language: "python"},
			{ID: "python-shared", Kind: "function", Name: "Shared", QualifiedName: "scripts.Shared", Language: "python"},
			{ID: "go-shared", Kind: "function", Name: "Shared", QualifiedName: "pkg.Shared", Language: "go"},
			{ID: "go-only", Kind: "function", Name: "GoOnly", QualifiedName: "pkg.GoOnly", Language: "go"},
			{ID: "openapi-only", Kind: "schema", Name: "OpenAPIOnly", QualifiedName: "api.OpenAPIOnly", Language: "openapi"},
			{ID: "javascript-caller", Kind: "function", Name: "caller", Language: "javascript"},
			{ID: "typescript-family", Kind: "function", Name: "FamilyTarget", QualifiedName: "src.FamilyTarget", Language: "typescript"},
			{ID: "missing-domain-caller", Kind: "function", Name: "missing_domain_caller"},
			{ID: "u-unique", Kind: "unresolved_symbol", Name: "scope/Unique", Language: "go"},
			{ID: "u-other", Kind: "unresolved_symbol", Name: "pkg.Other", Language: "go"},
			{ID: "u-thing", Kind: "unresolved_symbol", Name: "Thing", Language: "go"},
			{ID: "u-dup", Kind: "unresolved_symbol", Name: "Dup", Language: "go"},
			{ID: "u-self", Kind: "unresolved_symbol", Name: "Self", Language: "go"},
			{ID: "u-python-shared", Kind: "unresolved_symbol", Name: "Shared", Language: "python"},
			{ID: "u-python-go-only", Kind: "unresolved_symbol", Name: "GoOnly", Language: "python"},
			{ID: "u-python-openapi-only", Kind: "unresolved_symbol", Name: "OpenAPIOnly", Language: "python"},
			{ID: "u-go-mismatched-caller", Kind: "unresolved_symbol", Name: "GoOnly", Language: "go"},
			{ID: "u-javascript-family", Kind: "unresolved_symbol", Name: "FamilyTarget", Language: "javascript"},
			{ID: "u-missing-domain", Kind: "unresolved_symbol", Name: "Unique"},
			{ID: "u-go-from-missing-caller", Kind: "unresolved_symbol", Name: "GoOnly", Language: "go"},
			{ID: "orphan", Kind: "unresolved_symbol", Name: "Orphan", Language: "go"},
		},
		Edges: []rkcmodel.Edge{
			{ID: "own-caller", Kind: "declares", From: "go-scope", To: "caller", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-unique", Kind: "declares", From: "go-scope", To: "unique", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-other", Kind: "declares", From: "go-scope", To: "other", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-thing", Kind: "declares", From: "go-scope", To: "thing", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-dup-a", Kind: "declares", From: "go-scope", To: "dup-a", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-dup-b", Kind: "declares", From: "go-scope", To: "dup-b", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-self", Kind: "declares", From: "go-scope", To: "self", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-python-caller", Kind: "contains", From: "python-scope", To: "python-caller", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-python-shared", Kind: "contains", From: "python-scope", To: "python-shared", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-javascript-caller", Kind: "declares", From: "javascript-scope", To: "javascript-caller", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-typescript-family", Kind: "declares", From: "javascript-scope", To: "typescript-family", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "already-resolved", Kind: "calls", From: "caller", To: "unique", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "missing-target", Kind: "calls", From: "caller", To: "missing", Resolution: rkcmodel.ResolutionUnresolved},
			{ID: "concrete-target", Kind: "calls", From: "caller", To: "unique", Resolution: rkcmodel.ResolutionUnresolved},
			{ID: "fallback-separator", Kind: "calls", From: "caller", To: "u-unique", Resolution: rkcmodel.ResolutionUnresolved, Confidence: 0.9},
			{ID: "blank-spelling", Kind: "calls", From: "caller", To: "u-other", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"spelling": " "}},
			{ID: "non-string-spelling", Kind: "calls", From: "caller", To: "u-thing", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"spelling": 17}},
			{ID: "ambiguous", Kind: "calls", From: "caller", To: "u-dup", Resolution: rkcmodel.ResolutionUnresolved},
			{ID: "self-reference", Kind: "calls", From: "self", To: "u-self", Resolution: rkcmodel.ResolutionUnresolved},
			{ID: "same-domain", Kind: "calls", From: "python-caller", To: "u-python-shared", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "same-domain"}},
			{ID: "cross-domain-candidate", Kind: "calls", From: "python-caller", To: "u-python-go-only", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "cross-domain-candidate"}},
			{ID: "python-openapi-candidate", Kind: "calls", From: "python-caller", To: "u-python-openapi-only", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "python-openapi-candidate"}},
			{ID: "mismatched-domains", Kind: "calls", From: "python-caller", To: "u-go-mismatched-caller", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "mismatched-domains"}},
			{ID: "javascript-typescript-family", Kind: "calls", From: "javascript-caller", To: "u-javascript-family", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "javascript-typescript-family"}},
			{ID: "missing-domain", Kind: "calls", From: "caller", To: "u-missing-domain", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "missing-domain"}},
			{ID: "missing-caller-domain", Kind: "calls", From: "missing-domain-caller", To: "u-go-from-missing-caller", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "missing-caller-domain"}},
		},
	}

	resolveHeuristicEdges(&bundle)
	for id, want := range map[string]struct {
		target string
		proof  string
	}{
		"blank-spelling":               {target: "other", proof: "exact_qualified"},
		"non-string-spelling":          {target: "thing", proof: "shared_language_scope"},
		"same-domain":                  {target: "python-shared", proof: "shared_language_scope"},
		"javascript-typescript-family": {target: "typescript-family", proof: "shared_language_scope"},
	} {
		edge := pipelineEdgeByOriginalID(t, bundle.Edges, id)
		if edge.Resolution != rkcmodel.ResolutionSyntaxInferred || edge.Attributes["resolver"] != "unique_name_match" {
			t.Errorf("edge %q was not resolved: %+v", id, edge)
		}
		if edge.To != want.target || edge.Attributes["resolver_proof"] != want.proof {
			t.Errorf("edge %q target/proof = %q/%v, want %q/%q", id, edge.To, edge.Attributes["resolver_proof"], want.target, want.proof)
		}
	}
	for _, id := range []string{"fallback-separator", "ambiguous", "self-reference", "missing-target", "concrete-target", "cross-domain-candidate", "python-openapi-candidate", "mismatched-domains", "missing-domain", "missing-caller-domain"} {
		if edge := pipelineEdgeByOriginalID(t, bundle.Edges, id); edge.Resolution != rkcmodel.ResolutionUnresolved {
			t.Errorf("edge %q unexpectedly resolved: %+v", id, edge)
		}
	}
	if edge := pipelineEdgeByOriginalID(t, bundle.Edges, "fallback-separator"); edge.Confidence != 0.9 {
		t.Fatalf("unresolved edge confidence changed: %+v", edge)
	}
	for _, node := range bundle.Nodes {
		if node.ID == "orphan" {
			t.Fatal("unreferenced placeholder was retained")
		}
	}
}

func TestResolveHeuristicEdgesRequiresScopeAndExactQualifiedIdentity(t *testing.T) {
	bundle := rkcmodel.Bundle{
		Nodes: []rkcmodel.Node{
			{ID: "package-a", Kind: "package", Name: "a", QualifiedName: "example/a", Language: "go"},
			{ID: "package-b", Kind: "package", Name: "b", QualifiedName: "example/b", Language: "go"},
			{ID: "package-target", Kind: "package", Name: "target", QualifiedName: "example/target", Language: "go"},
			{ID: "caller-a", Kind: "function", Name: "Caller", Language: "go"},
			{ID: "compiler-caller", Kind: "function", Name: "CompilerCaller", Language: "go"},
			{ID: "compiler-target", Kind: "function", Name: "CompilerTarget", QualifiedName: "example/a.CompilerTarget", Language: "go"},
			{ID: "local", Kind: "function", Name: "Local", QualifiedName: "example/a.Local", Language: "go"},
			{ID: "exact", Kind: "function", Name: "Exact", QualifiedName: "example/a.Exact", Language: "go"},
			{ID: "cross-only", Kind: "function", Name: "CrossOnly", QualifiedName: "example/b.CrossOnly", Language: "go"},
			{ID: "collision-is", Kind: "method", Name: "Is", QualifiedName: "example/b.OperationError.Is", Language: "go"},
			{ID: "default-import-target", Kind: "function", Name: "DefaultFn", QualifiedName: "example/target.DefaultFn", Language: "go"},
			{ID: "alias-import-target", Kind: "function", Name: "AliasFn", QualifiedName: "example/target.AliasFn", Language: "go"},
			{ID: "ambiguous-owner", Kind: "function", Name: "MultiOwned", QualifiedName: "example.MultiOwned", Language: "go"},
			{ID: "target-dependency", Kind: "external_dependency", Name: "target", QualifiedName: "example/target", Language: "go"},
			{ID: "unowned-caller", Kind: "function", Name: "UnownedCaller", Language: "go", ArtifactID: "shared-artifact"},
			{ID: "unowned-target", Kind: "function", Name: "Unowned", QualifiedName: "Unowned", Language: "go", ArtifactID: "shared-artifact"},
			{ID: "framework-caller", Kind: "schema", Name: "FrameworkCaller", Language: "openapi", ArtifactID: "api-artifact"},
			{ID: "framework-local", Kind: "schema", Name: "FrameworkOnly", Language: "openapi", ArtifactID: "api-artifact"},
			{ID: "framework-foreign", Kind: "schema", Name: "FrameworkOnly", Language: "openapi", ArtifactID: "other-artifact"},
			{ID: "cycle-a", Kind: "namespace", Name: "CycleA", Language: "go"},
			{ID: "cycle-b", Kind: "namespace", Name: "CycleB", Language: "go"},
			{ID: "cycle-caller", Kind: "function", Name: "CycleCaller", Language: "go"},
			{ID: "cycle-target", Kind: "function", Name: "CycleTarget", QualifiedName: "CycleTarget", Language: "go"},
			{ID: "u-local", Kind: "unresolved_symbol", Name: "Local", Language: "go"},
			{ID: "u-compiler-target", Kind: "unresolved_symbol", Name: "CompilerTarget", Language: "go"},
			{ID: "u-exact", Kind: "unresolved_symbol", Name: "example/a.Exact", Language: "go"},
			{ID: "u-cross", Kind: "unresolved_symbol", Name: "CrossOnly", Language: "go"},
			{ID: "u-collision", Kind: "unresolved_symbol", Name: "errors.Is", Language: "go"},
			{ID: "u-default-import", Kind: "unresolved_symbol", Name: "target.DefaultFn", Language: "go"},
			{ID: "u-alias-import", Kind: "unresolved_symbol", Name: "alias.AliasFn", Language: "go"},
			{ID: "u-ambiguous-owner", Kind: "unresolved_symbol", Name: "MultiOwned", Language: "go"},
			{ID: "u-unowned", Kind: "unresolved_symbol", Name: "Unowned", Language: "go"},
			{ID: "u-framework", Kind: "unresolved_symbol", Name: "FrameworkOnly", Language: "openapi"},
			{ID: "u-cycle-target", Kind: "unresolved_symbol", Name: "CycleTarget", Language: "go"},
		},
		Edges: []rkcmodel.Edge{
			{ID: "own-caller-a", Kind: "declares", From: "package-a", To: "caller-a", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-compiler-caller", Kind: "declares", From: "package-a", To: "compiler-caller", Resolution: rkcmodel.ResolutionCompilerResolved},
			{ID: "own-compiler-target", Kind: "declares", From: "package-a", To: "compiler-target", Resolution: rkcmodel.ResolutionCompilerResolved},
			{ID: "own-local", Kind: "declares", From: "package-a", To: "local", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-exact", Kind: "declares", From: "package-a", To: "exact", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-cross", Kind: "declares", From: "package-b", To: "cross-only", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-collision", Kind: "declares", From: "package-b", To: "collision-is", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-default-import", Kind: "declares", From: "package-target", To: "default-import-target", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-alias-import", Kind: "declares", From: "package-target", To: "alias-import-target", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-ambiguous-a", Kind: "declares", From: "package-a", To: "ambiguous-owner", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "own-ambiguous-b", Kind: "declares", From: "package-b", To: "ambiguous-owner", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "default-import", Kind: "imports", From: "package-a", To: "target-dependency", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "alias-import", Kind: "imports", From: "package-a", To: "target-dependency", Resolution: rkcmodel.ResolutionDeclared, Attributes: map[string]any{"alias": "alias"}},
			{ID: "cycle-a-contains-b", Kind: "contains", From: "cycle-a", To: "cycle-b", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "cycle-b-declares-a", Kind: "declares", From: "cycle-b", To: "cycle-a", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "cycle-a-declares-caller", Kind: "declares", From: "cycle-a", To: "cycle-caller", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "cycle-b-declares-target", Kind: "declares", From: "cycle-b", To: "cycle-target", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "local-call", Kind: "calls", From: "caller-a", To: "u-local", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "local-call"}},
			{ID: "compiler-owned-call", Kind: "calls", From: "compiler-caller", To: "u-compiler-target", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "compiler-owned-call"}},
			{ID: "exact-qualified", Kind: "calls", From: "caller-a", To: "u-exact", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "exact-qualified"}},
			{ID: "cross-package", Kind: "calls", From: "caller-a", To: "u-cross", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "cross-package"}},
			{ID: "external-qualifier-collision", Kind: "calls", From: "caller-a", To: "u-collision", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "external-qualifier-collision"}},
			{ID: "default-import-call", Kind: "calls", From: "caller-a", To: "u-default-import", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "default-import-call"}},
			{ID: "alias-import-call", Kind: "calls", From: "caller-a", To: "u-alias-import", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "alias-import-call"}},
			{ID: "ambiguous-ownership", Kind: "calls", From: "caller-a", To: "u-ambiguous-owner", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "ambiguous-ownership"}},
			{ID: "unowned-language-call", Kind: "calls", From: "unowned-caller", To: "u-unowned", Resolution: rkcmodel.ResolutionUnresolved, Producer: "rkc.go-ast", Attributes: map[string]any{"original_test_id": "unowned-language-call"}},
			{ID: "framework-artifact-call", Kind: "references", From: "framework-caller", To: "u-framework", Resolution: rkcmodel.ResolutionUnresolved, Producer: openapi.PluginID, Attributes: map[string]any{"original_test_id": "framework-artifact-call"}},
			{ID: "cyclic-ownership-call", Kind: "calls", From: "cycle-caller", To: "u-cycle-target", Resolution: rkcmodel.ResolutionUnresolved, Attributes: map[string]any{"original_test_id": "cyclic-ownership-call"}},
		},
	}

	resolveHeuristicEdges(&bundle)
	for id, want := range map[string]struct {
		target string
		proof  string
	}{
		"local-call":              {target: "local", proof: "shared_language_scope"},
		"compiler-owned-call":     {target: "compiler-target", proof: "shared_language_scope"},
		"exact-qualified":         {target: "exact", proof: "exact_qualified"},
		"framework-artifact-call": {target: "framework-local", proof: "framework_artifact"},
	} {
		edge := pipelineEdgeByOriginalID(t, bundle.Edges, id)
		if edge.To != want.target || edge.Resolution != rkcmodel.ResolutionSyntaxInferred || edge.Attributes["resolver_proof"] != want.proof {
			t.Errorf("edge %q = %+v, want target=%q proof=%q", id, edge, want.target, want.proof)
		}
	}
	for id, target := range map[string]string{
		"cross-package":                "u-cross",
		"external-qualifier-collision": "u-collision",
		"default-import-call":          "u-default-import",
		"alias-import-call":            "u-alias-import",
		"ambiguous-ownership":          "u-ambiguous-owner",
		"unowned-language-call":        "u-unowned",
		"cyclic-ownership-call":        "u-cycle-target",
	} {
		edge := pipelineEdgeByOriginalID(t, bundle.Edges, id)
		if edge.To != target || edge.Resolution != rkcmodel.ResolutionUnresolved || edge.Attributes["resolver"] != nil {
			t.Errorf("edge %q was not kept unresolved: %+v", id, edge)
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
