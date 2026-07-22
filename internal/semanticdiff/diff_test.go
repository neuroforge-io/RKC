package semanticdiff

import (
	"reflect"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestCompareReportsStructuralChangesSummariesAndSeverityOrdering(t *testing.T) {
	t.Parallel()

	before := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "before"},
		Artifacts: []rkcmodel.Artifact{
			{ID: "unchanged-old-id", Path: "unchanged.go", SHA256: "same", Status: "included"},
			{ID: "removed", Path: "removed.go", SHA256: "old"},
			{ID: "modified-before", Path: "modified.go", SHA256: "old", Status: "included", Language: "go", MediaType: "text/x-go"},
		},
		Nodes: []rkcmodel.Node{
			{ID: "public-removed", LogicalID: "public-removed", Kind: "function", Name: "Public", PublicSurface: true},
			{ID: "private-removed", LogicalID: "private-removed", Kind: "function", Name: "private"},
			{ID: "breaking-before", LogicalID: "breaking", Kind: "function", Name: "API", Signature: "func API()", Visibility: "public", PublicSurface: true},
			{ID: "rename-before", LogicalID: "rename", Kind: "function", Name: "Rename", QualifiedName: "pkg.Old"},
			{ID: "semantic-before", LogicalID: "semantic", Kind: "method", Name: "Work", SemanticHash: "old"},
			{ID: "info-before", LogicalID: "info", Kind: "variable", Name: "old", ArtifactID: "old-artifact"},
			{ID: "stable", LogicalID: "stable", Kind: "class", Name: "Stable"},
		},
		Edges: []rkcmodel.Edge{
			{ID: "edge-removed", Kind: "calls", From: "a", To: "b", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "edge-mod-before", Kind: "imports", From: "a", To: "c", Resolution: rkcmodel.ResolutionDeclared, Confidence: .5, Producer: "old"},
			{ID: "edge-stable-before", Kind: "references", From: "a", To: "d", Resolution: "resolved", Confidence: 1, Producer: "same"},
		},
	}
	after := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "after"},
		Artifacts: []rkcmodel.Artifact{
			{ID: "unchanged-new-id", Path: "unchanged.go", SHA256: "same", Status: "included"},
			{ID: "added", Path: "added.go", SHA256: "new"},
			{ID: "modified-after", Path: "modified.go", SHA256: "new", Status: "semantic_parsed", Language: "rust", MediaType: "text/x-rust", Generated: true, Vendored: true},
		},
		Nodes: []rkcmodel.Node{
			{ID: "added", LogicalID: "added", Kind: "function", Name: "Added", Visibility: "public"},
			{ID: "breaking-after", LogicalID: "breaking", Kind: "function", Name: "API", Signature: "func API(int)", Visibility: "private"},
			{ID: "rename-after", LogicalID: "rename", Kind: "function", Name: "Rename", QualifiedName: "pkg.New"},
			{ID: "semantic-after", LogicalID: "semantic", Kind: "method", Name: "Work", SemanticHash: "new"},
			{ID: "info-after", LogicalID: "info", Kind: "variable", Name: "new", ArtifactID: "new-artifact"},
			{ID: "stable-new-id", LogicalID: "stable", Kind: "class", Name: "Stable"},
		},
		Edges: []rkcmodel.Edge{
			{ID: "edge-added", Kind: "calls", From: "b", To: "c", Resolution: rkcmodel.ResolutionDeclared},
			{ID: "edge-mod-after", Kind: "imports", From: "a", To: "c", Resolution: rkcmodel.ResolutionUnresolved, Confidence: .9, Producer: "new"},
			{ID: "edge-stable-after", Kind: "references", From: "a", To: "d", Resolution: rkcmodel.ResolutionCompilerResolved, Confidence: 1, Producer: "same"},
		},
	}

	report := Compare(before, after)
	if report.SchemaVersion != "1" || report.FromSnapshot != "before" || report.ToSnapshot != "after" {
		t.Fatalf("report envelope = %+v", report)
	}
	wantSummary := Summary{
		ArtifactsAdded: 1, ArtifactsRemoved: 1, ArtifactsModified: 1,
		NodesAdded: 1, NodesRemoved: 2, NodesModified: 4,
		EdgesAdded: 1, EdgesRemoved: 1, EdgesModified: 1,
		BreakingChanges: 2, RiskChanges: 2,
	}
	if report.Summary != wantSummary {
		t.Fatalf("summary = %+v, want %+v", report.Summary, wantSummary)
	}
	if len(report.Artifacts) != 3 || report.Artifacts[0].Kind != "added" || report.Artifacts[0].After.Path != "added.go" ||
		report.Artifacts[1].Kind != "modified" || report.Artifacts[1].Before.Path != "modified.go" ||
		report.Artifacts[2].Kind != "removed" || report.Artifacts[2].Before.Path != "removed.go" {
		t.Fatalf("artifact changes/order = %+v", report.Artifacts)
	}
	if want := []string{"content", "status", "language", "media_type", "generated", "vendored"}; !reflect.DeepEqual(report.Artifacts[1].Fields, want) {
		t.Errorf("artifact modified fields = %v", report.Artifacts[1].Fields)
	}

	if len(report.Nodes) != 7 {
		t.Fatalf("node changes = %+v", report.Nodes)
	}
	if report.Nodes[0].Severity != SeverityBreaking || report.Nodes[1].Severity != SeverityBreaking || report.Nodes[2].Severity != SeverityRisk || report.Nodes[3].Severity != SeverityRisk {
		t.Fatalf("severity ordering = %+v", report.Nodes)
	}
	changes := map[string]NodeChange{}
	for _, change := range report.Nodes {
		changes[change.LogicalKey] = change
	}
	if got := changes["public-removed"]; got.Kind != "removed" || got.Severity != SeverityBreaking || !reflect.DeepEqual(got.Reasons, []string{"public symbol removed"}) || got.Before == nil || got.After != nil {
		t.Errorf("public removal = %+v", got)
	}
	if got := changes["private-removed"]; got.Kind != "removed" || got.Severity != SeverityInfo || got.Before == nil {
		t.Errorf("private removal = %+v", got)
	}
	if got := changes["added"]; got.Kind != "added" || got.Severity != SeverityInfo || got.After == nil || got.Before != nil {
		t.Errorf("addition = %+v", got)
	}
	if got := changes["breaking"]; got.Kind != "modified" || got.Severity != SeverityBreaking || !reflect.DeepEqual(got.Reasons, []string{"public signature changed", "public symbol became non-public"}) ||
		!reflect.DeepEqual(got.Fields, []string{"signature", "visibility", "public_surface"}) {
		t.Errorf("breaking modification = %+v", got)
	}
	if got := changes["rename"]; got.Severity != SeverityRisk || !reflect.DeepEqual(got.Reasons, []string{"logical name changed"}) || !reflect.DeepEqual(got.Fields, []string{"qualified_name"}) {
		t.Errorf("rename risk = %+v", got)
	}
	if got := changes["semantic"]; got.Severity != SeverityRisk || !reflect.DeepEqual(got.Reasons, []string{"implementation semantics changed"}) {
		t.Errorf("semantic risk = %+v", got)
	}
	if got := changes["info"]; got.Severity != SeverityInfo || !reflect.DeepEqual(got.Fields, []string{"name", "artifact"}) {
		t.Errorf("informational modification = %+v", got)
	}

	if len(report.Edges) != 3 || report.Edges[0].Kind != "removed" || report.Edges[1].Kind != "added" || report.Edges[2].Kind != "modified" {
		t.Fatalf("edge changes/order = %+v", report.Edges)
	}
	if !reflect.DeepEqual(report.Edges[2].Fields, []string{"resolution", "confidence", "producer"}) {
		t.Errorf("edge modified fields = %v", report.Edges[2].Fields)
	}
}

func TestCompareEqualBundlesIsEmptyAndDeterministic(t *testing.T) {
	t.Parallel()

	bundle := rkcmodel.Bundle{
		Snapshot:  rkcmodel.Snapshot{ID: "same"},
		Artifacts: []rkcmodel.Artifact{{ID: "a", Path: "a", SHA256: "x"}},
		Nodes:     []rkcmodel.Node{{ID: "n", Kind: "function", QualifiedName: "pkg.N"}},
		Edges:     []rkcmodel.Edge{{ID: "e", Kind: "calls", From: "n", To: "n", Resolution: "resolved"}},
	}
	report := Compare(bundle, bundle)
	if report.Summary != (Summary{}) || len(report.Artifacts) != 0 || len(report.Nodes) != 0 || len(report.Edges) != 0 {
		t.Fatalf("equal comparison = %+v", report)
	}

	alias := bundle
	alias.Edges = []rkcmodel.Edge{{ID: "new-id", Kind: "calls", From: "n", To: "n", Resolution: rkcmodel.ResolutionCompilerResolved}}
	if report := Compare(bundle, alias); len(report.Edges) != 0 {
		t.Fatalf("resolution alias or occurrence ID produced semantic change: %+v", report.Edges)
	}
}

func TestKeysFieldClassifiersAndSeverityHelpers(t *testing.T) {
	t.Parallel()

	if got := nodeKey(rkcmodel.Node{ID: "id", LogicalID: "logical", QualifiedName: "pkg.N"}); got != "logical" {
		t.Fatalf("logical nodeKey = %q", got)
	}
	if got := nodeKey(rkcmodel.Node{ID: "id", Language: "go", Kind: "function", QualifiedName: "pkg.N"}); got != "go\x00function\x00pkg.N" {
		t.Fatalf("qualified nodeKey = %q", got)
	}
	if got := nodeKey(rkcmodel.Node{ID: "id"}); got != "id" {
		t.Fatalf("ID nodeKey = %q", got)
	}
	edge := rkcmodel.Edge{Kind: "calls", From: "a", To: "b"}
	if got := edgeKey(edge); got != "calls\x00a\x00b" {
		t.Fatalf("edgeKey = %q", got)
	}

	artifactLeft := rkcmodel.Artifact{SHA256: "a", Status: "a", Language: "a", MediaType: "a"}
	artifactRight := rkcmodel.Artifact{SHA256: "b", Status: "b", Language: "b", MediaType: "b", Generated: true, Vendored: true}
	if got := artifactFields(artifactLeft, artifactRight); !reflect.DeepEqual(got, []string{"content", "status", "language", "media_type", "generated", "vendored"}) || len(artifactFields(artifactLeft, artifactLeft)) != 0 {
		t.Fatalf("artifactFields = %v", got)
	}
	nodeLeft := rkcmodel.Node{Name: "a", QualifiedName: "a", Signature: "a", Visibility: "a", ArtifactID: "a", SemanticHash: "a"}
	nodeRight := rkcmodel.Node{Name: "b", QualifiedName: "b", Signature: "b", Visibility: "b", PublicSurface: true, ArtifactID: "b", SemanticHash: "b"}
	if got := nodeFields(nodeLeft, nodeRight); !reflect.DeepEqual(got, []string{"name", "qualified_name", "signature", "visibility", "public_surface", "artifact", "semantic_hash"}) || len(nodeFields(nodeLeft, nodeLeft)) != 0 {
		t.Fatalf("nodeFields = %v", got)
	}
	edgeLeft := rkcmodel.Edge{Resolution: "resolved", Confidence: .1, Producer: "a"}
	edgeRight := rkcmodel.Edge{Resolution: rkcmodel.ResolutionUnresolved, Confidence: .2, Producer: "b"}
	if got := edgeFields(edgeLeft, edgeRight); !reflect.DeepEqual(got, []string{"resolution", "confidence", "producer"}) || len(edgeFields(edgeLeft, edgeLeft)) != 0 {
		t.Fatalf("edgeFields = %v", got)
	}

	for _, node := range []rkcmodel.Node{{PublicSurface: true}, {Visibility: "public"}, {Visibility: "exported"}} {
		if !isPublic(node) {
			t.Errorf("public node rejected: %+v", node)
		}
	}
	if isPublic(rkcmodel.Node{Visibility: "private"}) {
		t.Fatal("private node classified public")
	}
	if severity, reasons := classifyNodeRemoval(rkcmodel.Node{Visibility: "private"}); severity != SeverityInfo || reasons != nil {
		t.Fatalf("private removal = %q, %v", severity, reasons)
	}
	if severity, reasons := classifyNodeModification(rkcmodel.Node{}, rkcmodel.Node{}, nil); severity != SeverityInfo || reasons != nil {
		t.Fatalf("unchanged classification = %q, %v", severity, reasons)
	}
	if severity, reasons := classifyNodeModification(rkcmodel.Node{}, rkcmodel.Node{}, []string{"qualified_name", "semantic_hash"}); severity != SeverityRisk || !reflect.DeepEqual(reasons, []string{"logical name changed"}) {
		t.Fatalf("combined risk classification = %q, %v", severity, reasons)
	}
	if severityRank(SeverityBreaking) != 3 || severityRank(SeverityRisk) != 2 || severityRank(SeverityInfo) != 1 || severityRank("future") != 1 {
		t.Fatal("severity ranks mismatch")
	}
	if got := stringSet([]string{"a", "b", "a"}); !reflect.DeepEqual(got, map[string]bool{"a": true, "b": true}) {
		t.Fatalf("stringSet = %#v", got)
	}
	if got := unionKeys(map[string]int{"b": 1, "a": 2}, map[string]int{"c": 3, "b": 4}); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("unionKeys = %v", got)
	}
}
