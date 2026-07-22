package modelruntime

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestBuildEvidencePacketIsBoundedCoherentAndRedacts(t *testing.T) {
	root := t.TempDir()
	contents := []byte("package auth\nconst token = \"ghp_" + "abcdefghijklmnopqrstuvwxyz0123456789\"\nfunc Login() {}\n")
	if err := os.WriteFile(filepath.Join(root, "auth.go"), contents, 0o644); err != nil {
		t.Fatal(err)
	}
	evidence := rkcmodel.Evidence{ID: "e1", Kind: "declared", Method: "go.ast", Confidence: 1, Source: &rkcmodel.SourceRange{ArtifactID: "a1", Path: "auth.go", StartLine: 1, EndLine: 3}}
	bundle := rkcmodel.Bundle{
		Snapshot: rkcmodel.Snapshot{ID: "s1"},
		Artifacts: []rkcmodel.Artifact{
			packetArtifact("a1", "auth.go", contents),
		},
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
	bundle := rkcmodel.Bundle{Snapshot: rkcmodel.Snapshot{ID: "s"}, Artifacts: []rkcmodel.Artifact{packetArtifact("a", "../outside", []byte("x"))}, Nodes: []rkcmodel.Node{{ID: "n", Kind: "function", Name: "N", EvidenceIDs: []string{"e"}}}, Evidence: []rkcmodel.Evidence{{ID: "e", Kind: "declared", Method: "test", Confidence: 1, Source: &rkcmodel.SourceRange{ArtifactID: "a", Path: "../outside", StartLine: 1, EndLine: 1}}}}
	report, err := BuildEvidencePacket(bundle, "n", PacketBuildOptions{RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.MissingSourceArtifacts) != 1 || len(report.Packet.SourceExcerpts) != 0 {
		t.Fatalf("expected explicit missing source report: %+v", report)
	}
}

func TestSourceExcerptRejectsSymlinkAndStaleContent(t *testing.T) {
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		contents := []byte("outside evidence\n")
		outside := filepath.Join(t.TempDir(), "outside.go")
		if err := os.WriteFile(outside, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(root, "linked.go")); err != nil {
			t.Fatal(err)
		}
		bundle := packetSourceBundle(packetArtifact("a", "linked.go", contents), rkcmodel.SourceRange{ArtifactID: "a", Path: "linked.go", StartByte: 0, EndByte: int64(len(contents))})
		report, err := BuildEvidencePacket(bundle, "n", PacketBuildOptions{RepositoryRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Packet.SourceExcerpts) != 0 || len(report.MissingSourceArtifacts) != 1 || !contains(report.MissingSourceArtifacts[0], "symlink") {
			t.Fatalf("symlink source was not rejected explicitly: %+v", report)
		}
	})

	t.Run("content changed", func(t *testing.T) {
		root := t.TempDir()
		inventoried := []byte("original\n")
		path := filepath.Join(root, "source.go")
		if err := os.WriteFile(path, inventoried, 0o600); err != nil {
			t.Fatal(err)
		}
		artifact := packetArtifact("a", "source.go", inventoried)
		if err := os.WriteFile(path, []byte("modified\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		bundle := packetSourceBundle(artifact, rkcmodel.SourceRange{ArtifactID: "a", Path: "source.go", StartByte: 0, EndByte: int64(len(inventoried))})
		report, err := BuildEvidencePacket(bundle, "n", PacketBuildOptions{RepositoryRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Packet.SourceExcerpts) != 0 || len(report.MissingSourceArtifacts) != 1 || !contains(report.MissingSourceArtifacts[0], "content changed") {
			t.Fatalf("stale source was not rejected explicitly: %+v", report)
		}
	})
}

func TestSourceExcerptIsStrictlyRangedAndBounded(t *testing.T) {
	root := t.TempDir()
	contents := []byte("0123456789")
	if err := os.WriteFile(filepath.Join(root, "source.txt"), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	artifact := packetArtifact("a", "source.txt", contents)

	invalid := packetSourceBundle(artifact, rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt", StartByte: 2, EndByte: 99})
	report, err := BuildEvidencePacket(invalid, "n", PacketBuildOptions{RepositoryRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Packet.SourceExcerpts) != 0 || len(report.MissingSourceArtifacts) != 1 || !contains(report.MissingSourceArtifacts[0], "outside") {
		t.Fatalf("invalid range silently fell back to source content: %+v", report)
	}

	bounded := packetSourceBundle(artifact, rkcmodel.SourceRange{ArtifactID: "a", Path: "source.txt", StartByte: 2, EndByte: 8})
	report, err = BuildEvidencePacket(bounded, "n", PacketBuildOptions{RepositoryRoot: root, MaximumExcerptBytes: 4, MaximumTotalSourceBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Packet.SourceExcerpts) != 1 || report.Packet.SourceExcerpts[0].Text != "2345" || !report.Packet.SourceExcerpts[0].Truncated || report.IncludedSourceBytes != 4 {
		t.Fatalf("bounded excerpt = %+v", report)
	}
}

func packetArtifact(id, path string, contents []byte) rkcmodel.Artifact {
	digest := sha256.Sum256(contents)
	return rkcmodel.Artifact{ID: id, Path: path, Kind: "file", Text: true, Status: "text", SizeBytes: int64(len(contents)), SHA256: fmt.Sprintf("%x", digest[:])}
}

func packetSourceBundle(artifact rkcmodel.Artifact, source rkcmodel.SourceRange) rkcmodel.Bundle {
	return rkcmodel.Bundle{
		Snapshot:  rkcmodel.Snapshot{ID: "s"},
		Artifacts: []rkcmodel.Artifact{artifact},
		Nodes:     []rkcmodel.Node{{ID: "n", Kind: "function", Name: "N", EvidenceIDs: []string{"e"}}},
		Evidence:  []rkcmodel.Evidence{{ID: "e", Kind: "declared", Method: "test", Confidence: 1, Source: &source}},
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
