package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/pipeline"
)

func TestResidualPlanCacheAndDefaultPathUX(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	writeTestFile(t, filepath.Join(repository, "main.go"), "package fixture\n")
	output, err := captureStdout(t, func() error {
		return runPlan([]string{
			"--no-cache", "--no-plugins", "--no-frameworks", "--no-secret-scan",
			repository,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"Scan plan:", "Stage cache: disabled", "Stages:", "Planning reads and hashes source",
	} {
		if !strings.Contains(output, expected) {
			t.Errorf("text plan missing %q: %s", expected, output)
		}
	}

	cacheRoot := filepath.Join(root, "cache")
	output, err = captureStdout(t, func() error {
		return runCacheInspect([]string{"--cache-dir", cacheRoot})
	})
	if err != nil || !strings.Contains(output, "Health: true") {
		t.Fatalf("empty cache inspection = %q, %v", output, err)
	}
	output, err = captureStdout(t, func() error {
		return runCachePrune([]string{
			"--cache-dir", cacheRoot, "--all", "--dry-run",
		})
	})
	if err != nil || !strings.Contains(output, "Would remove") ||
		!strings.Contains(output, "Entries remaining:") {
		t.Fatalf("dry-run cache pruning = %q, %v", output, err)
	}
	output, err = captureStdout(t, func() error {
		printCacheReport(pipeline.StageCacheReport{
			Root: cacheRoot, Healthy: false, InvalidEntries: 1,
			Entries: []pipeline.StageCacheEntry{
				{StageID: "invalid", CacheKey: "stage:short", Valid: false, Issue: "corrupt"},
				{StageID: "hidden", CacheKey: "stage:" + strings.Repeat("a", 64), Valid: true},
			},
		}, 1)
		return nil
	})
	if err != nil || !strings.Contains(output, "invalid: corrupt") ||
		!strings.Contains(output, "1 more entrie") {
		t.Fatalf("bounded cache report = %q, %v", output, err)
	}
	if got := shortDigest("stage:short"); got != "short" {
		t.Fatalf("short digest = %q", got)
	}
	if _, err := openCLIStageCache(" "); err == nil {
		t.Fatal("blank cache directory was accepted")
	}

	t.Run("default path fallbacks", func(t *testing.T) {
		t.Setenv("XDG_CACHE_HOME", "")
		t.Setenv("HOME", "")
		if got := defaultStageCacheDirectory(); got != filepath.Join(".rkc-state", "cache") {
			t.Fatalf("fallback stage cache = %q", got)
		}
		if _, err := defaultRunJournalDirectory(); err == nil {
			t.Fatal("missing user cache directory produced a run-journal default")
		}
	})
}
