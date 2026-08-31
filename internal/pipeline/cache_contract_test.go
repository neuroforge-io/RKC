package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/internal/cas"
	"github.com/neuroforge-io/RKC/internal/scheduler"
	"github.com/neuroforge-io/RKC/pkg/pluginapi"
	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func TestStageCacheNilAndOpenFailureContracts(t *testing.T) {
	var nilCache *StageCache
	if nilCache.Root() != "" {
		t.Fatalf("nil cache root = %q", nilCache.Root())
	}
	if _, _, err := nilCache.Load(context.Background(), stageCacheKey("0")); err == nil {
		t.Fatal("nil cache Load succeeded")
	}
	if err := nilCache.Store(context.Background(), stageCacheKey("0"), scheduler.Result{}); err == nil {
		t.Fatal("nil cache Store succeeded")
	}
	if err := nilCache.Invalidate(context.Background(), stageCacheKey("0")); err == nil {
		t.Fatal("nil cache Invalidate succeeded")
	}
	if digest, err := nilCache.putPayload([]byte("deterministic")); err != nil ||
		digest != cas.DigestBytes([]byte("deterministic")) {
		t.Fatalf("nil cache putPayload = %q, %v", digest, err)
	}
	if _, err := nilCache.readPayload(strings.Repeat("0", 64)); err == nil {
		t.Fatal("nil cache readPayload succeeded")
	}
	if _, err := nilCache.Inspect(context.Background(), true); err == nil {
		t.Fatal("nil cache Inspect succeeded")
	}
	if _, err := nilCache.Prune(
		context.Background(),
		StageCachePruneOptions{All: true},
	); err == nil {
		t.Fatal("nil cache Prune succeeded")
	}
	if ok, issue, err := nilCache.probe(
		context.Background(),
		stageCacheKey("0"),
		"go-syntax",
	); ok || issue != "stage cache is nil" || err != nil {
		t.Fatalf("nil cache probe = %t, %q, %v", ok, issue, err)
	}

	objectBlocked := t.TempDir()
	if err := os.WriteFile(filepath.Join(objectBlocked, "objects"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStageCache(objectBlocked); err == nil ||
		!strings.Contains(err.Error(), "open stage cache objects") {
		t.Fatalf("OpenStageCache(blocked objects) = %v", err)
	}

	metadataBlocked := t.TempDir()
	if _, err := cas.Open(filepath.Join(metadataBlocked, "objects")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(metadataBlocked, "entries"), []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenStageCache(metadataBlocked); err == nil ||
		!strings.Contains(err.Error(), "open stage cache metadata") {
		t.Fatalf("OpenStageCache(blocked metadata) = %v", err)
	}
}

func TestStageCacheLoadStoreAndProbeFailureContracts(t *testing.T) {
	cache := openContractStageCache(t)
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := cache.Store(cancelled, stageCacheKey("1"), scheduler.Result{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Store(cancelled) = %v", err)
	}
	if err := cache.Store(ctx, stageCacheKey("1"), scheduler.Result{}); err == nil ||
		!strings.Contains(err.Error(), "not bound") {
		t.Fatalf("Store(unbound) = %v", err)
	}
	if err := cache.Store(ctx, stageCacheKey("1"), scheduler.Result{
		CacheKey: stageCacheKey("1"), StageID: "go-syntax", ObjectDigest: "invalid",
	}); err == nil || !strings.Contains(err.Error(), "object digest") {
		t.Fatalf("Store(invalid digest) = %v", err)
	}
	missingDigest := strings.Repeat("0", 64)
	if err := cache.Store(ctx, stageCacheKey("1"), scheduler.Result{
		CacheKey: stageCacheKey("1"), StageID: "go-syntax", ObjectDigest: missingDigest,
	}); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("Store(missing object) = %v", err)
	}

	validPayload := validStagePayload("go-syntax")
	validObject := putRawStagePayload(t, cache, validPayload)
	validKey := stageCacheKey("2")
	validResult := scheduler.Result{
		CacheKey: validKey, StageID: "go-syntax", ObjectDigest: validObject,
		CacheHit: true, DoNotCache: true,
	}
	if err := cache.Store(ctx, validKey, validResult); err != nil {
		t.Fatal(err)
	}
	loaded, ok, err := cache.Load(ctx, validKey)
	if err != nil || !ok || loaded.CacheHit || loaded.DoNotCache {
		t.Fatalf("Load(valid) = %+v, %t, %v", loaded, ok, err)
	}

	invalidPointerKey := stageCacheKey("3")
	if err := cache.metadata.Store(ctx, invalidPointerKey, scheduler.Result{
		CacheKey: "wrong", StageID: "go-syntax", ObjectDigest: validObject,
	}); err != nil {
		t.Fatal(err)
	}
	if result, ok, err := cache.Load(ctx, invalidPointerKey); err != nil || ok ||
		result.StageID != "" || result.CacheKey != "" || result.ObjectDigest != "" {
		t.Fatalf("Load(invalid pointer) = %+v, %t, %v", result, ok, err)
	}

	invalidDigestKey := stageCacheKey("4")
	if err := cache.metadata.Store(ctx, invalidDigestKey, scheduler.Result{
		CacheKey: invalidDigestKey, StageID: "go-syntax", ObjectDigest: "bad",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.Load(ctx, invalidDigestKey); err != nil || ok {
		t.Fatalf("Load(invalid digest pointer) = %t, %v", ok, err)
	}

	missingKey := stageCacheKey("5")
	if err := cache.metadata.Store(ctx, missingKey, scheduler.Result{
		CacheKey: missingKey, StageID: "go-syntax", ObjectDigest: missingDigest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.Load(ctx, missingKey); err != nil || ok {
		t.Fatalf("Load(missing object pointer) = %t, %v", ok, err)
	}

	corruptObject, err := cache.objects.PutBytes([]byte("first"))
	if err != nil {
		t.Fatal(err)
	}
	corruptPath, err := cache.objects.Path(corruptObject.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(corruptPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, []byte("other"), 0o644); err != nil {
		t.Fatal(err)
	}
	corruptKey := stageCacheKey("6")
	if err := cache.metadata.Store(ctx, corruptKey, scheduler.Result{
		CacheKey: corruptKey, StageID: "go-syntax", ObjectDigest: corruptObject.Digest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := cache.Load(ctx, corruptKey); err != nil || ok {
		t.Fatalf("Load(corrupt object) = %t, %v", ok, err)
	}

	if ok, issue, err := cache.probe(ctx, stageCacheKey("7"), "go-syntax"); ok ||
		issue != "" || err != nil {
		t.Fatalf("probe(miss) = %t, %q, %v", ok, issue, err)
	}
	if ok, issue, err := cache.probe(cancelled, validKey, "go-syntax"); ok ||
		issue != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("probe(cancelled) = %t, %q, %v", ok, issue, err)
	}

	probeCases := []struct {
		character string
		result    scheduler.Result
		stageID   string
		issue     string
	}{
		{"8", scheduler.Result{CacheKey: "wrong", StageID: "go-syntax", ObjectDigest: validObject}, "go-syntax", "metadata cache key mismatch"},
		{"9", scheduler.Result{CacheKey: stageCacheKey("9"), StageID: "other", ObjectDigest: validObject}, "go-syntax", "metadata stage mismatch"},
		{"a", scheduler.Result{CacheKey: stageCacheKey("a"), StageID: "go-syntax", ObjectDigest: "bad"}, "go-syntax", "invalid sha256 digest"},
	}
	for _, test := range probeCases {
		key := stageCacheKey(test.character)
		if err := cache.metadata.Store(ctx, key, test.result); err != nil {
			t.Fatal(err)
		}
		ok, issue, err := cache.probe(ctx, key, test.stageID)
		if ok || err != nil || !strings.Contains(issue, test.issue) {
			t.Fatalf("probe(%s) = %t, %q, %v", test.character, ok, issue, err)
		}
	}

	badPayloadObject := putRawStagePayload(t, cache, []byte("{"))
	badPayloadKey := stageCacheKey("b")
	if err := cache.metadata.Store(ctx, badPayloadKey, scheduler.Result{
		CacheKey: badPayloadKey, StageID: "go-syntax", ObjectDigest: badPayloadObject,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, issue, err := cache.probe(ctx, badPayloadKey, "go-syntax"); ok ||
		err != nil || !strings.Contains(issue, "decode stage payload") {
		t.Fatalf("probe(bad payload) = %t, %q, %v", ok, issue, err)
	}

	otherPayloadObject := putRawStagePayload(t, cache, validStagePayload("other"))
	otherPayloadKey := stageCacheKey("c")
	if err := cache.metadata.Store(ctx, otherPayloadKey, scheduler.Result{
		CacheKey: otherPayloadKey, StageID: "go-syntax", ObjectDigest: otherPayloadObject,
	}); err != nil {
		t.Fatal(err)
	}
	if ok, issue, err := cache.probe(ctx, otherPayloadKey, "go-syntax"); ok ||
		err != nil || issue != "payload stage mismatch" {
		t.Fatalf("probe(stage mismatch) = %t, %q, %v", ok, issue, err)
	}
	if ok, issue, err := cache.probe(ctx, validKey, "go-syntax"); !ok ||
		issue != "" || err != nil {
		t.Fatalf("probe(valid) = %t, %q, %v", ok, issue, err)
	}
}

func TestStageCachePayloadInspectionPruneAndRestoreContracts(t *testing.T) {
	cache := openContractStageCache(t)
	ctx := context.Background()

	payloadCases := []struct {
		name string
		data []byte
		want string
	}{
		{"invalid-json", []byte("{"), "decode stage payload"},
		{"multiple-json", append(validStagePayload("go-syntax"), []byte("\n{}")...), "multiple JSON values"},
		{"trailing-invalid", append(validStagePayload("go-syntax"), []byte("\n{")...), "decode trailing stage payload"},
		{"schema", marshalStagePayload(t, stageFragmentPayload{
			SchemaVersion: "2.0", StageID: "go-syntax", StageVersion: pipelineStageVersion,
		}), "unsupported"},
		{"identity", marshalStagePayload(t, stageFragmentPayload{
			SchemaVersion: stagePayloadSchemaVersion, StageID: "", StageVersion: pipelineStageVersion,
		}), "identity is invalid"},
		{"ownership", marshalStagePayload(t, stageFragmentPayload{
			SchemaVersion: stagePayloadSchemaVersion, StageID: "go-syntax",
			StageVersion: pipelineStageVersion, Fragment: rkcmodel.Fragment{
				Nodes: []rkcmodel.Node{{ID: "node"}},
			},
		}), "ownership does not match"},
	}
	for _, test := range payloadCases {
		t.Run(test.name, func(t *testing.T) {
			digest := putRawStagePayload(t, cache, test.data)
			if _, err := cache.readPayload(digest); err == nil ||
				!strings.Contains(err.Error(), test.want) {
				t.Fatalf("readPayload(%s) = %v", test.name, err)
			}
		})
	}
	validObject := putRawStagePayload(t, cache, validStagePayload("go-syntax"))
	if payload, err := cache.readPayload(validObject); err != nil ||
		payload.StageID != "go-syntax" {
		t.Fatalf("readPayload(valid) = %+v, %v", payload, err)
	}

	inspectEntries := []scheduler.Result{
		{CacheKey: "wrong", StageID: "go-syntax", ObjectDigest: validObject},
		{StageID: "go-syntax", ObjectDigest: "bad"},
		{StageID: "go-syntax", ObjectDigest: strings.Repeat("0", 64)},
	}
	for index, result := range inspectEntries {
		key := stageCacheKey(string(rune('d' + index)))
		if result.CacheKey == "" {
			result.CacheKey = key
		}
		if err := cache.metadata.Store(ctx, key, result); err != nil {
			t.Fatal(err)
		}
	}
	stageMismatchObject := putRawStagePayload(t, cache, validStagePayload("other"))
	stageMismatchKey := stageCacheKey("7")
	if err := cache.metadata.Store(ctx, stageMismatchKey, scheduler.Result{
		CacheKey: stageMismatchKey, StageID: "go-syntax", ObjectDigest: stageMismatchObject,
	}); err != nil {
		t.Fatal(err)
	}
	report, err := cache.Inspect(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if report.Healthy || report.InvalidEntries != 4 || report.OrphanObjects < 2 {
		t.Fatalf("Inspect(invalid entries) = %+v", report)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := cache.Inspect(cancelled, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("Inspect(cancelled) = %v", err)
	}

	pruneCache := openContractStageCache(t)
	oldObject, err := pruneCache.objects.PutBytes([]byte("old"))
	if err != nil {
		t.Fatal(err)
	}
	recentObject, err := pruneCache.objects.PutBytes([]byte("recent"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	oldKey := stageCacheKey("1")
	recentKey := stageCacheKey("2")
	for key, object := range map[string]cas.ObjectInfo{
		oldKey: oldObject, recentKey: recentObject,
	} {
		if err := pruneCache.metadata.Store(ctx, key, scheduler.Result{
			CacheKey: key, StageID: "stage", ObjectDigest: object.Digest,
		}); err != nil {
			t.Fatal(err)
		}
	}
	oldPath := stageMetadataPath(pruneCache, oldKey)
	if err := os.Chtimes(oldPath, now.Add(-2*time.Hour), now.Add(-2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	pruned, err := pruneCache.Prune(ctx, StageCachePruneOptions{
		OlderThan: time.Hour,
		Now:       now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if pruned.EntriesSelected != 1 || pruned.EntriesRemaining != 1 ||
		pruned.ObjectsSelected != 1 {
		t.Fatalf("Prune(aged) = %+v", pruned)
	}
	if present, err := pruneCache.objects.Has(oldObject.Digest); err != nil || present {
		t.Fatalf("old payload remains: %t, %v", present, err)
	}
	if present, err := pruneCache.objects.Has(recentObject.Digest); err != nil || !present {
		t.Fatalf("recent payload missing: %t, %v", present, err)
	}

	state := &stagedScanState{
		opts:      Options{Cache: pruneCache},
		bundle:    rkcmodel.Bundle{},
		parsed:    map[string]struct{}{},
		fragments: map[string]rkcmodel.Fragment{},
	}
	if err := state.restoreFragment(cancelled, "go-syntax", scheduler.Result{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("restoreFragment(cancelled) = %v", err)
	}
	missingArtifactPayload := stageFragmentPayload{
		SchemaVersion: stagePayloadSchemaVersion, StageID: "go-syntax",
		StageVersion: pipelineStageVersion, ParsedArtifactIDs: []string{"missing"},
	}
	missingArtifactObject := putRawStagePayload(
		t,
		pruneCache,
		marshalStagePayload(t, missingArtifactPayload),
	)
	if err := state.restoreFragment(ctx, "go-syntax", scheduler.Result{
		ObjectDigest: missingArtifactObject,
	}); err == nil || !errors.Is(err, scheduler.ErrCacheRejected) {
		t.Fatalf("restoreFragment(missing artifact) = %v", err)
	}
	otherObject := putRawStagePayload(t, pruneCache, validStagePayload("other"))
	if err := state.restoreFragment(ctx, "go-syntax", scheduler.Result{
		ObjectDigest: otherObject,
	}); err == nil || !errors.Is(err, scheduler.ErrCacheRejected) {
		t.Fatalf("restoreFragment(stage mismatch) = %v", err)
	}

	state.bundle.Artifacts = []rkcmodel.Artifact{{ID: "known"}}
	restorePayload := stageFragmentPayload{
		SchemaVersion: stagePayloadSchemaVersion, StageID: "go-syntax",
		StageVersion: pipelineStageVersion, ParsedArtifactIDs: []string{"known"},
		Fragment:  rkcmodel.Fragment{Nodes: []rkcmodel.Node{{ID: "node"}}},
		Ownership: stageOwnership{NodeIDs: []string{"node"}},
	}
	restoreObject := putRawStagePayload(t, pruneCache, marshalStagePayload(t, restorePayload))
	if err := state.restoreFragment(ctx, "go-syntax", scheduler.Result{
		ObjectDigest: restoreObject,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := state.parsed["known"]; !ok || len(state.fragments["go-syntax"].Nodes) != 1 {
		t.Fatalf("restored state = parsed %+v fragments %+v", state.parsed, state.fragments)
	}

	badFragment := rkcmodel.Fragment{
		Nodes: []rkcmodel.Node{{ID: "bad", Attributes: map[string]any{"channel": make(chan int)}}},
	}
	if _, err := state.recordFragment("go-syntax", badFragment, nil, true); err == nil ||
		!strings.Contains(err.Error(), "encode") {
		t.Fatalf("recordFragment(unencodable) = %v", err)
	}
	goodFragment := rkcmodel.Fragment{
		Artifacts: []rkcmodel.Artifact{{ID: "artifact"}},
		Conflicts: []rkcmodel.Conflict{{ID: "conflict"}},
		Claims:    []rkcmodel.Claim{{ID: "claim"}},
		Paths:     []rkcmodel.ExecutionPath{{ID: "path"}},
	}
	result, err := state.recordFragment(
		"framework",
		goodFragment,
		[]pluginapi.FileRef{{ArtifactID: ""}, {ArtifactID: "known"}},
		true,
	)
	if err != nil || result.ObjectDigest == "" {
		t.Fatalf("recordFragment(valid) = %+v, %v", result, err)
	}
	ownership := ownershipForFragment(goodFragment)
	if len(ownership.ArtifactIDs) != 1 || len(ownership.ConflictIDs) != 1 ||
		len(ownership.ClaimIDs) != 1 || len(ownership.PathIDs) != 1 {
		t.Fatalf("ownership = %+v", ownership)
	}
}

func TestStagedFailureContracts(t *testing.T) {
	state := &stagedScanState{
		bundle:    rkcmodel.Bundle{},
		parsed:    map[string]struct{}{},
		fragments: map[string]rkcmodel.Fragment{},
	}
	adapterFailure := errors.New("adapter failed")
	result, err := state.handleFragmentResult(
		"markdown",
		nil,
		rkcmodel.Fragment{},
		adapterFailure,
		"RKC-DOC-2001",
		"rkc.markdown",
		true,
	)
	if err != nil || result.ObjectDigest == "" {
		t.Fatalf("handleFragmentResult(error) = %+v, %v", result, err)
	}
	fragment := state.fragments["markdown"]
	if len(fragment.Diagnostics) != 1 ||
		fragment.Diagnostics[0].Code != "RKC-DOC-2001" {
		t.Fatalf("diagnostic fragment = %+v", fragment)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	stage := state.stage(
		"cancelled",
		nil,
		nil,
		func(context.Context) (scheduler.Result, error) {
			t.Fatal("cancelled stage executed")
			return scheduler.Result{}, nil
		},
	)
	if _, err := stage.Run(cancelled, scheduler.Inputs{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stage = %v", err)
	}

	if _, err := state.runValidate(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "canonical bundle validation failed") {
		t.Fatalf("runValidate(invalid bundle) = %v", err)
	}
}

func TestStageCacheOversizeAndPruneContracts(t *testing.T) {
	cache := openContractStageCache(t)
	if stagePayloadExceedsLimit(maximumStagePayloadBytes) ||
		!stagePayloadExceedsLimit(maximumStagePayloadBytes+1) ||
		!stagePayloadExceedsLimit(-1) {
		t.Fatal("stage payload size boundary is not fail-closed")
	}
	if _, err := cache.Prune(context.Background(), StageCachePruneOptions{}); err == nil ||
		!strings.Contains(err.Error(), "positive age") {
		t.Fatalf("Prune(no selection) = %v", err)
	}

	payload := validStagePayload("go-syntax")
	digest := putRawStagePayload(t, cache, payload)
	key := stageCacheKey("8")
	if err := cache.metadata.Store(context.Background(), key, scheduler.Result{
		CacheKey: key, StageID: "go-syntax", ObjectDigest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	report, err := cache.Inspect(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Healthy || report.ValidEntries != 1 || report.InvalidEntries != 0 {
		t.Fatalf("Inspect(healthy) = %+v", report)
	}
	pruned, err := cache.Prune(context.Background(), StageCachePruneOptions{
		All: true, DryRun: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !pruned.DryRun || pruned.EntriesSelected != 1 || pruned.ObjectsSelected != 1 {
		t.Fatalf("Prune(dry run) = %+v", pruned)
	}
	if _, ok, err := cache.Load(context.Background(), key); err != nil || !ok {
		t.Fatalf("dry-run entry missing: %t, %v", ok, err)
	}
}

func TestSequentialCompilerPortableContract(t *testing.T) {
	if _, _, err := scanSequential(nil, Options{}); err == nil ||
		!strings.Contains(err.Error(), "context is required") {
		t.Fatalf("scanSequential(nil context) = %v", err)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if _, _, err := scanSequential(context.Background(), Options{Root: missing}); err == nil ||
		!strings.Contains(err.Error(), "stat root") {
		t.Fatalf("scanSequential(missing root) = %v", err)
	}

	root := t.TempDir()
	files := map[string]string{
		"main.go": `package example

func Hello() string { return "hello" }
`,
		"app.ts": `export function greet(name: string): string {
  return "hello " + name;
}
`,
		"README.md": `# Example

This repository demonstrates the portable compiler.
`,
		"schema.json":  `{"$schema":"https://json-schema.org/draft/2020-12/schema","type":"object"}`,
		".env.example": "EXAMPLE_TOKEN=\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bundle, coverage, err := scanSequential(context.Background(), Options{
		Root:             root,
		DisablePythonAST: true,
		ToolVersion:      "contract-test",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Artifacts) != len(files) {
		t.Fatalf("artifacts = %d, want %d", len(bundle.Artifacts), len(files))
	}
	if coverage.SnapshotID == "" || coverage.SnapshotID != bundle.Snapshot.ID {
		t.Fatalf("coverage is not bound to snapshot: %+v", coverage)
	}
	kinds := map[string]bool{}
	for _, node := range bundle.Nodes {
		kinds[node.Kind] = true
	}
	if !kinds["function"] || !kinds["document"] {
		t.Fatalf("portable compiler node kinds = %+v", kinds)
	}
}

func openContractStageCache(t *testing.T) *StageCache {
	t.Helper()
	cache, err := OpenStageCache(filepath.Join(t.TempDir(), "stage-cache"))
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func stageCacheKey(character string) string {
	return "stage:" + strings.Repeat(character, 64)
}

func validStagePayload(stageID string) []byte {
	payload := stageFragmentPayload{
		SchemaVersion: stagePayloadSchemaVersion,
		StageID:       stageID,
		StageVersion:  pipelineStageVersion,
		Ownership:     stageOwnership{},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		panic(err)
	}
	return data
}

func marshalStagePayload(t *testing.T, payload stageFragmentPayload) []byte {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func putRawStagePayload(t *testing.T, cache *StageCache, data []byte) string {
	t.Helper()
	object, err := cache.objects.PutBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	return object.Digest
}

func stageMetadataPath(cache *StageCache, key string) string {
	digest := strings.TrimPrefix(key, "stage:")
	return filepath.Join(cache.metadata.Root, digest[:2], digest[2:]+".json")
}
