package pipeline

import (
	"reflect"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestMergedProjectKeepsSourceAndArtifactTogether(t *testing.T) {
	syntax := rkcmodel.Node{ID: "project", Kind: "project", Name: "example", ArtifactID: "typescript", EvidenceIDs: []string{"syntax-evidence"}}
	manifest := rkcmodel.Node{ID: "project", Kind: "project", Name: "example", ArtifactID: "manifest", Source: &rkcmodel.SourceRange{ArtifactID: "manifest", Path: "package.json", Anchor: "#"}, EvidenceIDs: []string{"manifest-evidence"}}
	for _, nodes := range [][]rkcmodel.Node{{syntax, manifest}, {manifest, syntax}} {
		bundle := rkcmodel.Bundle{Nodes: nodes}
		dedupeBundle(&bundle)
		if len(bundle.Nodes) != 1 {
			t.Fatal("shared project was duplicated")
		}
		node := bundle.Nodes[0]
		if node.ArtifactID != "manifest" || !reflect.DeepEqual(node.Source, manifest.Source) {
			t.Fatalf("merged incompatible source provenance: %+v", node)
		}
		if !reflect.DeepEqual(node.EvidenceIDs, []string{"manifest-evidence", "syntax-evidence"}) {
			t.Fatalf("lost supporting evidence: %v", node.EvidenceIDs)
		}
	}
}
