package main

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/neuroforge-io/RKC/internal/pipeline"
	"github.com/neuroforge-io/RKC/internal/safeoutput"
)

func TestScanCacheUXAndAdministrativeCommands(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	writeTestFile(t, filepath.Join(repository, "go.mod"), "module example.test/cache\n\ngo 1.23\n")
	writeTestFile(t, filepath.Join(repository, "main.go"), "package cache\n\nfunc Ready() bool { return true }\n")
	output := filepath.Join(root, "atlas")
	cacheDir := filepath.Join(root, "cache")
	args := []string{
		"--out", output,
		"--cache-dir", cacheDir,
		"--no-python",
		"--no-typescript",
		"--no-frameworks",
		"--no-secret-scan",
		"--json",
		repository,
	}
	firstOutput, err := captureStdout(t, func() error { return runScanContext(context.Background(), args) })
	if err != nil {
		t.Fatal(err)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(firstOutput), &first); err != nil {
		t.Fatal(err)
	}
	if first["cache"] != cacheDir || first["cache_hits"] != float64(0) {
		t.Fatalf("cold scan cache summary = %+v", first)
	}

	warmArgs := append([]string{"--force"}, args...)
	warmOutput, err := captureStdout(t, func() error { return runScanContext(context.Background(), warmArgs) })
	if err != nil {
		t.Fatal(err)
	}
	var warm map[string]any
	if err := json.Unmarshal([]byte(warmOutput), &warm); err != nil {
		t.Fatal(err)
	}
	if warm["cache_hits"] != float64(1) ||
		warm["snapshot_id"] != first["snapshot_id"] ||
		warm["deterministic_digest"] != first["deterministic_digest"] {
		t.Fatalf("warm scan cache summary = %+v; cold = %+v", warm, first)
	}

	inspectOutput, err := captureStdout(t, func() error {
		return runCache([]string{"inspect", "--cache-dir", cacheDir, "--verify", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var inspect pipeline.StageCacheReport
	if err := json.Unmarshal([]byte(inspectOutput), &inspect); err != nil {
		t.Fatal(err)
	}
	if !inspect.Healthy || inspect.EntryCount != 1 || inspect.ValidEntries != 1 {
		t.Fatalf("cache inspect = %+v", inspect)
	}
	planOutput, err := captureStdout(t, func() error {
		return runPlanContext(context.Background(), []string{
			"--cache-dir", cacheDir,
			"--no-python",
			"--no-typescript",
			"--no-frameworks",
			"--no-secret-scan",
			"--json",
			repository,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	var plan pipeline.ScanPlan
	if err := json.Unmarshal([]byte(planOutput), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Summary.CacheHit != 1 ||
		plannedCLIStage(t, plan, "go-syntax").Disposition != "cache-hit" {
		t.Fatalf("warm CLI plan = %+v", plan)
	}
	if _, err := captureStdout(t, func() error {
		return runCache([]string{"verify", "--cache-dir", cacheDir})
	}); err != nil {
		t.Fatal(err)
	}
	if err := runCache([]string{"prune", "--cache-dir", cacheDir, "--all"}); err == nil ||
		!strings.Contains(err.Error(), "--yes") {
		t.Fatalf("unconfirmed cache prune = %v", err)
	}
	dryOutput, err := captureStdout(t, func() error {
		return runCache([]string{"prune", "--cache-dir", cacheDir, "--all", "--dry-run", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var dry pipeline.StageCachePruneReport
	if err := json.Unmarshal([]byte(dryOutput), &dry); err != nil {
		t.Fatal(err)
	}
	if !dry.DryRun || dry.EntriesSelected != 1 || dry.ObjectsSelected != 1 {
		t.Fatalf("cache prune dry-run = %+v", dry)
	}
	actualOutput, err := captureStdout(t, func() error {
		return runCache([]string{"prune", "--cache-dir", cacheDir, "--all", "--yes", "--json"})
	})
	if err != nil {
		t.Fatal(err)
	}
	var actual pipeline.StageCachePruneReport
	if err := json.Unmarshal([]byte(actualOutput), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.DryRun || actual.EntriesSelected != 1 || actual.ObjectsSelected != 1 {
		t.Fatalf("cache prune = %+v", actual)
	}
}

func TestScanRejectsRepositoryAndOutputCacheOverlap(t *testing.T) {
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	writeTestFile(t, filepath.Join(repository, "main.go"), "package fixture\n")
	output := filepath.Join(root, "atlas")
	for _, cacheDir := range []string{
		filepath.Join(repository, ".cache-custom"),
		filepath.Join(output, "cache"),
		filepath.Join(root, "shared"),
	} {
		testOutput := output
		if cacheDir == filepath.Join(root, "shared") {
			testOutput = filepath.Join(cacheDir, "atlas")
		}
		err := runScanContext(context.Background(), []string{
			"--out", testOutput,
			"--cache-dir", cacheDir,
			"--no-plugins",
			"--no-frameworks",
			repository,
		})
		if !errors.Is(err, safeoutput.ErrUnsafeTarget) {
			t.Errorf("overlapping cache %s error = %v", cacheDir, err)
		}
	}
}

func TestCacheCommandValidation(t *testing.T) {
	for _, args := range [][]string{
		nil,
		{"unknown"},
		{"inspect", "extra"},
		{"inspect", "--limit", "-1"},
		{"verify", "extra"},
		{"prune", "extra"},
		{"prune", "--older-than", "0"},
	} {
		if err := runCache(args); err == nil {
			t.Errorf("runCache(%v) succeeded", args)
		}
	}
	for _, args := range [][]string{
		{"one", "two"},
		{"--definitely-invalid"},
	} {
		if err := runPlanContext(context.Background(), args); err == nil {
			t.Errorf("runPlan(%v) succeeded", args)
		}
	}
	if got := shortDigest("stage:" + strings.Repeat("a", 64)); got != strings.Repeat("a", 12) {
		t.Fatalf("shortDigest = %q", got)
	}
	for value, want := range map[int64]string{
		1:                  "1 B",
		1024:               "1.00 KiB",
		1024 * 1024:        "1.00 MiB",
		1024 * 1024 * 1024: "1.00 GiB",
	} {
		if got := formatByteCount(value); got != want {
			t.Errorf("formatByteCount(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestPlanFailsClosedAtConfigurationAndCacheBoundaries(t *testing.T) {
	if err := runPlanContext(nil, nil); err == nil || !strings.Contains(err.Error(), "context is required") {
		t.Fatalf("nil plan context = %v", err)
	}
	root := t.TempDir()
	repository := filepath.Join(root, "repository")
	writeTestFile(t, filepath.Join(repository, "main.go"), "package fixture\n")
	writeTestFile(t, filepath.Join(repository, "README.md"), "# Fixture\n")

	malformed := filepath.Join(root, "malformed.json")
	writeTestFile(t, malformed, "{")
	if err := runPlanContext(context.Background(), []string{"--config", malformed, "--no-cache", repository}); err == nil ||
		!strings.Contains(err.Error(), "decode configuration") {
		t.Fatalf("malformed plan configuration = %v", err)
	}

	configuration, err := json.Marshal(defaultConfiguration())
	if err != nil {
		t.Fatal(err)
	}
	firstConfig := filepath.Join(root, "first.json")
	secondConfig := filepath.Join(root, "second.json")
	writeTestFile(t, firstConfig, string(configuration))
	writeTestFile(t, secondConfig, string(configuration))
	if err := runPlanContext(context.Background(), []string{
		"--config", firstConfig, "--config", secondConfig, "--no-cache", repository,
	}); err == nil || !strings.Contains(err.Error(), "--config must be supplied only once") {
		t.Fatalf("repeated plan configuration = %v", err)
	}

	insideCache := filepath.Join(repository, ".cache-custom")
	if err := runPlanContext(context.Background(), []string{
		"--cache-dir", insideCache, "--no-plugins", "--no-frameworks", repository,
	}); !errors.Is(err, safeoutput.ErrUnsafeTarget) {
		t.Fatalf("repository-contained plan cache = %v", err)
	}

	cacheFile := filepath.Join(root, "cache-file")
	writeTestFile(t, cacheFile, "not a directory")
	if err := runPlanContext(context.Background(), []string{
		"--cache-dir", cacheFile, "--no-plugins", "--no-frameworks", repository,
	}); err == nil {
		t.Fatal("regular file was accepted as a plan cache root")
	}

	cacheRoot := filepath.Join(root, "cache")
	output, err := captureStdout(t, func() error {
		return runPlanContext(context.Background(), []string{
			"--cache-dir", cacheRoot, "--no-frameworks", "--no-secret-scan", repository,
		})
	})
	if err != nil || !strings.Contains(output, "Stage cache: "+cacheRoot) {
		t.Fatalf("enabled plan cache output = %q, %v", output, err)
	}

	if err := runPlanContext(context.Background(), []string{
		"--no-cache", "--max-files", "1", "--no-plugins", "--no-frameworks", repository,
	}); err == nil || !strings.Contains(err.Error(), "plan inventory") {
		t.Fatalf("plan inventory limit = %v", err)
	}
}

func TestCacheCommandsRejectInvalidRootsAndBrokenOutput(t *testing.T) {
	root := t.TempDir()
	cacheFile := filepath.Join(root, "cache-file")
	writeTestFile(t, cacheFile, "not a directory")
	for name, args := range map[string][]string{
		"inspect": {"inspect", "--cache-dir", cacheFile},
		"verify":  {"verify", "--cache-dir", cacheFile},
		"prune":   {"prune", "--cache-dir", cacheFile, "--all", "--dry-run"},
	} {
		if err := runCache(args); err == nil {
			t.Errorf("cache %s accepted a regular-file root", name)
		}
	}
	cacheRoot := filepath.Join(root, "empty-cache")
	if err := withClosedStdout(func() error {
		return runCache([]string{"verify", "--cache-dir", cacheRoot, "--json"})
	}); err == nil {
		t.Fatal("cache verify succeeded with a closed stdout")
	}
}

func plannedCLIStage(t *testing.T, plan pipeline.ScanPlan, stageID string) pipeline.StagePlan {
	t.Helper()
	for _, stage := range plan.Stages {
		if stage.ID == stageID {
			return stage
		}
	}
	t.Fatalf("plan stage %s is missing", stageID)
	return pipeline.StagePlan{}
}
