package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/safeoutput"
	"github.com/neuroforge-io/RKC/pkg/knowledgepack"
)

func TestKnowledgeBuildVerifyAndOwnedReplacement(t *testing.T) {
	_, atlas, _ := makeScannedFixture(t)
	output := filepath.Join(t.TempDir(), "knowledge")
	stdout, err := captureStdout(t, func() error { return runKnowledge([]string{"build", "--out", output, "--json", atlas}) })
	if err != nil {
		t.Fatal(err)
	}
	var manifest knowledgepack.Manifest
	if err := json.Unmarshal([]byte(stdout), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.UnitsCount < 3 || manifest.SourcesCount != 1 {
		t.Fatalf("invalid pack: %+v", manifest)
	}
	stdout, err = captureStdout(t, func() error { return runKnowledge([]string{"verify", "--dir", output, "--json"}) })
	if err != nil || !strings.Contains(stdout, `"ok": true`) {
		t.Fatalf("verify: %s %v", stdout, err)
	}
	data, err := os.ReadFile(filepath.Join(output, "units.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Alpha calls Beta") {
		t.Fatal("verified repository body missing from knowledge pack")
	}
	if err := runKnowledge([]string{"build", "--out", output, atlas}); !errors.Is(err, safeoutput.ErrTargetExists) {
		t.Fatalf("unexpected existing output result: %v", err)
	}
	_, err = captureStdout(t, func() error { return runKnowledge([]string{"build", "--out", output, "--force", atlas}) })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := knowledgepack.Verify(context.Background(), output); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(output, "personal.txt"), "preserve this")
	if err := runKnowledge([]string{"build", "--out", output, "--force", atlas}); !errors.Is(err, safeoutput.ErrTargetUnowned) {
		t.Fatalf("unmanifested content was not protected: %v", err)
	}
	if _, err := os.Stat(filepath.Join(output, "personal.txt")); err != nil {
		t.Fatal("personal file lost")
	}
}

func TestKnowledgeRejectsAtlasOverlapAndNeverPublishesPartialPack(t *testing.T) {
	_, atlas, _ := makeScannedFixture(t)
	for _, target := range []string{atlas, filepath.Join(atlas, "nested"), filepath.Dir(atlas)} {
		if err := runKnowledge([]string{"build", "--out", target, "--force", atlas}); !errors.Is(err, safeoutput.ErrUnsafeTarget) {
			t.Fatalf("unsafe target %s result: %v", target, err)
		}
	}
	output := filepath.Join(t.TempDir(), "limited")
	if err := runKnowledge([]string{"build", "--out", output, "--max-units", "1", atlas}); err == nil {
		t.Fatal("partial pack accepted")
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed pack published output")
	}
	if err := runKnowledge([]string{"build", "--out", output, atlas, atlas}); err == nil {
		t.Fatal("duplicate source accepted")
	}
	for _, args := range [][]string{{}, {"unknown"}, {"build"}, {"verify"}, {"verify", "--dir", output, "extra"}, {"build", "--out", output, "--max-unit-text-bytes", "2", atlas}} {
		if err := runKnowledge(args); err == nil {
			t.Fatalf("invalid arguments accepted: %v", args)
		}
	}
	stdout, err := captureStdout(t, func() error { return runKnowledge([]string{"--help"}) })
	if err != nil || !strings.Contains(stdout, "knowledge build") {
		t.Fatalf("help: %s %v", stdout, err)
	}
}
