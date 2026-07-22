package modelruntime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/repository-knowledge-compiler/rkc/pkg/rkcmodel"
)

func TestBuildEvidencePacketIsBoundedCoherentAndRedacts(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "auth.go"), []byte("package auth\nconst token = \"ghp_abcdefghijklmnopqrstuvwxyz0123456789\"\nfunc Login() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	evidence := rkcmodel.Evidence{ID: "e1", Kind: "declared", Method: "go.ast", Confidence: 1, Source: &rkcmodel.SourceRange{ArtifactID: "a1", Path: "auth.go", StartLine: 1, EndLine: 3}}
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "s1"},
		Nodes: []rkcmodel.Node{
			{ID: "n1", Kind: "function", Name: "Login", QualifiedName: "auth.Login", EvidenceIDs: []string{"e1"}},
			{ID: "n2", Kind: "module", Name: "auth", QualifiedName: "auth", EvidenceIDs: []string{"e1"}},
		},
		Edges:    []rkcmodel.Edge{{ID: "edge", Kind: "declares", From: "n2", To: "n1", Resolution: "declared", EvidenceIDs: []string{"e1"}}},
		Evidence: []rkcmodel.Evidence{evidence},
	}
	report, err := BuildEvidencePacket(bundle, "n1", PacketBuildOptions{RepositoryRoot: root, MaximumTotalSourceBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if report.Packet.Subject.ID != "n1" || len(report.Packet.RelatedNodes) != 1 || len(report.Packet.Edges) != 1 || len(report.Packet.Evidence) != 1 {
		t.Fatalf("unexpected packet: %+v", report.Packet)
	}
	if len(report.Packet.SourceExcerpts) != 1 || report.RedactedSecretFindings != 1 {
		t.Fatalf("expected redacted source excerpt: %+v", report)
	}
	if got := report.Packet.SourceExcerpts[0].Text; got == "" || contains(got, "ghp_") {
		t.Fatalf("secret was not redacted: %q", got)
	}
	second, err := BuildEvidencePacket(bundle, "n1", PacketBuildOptions{RepositoryRoot: root, MaximumTotalSourceBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	if second.Packet.PacketID != report.Packet.PacketID {
		t.Fatalf("packet ID is not deterministic: %s != %s", second.Packet.PacketID, report.Packet.PacketID)
	}
}

func TestSourcePathEscapeRejectedWithoutFailingPacket(t *testing.T) {
	root := t.TempDir()
	bundle := rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{ID: "s"}, Nodes: []rkcmodel.Node{{ID: "n", Kind: "function", Name: "N", EvidenceIDs: []string{"e"}}}, Evidence: []rkcmodel.Evidence{{ID: "e", Kind: "declared", Method: "test", Confidence: 1, Source: &rkcmodel.SourceRange{Path: "../outside", StartLine: 1, EndLine: 1}}}}
	report, err := BuildEvidencePacket(bundle, "n", PacketBuildOptions{RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.MissingSourceArtifacts) != 1 || len(report.Packet.SourceExcerpts) != 0 {
		t.Fatalf("expected explicit missing source report: %+v", report)
	}
}

func contains(value, needle string) bool {
	for i := 0; i+len(needle) <= len(value); i++ {
		if value[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
