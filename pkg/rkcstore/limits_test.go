package rkcstore

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/neuroforge-io/RKC/pkg/rkcmodel"
)

func requireResourceExhausted(t *testing.T, err error, field string) *OperationError {
	t.Helper()
	var operation *OperationError
	if !errors.Is(err, ErrResourceExhausted) || !errors.As(err, &operation) ||
		operation.Code != CodeResourceExhausted || operation.Field != field {
		t.Fatalf("resource error = %#v, want field %q", err, field)
	}
	return operation
}

func encodedSize(t *testing.T, value any) int64 {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return int64(len(data))
}

func newStoreWithOptions(t *testing.T, options MemoryOptions) *MemoryStore {
	t.Helper()
	store, err := NewMemoryStoreWithOptions(options)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestMemoryOptionsValidationAndDefaults(t *testing.T) {
	defaults := DefaultMemoryOptions()
	store, err := NewMemoryStore()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(store.options, defaults) {
		t.Fatalf("default options = %+v, want %+v", store.options, defaults)
	}

	tests := []struct {
		name   string
		field  string
		mutate func(*MemoryOptions)
	}{
		{"metadata keys", "max_metadata_keys", func(value *MemoryOptions) { value.MaxMetadataKeys = 0 }},
		{"metadata bytes", "max_metadata_bytes", func(value *MemoryOptions) { value.MaxMetadataBytes = -1 }},
		{"open builds", "max_open_builds", func(value *MemoryOptions) { value.MaxOpenBuilds = 0 }},
		{"record bytes", "max_record_bytes", func(value *MemoryOptions) { value.MaxRecordBytes = 0 }},
		{"batch bytes", "max_batch_bytes", func(value *MemoryOptions) { value.MaxBatchBytes = 0 }},
		{"build records", "max_build_records", func(value *MemoryOptions) { value.MaxBuildRecords = 0 }},
		{"build bytes", "max_build_bytes", func(value *MemoryOptions) { value.MaxBuildBytes = 0 }},
		{"closed builds", "max_closed_build_tombstones", func(value *MemoryOptions) { value.MaxClosedBuildTombstones = 0 }},
		{"record exceeds batch", "max_record_bytes", func(value *MemoryOptions) { value.MaxRecordBytes = value.MaxBatchBytes + 1 }},
		{"batch exceeds build", "max_batch_bytes", func(value *MemoryOptions) { value.MaxBatchBytes = value.MaxBuildBytes + 1 }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options := defaults
			test.mutate(&options)
			_, err := NewMemoryStoreWithOptions(options)
			var operation *OperationError
			if !errors.Is(err, ErrInvalidArgument) || !errors.As(err, &operation) ||
				operation.Code != CodeInvalidArgument || operation.Field != test.field {
				t.Fatalf("option error = %#v, want invalid %q", err, test.field)
			}
		})
	}
}

func TestMemoryMetadataAndOpenBuildLimits(t *testing.T) {
	options := DefaultMemoryOptions()
	options.MaxMetadataKeys = 1
	options.MaxMetadataBytes = 4
	options.MaxOpenBuilds = 1
	store := newStoreWithOptions(t, options)
	ctx := context.Background()

	if _, err := store.BeginBuild(ctx, BuildOptions{
		RepositoryID: "repo", Metadata: map[string]string{"a": "", "b": ""},
	}); err == nil {
		t.Fatal("metadata key limit was not enforced")
	} else {
		requireResourceExhausted(t, err, "metadata")
	}
	if _, err := store.BeginBuild(ctx, BuildOptions{
		RepositoryID: "repo", Metadata: map[string]string{"abc": "12"},
	}); err == nil {
		t.Fatal("metadata byte limit was not enforced")
	} else {
		requireResourceExhausted(t, err, "metadata")
	}
	if len(store.builds) != 0 || store.openBuilds != 0 {
		t.Fatal("rejected metadata created a build")
	}

	metadata := map[string]string{"a": "123"}
	build, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "repo", Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	metadata["a"] = "mutated"
	if store.builds[build].options.Metadata["a"] != "123" {
		t.Fatal("build metadata aliases caller state")
	}
	_, err = store.BeginBuild(ctx, BuildOptions{RepositoryID: "other"})
	operation := requireResourceExhausted(t, err, "builds")
	if operation.BuildID != "" {
		t.Fatalf("open-build error unexpectedly names build %q", operation.BuildID)
	}
	if err := store.Abort(ctx, build, errors.New("reason-too-large")); err != nil {
		t.Fatal(err)
	}
	if store.openBuilds != 0 || store.builds[build].abortReason != "reas" {
		t.Fatalf("closed build accounting = %d, reason %q", store.openBuilds, store.builds[build].abortReason)
	}
	if _, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "other"}); err != nil {
		t.Fatalf("closed build did not release capacity: %v", err)
	}
}

func TestMemoryRecordAndBatchByteLimitsAreAtomic(t *testing.T) {
	ctx := context.Background()
	nodeA := rkcmodel.Node{ID: "node-a", Kind: "function", Name: strings.Repeat("a", 64)}
	nodeB := rkcmodel.Node{ID: "node-b", Kind: "function", Name: strings.Repeat("b", 64)}
	sizeA, sizeB := encodedSize(t, nodeA), encodedSize(t, nodeB)

	t.Run("record bytes", func(t *testing.T) {
		options := DefaultMemoryOptions()
		options.MaxRecordBytes = sizeA - 1
		store := newStoreWithOptions(t, options)
		build, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "repo"})
		if err != nil {
			t.Fatal(err)
		}
		err = store.PutNodes(ctx, build, []rkcmodel.Node{nodeA})
		requireResourceExhausted(t, err, "values")
		if len(store.builds[build].nodes) != 0 || store.builds[build].recordCount != 0 || store.builds[build].recordBytes != 0 {
			t.Fatal("record-limit rejection mutated the build")
		}
	})

	t.Run("batch bytes", func(t *testing.T) {
		options := DefaultMemoryOptions()
		options.MaxRecordBytes = maxInt64(sizeA, sizeB)
		options.MaxBatchBytes = sizeA + sizeB - 1
		store := newStoreWithOptions(t, options)
		build, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "repo"})
		if err != nil {
			t.Fatal(err)
		}
		err = store.PutNodes(ctx, build, []rkcmodel.Node{nodeA, nodeB})
		requireResourceExhausted(t, err, "values")
		if len(store.builds[build].nodes) != 0 || store.builds[build].recordCount != 0 || store.builds[build].recordBytes != 0 {
			t.Fatal("batch-limit rejection partially inserted records")
		}
	})

	t.Run("batch count", func(t *testing.T) {
		store := newStoreWithOptions(t, DefaultMemoryOptions())
		build, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "repo"})
		if err != nil {
			t.Fatal(err)
		}
		err = store.PutNodes(ctx, build, make([]rkcmodel.Node, MaxBatchSize+1))
		requireResourceExhausted(t, err, "values")
		if store.builds[build].recordCount != 0 {
			t.Fatal("batch-count rejection changed accounting")
		}
	})
}

func TestMemoryCumulativeBuildLimitsAreAtomic(t *testing.T) {
	ctx := context.Background()
	artifact := rkcmodel.Artifact{ID: "artifact", Path: "main.go", Kind: "file", Status: "parsed", Text: true}
	nodeA := rkcmodel.Node{ID: "node-a", Kind: "function", Name: "Alpha"}
	nodeB := rkcmodel.Node{ID: "node-b", Kind: "function", Name: "Beta"}

	t.Run("records", func(t *testing.T) {
		options := DefaultMemoryOptions()
		options.MaxBuildRecords = 2
		store := newStoreWithOptions(t, options)
		build, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "repo"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutArtifacts(ctx, build, []rkcmodel.Artifact{artifact}); err != nil {
			t.Fatal(err)
		}
		err = store.PutNodes(ctx, build, []rkcmodel.Node{nodeA, nodeB})
		requireResourceExhausted(t, err, "values")
		if len(store.builds[build].nodes) != 0 || store.builds[build].recordCount != 1 {
			t.Fatal("record-budget rejection partially inserted a batch")
		}
		if err := store.PutNodes(ctx, build, []rkcmodel.Node{nodeA}); err != nil {
			t.Fatalf("exact record budget failed: %v", err)
		}
	})

	t.Run("bytes", func(t *testing.T) {
		artifactBytes, nodeBytes := encodedSize(t, artifact), encodedSize(t, nodeA)
		options := DefaultMemoryOptions()
		options.MaxRecordBytes = maxInt64(artifactBytes, nodeBytes)
		options.MaxBatchBytes = options.MaxRecordBytes
		options.MaxBuildBytes = artifactBytes + nodeBytes - 1
		store := newStoreWithOptions(t, options)
		build, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "repo"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.PutArtifacts(ctx, build, []rkcmodel.Artifact{artifact}); err != nil {
			t.Fatal(err)
		}
		err = store.PutNodes(ctx, build, []rkcmodel.Node{nodeA})
		operation := requireResourceExhausted(t, err, "values")
		if operation.BuildID != build {
			t.Fatalf("build-budget error build = %q, want %q", operation.BuildID, build)
		}
		staged := store.builds[build]
		if len(staged.nodes) != 0 || staged.recordCount != 1 || staged.recordBytes != artifactBytes {
			t.Fatalf("byte-budget rejection mutated build: %+v", staged)
		}
	})
}

func TestMemoryCoverageParticipatesInAllResourceBudgets(t *testing.T) {
	ctx := context.Background()
	artifact := rkcmodel.Artifact{ID: "artifact", Path: "main.go", Kind: "file", Status: "parsed", Text: true}
	first := rkcmodel.Coverage{SnapshotID: "first", NodeKinds: map[string]int{"function": 1}}
	second := rkcmodel.Coverage{SnapshotID: strings.Repeat("second", 20), NodeKinds: map[string]int{"function": 2}}
	artifactBytes, secondBytes := encodedSize(t, artifact), encodedSize(t, second)

	options := DefaultMemoryOptions()
	options.MaxRecordBytes = maxInt64(artifactBytes, secondBytes)
	options.MaxBatchBytes = options.MaxRecordBytes
	options.MaxBuildBytes = options.MaxRecordBytes + artifactBytes - 1
	store := newStoreWithOptions(t, options)
	build, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.PutArtifacts(ctx, build, []rkcmodel.Artifact{artifact}); err != nil {
		t.Fatal(err)
	}
	if err := store.PutCoverage(ctx, build, first); err != nil {
		t.Fatal(err)
	}
	before := store.builds[build]
	beforeCount, beforeBytes, beforeCoverageBytes := before.recordCount, before.recordBytes, before.coverageBytes
	err = store.PutCoverage(ctx, build, second)
	requireResourceExhausted(t, err, "values")
	after := store.builds[build]
	if !reflect.DeepEqual(*after.coverage, first) || after.recordCount != beforeCount ||
		after.recordBytes != beforeBytes || after.coverageBytes != beforeCoverageBytes {
		t.Fatal("rejected coverage replacement mutated staged coverage or accounting")
	}

	recordOptions := DefaultMemoryOptions()
	recordOptions.MaxBuildRecords = 1
	recordStore := newStoreWithOptions(t, recordOptions)
	recordBuild, err := recordStore.BeginBuild(ctx, BuildOptions{RepositoryID: "repo"})
	if err != nil {
		t.Fatal(err)
	}
	if err := recordStore.PutArtifacts(ctx, recordBuild, []rkcmodel.Artifact{artifact}); err != nil {
		t.Fatal(err)
	}
	err = recordStore.PutCoverage(ctx, recordBuild, first)
	requireResourceExhausted(t, err, "values")
	if recordStore.builds[recordBuild].coverage != nil || recordStore.builds[recordBuild].recordCount != 1 {
		t.Fatal("coverage record-limit rejection mutated build")
	}
}

func TestMemoryClosedBuildTombstonesAreDeterministicAndBounded(t *testing.T) {
	options := DefaultMemoryOptions()
	options.MaxOpenBuilds = 2
	options.MaxClosedBuildTombstones = 2
	store := newStoreWithOptions(t, options)
	ctx := context.Background()

	open, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "open"})
	if err != nil {
		t.Fatal(err)
	}
	closed := make([]BuildID, 0, 3)
	for index := 0; index < 3; index++ {
		id, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: RepositoryID("closed-" + string(rune('a'+index)))})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Abort(ctx, id, nil); err != nil {
			t.Fatal(err)
		}
		closed = append(closed, id)
	}
	if _, ok := store.builds[open]; !ok || store.builds[open].state != buildOpen {
		t.Fatal("closed tombstone retention evicted the open build")
	}
	if _, ok := store.builds[closed[0]]; ok {
		t.Fatal("oldest closed tombstone was retained beyond the limit")
	}
	if !reflect.DeepEqual(store.closedBuildOrder, closed[1:]) || len(store.builds) != 3 || store.openBuilds != 1 {
		t.Fatalf("tombstone state = order %v, builds %d, open %d", store.closedBuildOrder, len(store.builds), store.openBuilds)
	}
	if err := store.Abort(ctx, closed[0], nil); !errors.Is(err, ErrBuildNotFound) {
		t.Fatalf("evicted tombstone error = %v", err)
	}
	for _, id := range closed[1:] {
		if err := store.Abort(ctx, id, nil); err != nil {
			t.Fatalf("retained tombstone %q lost idempotence: %v", id, err)
		}
	}
}

func TestMemoryRecoverAndCommitReleaseOpenBuildCapacity(t *testing.T) {
	ctx := context.Background()
	options := DefaultMemoryOptions()
	options.MaxOpenBuilds = 1
	options.MaxClosedBuildTombstones = 1
	store := newStoreWithOptions(t, options)

	first, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "recover"})
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := store.Recover(ctx)
	if err != nil || !reflect.DeepEqual(recovered.AbortedBuilds, []BuildID{first}) || store.openBuilds != 0 {
		t.Fatalf("Recover = %+v, %v, open=%d", recovered, err, store.openBuilds)
	}
	if _, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "after-recover"}); err != nil {
		t.Fatalf("Recover did not release open-build capacity: %v", err)
	}

	commitStore := newStoreWithOptions(t, options)
	bundle := conformanceBundle("snapshot", "repo", "", time.Unix(1, 0).UTC())
	build := beginAndStage(t, commitStore, bundle, true)
	if err := commitStore.Commit(ctx, build, bundle.Snapshot); err != nil {
		t.Fatal(err)
	}
	if commitStore.openBuilds != 0 {
		t.Fatalf("Commit left %d open builds", commitStore.openBuilds)
	}
	if _, err := commitStore.BeginBuild(ctx, BuildOptions{RepositoryID: "repo", ParentSnapshotID: "snapshot"}); err != nil {
		t.Fatalf("Commit did not release open-build capacity: %v", err)
	}
}

func TestMemoryRecoverTombstoneRetentionOrderIsStable(t *testing.T) {
	ctx := context.Background()
	options := DefaultMemoryOptions()
	options.MaxOpenBuilds = 2
	options.MaxClosedBuildTombstones = 1
	store := newStoreWithOptions(t, options)
	left, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "left"})
	if err != nil {
		t.Fatal(err)
	}
	right, err := store.BeginBuild(ctx, BuildOptions{RepositoryID: "right"})
	if err != nil {
		t.Fatal(err)
	}
	want := []BuildID{left, right}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	result, err := store.Recover(ctx)
	if err != nil || !reflect.DeepEqual(result.AbortedBuilds, want) {
		t.Fatalf("Recover = %+v, %v, want %v", result, err, want)
	}
	if _, ok := store.builds[want[0]]; ok {
		t.Fatalf("deterministic oldest recovered tombstone %q was retained", want[0])
	}
	if build, ok := store.builds[want[1]]; !ok || build.state != buildAborted {
		t.Fatalf("deterministic newest recovered tombstone %q was not retained", want[1])
	}
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
