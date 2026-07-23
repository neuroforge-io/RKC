package pipeline

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/cas"
	"github.com/neuroforge-io/RKC/internal/scheduler"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestStageCacheWarmReuseSelectiveInvalidationAndCleanEquivalence(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, ".env"), "RKC_PORT=8787\n")
	mustWritePipelineFile(t, filepath.Join(root, "README.md"), "# Fixture\n\nCalls `Run` and `greet`.\n")
	mustWritePipelineFile(t, filepath.Join(root, "main.go"), "package fixture\n\nfunc Run() bool { return true }\n")
	mustWritePipelineFile(t, filepath.Join(root, "web/index.ts"), "export const greet = (name: string) => `hello ${name}`\n")

	cache, err := OpenStageCache(filepath.Join(t.TempDir(), "stage-cache"))
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Root: root, ToolVersion: "cache-test", DisablePythonAST: true,
		Cache: cache,
	}
	var firstEvents []scheduler.Event
	options.OnStageEvent = func(event scheduler.Event) {
		firstEvents = append(firstEvents, event)
	}
	firstBundle, firstCoverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if cachedStages(firstEvents) != nil {
		t.Fatalf("cold scan unexpectedly used cached stages: %v", cachedStages(firstEvents))
	}

	var warmEvents []scheduler.Event
	options.OnStageEvent = func(event scheduler.Event) {
		warmEvents = append(warmEvents, event)
	}
	warmBundle, warmCoverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	wantWarm := []string{
		"env-keys", "go-syntax", "json-schema", "manifests",
		"markdown", "openapi", "secret-scan", "typescript-syntax",
	}
	if got := cachedStages(warmEvents); !equalStrings(got, wantWarm) {
		t.Fatalf("warm cached stages = %v, want %v", got, wantWarm)
	}
	requireCanonicalScanEquality(t, firstBundle, firstCoverage, warmBundle, warmCoverage)

	report, err := cache.Inspect(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Healthy || report.EntryCount != len(wantWarm) ||
		report.ValidEntries != len(wantWarm) || report.InvalidEntries != 0 ||
		report.ObjectCount != len(wantWarm) || report.OrphanObjects != 0 {
		t.Fatalf("verified cache report = %+v", report)
	}

	mustWritePipelineFile(
		t,
		filepath.Join(root, "README.md"),
		"# Fixture\n\nCalls `Run` and `greet`; documentation changed.\n",
	)
	var incrementalEvents []scheduler.Event
	options.OnStageEvent = func(event scheduler.Event) {
		incrementalEvents = append(incrementalEvents, event)
	}
	incrementalBundle, incrementalCoverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	wantSelectiveHits := []string{
		"env-keys", "go-syntax", "json-schema", "openapi", "typescript-syntax",
	}
	if got := cachedStages(incrementalEvents); !equalStrings(got, wantSelectiveHits) {
		t.Fatalf("selective cached stages = %v, want %v", got, wantSelectiveHits)
	}
	cleanOptions := options
	cleanOptions.Cache = nil
	cleanOptions.OnStageEvent = nil
	cleanBundle, cleanCoverage, err := Scan(context.Background(), cleanOptions)
	if err != nil {
		t.Fatal(err)
	}
	requireCanonicalScanEquality(
		t,
		incrementalBundle,
		incrementalCoverage,
		cleanBundle,
		cleanCoverage,
	)

	dryRun, err := cache.Prune(context.Background(), StageCachePruneOptions{
		All: true, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if dryRun.EntriesSelected == 0 || dryRun.ObjectsSelected == 0 ||
		dryRun.MetadataBytes <= 0 || dryRun.PayloadBytes <= 0 {
		t.Fatalf("cache prune dry-run = %+v", dryRun)
	}
	afterDryRun, err := cache.Inspect(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if afterDryRun.EntryCount != report.EntryCount+3 {
		t.Fatalf(
			"dry-run changed cache or selective miss count unexpected: entries=%d, want %d",
			afterDryRun.EntryCount,
			report.EntryCount+3,
		)
	}
	pruned, err := cache.Prune(context.Background(), StageCachePruneOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if pruned.EntriesSelected != dryRun.EntriesSelected ||
		pruned.ObjectsSelected != dryRun.ObjectsSelected ||
		pruned.EntriesRemaining != 0 {
		t.Fatalf("cache prune = %+v, dry-run = %+v", pruned, dryRun)
	}
	empty, err := cache.Inspect(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !empty.Healthy || empty.EntryCount != 0 || empty.ObjectCount != 0 {
		t.Fatalf("cache after prune = %+v", empty)
	}
}

func TestStageCacheRejectsCorruptAndMalformedPayloadsThenRecomputes(t *testing.T) {
	root := t.TempDir()
	mustWritePipelineFile(t, filepath.Join(root, "main.go"), "package fixture\n\nfunc Run() {}\n")
	cache, err := OpenStageCache(filepath.Join(t.TempDir(), "stage-cache"))
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Root: root, ToolVersion: "cache-test", DisablePythonAST: true,
		DisableTypeScript: true, DisableFrameworks: true, DisableSecretScan: true,
		Cache: cache,
	}
	baseline, baselineCoverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	report, err := cache.Inspect(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if report.EntryCount != 1 || report.Entries[0].StageID != "go-syntax" {
		t.Fatalf("initial cache report = %+v", report)
	}
	objectPath, err := cache.objects.Path(report.Entries[0].ObjectDigest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(objectPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(objectPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	var corruptEvents []scheduler.Event
	options.OnStageEvent = func(event scheduler.Event) {
		corruptEvents = append(corruptEvents, event)
	}
	recomputed, recomputedCoverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if containsStringValue(cachedStages(corruptEvents), "go-syntax") {
		t.Fatalf("corrupt payload was reported as cached: %v", corruptEvents)
	}
	requireCanonicalScanEquality(
		t,
		baseline,
		baselineCoverage,
		recomputed,
		recomputedCoverage,
	)

	report, err = cache.Inspect(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Healthy || report.EntryCount != 1 || report.ObjectCount != 1 {
		t.Fatalf("cache after corruption recovery = %+v", report)
	}
	entry := report.Entries[0]
	malformed, err := cache.objects.PutBytes([]byte(`{"schema_version":"1.0","stage_id":"go-syntax"}`))
	if err != nil {
		t.Fatal(err)
	}
	result, ok, err := cache.metadata.Load(context.Background(), entry.CacheKey)
	if err != nil || !ok {
		t.Fatalf("load cache metadata = %+v, %t, %v", result, ok, err)
	}
	result.ObjectDigest = malformed.Digest
	if err := cache.metadata.Delete(context.Background(), entry.CacheKey); err != nil {
		t.Fatal(err)
	}
	if err := cache.metadata.Store(context.Background(), entry.CacheKey, result); err != nil {
		t.Fatal(err)
	}
	var malformedEvents []scheduler.Event
	options.OnStageEvent = func(event scheduler.Event) {
		malformedEvents = append(malformedEvents, event)
	}
	restored, restoredCoverage, err := Scan(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if containsStringValue(cachedStages(malformedEvents), "go-syntax") {
		t.Fatalf("malformed payload was reported as cached: %v", malformedEvents)
	}
	requireCanonicalScanEquality(
		t,
		baseline,
		baselineCoverage,
		restored,
		restoredCoverage,
	)
	finalReport, err := cache.Inspect(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !finalReport.Healthy || finalReport.EntryCount != 1 ||
		finalReport.ValidEntries != 1 || finalReport.OrphanObjects != 1 {
		t.Fatalf("cache after malformed recovery = %+v", finalReport)
	}
}

func TestStageCachePruneAgeValidationAndMissingObject(t *testing.T) {
	cache, err := OpenStageCache(filepath.Join(t.TempDir(), "stage-cache"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Prune(context.Background(), StageCachePruneOptions{}); err == nil {
		t.Fatal("Prune without age or all succeeded")
	}
	if _, err := OpenStageCache(""); err == nil {
		t.Fatal("OpenStageCache(empty) succeeded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cache.Inspect(ctx, true); err == nil {
		t.Fatal("Inspect(cancelled) succeeded")
	}

	object, err := cache.objects.PutBytes([]byte("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.objects.Verify(object.Digest); err != nil {
		t.Fatal(err)
	}
	pruned, err := cache.Prune(context.Background(), StageCachePruneOptions{
		OlderThan: time.Hour, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if pruned.EntriesSelected != 0 || pruned.ObjectsSelected != 1 {
		t.Fatalf("orphan prune = %+v", pruned)
	}
	if present, err := cache.objects.Has(object.Digest); err != nil || present {
		t.Fatalf("orphan object remains: present=%t err=%v", present, err)
	}

	if _, err := cas.NormalizeDigest(strings.Repeat("z", 64)); err == nil {
		t.Fatal("test precondition: invalid digest accepted")
	}
}

func cachedStages(events []scheduler.Event) []string {
	var stages []string
	for _, event := range events {
		if event.State == "cached" {
			stages = append(stages, event.StageID)
		}
	}
	sort.Strings(stages)
	if len(stages) == 0 {
		return nil
	}
	return stages
}

func requireCanonicalScanEquality(
	t *testing.T,
	left rkcmodel.Bundle,
	leftCoverage rkcmodel.Coverage,
	right rkcmodel.Bundle,
	rightCoverage rkcmodel.Coverage,
) {
	t.Helper()
	leftJSON, err := rkcmodel.CanonicalJSON(left)
	if err != nil {
		t.Fatal(err)
	}
	rightJSON, err := rkcmodel.CanonicalJSON(right)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leftJSON, rightJSON) {
		t.Fatalf("canonical bundles differ:\nleft: %s\nright: %s", leftJSON, rightJSON)
	}
	if rkcmodel.DigestJSON(leftCoverage) != rkcmodel.DigestJSON(rightCoverage) {
		t.Fatalf("coverage differs:\nleft: %+v\nright: %+v", leftCoverage, rightCoverage)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsStringValue(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
