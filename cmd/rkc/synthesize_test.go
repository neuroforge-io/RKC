package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/repository-knowledge-compiler/rkc/internal/modelruntime"
)

func TestSynthesizePacketOnlyLeavesCanonicalBundleUntouched(t *testing.T) {
	repository, err := filepath.Abs(filepath.Join("..", "..", "examples", "sample-go"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	output := filepath.Join(root, "output")
	if err := runScan([]string{"--out", output, "--no-python", "--no-typescript", "--force", repository}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(output, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	derived := filepath.Join(root, "synthesis")
	if err := runSynthesize([]string{"--dir", output, "--repo-root", repository, "--out", derived, "--packet-only", "--query", "Login", "--limit", "1", "--force"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(output, "bundle.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("derived synthesis mutated canonical bundle")
	}
	manifestData, err := os.ReadFile(filepath.Join(derived, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest synthesisManifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.PacketOnly || manifest.PacketsWritten != 1 || manifest.ResponsesReceived != 0 {
		t.Fatalf("unexpected manifest: %+v", manifest)
	}
	packetFiles, err := filepath.Glob(filepath.Join(derived, "packets", "*.json"))
	if err != nil || len(packetFiles) != 1 {
		t.Fatalf("packet files=%v err=%v", packetFiles, err)
	}
	var report modelruntime.PacketBuildReport
	data, err := os.ReadFile(packetFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Packet.Subject.Name != "Login" || len(report.Packet.Evidence) == 0 || len(report.Packet.SourceExcerpts) == 0 {
		t.Fatalf("incomplete packet: %+v", report)
	}
}
