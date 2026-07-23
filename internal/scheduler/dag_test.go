package scheduler

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type memoryCache struct {
	mu       sync.Mutex
	values   map[string]Result
	loadErr  error
	storeErr error
	loads    int
	stores   int
	invalids int
}

func (cache *memoryCache) Invalidate(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.invalids++
	delete(cache.values, key)
	return nil
}

func (cache *memoryCache) Load(ctx context.Context, key string) (Result, bool, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, false, err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.loads++
	if cache.loadErr != nil {
		return Result{}, false, cache.loadErr
	}
	result, ok := cache.values[key]
	return result, ok, nil
}

func (cache *memoryCache) Store(ctx context.Context, key string, result Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.stores++
	if cache.storeErr != nil {
		return cache.storeErr
	}
	if cache.values == nil {
		cache.values = map[string]Result{}
	}
	cache.values[key] = result
	return nil
}

func TestValidateAndNormalizeRejectsInvalidDAGs(t *testing.T) {
	tests := []struct {
		name   string
		stages []Stage
		want   error
		text   string
	}{
		{name: "missing ID", stages: []Stage{{}}, text: "stage id is required"},
		{name: "duplicate", stages: []Stage{{ID: "a"}, {ID: "a"}}, want: ErrDuplicateStage},
		{name: "missing dependency", stages: []Stage{{ID: "a", Dependencies: []string{"missing"}}}, want: ErrMissingDependency},
		{name: "self cycle", stages: []Stage{{ID: "a", Dependencies: []string{"a"}}}, want: ErrDependencyCycle},
		{name: "multi cycle", stages: []Stage{{ID: "a", Dependencies: []string{"b"}}, {ID: "b", Dependencies: []string{"c"}}, {ID: "c", Dependencies: []string{"a"}}}, want: ErrDependencyCycle},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateAndNormalize(test.stages)
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("validateAndNormalize() = %v, want %v", err, test.want)
			}
			if test.text != "" && (err == nil || !strings.Contains(err.Error(), test.text)) {
				t.Fatalf("validateAndNormalize() = %v, want text %q", err, test.text)
			}
		})
	}
}

func TestValidateAndNormalizeSortsAndDeduplicates(t *testing.T) {
	input := []Stage{
		{ID: "z", Dependencies: []string{"a", "a", "", "b"}, InputDigests: []string{"2", "1", "2", ""}},
		{ID: "b"},
		{ID: "a"},
	}
	got, err := validateAndNormalize(input)
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{got[0].ID, got[1].ID, got[2].ID}
	if !reflect.DeepEqual(ids, []string{"a", "b", "z"}) {
		t.Fatalf("normalized IDs = %v", ids)
	}
	if !reflect.DeepEqual(got[2].Dependencies, []string{"a", "b"}) || !reflect.DeepEqual(got[2].InputDigests, []string{"1", "2"}) {
		t.Fatalf("normalized stage = %+v", got[2])
	}
	if !reflect.DeepEqual(input[0].Dependencies, []string{"a", "a", "", "b"}) {
		t.Fatal("validateAndNormalize mutated caller dependencies")
	}
	if hasCycle(map[string]Stage{"done": {ID: "done"}}) {
		t.Fatal("acyclic graph reported a cycle")
	}
	ready := readyStages(got, map[string]bool{"a": true})
	readyIDs := make([]string, len(ready))
	for index := range ready {
		readyIDs[index] = ready[index].ID
	}
	if !reflect.DeepEqual(readyIDs, []string{"b"}) {
		t.Fatalf("readyStages() = %v, want [b]", readyIDs)
	}
}

func TestExecuteDependenciesEventsValuesAndDeterministicReport(t *testing.T) {
	var callbackMu sync.Mutex
	var callbacks []Event
	values := map[string]any{"request": "value"}
	stages := []Stage{
		{ID: "b", Version: "1", InputDigests: []string{"b-input"}, Run: func(_ context.Context, inputs Inputs) (Result, error) {
			if len(inputs.Results) != 0 || inputs.Values["request"] != "value" {
				t.Errorf("stage b inputs = %+v", inputs)
			}
			return Result{ObjectDigest: "digest-b", Metadata: map[string]any{"stage": "b"}}, nil
		}},
		{ID: "a", Version: "1", InputDigests: []string{"a-input"}, Run: func(_ context.Context, inputs Inputs) (Result, error) {
			if len(inputs.Results) != 0 || inputs.Values["request"] != "value" {
				t.Errorf("stage a inputs = %+v", inputs)
			}
			return Result{ObjectDigest: "digest-a"}, nil
		}},
		{ID: "c", Version: "2", Dependencies: []string{"b", "a", "a"}, Configuration: map[string]any{"mode": "strict"}, Run: func(_ context.Context, inputs Inputs) (Result, error) {
			if len(inputs.Results) != 2 || inputs.Results["a"].ObjectDigest != "digest-a" || inputs.Results["b"].ObjectDigest != "digest-b" {
				t.Errorf("stage c dependency inputs = %+v", inputs.Results)
			}
			return Result{ObjectDigest: "digest-c"}, nil
		}},
	}
	report, err := Execute(context.Background(), stages, Options{
		Workers: 2,
		Values:  values,
		OnEvent: func(event Event) {
			callbackMu.Lock()
			callbacks = append(callbacks, event)
			callbackMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 3 || report.Duration <= 0 {
		t.Fatalf("Execute() report = %+v", report)
	}
	for id, result := range report.Results {
		if result.StageID != id || !strings.HasPrefix(result.CacheKey, "stage:") || result.CacheHit {
			t.Errorf("result %s = %+v", id, result)
		}
	}
	if got := []string{report.Events[0].StageID, report.Events[1].StageID, report.Events[2].StageID}; !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("report event order = %v", got)
	}
	for _, event := range report.Events {
		if event.State != "complete" || event.StartedAt.IsZero() || event.Duration < 0 || event.Error != "" {
			t.Errorf("completion event = %+v", event)
		}
	}
	callbackMu.Lock()
	defer callbackMu.Unlock()
	if len(callbacks) != 6 {
		t.Fatalf("OnEvent callback count = %d, want 6", len(callbacks))
	}
	states := map[string][]string{}
	for _, event := range callbacks {
		states[event.StageID] = append(states[event.StageID], event.State)
	}
	for id, got := range states {
		sort.Strings(got)
		if !reflect.DeepEqual(got, []string{"complete", "running"}) {
			t.Errorf("callback states for %s = %v", id, got)
		}
	}
}

func TestExecuteUsesCacheAndSkipsRunner(t *testing.T) {
	cache := &memoryCache{values: map[string]Result{}}
	var runs atomic.Int32
	var restores atomic.Int32
	stage := Stage{ID: "cached", Version: "v1", InputDigests: []string{"input"}, Run: func(context.Context, Inputs) (Result, error) {
		runs.Add(1)
		return Result{ObjectDigest: "object", Metadata: map[string]any{"kept": true}}, nil
	}, Restore: func(_ context.Context, _ Inputs, result Result) error {
		if result.ObjectDigest != "object" {
			return errors.New("unexpected restored object")
		}
		restores.Add(1)
		return nil
	}}
	first, err := Execute(context.Background(), []Stage{stage}, Options{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	if first.Results["cached"].CacheHit || cache.stores != 1 || runs.Load() != 1 || restores.Load() != 0 {
		t.Fatalf("first cached execution: result=%+v stores=%d runs=%d restores=%d", first.Results["cached"], cache.stores, runs.Load(), restores.Load())
	}
	second, err := Execute(context.Background(), []Stage{stage}, Options{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	result := second.Results["cached"]
	if !result.CacheHit || result.StageID != "cached" || result.ObjectDigest != "object" || runs.Load() != 1 || restores.Load() != 1 || cache.loads != 2 {
		t.Fatalf("cache hit result=%+v loads=%d runs=%d restores=%d", result, cache.loads, runs.Load(), restores.Load())
	}
	if len(second.Events) != 1 || second.Events[0].State != "cached" {
		t.Fatalf("cache hit events = %+v", second.Events)
	}
}

func TestExecuteSupportsDynamicKeysAndSelectiveCacheDisable(t *testing.T) {
	cache := &memoryCache{values: map[string]Result{}}
	var dynamicCalls atomic.Int32
	var runs atomic.Int32
	stage := Stage{
		ID: "dynamic", Version: "v1", Dependencies: []string{"dependency"},
		IgnoreDependencyDigests: true,
		DynamicInputDigests: func(_ context.Context, inputs Inputs) ([]string, error) {
			dynamicCalls.Add(1)
			if inputs.Results["dependency"].ObjectDigest == "" {
				return nil, errors.New("missing dependency result")
			}
			return []string{"selected-input"}, nil
		},
		Run: func(context.Context, Inputs) (Result, error) {
			runs.Add(1)
			return Result{ObjectDigest: "dynamic-output"}, nil
		},
	}
	dependency := Stage{
		ID: "dependency", Version: "v1", DisableCache: true,
		Run: func(context.Context, Inputs) (Result, error) {
			runs.Add(1)
			return Result{ObjectDigest: "changing-dependency-output"}, nil
		},
	}
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := Execute(context.Background(), []Stage{stage, dependency}, Options{Cache: cache}); err != nil {
			t.Fatal(err)
		}
	}
	if dynamicCalls.Load() != 2 || runs.Load() != 3 {
		t.Fatalf("dynamic calls=%d runs=%d, want 2 and 3", dynamicCalls.Load(), runs.Load())
	}
	if cache.loads != 2 || cache.stores != 1 {
		t.Fatalf("cache loads=%d stores=%d, want 2 and 1", cache.loads, cache.stores)
	}
}

func TestExecuteRecomputesRejectedCachePayload(t *testing.T) {
	key := mustCacheKey(t, "rejected", "v1", nil, nil)
	cache := &memoryCache{values: map[string]Result{
		key: {ObjectDigest: "corrupt"},
	}}
	var runs atomic.Int32
	stage := Stage{
		ID: "rejected", Version: "v1",
		Restore: func(context.Context, Inputs, Result) error {
			return fmt.Errorf("%w: corrupt payload", ErrCacheRejected)
		},
		Run: func(context.Context, Inputs) (Result, error) {
			runs.Add(1)
			return Result{ObjectDigest: "recomputed"}, nil
		},
	}
	report, err := Execute(context.Background(), []Stage{stage}, Options{Cache: cache})
	if err != nil {
		t.Fatal(err)
	}
	result := report.Results["rejected"]
	if result.CacheHit || result.ObjectDigest != "recomputed" || runs.Load() != 1 {
		t.Fatalf("rejected cache result = %+v, runs=%d", result, runs.Load())
	}
	if cache.invalids != 1 || cache.stores != 1 {
		t.Fatalf("cache invalidations=%d stores=%d, want 1 and 1", cache.invalids, cache.stores)
	}
}

func TestExecuteErrorPaths(t *testing.T) {
	sentinel := errors.New("runner failed")
	tests := []struct {
		name    string
		stage   Stage
		options Options
		want    string
	}{
		{name: "nil runner", stage: Stage{ID: "nil"}, want: "stage has no runner"},
		{name: "runner", stage: Stage{ID: "run", Run: func(context.Context, Inputs) (Result, error) { return Result{}, sentinel }}, want: "runner failed"},
		{name: "cache load", stage: Stage{ID: "load", Run: func(context.Context, Inputs) (Result, error) { return Result{}, nil }}, options: Options{Cache: &memoryCache{loadErr: errors.New("load failed")}}, want: "load cache: load failed"},
		{name: "cache store", stage: Stage{ID: "store", Run: func(context.Context, Inputs) (Result, error) { return Result{}, nil }}, options: Options{Cache: &memoryCache{storeErr: errors.New("store failed")}}, want: "store cache: store failed"},
		{name: "dynamic inputs", stage: Stage{ID: "dynamic", DynamicInputDigests: func(context.Context, Inputs) ([]string, error) { return nil, errors.New("dynamic failed") }, Run: func(context.Context, Inputs) (Result, error) { return Result{}, nil }}, want: "resolve dynamic input digests: dynamic failed"},
		{name: "cache restore", stage: Stage{ID: "restore", Restore: func(context.Context, Inputs, Result) error { return errors.New("restore failed") }, Run: func(context.Context, Inputs) (Result, error) { return Result{}, nil }}, options: Options{Cache: &memoryCache{values: map[string]Result{mustCacheKey(t, "restore", "", nil, nil): {ObjectDigest: "cached"}}}}, want: "restore cached result: restore failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report, err := Execute(context.Background(), []Stage{test.stage}, test.options)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Execute() error = %v, want %q", err, test.want)
			}
			if len(report.Events) != 1 || report.Events[0].State != "failed" || !strings.Contains(report.Events[0].Error, test.want[strings.LastIndex(test.want, ":")+1:]) {
				t.Fatalf("failed report = %+v", report)
			}
		})
	}
}

func TestExecuteCancellationAndTimeout(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	report, err := Execute(cancelled, []Stage{{ID: "never", Run: func(context.Context, Inputs) (Result, error) {
		t.Fatal("runner called for pre-cancelled context")
		return Result{}, nil
	}}}, Options{})
	if !errors.Is(err, context.Canceled) || len(report.Results) != 0 {
		t.Fatalf("pre-cancelled Execute() = %+v, %v", report, err)
	}

	ctx, stop := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer stop()
	report, err = Execute(ctx, []Stage{{ID: "wait", Run: func(ctx context.Context, _ Inputs) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}}}, Options{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed Execute() error = %v", err)
	}
	if len(report.Events) != 1 || report.Events[0].State != "failed" {
		t.Fatalf("timed Execute() report = %+v", report)
	}
}

func TestExecuteEmptyAndDefaultWorkerCount(t *testing.T) {
	report, err := Execute(context.Background(), nil, Options{Workers: -4})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 0 || len(report.Events) != 0 || report.Results == nil {
		t.Fatalf("Execute(empty) = %+v", report)
	}
}

func TestExecuteAdmitsStagesWithinResourceBudget(t *testing.T) {
	var concurrent atomic.Int32
	var maximum atomic.Int32
	stages := make([]Stage, 0, 4)
	for _, id := range []string{"a", "b", "c", "d"} {
		stages = append(stages, Stage{
			ID: id, Resources: ResourceRequest{
				MemoryMiB: 50, CPU: 1, Processes: 1, OpenFiles: 10, IOClass: "normal",
			},
			Run: func(context.Context, Inputs) (Result, error) {
				current := concurrent.Add(1)
				for {
					observed := maximum.Load()
					if current <= observed || maximum.CompareAndSwap(observed, current) {
						break
					}
				}
				time.Sleep(15 * time.Millisecond)
				concurrent.Add(-1)
				return Result{ObjectDigest: id}, nil
			},
		})
	}
	report, err := Execute(context.Background(), stages, Options{
		Workers: 4,
		Budget: ResourceBudget{
			MemoryMiB: 100, CPU: 2, Processes: 2, OpenFiles: 20,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 4 || maximum.Load() != 2 {
		t.Fatalf("resource-admitted report=%+v maximum concurrency=%d", report, maximum.Load())
	}
}

func TestResourceAdmissionValidationAndOversize(t *testing.T) {
	stage := Stage{
		ID: "oversize", Resources: ResourceRequest{MemoryMiB: 101},
		Run: func(context.Context, Inputs) (Result, error) {
			return Result{}, errors.New("oversized stage executed")
		},
	}
	report, err := Execute(context.Background(), []Stage{stage}, Options{
		Budget: ResourceBudget{MemoryMiB: 100},
	})
	if !errors.Is(err, ErrResourceBudget) || len(report.Results) != 0 {
		t.Fatalf("oversized Execute() = %+v, %v", report, err)
	}
	for _, resources := range []ResourceRequest{
		{MemoryMiB: -1},
		{CPU: -1},
		{Processes: -1},
		{OpenFiles: -1},
		{IOClass: "impossible"},
	} {
		if _, err := validateAndNormalize([]Stage{{ID: "invalid", Resources: resources}}); err == nil {
			t.Errorf("resources %+v were accepted", resources)
		}
	}
	batch, deferred, err := admitStages([]Stage{
		{ID: "a", Resources: ResourceRequest{MemoryMiB: 60}},
		{ID: "b", Resources: ResourceRequest{MemoryMiB: 40}},
		{ID: "c", Resources: ResourceRequest{MemoryMiB: 50}},
	}, 3, ResourceBudget{MemoryMiB: 100})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{batch[0].ID, batch[1].ID}; !reflect.DeepEqual(got, []string{"a", "b"}) ||
		len(deferred) != 1 || deferred[0].ID != "c" {
		t.Fatalf("admitStages batch=%+v deferred=%+v", batch, deferred)
	}
}

func TestCacheKeyIsDeterministicAndDoesNotMutateInputs(t *testing.T) {
	digests := []string{"z", "a", "m"}
	configuration := map[string]any{"enabled": true, "count": float64(2)}
	first, err := CacheKey("stage", "1", digests, configuration)
	if err != nil {
		t.Fatal(err)
	}
	second, err := CacheKey("stage", "1", []string{"m", "z", "a"}, map[string]any{"count": float64(2), "enabled": true})
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.HasPrefix(first, "stage:") || len(first) != len("stage:")+64 {
		t.Fatalf("CacheKey() = %q and %q", first, second)
	}
	if !reflect.DeepEqual(digests, []string{"z", "a", "m"}) {
		t.Fatalf("CacheKey mutated input: %v", digests)
	}
	differentVersion, err := CacheKey("stage", "2", digests, configuration)
	if err != nil {
		t.Fatal(err)
	}
	differentStage, err := CacheKey("other", "1", digests, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if first == differentVersion || first == differentStage {
		t.Fatal("CacheKey failed to bind stage identity/version")
	}
	if got := uniqueSorted([]string{" b ", "a", "a", ""}); !reflect.DeepEqual(got, []string{" b ", "a"}) {
		// Scheduler digests are opaque, so whitespace is retained while empty
		// values and exact duplicates are removed.
		t.Fatalf("uniqueSorted() = %q", got)
	}
	var events int
	emit(func(Event) { events++ }, Event{})
	emit(nil, Event{})
	if events != 1 {
		t.Fatalf("emit callback count = %d", events)
	}
}

func TestCacheKeyRejectsUnserializableConfiguration(t *testing.T) {
	if _, err := CacheKey("stage", "1", nil, map[string]any{"channel": make(chan int)}); err == nil {
		t.Fatal("CacheKey accepted an unserializable configuration")
	}

	stage := Stage{
		ID:            "invalid-configuration",
		Version:       "1",
		Configuration: map[string]any{"channel": make(chan int)},
		Run: func(context.Context, Inputs) (Result, error) {
			return Result{}, errors.New("stage with an invalid cache-key configuration was executed")
		},
	}
	if _, err := Execute(context.Background(), []Stage{stage}, Options{}); err == nil || !strings.Contains(err.Error(), "compute cache key") {
		t.Fatalf("Execute invalid configuration error = %v", err)
	}
}

func TestExecuteRejectsNilContext(t *testing.T) {
	if _, err := Execute(nil, nil, Options{}); err == nil || !strings.Contains(err.Error(), "context is required") {
		t.Fatalf("Execute nil context error = %v", err)
	}
}

func mustCacheKey(t *testing.T, stageID, version string, inputDigests []string, configuration any) string {
	t.Helper()
	key, err := CacheKey(stageID, version, inputDigests, configuration)
	if err != nil {
		t.Fatal(err)
	}
	return key
}
